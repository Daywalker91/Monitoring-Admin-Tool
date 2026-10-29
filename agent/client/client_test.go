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
	"sync/atomic"
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

// jobServer schickt nach dem hello die Jobs und meldet alle Job-Ergebnisse.
func jobServer(t *testing.T, jobs ...proto.JobRequest) (config.Config, chan proto.JobResult) {
	t.Helper()
	results := make(chan proto.JobResult, 10)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.Read(r.Context()) // hello
		for _, j := range jobs {
			data, _ := proto.Encode(proto.TypeJobRequest, j)
			conn.Write(r.Context(), websocket.MessageText, data)
		}
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var env proto.Envelope
			var res proto.JobResult
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeJobResult && json.Unmarshal(env.Data, &res) == nil {
				results <- res
			}
		}
	}))
	t.Cleanup(ts.Close)
	cfg := config.Config{ServerURL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/agent",
		Token: "tok", IntervalSec: 10, ProcessIntervalSec: 30}
	return cfg, results
}

func TestJobs(t *testing.T) {
	update := proto.JobRequest{JobID: "j3", Action: proto.ActionAgentUpdate, Params: map[string]string{"version": "v0.5.1"}}
	cfg, results := jobServer(t,
		proto.JobRequest{JobID: "j1", Action: "shell.exec", Params: map[string]string{"cmd": "rm -rf /"}},
		proto.JobRequest{JobID: "j2", Action: proto.ActionAgentUpdate, Params: map[string]string{"bad key": "x"}},
		update)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(cfg, proto.Hello{AgentID: "t", Capabilities: []proto.Capability{proto.CapMetrics, proto.CapUpdate}},
		collector.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	var gotParams map[string]string
	c.HandleJob(proto.ActionAgentUpdate, func(_ context.Context, p map[string]string) (string, bool, error) {
		gotParams = p
		return "installiert", true, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Run(ctx); err != ErrRestart {
		t.Fatalf("Run = %v, erwartet ErrRestart", err)
	}
	want := map[string]bool{"j1": false, "j2": false, "j3": true}
	for range want {
		select {
		case res := <-results:
			if ok, known := want[res.JobID]; !known || res.OK != ok {
				t.Errorf("ergebnis %+v", res)
			}
		case <-ctx.Done():
			t.Fatal("zu wenige job-ergebnisse")
		}
	}
	if gotParams["version"] != "v0.5.1" {
		t.Errorf("params = %v", gotParams)
	}
}

func TestJobWithoutCapability(t *testing.T) {
	cfg, results := jobServer(t, proto.JobRequest{JobID: "j1", Action: proto.ActionAgentUpdate,
		Params: map[string]string{"version": "v0.5.1"}})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Fähigkeit "update" nicht gemeldet (z. B. Agent läuft im Vordergrund).
	c, err := New(cfg, proto.Hello{AgentID: "t", Capabilities: []proto.Capability{proto.CapMetrics}}, collector.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	c.HandleJob(proto.ActionAgentUpdate, func(context.Context, map[string]string) (string, bool, error) {
		called = true
		return "", true, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go c.Run(ctx)
	select {
	case res := <-results:
		if res.OK || called {
			t.Fatalf("job ohne fähigkeit ausgeführt: %+v", res)
		}
	case <-ctx.Done():
		t.Fatal("kein job-ergebnis")
	}
}

// Ein Job, der während einer Trennung fertig wird, meldet sein Ergebnis über
// die nächste Verbindung (nach deren hello).
func TestJobResultAfterReconnect(t *testing.T) {
	firstGone := make(chan struct{})
	results := make(chan proto.JobResult, 1)
	var conns atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, data, err := conn.Read(r.Context()); err != nil || !strings.Contains(string(data), `"hello"`) {
			t.Errorf("erste nachricht ist kein hello")
			return
		}
		if conns.Add(1) == 1 {
			data, _ := proto.Encode(proto.TypeJobRequest, proto.JobRequest{JobID: "j1", Action: proto.ActionAgentUpdate,
				Params: map[string]string{"version": "v9.9.9"}})
			conn.Write(r.Context(), websocket.MessageText, data)
			conn.Close(websocket.StatusGoingAway, "server startet neu")
			close(firstGone)
			return
		}
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var env proto.Envelope
			var res proto.JobResult
			if json.Unmarshal(data, &env) == nil && env.Type == proto.TypeJobResult && json.Unmarshal(env.Data, &res) == nil {
				results <- res
			}
		}
	}))
	t.Cleanup(ts.Close)
	cfg := config.Config{ServerURL: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/agent",
		Token: "tok", IntervalSec: 10, ProcessIntervalSec: 30}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(cfg, proto.Hello{AgentID: "t", Capabilities: []proto.Capability{proto.CapUpdate}}, collector.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	c.HandleJob(proto.ActionAgentUpdate, func(ctx context.Context, _ map[string]string) (string, bool, error) {
		select {
		case <-firstGone:
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
		time.Sleep(100 * time.Millisecond) // sicher nach dem Ende der ersten Sitzung
		return "fertig", false, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go c.Run(ctx)
	select {
	case res := <-results:
		if res.JobID != "j1" || !res.OK || res.Output != "fertig" {
			t.Fatalf("ergebnis %+v", res)
		}
	case <-ctx.Done():
		t.Fatal("ergebnis nicht nachgeliefert")
	}
}

// Ein lesender Job läuft auch, während ein ändernder Job noch arbeitet.
func TestReadOnlyJobWhileBusy(t *testing.T) {
	cfg, results := jobServer(t,
		proto.JobRequest{JobID: "upgrade", Action: proto.ActionPackagesUpgrade},
		proto.JobRequest{JobID: "reboot", Action: proto.ActionReboot},
		proto.JobRequest{JobID: "logs", Action: proto.ActionLogRead, Params: map[string]string{"source": "journal"}})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(cfg, proto.Hello{AgentID: "t", Capabilities: []proto.Capability{proto.CapPackages, proto.CapReboot, proto.CapLogs}},
		collector.New(), log)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	c.HandleJob(proto.ActionPackagesUpgrade, func(ctx context.Context, _ map[string]string) (string, bool, error) {
		<-release
		return "fertig", false, nil
	})
	c.HandleJob(proto.ActionReboot, func(context.Context, map[string]string) (string, bool, error) { return "", false, nil })
	c.HandleJob(proto.ActionLogRead, func(context.Context, map[string]string) (string, bool, error) { return "zeilen", false, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go c.Run(ctx)

	got := map[string]proto.JobResult{}
	for len(got) < 2 {
		select {
		case res := <-results:
			got[res.JobID] = res
		case <-ctx.Done():
			t.Fatalf("ergebnisse: %+v", got)
		}
	}
	if !got["logs"].OK || got["reboot"].OK {
		t.Fatalf("während des upgrades: %+v", got)
	}
	close(release)
	if res := <-results; res.JobID != "upgrade" || !res.OK {
		t.Fatalf("upgrade: %+v", res)
	}
}
