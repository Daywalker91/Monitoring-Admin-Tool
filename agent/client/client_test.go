package client

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/collector"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// tlsServer nimmt eine WebSocket-Verbindung an und meldet die erste Nachricht.
func tlsServer(t *testing.T) (*httptest.Server, chan proto.Envelope) {
	t.Helper()
	got := make(chan proto.Envelope, 1)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var env proto.Envelope
		if json.Unmarshal(data, &env) == nil {
			select {
			case got <- env:
			default:
			}
		}
		conn.Read(r.Context()) // offen halten, bis der Client geht
	}))
	t.Cleanup(ts.Close)
	return ts, got
}

func testConfig(ts *httptest.Server, caFile string) config.Config {
	return config.Config{
		ServerURL: "wss" + strings.TrimPrefix(ts.URL, "https") + "/ws/agent",
		Token:     "tok", IntervalSec: 10, ProcessIntervalSec: 30, CAFile: caFile,
	}
}

func TestTLSWithCAFile(t *testing.T) {
	ts, got := tlsServer(t)
	caFile := filepath.Join(t.TempDir(), "server.crt")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(caFile, pemData, 0o644); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(testConfig(ts, caFile), proto.Hello{AgentID: "t"}, collector.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go c.Run(ctx)

	select {
	case env := <-got:
		if env.Type != proto.TypeHello {
			t.Fatalf("erste nachricht: %s", env.Type)
		}
	case <-ctx.Done():
		t.Fatal("keine hello-nachricht über tls angekommen")
	}
}

func TestTLSWithoutCAFails(t *testing.T) {
	ts, _ := tlsServer(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(testConfig(ts, ""), proto.Hello{AgentID: "t"}, collector.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	connected, err := c.session(context.Background())
	if connected || err == nil {
		t.Fatalf("unbekanntes zertifikat akzeptiert (connected=%v, err=%v)", connected, err)
	}
}

func TestInvalidCAFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("kein zertifikat"), 0o644)
	cfg := config.Config{ServerURL: "wss://x/ws/agent", CAFile: bad}
	if _, err := New(cfg, proto.Hello{}, collector.New(), slog.Default()); err == nil {
		t.Fatal("ungültige ca-datei akzeptiert")
	}
}

func TestReplacedCloseCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.Read(r.Context()) // hello abwarten
		conn.Close(proto.CloseReplaced, "agent-id wird bereits verwendet")
	}))
	t.Cleanup(ts.Close)

	cfg := config.Config{ServerURL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/agent",
		Token: "tok", IntervalSec: 10, ProcessIntervalSec: 30}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(cfg, proto.Hello{AgentID: "t"}, collector.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connected, err := c.session(ctx)
	if !connected || websocket.CloseStatus(err) != proto.CloseReplaced {
		t.Fatalf("connected=%v err=%v, erwartet close-code %d", connected, err, proto.CloseReplaced)
	}
}
