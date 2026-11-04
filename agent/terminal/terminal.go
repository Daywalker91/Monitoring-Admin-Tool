// Package terminal stellt das Web-Terminal des Agenten bereit (M11).
//
// Das Terminal ist die einzige freie Shell in MAT und deshalb eng begrenzt:
// freigeschaltet nur über den Block "terminal" in agent.json, die Shell läuft als
// der dort eingetragene, unprivilegierte Benutzer (nie root), Leerlauf- und
// Gesamtzeit sind begrenzt, und es laufen höchstens max_sessions Sitzungen.
// Eingaben und Ausgaben werden weder gespeichert noch geloggt.
//
// Unter Linux startet systemd die Shell als eigene, kurzlebige Unit
// (systemd-run --pty --uid=…): Sie erbt so nicht die Sperren der Agent-Unit
// (NoNewPrivileges, ProtectHome), sudo funktioniert wie bei SSH, und die Unit
// lässt sich samt aller Kindprozesse sauber beenden.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Proc ist eine laufende Shell an einem PTY.
type Proc interface {
	io.ReadWriter // Ausgabe lesen (Fehler/EOF = Shell beendet), Eingabe schreiben
	Resize(cols, rows int) error
	// Close beendet die Shell samt Kindprozessen; mehrfacher Aufruf ist erlaubt.
	Close() error
}

// Spawner startet Shells.
type Spawner interface {
	Start(sessionID string, cols, rows int) (Proc, error)
}

// Conn ist die Terminal-Verbindung zum Server (*websocket.Conn).
type Conn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

// Eingabe-Rate: genug für Tippen und Einfügen, aber kein Fluten des PTY.
const (
	inputRate  = 64 << 10  // Bytes pro Sekunde
	inputBurst = 256 << 10 // auf einmal
	readBuf    = 16 << 10
	exitWait   = 2 * time.Second // so lange darf die Abschlussnachricht dauern
)

// Gründe für das Ende einer Sitzung (gehen an Browser und Audit-Log).
const (
	ReasonShellExit = "shell beendet"
	ReasonIdle      = "leerlauf"
	ReasonMaxTime   = "zeitlimit erreicht"
	ReasonClosed    = "verbindung getrennt"
	ReasonShutdown  = "agent wird beendet"
)

// Manager verwaltet die Sitzungen eines Agenten.
type Manager struct {
	user        string
	maxSessions int
	idle        time.Duration
	maxDuration time.Duration
	spawner     Spawner
	log         *slog.Logger

	active atomic.Int32
	ctx    context.Context // endet mit Shutdown
	stop   context.CancelFunc
	wg     sync.WaitGroup
}

// newManager erzeugt einen Manager für eine geprüfte Konfiguration.
func newManager(cfg *config.Terminal, sp Spawner, log *slog.Logger) *Manager {
	ctx, stop := context.WithCancel(context.Background())
	return &Manager{user: cfg.User, maxSessions: cfg.MaxSessions,
		idle: time.Duration(cfg.IdleTimeoutSec) * time.Second, maxDuration: time.Duration(cfg.MaxDurationSec) * time.Second,
		spawner: sp, log: log, ctx: ctx, stop: stop}
}

// User liefert den Benutzer, als der die Shell läuft.
func (m *Manager) User() string { return m.user }

// Shutdown beendet alle Sitzungen und wartet kurz darauf.
func (m *Manager) Shutdown() {
	m.stop()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		m.log.Warn("terminal-sitzungen enden nicht rechtzeitig")
	}
}

// Serve führt eine Sitzung über conn aus, bis sie endet. ctx ist die Lebensdauer
// des Agenten.
func (m *Manager) Serve(ctx context.Context, req proto.TerminalOpen, conn Conn) {
	m.wg.Add(1)
	defer m.wg.Done()
	log := m.log.With("session_id", req.SessionID, "gui_user", req.User)

	if n := m.active.Add(1); int(n) > m.maxSessions {
		m.active.Add(-1)
		log.Warn("terminal abgelehnt: zu viele sitzungen", "max_sessions", m.maxSessions)
		Refuse(ctx, conn, fmt.Sprintf("höchstens %d gleichzeitige sitzung(en) auf diesem host", m.maxSessions))
		return
	}
	defer m.active.Add(-1)

	proc, err := m.spawner.Start(req.SessionID, req.Cols, req.Rows)
	if err != nil {
		log.Error("terminal starten", "err", err)
		Refuse(ctx, conn, "shell konnte nicht gestartet werden")
		return
	}
	log.Info("terminal geöffnet", "user", m.user)
	started := time.Now()
	reason := m.run(ctx, proc, conn)
	proc.Close()
	sendExit(conn, reason)
	conn.Close(websocket.StatusNormalClosure, "")
	log.Info("terminal beendet", "reason", reason, "duration", time.Since(started).Round(time.Second))
}

