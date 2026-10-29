package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func sendServices(t *testing.T, ctx context.Context, conn *websocket.Conn, list ...proto.Service) {
	t.Helper()
	data, _ := proto.Encode(proto.TypeServices, proto.Services{Time: time.Now(), List: list})
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

// activeAlerts liefert die aktiven Alarme als "typ/ziel".
func activeAlerts(t *testing.T, c *http.Client, url string) map[string]store.Alert {
	t.Helper()
	var list []store.Alert
	getJSON(t, c, url+"/api/alerts", &list)
	out := map[string]store.Alert{}
	for _, a := range list {
		if a.ResolvedAt == nil {
			out[a.Kind+"/"+a.Target] = a
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal(what)
}

func TestServicesAPI(t *testing.T) {
	ts, _ := newAlertTestServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := fakeAgentCaps(t, ctx, ts, "v1.2.0", "", proto.CapMetrics)
	ssh := proto.Service{Name: "ssh", State: proto.SvcRunning, Detail: "active/running", StartType: "enabled"}
	nginx := proto.Service{Name: "nginx", State: proto.SvcFailed, Detail: "failed/failed", StartType: "enabled"}
	backup := proto.Service{Name: "backup", State: proto.SvcStopped, Detail: "inactive/dead", StartType: "static"}
	sendServices(t, ctx, conn, ssh, nginx, backup)

	var v servicesView
	waitFor(t, "dienstliste", func() bool { getJSON(t, c, ts.URL+"/api/hosts/a1/services", &v); return len(v.List) == 3 })
	if v.Updated == 0 || len(v.Watches) != 0 {
		t.Fatalf("liste: %+v", v)
	}
	h := waitHost(t, c, ts, "zusammenfassung", func(h hostView) bool { return h.Services != nil })
	if h.Services.Total != 3 || h.Services.Failed != 1 {
		t.Errorf("zusammenfassung: %+v", h.Services)
	}
	// Fehlgeschlagene Unit löst sofort aus (Standard ohne Mindestdauer).
	waitFor(t, "alarm unit_failed", func() bool { _, ok := activeAlerts(t, c, ts.URL)["unit_failed/nginx"]; return ok })

	url := ts.URL + "/api/hosts/a1/services/watch"
	for _, tc := range []struct {
		url, origin string
		body        watchRequest
		want        int
	}{
		{url, "http://evil.example", watchRequest{"backup", true}, http.StatusForbidden},
		{url, ts.URL, watchRequest{"-x", true}, http.StatusBadRequest},
		{url, ts.URL, watchRequest{"a/b", true}, http.StatusBadRequest},
		{ts.URL + "/api/hosts/fehlt/services/watch", ts.URL, watchRequest{"backup", true}, http.StatusNotFound},
	} {
		if code := postJSON(t, c, tc.url, tc.origin, tc.body).StatusCode; code != tc.want {
			t.Errorf("%+v: %d, erwartet %d", tc.body, code, tc.want)
		}
	}

	// Überwachte Dienste ohne Mindestdauer, damit der Test nicht warten muss.
	rules := map[string]any{"rules": []store.AlertRule{{Kind: "service", Enabled: true, ForSec: 0}}}
	if r := postJSON(t, c, ts.URL+"/api/hosts/a1/alert-rules", ts.URL, rules); r.StatusCode != http.StatusOK {
		t.Fatalf("regel: %d", r.StatusCode)
	}
	for _, name := range []string{"backup", "ssh", "weg"} {
		if code := postJSON(t, c, url, ts.URL, watchRequest{name, true}).StatusCode; code != http.StatusNoContent {
			t.Fatalf("überwachen %s: %d", name, code)
		}
	}
	waitFor(t, "alarme service", func() bool {
		a := activeAlerts(t, c, ts.URL)
		_, b := a["service/backup"]
		_, w := a["service/weg"]
		_, s := a["service/ssh"]
		return b && w && !s
	})
	if a := activeAlerts(t, c, ts.URL)["service/weg"]; a.Detail != "nicht vorhanden" {
		t.Errorf("fehlender dienst: %+v", a)
	}

	// backup läuft wieder, nginx erholt sich, "weg" wird nicht mehr überwacht.
	backup.State, backup.Detail = proto.SvcRunning, "active/running"
	nginx.State, nginx.Detail = proto.SvcRunning, "active/running"
	sendServices(t, ctx, conn, ssh, nginx, backup)
	postJSON(t, c, url, ts.URL, watchRequest{"weg", false})
	waitFor(t, "alarme beendet", func() bool { return len(activeAlerts(t, c, ts.URL)) == 0 })

	getJSON(t, c, ts.URL+"/api/hosts/a1/services", &v)
	if b, _ := json.Marshal(v.Watches); string(b) != `["backup","ssh"]` {
		t.Errorf("überwacht: %s", b)
	}
}

// Die Mindestdauer läuft auch ab, wenn der Agent keine neue Liste schickt.
func TestServicesReevaluate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Unix(1_800_000_000, 0)
	ev, err := alert.New(ctx, st, func(alert.Event) {}, log, now)
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, auth.New(st), "secret", fstest.MapFS{}, log).WithAlerts(ev, nil)
	s.nowFor = func() time.Time { return now }
	st.UpsertHost(ctx, proto.Hello{AgentID: "a1", Hostname: "pi", IntervalSec: 10}, "", now)
	st.SetServiceWatch(ctx, "a1", "backup", true)
	s.conns["a1"] = agentConn{} // gilt als verbunden

	s.observeServices(ctx, "a1", "pi", proto.Services{List: []proto.Service{{Name: "backup", State: proto.SvcStopped}}}, []byte("{}"), now)
	if n := len(ev.ActiveCount()["a1"]); n != 0 {
		t.Fatalf("vor ablauf der mindestdauer: %d", n)
	}
	now = now.Add(61 * time.Second)
	s.reevaluateServices(ctx)
	if n := ev.ActiveCount()["a1"][alert.KindService]; n != 1 {
		t.Fatalf("nach ablauf: %d alarme", n)
	}
	alerts, _ := st.Alerts(ctx, true, time.Time{}, 10)
	if len(alerts) != 1 || alerts[0].Hostname != "pi" || alerts[0].Target != "backup" {
		t.Fatalf("alarm: %+v", alerts)
	}
}
