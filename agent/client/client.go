// Package client hält die ausgehende WebSocket-Verbindung zum Server und
// sendet die Messwerte des Collectors.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/collector"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

const (
	backoffMin = time.Second
	backoffMax = time.Minute
	// replacedWait gilt, wenn der Server die Verbindung beendet, weil ein anderer
	// Agent dieselbe ID benutzt. So verdrängen sich zwei Agenten nicht im Sekundentakt.
	replacedWait = 5 * time.Minute
	writeTimeout = 10 * time.Second
	dialTimeout  = 15 * time.Second
	// stableAfter: so lange muss eine Verbindung halten, bis sie als stabil gilt
	// (bestätigt u. a. ein Self-Update).
	stableAfter = time.Minute
	// maxPending begrenzt Job-Ergebnisse, die auf die nächste Verbindung warten.
	maxPending = 8
	// Dienste: jede Minute prüfen, bei Änderung senden, sonst alle 5 Minuten.
	servicesCheck = time.Minute
	servicesFull  = 5 * time.Minute
	// Update-Stand: alle 5 Minuten abfragen (apt läuft im Checker seltener),
	// bei Änderung senden, sonst stündlich.
	updatesCheck = 5 * time.Minute
	updatesFull  = time.Hour
)

// UpdateChecker liefert den Update-Stand des Hosts.
type UpdateChecker func(ctx context.Context) proto.Updates

// ServiceLister liest die Dienste des Hosts.
type ServiceLister func(ctx context.Context) ([]proto.Service, error)

// ErrRestart beendet Run, weil ein Job (z. B. agent.update) einen Neustart verlangt.
var ErrRestart = errors.New("neustart angefordert")

// JobFunc führt eine Aktion aus. params sind bereits auf Form und Größe geprüft,
// die Aktion prüft ihre Werte selbst. restart verlangt nach erfolgreicher
// Rückmeldung einen Neustart des Agenten.
type JobFunc func(ctx context.Context, params map[string]string) (output string, restart bool, err error)

// Client verbindet den Agenten mit dem Server.
type Client struct {
	cfg        config.Config
	hello      proto.Hello
	col        *collector.Collector
	log        *slog.Logger
	httpClient *http.Client // nil = Standard (System-Zertifikate)

	services ServiceLister      // nil = keine Dienstliste
	svcNow   chan struct{}      // Dienstliste sofort prüfen (nach einem Dienst-Job)
	updates  UpdateChecker      // nil = kein Update-Stand
	updNow   chan struct{}      // Update-Stand sofort prüfen (nach einem Paket-Job)
	jobs     map[string]JobFunc // erlaubte Aktionen; nur mit gemeldeter Fähigkeit
	busy     atomic.Bool        // es läuft immer höchstens ein ändernder Job …
	readBusy atomic.Bool        // … und daneben höchstens ein lesender (z. B. log.read)
	onStable func()

	// Jobs laufen unabhängig von der Verbindung weiter (ein Paket-Upgrade soll
	// nicht abbrechen, weil das WLAN kurz weg ist). Ergebnisse gehen über die
	// jeweils aktuelle Verbindung oder warten auf die nächste.
	mu          sync.Mutex
	conn        *websocket.Conn // aktuelle Verbindung nach dem hello, nil = getrennt
	pending     []proto.JobResult
	restart     chan struct{} // geschlossen, sobald ein Job einen Neustart verlangt
	restartOnce sync.Once
}

// New erzeugt einen Client. Ein Fehler entsteht nur bei ungültiger TLS-Konfiguration.
func New(cfg config.Config, hello proto.Hello, col *collector.Collector, log *slog.Logger) (*Client, error) {
	c := &Client{cfg: cfg, hello: hello, col: col, log: log, restart: make(chan struct{}),
		svcNow: make(chan struct{}, 1), updNow: make(chan struct{}, 1)}
	tlsCfg, err := cfg.TLSConfig()
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		c.httpClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	}
	return c, nil
}

// HTTPClient liefert den HTTP-Client mit derselben TLS-Konfiguration wie die
// WebSocket-Verbindung (nil = Standard).
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// HandleJob erlaubt eine Aktion. Der Agent muss die passende Fähigkeit im
// hello melden, sonst wird die Aktion abgelehnt.
func (c *Client) HandleJob(action string, fn JobFunc) {
	if c.jobs == nil {
		c.jobs = make(map[string]JobFunc)
	}
	c.jobs[action] = fn
}

