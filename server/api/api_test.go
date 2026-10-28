package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetUser(context.Background(), "admin", hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	gui := fstest.MapFS{
		"index.html": {Data: []byte("<h1>gui</h1>")},
		"login.html": {Data: []byte("<h1>login</h1>")},
		"style.css":  {Data: []byte("body{}")},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(st, auth.New(st), "secret", gui, log).Handler())
	t.Cleanup(ts.Close)
	return ts
}

const testPassword = "test-passwort-123"

// newClient liefert einen Client mit Cookie-Speicher, der Weiterleitungen nicht folgt.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func postLogin(t *testing.T, c *http.Client, ts *httptest.Server, user, pass string) int {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Username: user, Password: pass})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/login", bytes.NewReader(body))
	req.Header.Set("Origin", ts.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// login liefert einen angemeldeten Client.
func login(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	c := newClient(t)
	if code := postLogin(t, c, ts, "admin", testPassword); code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	return c
}

func status(t *testing.T, c *http.Client, method, url, origin string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAuthRequired(t *testing.T) {
	ts := newTestServer(t)
	anon := newClient(t)

	if code := status(t, anon, "GET", ts.URL+"/api/hosts", ""); code != http.StatusUnauthorized {
		t.Errorf("api ohne login: %d, erwartet 401", code)
	}
	if code := status(t, anon, "GET", ts.URL+"/", ""); code != http.StatusSeeOther {
		t.Errorf("gui ohne login: %d, erwartet 303", code)
	}
	for _, p := range []string{"/login.html", "/style.css", "/healthz"} {
		if code := status(t, anon, "GET", ts.URL+p, ""); code != http.StatusOK {
			t.Errorf("%s ohne login: %d, erwartet 200", p, code)
		}
	}

	if code := postLogin(t, anon, ts, "admin", "falsches-passwort"); code != http.StatusUnauthorized {
		t.Errorf("falsches passwort: %d", code)
	}

	c := login(t, ts)
	if code := status(t, c, "GET", ts.URL+"/api/hosts", ""); code != http.StatusOK {
		t.Errorf("api mit login: %d", code)
	}
	if code := status(t, c, "POST", ts.URL+"/api/logout", "http://evil.example"); code != http.StatusForbidden {
		t.Errorf("logout fremde herkunft: %d, erwartet 403", code)
	}
	if code := status(t, c, "POST", ts.URL+"/api/logout", ts.URL); code != http.StatusNoContent {
		t.Errorf("logout: %d", code)
	}
	if code := status(t, c, "GET", ts.URL+"/api/hosts", ""); code != http.StatusUnauthorized {
		t.Errorf("api nach logout: %d, erwartet 401", code)
	}
}

func TestLoginLockout(t *testing.T) {
	ts := newTestServer(t)
	c := newClient(t)
	for i := 0; i < 5; i++ {
		postLogin(t, c, ts, "admin", "falsches-passwort")
	}
	// Auch das richtige Passwort wird während der Sperre abgelehnt.
	if code := postLogin(t, c, ts, "admin", testPassword); code != http.StatusTooManyRequests {
		t.Fatalf("nach 5 fehlversuchen: %d, erwartet 429", code)
	}
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
	c := login(t, ts)
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
		resp, err := c.Get(ts.URL + "/api/hosts")
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

	resp, err := c.Get(ts.URL + "/api/hosts/test-1/history?hours=1")
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
	if code := deleteHost(t, c, ts, "test-1", ""); code != http.StatusConflict {
		t.Fatalf("löschen online: %d, erwartet 409", code)
	}
	if code := deleteHost(t, c, ts, "test-1", "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("löschen fremde herkunft: %d, erwartet 403", code)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	deadline = time.Now().Add(5 * time.Second)
	var code int
	for time.Now().Before(deadline) {
		if code = deleteHost(t, c, ts, "test-1", ts.URL); code != http.StatusConflict {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code != http.StatusNoContent {
		t.Fatalf("löschen offline: %d, erwartet 204", code)
	}
	if code := deleteHost(t, c, ts, "test-1", ""); code != http.StatusNotFound {
		t.Fatalf("erneut löschen: %d, erwartet 404", code)
	}
}

func deleteHost(t *testing.T, c *http.Client, ts *httptest.Server, id, origin string) int {
	t.Helper()
	return status(t, c, http.MethodDelete, ts.URL+"/api/hosts/"+id, origin)
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
	c := login(t, ts)
	resp, err := c.Get(ts.URL + "/")
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
