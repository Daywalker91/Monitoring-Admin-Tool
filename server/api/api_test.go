package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gui := fstest.MapFS{"index.html": {Data: []byte("<h1>gui</h1>")}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(st, "secret", gui, log).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/agent"
}

func TestAgentRejectsWrongToken(t *testing.T) {
	ts := newTestServer(t)
	h := http.Header{"Authorization": {"Bearer falsch"}}
	_, resp, err := websocket.Dial(context.Background(), wsURL(ts), &websocket.DialOptions{HTTPHeader: h})
	if err == nil {
		t.Fatal("verbindung mit falschem token wurde akzeptiert")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("erwartet 401, erhalten %v", resp)
	}
}

func TestAgentFlow(t *testing.T) {
	ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h := http.Header{"Authorization": {"Bearer secret"}}
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	send := func(typ string, v any) {
		data, err := proto.Encode(typ, v)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}
	send(proto.TypeHello, proto.Hello{AgentID: "test-1", Hostname: "test", IntervalSec: 10,
		Capabilities: []proto.Capability{proto.CapMetrics}})
	send(proto.TypeMetrics, proto.Metrics{CPU: proto.CPU{TotalPct: 12.5}})

	// Warten, bis der Server die Metriken gespeichert hat.
	var hosts []hostView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/api/hosts")
		if err != nil {
			t.Fatal(err)
		}
		hosts = nil
		json.NewDecoder(resp.Body).Decode(&hosts)
		resp.Body.Close()
		if len(hosts) == 1 && string(hosts[0].Metrics) != "null" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(hosts) != 1 || !hosts[0].Online {
		t.Fatalf("host nicht online: %+v", hosts)
	}
	var m proto.Metrics
	if err := json.Unmarshal(hosts[0].Metrics, &m); err != nil || m.CPU.TotalPct != 12.5 {
		t.Fatalf("metriken nicht gespeichert: %s", hosts[0].Metrics)
	}

	resp, err := http.Get(ts.URL + "/api/hosts/test-1/history?hours=1")
	if err != nil {
		t.Fatal(err)
	}
	var hist []store.HistoryPoint
	json.NewDecoder(resp.Body).Decode(&hist)
	resp.Body.Close()
	if len(hist) != 1 {
		t.Fatalf("historie: %+v", hist)
	}

	// Löschen: online → 409, fremde Herkunft → 403, offline → 204.
	if code := deleteHost(t, ts, "test-1", ""); code != http.StatusConflict {
		t.Fatalf("löschen online: %d, erwartet 409", code)
	}
	if code := deleteHost(t, ts, "test-1", "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("löschen fremde herkunft: %d, erwartet 403", code)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	deadline = time.Now().Add(5 * time.Second)
	var code int
	for time.Now().Before(deadline) {
		if code = deleteHost(t, ts, "test-1", ts.URL); code != http.StatusConflict {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code != http.StatusNoContent {
		t.Fatalf("löschen offline: %d, erwartet 204", code)
	}
	if code := deleteHost(t, ts, "test-1", ""); code != http.StatusNotFound {
		t.Fatalf("erneut löschen: %d, erwartet 404", code)
	}
}

func deleteHost(t *testing.T, ts *httptest.Server, id, origin string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/hosts/"+id, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestHelloRequiredFirst(t *testing.T) {
	ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{"Authorization": {"Bearer secret"}}
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	data, _ := proto.Encode(proto.TypeMetrics, proto.Metrics{})
	conn.Write(ctx, websocket.MessageText, data)
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("erwartet Policy-Violation, erhalten %v", err)
	}
}

func TestGUIAndHeaders(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "gui") {
		t.Fatalf("gui nicht ausgeliefert: %s", body)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("CSP-Header fehlt")
	}
}
