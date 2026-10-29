package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

type recordChannel struct {
	fail bool
	got  chan notify.Message
}

func (r *recordChannel) Name() string { return "record" }
func (r *recordChannel) Send(_ context.Context, m notify.Message) error {
	if r.fail {
		return errors.New("kanal kaputt")
	}
	r.got <- m
	return nil
}

func newAlertTestServer(t *testing.T, chans ...notify.Channel) (*httptest.Server, *store.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword(testPassword)
	if err := st.SetUser(ctx, "admin", hash, time.Now()); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := notify.NewDispatcher(log, chans...)
	go d.Run(ctx)
	ev, err := alert.New(ctx, st, NotifyEvent(d), log, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, auth.New(st), "secret", fstest.MapFS{}, log).WithMetricsToken("metrics-token-1234").WithAlerts(ev, d)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func postJSON(t *testing.T, c *http.Client, url, origin string, body any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAlertRulesAPI(t *testing.T) {
	ts, _ := newAlertTestServer(t)
	c := login(t, ts)

	resp, err := c.Get(ts.URL + "/api/alert-rules")
	if err != nil {
		t.Fatal(err)
	}
	var view alertRulesView
	json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()
	if len(view.Rules) != len(alert.DefaultRules) || len(view.Channels) != 0 {
		t.Fatalf("regeln: %+v", view)
	}

	rules := map[string]any{"rules": []store.AlertRule{{Kind: "cpu", Enabled: true, Threshold: 75, ForSec: 0}}}
	if r := postJSON(t, c, ts.URL+"/api/alert-rules", "http://evil.example", rules); r.StatusCode != http.StatusForbidden {
		t.Errorf("fremde herkunft: %d", r.StatusCode)
	}
	bad := map[string]any{"rules": []store.AlertRule{{Kind: "cpu", Enabled: true, Threshold: 500}}}
	if r := postJSON(t, c, ts.URL+"/api/alert-rules", ts.URL, bad); r.StatusCode != http.StatusBadRequest {
		t.Errorf("ungültige schwelle: %d", r.StatusCode)
	}
	if r := postJSON(t, c, ts.URL+"/api/alert-rules", ts.URL, map[string]any{"rules": 1, "x": 2}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("ungültiges json: %d", r.StatusCode)
	}
	if r := postJSON(t, c, ts.URL+"/api/alert-rules", ts.URL, rules); r.StatusCode != http.StatusOK {
		t.Fatalf("regeln speichern: %d", r.StatusCode)
	}

	// Ohne Kanäle gibt es keine Testnachricht.
	if r := postJSON(t, c, ts.URL+"/api/alerts/test", ts.URL, nil); r.StatusCode != http.StatusConflict {
		t.Errorf("test ohne kanäle: %d", r.StatusCode)
	}
}

func TestAlertFlow(t *testing.T) {
	rec := &recordChannel{got: make(chan notify.Message, 10)}
	ts, _ := newAlertTestServer(t, rec, &recordChannel{fail: true})
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Testnachricht: ein Kanal klappt, einer nicht.
	r := postJSON(t, c, ts.URL+"/api/alerts/test", ts.URL, nil)
	var results []notify.Result
	json.NewDecoder(r.Body).Decode(&results)
	if len(results) != 2 || !results[0].OK || results[1].OK || results[1].Error == "" {
		t.Fatalf("testergebnis: %+v", results)
	}
	<-rec.got

	// CPU-Regel ohne Mindestdauer: die erste Messung über 50 % löst aus.
	rules := map[string]any{"rules": []store.AlertRule{{Kind: "cpu", Enabled: true, Threshold: 50, ForSec: 0}}}
	if r := postJSON(t, c, ts.URL+"/api/alert-rules", ts.URL, rules); r.StatusCode != http.StatusOK {
		t.Fatalf("regeln: %d", r.StatusCode)
	}

	h := http.Header{"Authorization": {"Bearer secret"}}
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	data, _ := proto.Encode(proto.TypeHello, proto.Hello{AgentID: "a1", Hostname: "Büro-PC", IntervalSec: 10})
	conn.Write(ctx, websocket.MessageText, data)
	data, _ = proto.Encode(proto.TypeMetrics, proto.Metrics{CPU: proto.CPU{TotalPct: 88}})
	conn.Write(ctx, websocket.MessageText, data)

	select {
	case m := <-rec.got:
		if m.Title != "Büro-PC: CPU 88,0 %" || !m.High {
			t.Fatalf("nachricht: %+v", m)
		}
	case <-ctx.Done():
		t.Fatal("keine benachrichtigung")
	}

	resp, _ := c.Get(ts.URL + "/api/alerts")
	var alerts []store.Alert
	json.NewDecoder(resp.Body).Decode(&alerts)
	resp.Body.Close()
	if len(alerts) != 1 || alerts[0].Kind != "cpu" || alerts[0].Hostname != "Büro-PC" || alerts[0].ResolvedAt != nil {
		t.Fatalf("aktive alarme: %+v", alerts)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer metrics-token-1234")
	resp, _ = http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `mat_alerts_firing{host="a1",rule="cpu"} 1`) {
		t.Fatalf("metrics ohne alarm:\n%s", body)
	}
}

func TestHostRulesAndMuteAPI(t *testing.T) {
	ts, st := newAlertTestServer(t)
	c := login(t, ts)
	st.UpsertHost(context.Background(), proto.Hello{AgentID: "nas", Hostname: "NAS", IntervalSec: 10}, "x", time.Now())

	rules := map[string]any{"rules": []store.AlertRule{{Kind: "disk", Enabled: true, Threshold: 95, ForSec: 60}}}
	if r := postJSON(t, c, ts.URL+"/api/hosts/fehlt/alert-rules", ts.URL, rules); r.StatusCode != http.StatusNotFound {
		t.Errorf("unbekannter host: %d", r.StatusCode)
	}
	if r := postJSON(t, c, ts.URL+"/api/hosts/nas/alert-rules", "http://evil.example", rules); r.StatusCode != http.StatusForbidden {
		t.Errorf("fremde herkunft: %d", r.StatusCode)
	}
	if r := postJSON(t, c, ts.URL+"/api/hosts/nas/alert-rules", ts.URL, rules); r.StatusCode != http.StatusOK {
		t.Fatalf("host-regeln: %d", r.StatusCode)
	}
	resp, _ := c.Get(ts.URL + "/api/hosts/nas/alert-rules")
	var view hostRulesView
	json.NewDecoder(resp.Body).Decode(&view)
	resp.Body.Close()
	if len(view.Rules) != 1 || view.Rules[0].Threshold != 95 || len(view.Defaults) != len(alert.HostKinds) {
		t.Fatalf("host-regeln lesen: %+v", view)
	}

	if r := postJSON(t, c, ts.URL+"/api/hosts/nas/mute", ts.URL, map[string]int{"minutes": -1}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("negative dauer: %d", r.StatusCode)
	}
	if r := postJSON(t, c, ts.URL+"/api/hosts/nas/mute", ts.URL, map[string]int{"minutes": 60}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("stummschalten: %d", r.StatusCode)
	}
	hosts := func() []hostView {
		resp, err := c.Get(ts.URL + "/api/hosts")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var hs []hostView
		json.NewDecoder(resp.Body).Decode(&hs)
		return hs
	}
	if hs := hosts(); len(hs) != 1 || hs[0].MutedUntil == nil || *hs[0].MutedUntil < time.Now().Add(59*time.Minute).Unix() {
		t.Fatalf("muted_until: %+v", hs)
	}
	if code := status(t, c, http.MethodDelete, ts.URL+"/api/hosts/nas/mute", ts.URL); code != http.StatusNoContent {
		t.Fatalf("aufheben: %d", code)
	}
	if hs := hosts(); hs[0].MutedUntil != nil {
		t.Fatalf("nach aufheben: %+v", hs[0].MutedUntil)
	}

	// Erinnerung über die globalen Regeln.
	body := map[string]any{"rules": []store.AlertRule{{Kind: "cpu", Enabled: true, Threshold: 90, ForSec: 300}}, "reminder_hours": 6}
	r := postJSON(t, c, ts.URL+"/api/alert-rules", ts.URL, body)
	var rv alertRulesView
	json.NewDecoder(r.Body).Decode(&rv)
	if r.StatusCode != http.StatusOK || rv.ReminderHours != 6 {
		t.Fatalf("erinnerung: %d %+v", r.StatusCode, rv)
	}
}
