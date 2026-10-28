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
	jobTimeout  = 5 * time.Minute
)

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

	jobs     map[string]JobFunc // erlaubte Aktionen; nur mit gemeldeter Fähigkeit
	busy     atomic.Bool        // es läuft immer höchstens ein Job
	onStable func()
}

// New erzeugt einen Client. Ein Fehler entsteht nur bei ungültiger TLS-Konfiguration.
func New(cfg config.Config, hello proto.Hello, col *collector.Collector, log *slog.Logger) (*Client, error) {
	c := &Client{cfg: cfg, hello: hello, col: col, log: log}
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
	restart := make(chan struct{})
	var restartOnce sync.Once
	requestRestart := func() { restartOnce.Do(func() { close(restart) }) }
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
				c.handleMessage(sessCtx, conn, data, requestRestart)
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

	stable := time.NewTimer(stableAfter)
	defer stable.Stop()
	metricsTick := time.NewTicker(time.Duration(c.cfg.IntervalSec) * time.Second)
	defer metricsTick.Stop()
	procTick := time.NewTicker(time.Duration(c.cfg.ProcessIntervalSec) * time.Second)
	defer procTick.Stop()

	// Direkt nach dem Verbinden einen vollständigen Stand senden.
	if err := c.sendMetrics(sessCtx, conn); err != nil {
		return fail(err)
	}
	if err := c.sendProcesses(sessCtx, conn); err != nil {
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
		case <-restart:
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
		}
	}
}

// handleMessage verarbeitet eine Nachricht des Servers. Bisher gibt es nur Jobs.
func (c *Client) handleMessage(ctx context.Context, conn *websocket.Conn, data []byte, requestRestart func()) {
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
	if !c.busy.CompareAndSwap(false, true) {
		c.sendResult(ctx, conn, proto.JobResult{JobID: job.JobID, ExitCode: 1, Error: "agent führt bereits einen job aus"})
		return
	}
	c.log.Info("job gestartet", "job_id", job.JobID, "action", job.Action)
	go func() {
		defer c.busy.Store(false)
		jctx, cancel := context.WithTimeout(ctx, jobTimeout)
		defer cancel()
		out, restart, err := fn(jctx, job.Params)
		res := proto.JobResult{JobID: job.JobID, OK: err == nil, Output: proto.Truncate(out, proto.MaxJobOutputLen)}
		if err != nil {
			res.ExitCode = 1
			res.Error = proto.Truncate(err.Error(), 1024)
			c.log.Warn("job fehlgeschlagen", "job_id", job.JobID, "action", job.Action, "err", err)
		} else {
			c.log.Info("job erledigt", "job_id", job.JobID, "action", job.Action)
		}
		if c.sendResult(ctx, conn, res) && restart && err == nil {
			requestRestart()
		}
	}()
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