// run leitet Daten weiter, bis die Shell endet, die Verbindung abbricht oder ein
// Zeitlimit greift, und liefert den Grund. Lesen und Schreiben laufen unter ioCtx
// (Lebensdauer des Agenten): coder/websocket schließt die Verbindung, wenn der
// Kontext eines Reads endet, die Abschlussnachricht muss aber noch hinaus. Die
// Leser enden, sobald Serve die Shell und die Verbindung schließt.
func (m *Manager) run(ioCtx context.Context, proc Proc, conn Conn) string {
	ctx, cancel := context.WithCancel(ioCtx)
	defer cancel()
	var once sync.Once
	var reason string
	end := func(r string) {
		once.Do(func() { reason = r })
		cancel()
	}
	var lastInput atomic.Int64
	lastInput.Store(time.Now().UnixNano())

	go func() { // Shell → Browser
		buf := make([]byte, readBuf)
		for {
			n, err := proc.Read(buf)
			if n > 0 {
				if werr := conn.Write(ioCtx, websocket.MessageBinary, buf[:n]); werr != nil {
					end(ReasonClosed)
					return
				}
			}
			if err != nil {
				end(ReasonShellExit)
				return
			}
		}
	}()
	go func() { // Browser → Shell
		lim := newLimiter(inputRate, inputBurst)
		for {
			typ, data, err := conn.Read(ioCtx)
			if err != nil {
				end(ReasonClosed)
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if len(data) > proto.TerminalMaxFrame || lim.wait(ctx, len(data)) != nil {
					end(ReasonClosed)
					return
				}
				lastInput.Store(time.Now().UnixNano())
				if ctx.Err() != nil {
					return
				}
				if _, err := proc.Write(data); err != nil {
					end(ReasonShellExit)
					return
				}
			case websocket.MessageText:
				var ctl proto.TerminalControl
				// Nur resize; "close" endet schon im Server.
				if json.Unmarshal(data, &ctl) != nil || ctl.Validate(false) != nil || ctl.Type != proto.TerminalResize {
					continue // unbekannte Steuernachrichten ignorieren
				}
				if err := proc.Resize(ctl.Cols, ctl.Rows); err != nil {
					m.log.Debug("terminalgröße ändern", "err", err)
				}
			}
		}
	}()

	maxTimer := time.NewTimer(m.maxDuration)
	defer maxTimer.Stop()
	tick := time.NewTicker(min(m.idle/4, 5*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			end(ReasonClosed) // greift nur, wenn noch kein Grund feststeht
			return reason
		case <-m.ctx.Done():
			end(ReasonShutdown)
		case <-maxTimer.C:
			end(ReasonMaxTime)
		case <-tick.C:
			if time.Since(time.Unix(0, lastInput.Load())) >= m.idle {
				end(ReasonIdle)
			}
		}
	}
}

// Refuse lehnt eine Sitzung mit Grund ab und schließt die Verbindung.
func Refuse(ctx context.Context, conn Conn, reason string) {
	sendExit(conn, reason)
	conn.Close(websocket.StatusNormalClosure, "")
}

func sendExit(conn Conn, reason string) {
	data, err := json.Marshal(proto.TerminalControl{Type: proto.TerminalExit, Reason: proto.Truncate(reason, 200)})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), exitWait)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
}

// limiter ist ein einfacher Token-Bucket für die Eingabe.
type limiter struct {
	rate, burst float64
	tokens      float64
	last        time.Time
}

func newLimiter(rate, burst int) *limiter {
	return &limiter{rate: float64(rate), burst: float64(burst), tokens: float64(burst), last: time.Now()}
}

// wait wartet, bis n Bytes erlaubt sind (gebremst statt verworfen).
func (l *limiter) wait(ctx context.Context, n int) error {
	now := time.Now()
	l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	l.tokens -= float64(n)
	if l.tokens >= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(-l.tokens / l.rate * float64(time.Second))):
		return nil
	}
}

// errUnsupported: auf dieser Plattform gibt es kein Terminal.
var errUnsupported = errors.New("web-terminal gibt es nur unter linux mit systemd")
