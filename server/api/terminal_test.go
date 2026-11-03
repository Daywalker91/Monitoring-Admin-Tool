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
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func newTerminalServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword(testPassword)
	st.SetUser(context.Background(), "admin", hash, time.Now())
	srv := New(st, auth.New(st), "secret", fstest.MapFS{"index.html": {Data: []byte("x")}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// terminalAgent simuliert einen Agenten mit Terminal: Er verbindet sich je
// terminal_open zurück und spiegelt Eingaben ("echo:<eingabe>"). Die Eingabe
// "exit" beendet die Sitzung mit Grund "shell beendet"; answer=false lässt
// terminal_open unbeantwortet.
func terminalAgent(t *testing.T, ctx context.Context, ts *httptest.Server, id string, answer bool) {
	t.Helper()
	h := http.Header{"Authorization": {"Bearer secret"}}
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	hello, _ := proto.Encode(proto.TypeHello, proto.Hello{AgentID: id, Hostname: id, IntervalSec: 10,
		Capabilities: []proto.Capability{proto.CapMetrics, proto.CapTerminal}, TerminalUser: "hans"})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			var req proto.TerminalOpen
			if json.Unmarshal(data, &env) != nil || env.Type != proto.TypeTerminalOpen || !answer {
				continue
			}
			if json.Unmarshal(env.Data, &req) != nil || req.Validate() != nil {
				continue
			}
			go serveFakeTerminal(ctx, ts, req)
		}
	}()
}

func serveFakeTerminal(ctx context.Context, ts *httptest.Server, req proto.TerminalOpen) {
	h := http.Header{"Authorization": {"Bearer secret"}}
	conn, _, err := websocket.Dial(ctx, wsURL(ts)+"/terminal/"+req.SessionID, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var ctl proto.TerminalControl
			json.Unmarshal(data, &ctl)
			conn.Write(ctx, websocket.MessageBinary, []byte(ctl.Type))
			continue
		}
		if string(data) == "exit" {
			msg, _ := json.Marshal(proto.TerminalControl{Type: proto.TerminalExit, Reason: "shell beendet"})
			conn.Write(ctx, websocket.MessageText, msg)
			conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		conn.Write(ctx, websocket.MessageBinary, []byte("echo:"+string(data)))
	}
}

func requestTicket(t *testing.T, c *http.Client, ts *httptest.Server, host, password string) (string, int) {
	t.Helper()
	resp := postJSON(t, c, ts.URL+"/api/hosts/"+host+"/terminal", ts.URL,
		terminalRequest{Password: password, Cols: 80, Rows: 24})
	defer resp.Body.Close()
	var out struct{ Ticket string }
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Ticket, resp.StatusCode
}

func dialTerminal(ctx context.Context, c *http.Client, ts *httptest.Server, ticket, origin string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/terminal?ticket=" + ticket
	return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c, HTTPHeader: h})
}

// readUntilExit liest Daten bis zur exit-Nachricht.
func readUntilExit(t *testing.T, ctx context.Context, conn *websocket.Conn) (data, reason string) {
	t.Helper()
	var b strings.Builder
	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("lesen: %v (bisher %q)", err, b.String())
		}
		if typ == websocket.MessageBinary {
			b.Write(msg)
			continue
		}
		var ctl proto.TerminalControl
		if json.Unmarshal(msg, &ctl) != nil || ctl.Type != proto.TerminalExit {
			t.Fatalf("unerwartete steuernachricht %s", msg)
		}
		return b.String(), ctl.Reason
	}
}

func TestTerminalFlow(t *testing.T) {
	ts, _ := newTerminalServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	terminalAgent(t, ctx, ts, "t1", true)
	h := waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	if h.TerminalUser != "hans" {
		t.Fatalf("terminal_user %q", h.TerminalUser)
	}

	if _, code := requestTicket(t, c, ts, "t1", "falsches-passwort"); code != http.StatusUnauthorized {
		t.Fatalf("falsches passwort: %d", code)
	}
	if code := postJSON(t, c, ts.URL+"/api/hosts/t1/terminal", "http://evil.example",
		terminalRequest{Password: testPassword, Cols: 80, Rows: 24}).StatusCode; code != http.StatusForbidden {
		t.Fatalf("fremde herkunft: %d", code)
	}
	ticket, code := requestTicket(t, c, ts, "t1", testPassword)
	if code != http.StatusOK || ticket == "" {
		t.Fatalf("ticket: %d", code)
	}

	// ohne Origin kein Terminal; das Ticket ist danach trotzdem noch gültig
	if _, resp, err := dialTerminal(ctx, c, ts, ticket, ""); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ohne origin: %v", err)
	}
	conn, _, err := dialTerminal(ctx, c, ts, ticket, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// dasselbe Ticket nur einmal
	if _, resp, err := dialTerminal(ctx, c, ts, ticket, ts.URL); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ticket zweimal: %v", err)
	}

	conn.Write(ctx, websocket.MessageBinary, []byte("ls"))
	resize, _ := json.Marshal(proto.TerminalControl{Type: proto.TerminalResize, Cols: 100, Rows: 30})
	conn.Write(ctx, websocket.MessageText, resize)
	conn.Write(ctx, websocket.MessageText, []byte(`{"type":"exit","reason":"vom browser"}`)) // verworfen
	conn.Write(ctx, websocket.MessageBinary, []byte("exit"))
	data, reason := readUntilExit(t, ctx, conn)
	if data != "echo:ls"+"resize" || reason != "shell beendet" {
		t.Fatalf("daten %q, grund %q", data, reason)
	}

	var list []store.TerminalSession
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
		if getJSON(t, c, ts.URL+"/api/hosts/t1/terminal-sessions", &list); len(list) == 1 && list[0].EndedAt != nil {
			break
		}
	}
	if len(list) != 1 || list[0].EndedAt == nil || list[0].Reason != "shell beendet" || list[0].User != "admin" ||
		list[0].BytesIn != int64(len("ls")+len("exit")) || list[0].BytesOut != int64(len("echo:ls")+len("resize")) {
		t.Fatalf("audit: %+v", list)
	}
}