// WithServices sendet die Dienstliste des Hosts.
func (c *Client) WithServices(fn ServiceLister) { c.services = fn }

// WithUpdates sendet den Update-Stand des Hosts.
func (c *Client) WithUpdates(fn UpdateChecker) { c.updates = fn }

// OnStable wird einmal aufgerufen, sobald eine Verbindung stableAfter gehalten hat.
func (c *Client) OnStable(fn func()) { c.onStable = fn }

// Run verbindet sich und hält die Verbindung, bis ctx beendet wird (Rückgabe
// nil) oder ein Job einen Neustart verlangt (ErrRestart). Bei Fehlern wird mit
// exponentiellem Backoff (mit Jitter) neu verbunden.
func (c *Client) Run(ctx context.Context) error {
	backoff := backoffMin
	for {
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRestart) {
			return ErrRestart
		}
		if connected {
			backoff = backoffMin // nach erfolgreicher Verbindung wieder schnell neu versuchen
		}
		wait := jitter(backoff)
		if websocket.CloseStatus(err) == proto.CloseReplaced {
			wait = jitter(replacedWait)
			c.log.Error("server hat die verbindung ersetzt: agent-id wird bereits von einem anderen agenten verwendet",
				"agent_id", c.hello.AgentID, "retry_in", wait.Round(time.Second))
		} else {
			c.log.Warn("verbindung zum server getrennt", "err", err, "retry_in", wait.Round(time.Millisecond))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// session baut eine Verbindung auf und sendet, bis sie abbricht.
// connected meldet, ob der Verbindungsaufbau gelungen ist.
func (c *Client) session(ctx context.Context) (connected bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.Token)
	conn, resp, err := websocket.Dial(dialCtx, c.cfg.ServerURL, &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: c.httpClient,
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return false, errors.New("server lehnt token ab (401)")
		}
		return false, fmt.Errorf("verbinden: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(proto.MaxMessageBytes)
	c.log.Info("mit server verbunden", "url", c.cfg.ServerURL)

	// Der Leser verarbeitet Ping/Pong/Close und Jobs im Hintergrund, merkt sich
	// den Grund des Endes und beendet sessCtx.
	sessCtx, cancelSess := context.WithCancel(ctx)
	defer cancelSess()
	readDone := make(chan struct{})
	var readErr error
	go func() {
		defer close(readDone)
		defer cancelSess()
		for {
			typ, data, err := conn.Read(sessCtx)
			if err != nil {
				readErr = err
				return
			}
			if typ == websocket.MessageText {
				c.handleMessage(ctx, sessCtx, conn, data)
			}
		}
	}()
	// fail meldet einen Schreibfehler. Hat der Server die Verbindung mit einem
	// Close-Code beendet, ist das der eigentliche Grund und wird bevorzugt.
	fail := func(err error) (bool, error) {
		select {
		case <-readDone:
			if websocket.CloseStatus(readErr) != -1 {
				return true, readErr
			}
		case <-time.After(time.Second):
		}
		return true, err
	}

	if err := c.send(sessCtx, conn, proto.TypeHello, c.hello); err != nil {
		return fail(err)
	}
	c.attach(conn)
	defer c.detach(conn)

	stable := time.NewTimer(stableAfter)
	defer stable.Stop()
	metricsTick := time.NewTicker(time.Duration(c.cfg.IntervalSec) * time.Second)
	defer metricsTick.Stop()
	procTick := time.NewTicker(time.Duration(c.cfg.ProcessIntervalSec) * time.Second)
	defer procTick.Stop()
	svcTick := time.NewTicker(servicesCheck)
	defer svcTick.Stop()
	var svc serviceState
	updTick := time.NewTicker(updatesCheck)
	defer updTick.Stop()
	var upd updateState

	// Direkt nach dem Verbinden einen vollständigen Stand senden.
	if err := c.sendMetrics(sessCtx, conn); err != nil {
		return fail(err)
	}
	if err := c.sendProcesses(sessCtx, conn); err != nil {
		return fail(err)
	}
	if err := c.sendServices(sessCtx, conn, &svc); err != nil {
		return fail(err)
	}
	if err := c.sendUpdates(sessCtx, conn, &upd); err != nil {
		return fail(err)
	}

	for {
		select {
		case <-sessCtx.Done():
			if ctx.Err() != nil {
				conn.Close(websocket.StatusGoingAway, "agent wird beendet")
				return true, ctx.Err()
			}
			<-readDone
			return true, readErr
		case <-c.restart:
			conn.Close(websocket.StatusGoingAway, "agent startet neu")
			return true, ErrRestart
		case <-stable.C:
			if c.onStable != nil {
				c.onStable()
				c.onStable = nil
			}
		case <-metricsTick.C:
			if err := c.sendMetrics(sessCtx, conn); err != nil {
				return fail(err)
			}
		case <-procTick.C:
			if err := c.sendProcesses(sessCtx, conn); err != nil {
				return fail(err)
			}
		case <-svcTick.C:
			if err := c.sendServices(sessCtx, conn, &svc); err != nil {
				return fail(err)
			}
		case <-c.svcNow:
			if err := c.sendServices(sessCtx, conn, &svc); err != nil {
				return fail(err)
			}
		case <-updTick.C:
			if err := c.sendUpdates(sessCtx, conn, &upd); err != nil {
				return fail(err)
			}
		case <-c.updNow:
			if err := c.sendUpdates(sessCtx, conn, &upd); err != nil {
				return fail(err)
			}
		}
	}
}

// handleMessage verarbeitet eine Nachricht des Servers. Bisher gibt es nur Jobs.
// Jobs laufen unter runCtx (Lebensdauer des Agenten), Ablehnungen gehen direkt
// über die Verbindung der Sitzung.
func (c *Client) handleMessage(runCtx, ctx context.Context, conn *websocket.Conn, data []byte) {
	var env proto.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		c.log.Warn("unlesbare nachricht vom server", "err", err)
		return
	}
	if env.Type != proto.TypeJobRequest {
		c.log.Debug("unbekannter nachrichtentyp ignoriert", "type", env.Type)
		return
	}
	var job proto.JobRequest
	if err := json.Unmarshal(env.Data, &job); err != nil {
		c.log.Warn("unlesbarer job", "err", err)
		return
	}
	if err := job.Validate(); err != nil {
		c.log.Warn("job abgelehnt", "job_id", job.JobID, "action", job.Action, "err", err)
		c.sendResult(ctx, conn, proto.JobResult{JobID: job.JobID, ExitCode: 1, Error: "abgelehnt: " + err.Error()})
		return
	}
	fn, ok := c.jobs[job.Action]
	if !ok || !slices.Contains(c.hello.Capabilities, proto.ActionCapability[job.Action]) {
		c.log.Warn("job abgelehnt: aktion nicht unterstützt", "job_id", job.JobID, "action", job.Action)
		c.sendResult(ctx, conn, proto.JobResult{JobID: job.JobID, ExitCode: 1, Error: "aktion wird von diesem agenten nicht unterstützt"})
		return
	}
	busy := &c.busy
	if proto.ReadOnly(job.Action) {
		busy = &c.readBusy
	}
	if !busy.CompareAndSwap(false, true) {
		c.sendResult(ctx, conn, proto.JobResult{JobID: job.JobID, ExitCode: 1, Error: "agent führt bereits einen job aus"})
		return
	}
	c.log.Info("job gestartet", "job_id", job.JobID, "action", job.Action)
	go func() {
		defer busy.Store(false)
		jctx, cancel := context.WithTimeout(runCtx, proto.JobTimeout(job.Action))
		defer cancel()
		out, restart, err := fn(jctx, job.Params)
		res := proto.JobResult{JobID: job.JobID, OK: err == nil, Output: proto.CleanOutput(out, proto.MaxJobOutputLen)}
		if err != nil {
			res.ExitCode = 1
			res.Error = proto.CleanOutput(err.Error(), 1024)
			c.log.Warn("job fehlgeschlagen", "job_id", job.JobID, "action", job.Action, "err", err)
		} else {
			c.log.Info("job erledigt", "job_id", job.JobID, "action", job.Action)
		}
		c.deliver(res)
		switch proto.ActionCapability[job.Action] {
		case proto.CapServices:
			trigger(c.svcNow)
		case proto.CapPackages:
			trigger(c.updNow)
		}
		// Auch ohne zugestelltes Ergebnis neu starten: das hello der neuen
		// Version zeigt dem Server, dass es geklappt hat.
		if restart && err == nil {
			c.restartOnce.Do(func() { close(c.restart) })
		}
	}()
}

// trigger stößt eine sofortige Prüfung an, ohne zu blockieren.
func trigger(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// attach macht conn zur aktuellen Verbindung und schickt wartende Ergebnisse nach.
func (c *Client) attach(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, res := range pending {
		c.deliver(res)
	}
}

func (c *Client) detach(conn *websocket.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
}

// deliver schickt ein Job-Ergebnis über die aktuelle Verbindung oder hebt es
// für die nächste auf (höchstens maxPending, die ältesten fallen weg).
func (c *Client) deliver(res proto.JobResult) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil && c.sendResult(context.Background(), conn, res) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == maxPending {
		c.pending = c.pending[1:]
	}
	c.pending = append(c.pending, res)
	c.log.Info("job-ergebnis wartet auf die nächste verbindung", "job_id", res.JobID)
}

// sendResult meldet ein Job-Ergebnis; Schreiben ist bei coder/websocket
// nebenläufig erlaubt.
func (c *Client) sendResult(ctx context.Context, conn *websocket.Conn, res proto.JobResult) bool {
	if err := c.send(ctx, conn, proto.TypeJobResult, res); err != nil {
		c.log.Warn("job-ergebnis senden", "job_id", res.JobID, "err", err)
		return false
	}
	return true
}

func (c *Client) sendMetrics(ctx context.Context, conn *websocket.Conn) error {
	m, err := c.col.Metrics(ctx)
	if err != nil {
		c.log.Warn("metriken erfassen fehlgeschlagen", "err", err)
		return nil
	}
	return c.send(ctx, conn, proto.TypeMetrics, m)
}

func (c *Client) sendProcesses(ctx context.Context, conn *websocket.Conn) error {
	p, err := c.col.Processes(ctx, 10)
	if err != nil {
		c.log.Warn("prozesse erfassen fehlgeschlagen", "err", err)
		return nil
	}
	return c.send(ctx, conn, proto.TypeProcesses, p)
}

// serviceState merkt sich die zuletzt gesendete Dienstliste einer Sitzung.
type serviceState struct {
	last []proto.Service
	at   time.Time
}

// sendServices schickt die Dienstliste, wenn sie sich geändert hat oder
// servicesFull vergangen ist.
func (c *Client) sendServices(ctx context.Context, conn *websocket.Conn, st *serviceState) error {
	if c.services == nil {
		return nil
	}
	list, err := c.services(ctx)
	if err != nil {
		c.log.Warn("dienste lesen fehlgeschlagen", "err", err)
		return nil
	}
	now := time.Now()
	if !st.at.IsZero() && slices.Equal(list, st.last) && now.Sub(st.at) < servicesFull {
		return nil
	}
	if err := c.send(ctx, conn, proto.TypeServices, proto.Services{Time: now, List: list}); err != nil {
		return err
	}
	st.last, st.at = list, now
	return nil
}

// updateState merkt sich den zuletzt gesendeten Update-Stand einer Sitzung.
type updateState struct {
	last proto.Updates
	at   time.Time
}

// sendUpdates schickt den Update-Stand, wenn er sich geändert hat oder
// updatesFull vergangen ist.
func (c *Client) sendUpdates(ctx context.Context, conn *websocket.Conn, st *updateState) error {
	if c.updates == nil {
		return nil
	}
	u := c.updates(ctx)
	now := time.Now()
	if !st.at.IsZero() && sameUpdates(u, st.last) && now.Sub(st.at) < updatesFull {
		return nil
	}
	if err := c.send(ctx, conn, proto.TypeUpdates, u); err != nil {
		return err
	}
	st.last, st.at = u, now
	return nil
}

// sameUpdates vergleicht zwei Stände ohne den Zeitstempel.
func sameUpdates(a, b proto.Updates) bool {
	return a.Manager == b.Manager && a.Upgradable == b.Upgradable && a.Security == b.Security &&
		a.RebootRequired == b.RebootRequired && a.Error == b.Error && slices.Equal(a.Packages, b.Packages)
}

func (c *Client) send(ctx context.Context, conn *websocket.Conn, typ string, payload any) error {
	data, err := proto.Encode(typ, payload)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("%s senden: %w", typ, err)
	}
	c.log.Debug("gesendet", "type", typ, "bytes", len(data))
	return nil
}

// jitter streut die Wartezeit um ±20 %, damit nicht alle Agenten gleichzeitig neu verbinden.
func jitter(d time.Duration) time.Duration {
	f := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * f)
}
