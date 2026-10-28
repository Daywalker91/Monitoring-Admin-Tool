// Package client hält die ausgehende WebSocket-Verbindung zum Server und
// sendet die Messwerte des Collectors.
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/collector"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

const (
	backoffMin   = time.Second
	backoffMax   = time.Minute
	writeTimeout = 10 * time.Second
	dialTimeout  = 15 * time.Second
)

// Client verbindet den Agenten mit dem Server.
type Client struct {
	cfg   config.Config
	hello proto.Hello
	col   *collector.Collector
	log   *slog.Logger
}

// New erzeugt einen Client.
func New(cfg config.Config, hello proto.Hello, col *collector.Collector, log *slog.Logger) *Client {
	return &Client{cfg: cfg, hello: hello, col: col, log: log}
}

// Run verbindet sich und hält die Verbindung, bis ctx beendet wird. Bei
// Fehlern wird mit exponentiellem Backoff (mit Jitter) neu verbunden.
func (c *Client) Run(ctx context.Context) {
	backoff := backoffMin
	for {
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = backoffMin // nach erfolgreicher Verbindung wieder schnell neu versuchen
		}
		wait := jitter(backoff)
		c.log.Warn("verbindung zum server getrennt", "err", err, "retry_in", wait.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return
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
	conn, resp, err := websocket.Dial(dialCtx, c.cfg.ServerURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return false, errors.New("server lehnt token ab (401)")
		}
		return false, fmt.Errorf("verbinden: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(proto.MaxMessageBytes)
	c.log.Info("mit server verbunden", "url", c.cfg.ServerURL)

	// In M1 sendet der Server nichts außer Steuerframes. CloseRead liest im
	// Hintergrund (Ping/Pong/Close) und beendet sessCtx, wenn die Verbindung endet.
	sessCtx := conn.CloseRead(ctx)

	if err := c.send(sessCtx, conn, proto.TypeHello, c.hello); err != nil {
		return true, err
	}

	metricsTick := time.NewTicker(time.Duration(c.cfg.IntervalSec) * time.Second)
	defer metricsTick.Stop()
	procTick := time.NewTicker(time.Duration(c.cfg.ProcessIntervalSec) * time.Second)
	defer procTick.Stop()

	// Direkt nach dem Verbinden einen vollständigen Stand senden.
	if err := c.sendMetrics(sessCtx, conn); err != nil {
		return true, err
	}
	if err := c.sendProcesses(sessCtx, conn); err != nil {
		return true, err
	}

	for {
		select {
		case <-sessCtx.Done():
			if ctx.Err() != nil {
				conn.Close(websocket.StatusGoingAway, "agent wird beendet")
			}
			return true, sessCtx.Err()
		case <-metricsTick.C:
			if err := c.sendMetrics(sessCtx, conn); err != nil {
				return true, err
			}
		case <-procTick.C:
			if err := c.sendProcesses(sessCtx, conn); err != nil {
				return true, err
			}
		}
	}
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