func TestTerminalTicketBinding(t *testing.T) {
	ts, srv := newTerminalServer(t)
	a, b := login(t, ts), login(t, ts) // zwei Sessions desselben Benutzers
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	terminalAgent(t, ctx, ts, "t1", true)
	waitHost(t, a, ts, "online", func(h hostView) bool { return h.Online })

	ticket, _ := requestTicket(t, a, ts, "t1", testPassword)
	if _, resp, err := dialTerminal(ctx, b, ts, ticket, ts.URL); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ticket aus anderer session: %v", err)
	}
	// abgelaufen
	ticket, _ = requestTicket(t, a, ts, "t1", testPassword)
	srv.nowFor = func() time.Time { return time.Now().Add(terminalTicketTTL + time.Second) }
	if _, resp, err := dialTerminal(ctx, a, ts, ticket, ts.URL); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("abgelaufenes ticket: %v", err)
	}
	// ohne Anmeldung
	if _, resp, err := dialTerminal(ctx, newClient(t), ts, ticket, ts.URL); err == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("ohne anmeldung: %v", err)
	}
}

func TestTerminalRefused(t *testing.T) {
	ts, srv := newTerminalServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Host ohne Fähigkeit
	conn := fakeAgentCaps(t, ctx, ts, "v1.6.0", "", proto.CapMetrics)
	defer conn.CloseNow()
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	if _, code := requestTicket(t, c, ts, "a1", testPassword); code != http.StatusConflict {
		t.Fatalf("ohne fähigkeit: %d", code)
	}

	// laufender ändernder Job auf dem Host
	terminalAgent(t, ctx, ts, "t1", true)
	for deadline := time.Now().Add(5 * time.Second); !srv.online("t1") && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	now := time.Now()
	srv.store.CreateJob(ctx, store.Job{ID: "job-x", HostID: "t1", Action: proto.ActionPackagesUpgrade,
		CreatedAt: now.Unix(), Deadline: now.Add(time.Hour).Unix()})
	if _, code := requestTicket(t, c, ts, "t1", testPassword); code != http.StatusConflict {
		t.Fatalf("mit laufendem job: %d", code)
	}
	if _, code := requestTicket(t, c, ts, "fehlt", testPassword); code != http.StatusNotFound {
		t.Fatalf("unbekannter host: %d", code)
	}
}

func TestTerminalAgentSilent(t *testing.T) {
	old := terminalAgentWait
	terminalAgentWait = 200 * time.Millisecond
	t.Cleanup(func() { terminalAgentWait = old })
	ts, _ := newTerminalServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	terminalAgent(t, ctx, ts, "t1", false)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	ticket, _ := requestTicket(t, c, ts, "t1", testPassword)
	conn, _, err := dialTerminal(ctx, c, ts, ticket, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, reason := readUntilExit(t, ctx, conn); !strings.Contains(reason, "nicht geantwortet") {
		t.Fatalf("grund %q", reason)
	}
	// unbekannte Sitzung am Agent-Endpunkt
	h := http.Header{"Authorization": {"Bearer secret"}}
	if _, resp, err := websocket.Dial(ctx, wsURL(ts)+"/terminal/"+proto.TerminalIDPrefix+strings.Repeat("00", 16),
		&websocket.DialOptions{HTTPHeader: h}); err == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unbekannte sitzung: %v", err)
	}
	if _, resp, err := websocket.Dial(ctx, wsURL(ts)+"/terminal/x", nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ohne token: %v", err)
	}
}

func TestTerminalBrowserGone(t *testing.T) {
	ts, srv := newTerminalServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	terminalAgent(t, ctx, ts, "t1", true)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	ticket, _ := requestTicket(t, c, ts, "t1", testPassword)
	conn, _, err := dialTerminal(ctx, c, ts, ticket, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write(ctx, websocket.MessageBinary, []byte("x"))
	conn.Read(ctx) // echo: Agent ist verbunden
	conn.Close(websocket.StatusNormalClosure, "")

	var list []store.TerminalSession
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
		if getJSON(t, c, ts.URL+"/api/hosts/t1/terminal-sessions", &list); len(list) == 1 && list[0].EndedAt != nil {
			break
		}
	}
	if len(list) != 1 || list[0].Reason != "browser getrennt" {
		t.Fatalf("audit: %+v", list)
	}
	srv.term.mu.Lock()
	n := len(srv.term.sessions)
	srv.term.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sitzungen übrig", n)
	}
}
