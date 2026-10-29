package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestMaintenanceAPI(t *testing.T) {
	ts, st := newAlertTestServer(t)
	c := login(t, ts)
	ctx := context.Background()
	st.UpsertHost(ctx, proto.Hello{AgentID: "node1", Hostname: "node1", IntervalSec: 10}, "x", time.Now())
	st.SetHostMeta(ctx, "node1", "", "k3s")

	url := ts.URL + "/api/maintenance"
	now := time.Now()
	for _, tc := range []struct {
		origin string
		m      store.Maintenance
		want   int
	}{
		{"http://evil.example", store.Maintenance{Name: "x", Scope: "all", Kind: "once", Start: now.Unix(), DurationMin: 10}, http.StatusForbidden},
		{ts.URL, store.Maintenance{Name: "x", Scope: "host", Target: "fehlt", Kind: "once", Start: now.Unix(), DurationMin: 10}, http.StatusBadRequest},
		{ts.URL, store.Maintenance{Name: "x\ny", Scope: "all", Kind: "once", Start: now.Unix(), DurationMin: 10}, http.StatusBadRequest},
		{ts.URL, store.Maintenance{Name: "x", Scope: "all", Kind: "weekly", Weekdays: 0, DurationMin: 10}, http.StatusBadRequest},
	} {
		if code := postJSON(t, c, url, tc.origin, tc.m).StatusCode; code != tc.want {
			t.Errorf("%+v: %d, erwartet %d", tc.m, code, tc.want)
		}
	}

	// Gruppe k3s ist gerade in Wartung: Host-Liste zeigt es, ein Alarm wird nicht gemeldet.
	resp := postJSON(t, c, url, ts.URL, store.Maintenance{Name: "Patchday", Scope: "group", Target: "k3s",
		Kind: "once", Start: now.Add(-time.Minute).Unix(), DurationMin: 60, Enabled: true})
	var m store.Maintenance
	json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != http.StatusOK || m.ID == 0 {
		t.Fatalf("anlegen: %d %+v", resp.StatusCode, m)
	}
	var list []maintenanceView
	getJSON(t, c, url, &list)
	if len(list) != 1 || !list[0].Active || list[0].EndsAt == 0 {
		t.Fatalf("liste: %+v", list)
	}
	var hosts []hostView
	getJSON(t, c, ts.URL+"/api/hosts", &hosts)
	if len(hosts) != 1 || hosts[0].Maintenance != "Patchday" {
		t.Fatalf("host-liste: %+v", hosts)
	}

	// Umstellen auf einen anderen Host: node1 ist nicht mehr betroffen.
	m.Scope, m.Target = "all", ""
	m.Enabled = false
	req, _ := http.NewRequest(http.MethodPut, url+"/"+strconv.FormatInt(m.ID, 10), jsonBody(m))
	req.Header.Set("Origin", ts.URL)
	if r, _ := c.Do(req); r.StatusCode != http.StatusOK {
		t.Fatalf("ändern: %d", r.StatusCode)
	}
	hosts = nil
	getJSON(t, c, ts.URL+"/api/hosts", &hosts)
	if hosts[0].Maintenance != "" {
		t.Fatalf("abgeschaltetes fenster wirkt: %+v", hosts[0].Maintenance)
	}
	if code := status(t, c, http.MethodDelete, url+"/"+strconv.FormatInt(m.ID, 10), ts.URL); code != http.StatusNoContent {
		t.Fatalf("löschen: %d", code)
	}
	if code := status(t, c, http.MethodDelete, url+"/"+strconv.FormatInt(m.ID, 10), ts.URL); code != http.StatusNotFound {
		t.Fatalf("doppelt löschen: %d", code)
	}
}

func jsonBody(v any) *bytes.Reader {
	data, _ := json.Marshal(v)
	return bytes.NewReader(data)
}

func TestMaintenanceSilencesAlerts(t *testing.T) {
	got := make(chan notify.Message, 10)
	ts, st := newAlertTestServer(t, &recordChannel{got: got})
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := fakeAgentCaps(t, ctx, ts, "v1.2.0", "", proto.CapMetrics)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	st.SetHostMeta(ctx, "a1", "", "k3s")
	postJSON(t, c, ts.URL+"/api/maintenance", ts.URL, store.Maintenance{Name: "Patchday", Scope: "group", Target: "K3S",
		Kind: "once", Start: time.Now().Add(-time.Minute).Unix(), DurationMin: 60, Enabled: true})

	sendServices(t, ctx, conn, proto.Service{Name: "backup", State: proto.SvcFailed, Detail: "failed/failed"})
	var a store.Alert
	waitFor(t, "alarm", func() bool { var ok bool; a, ok = activeAlerts(t, c, ts.URL)["unit_failed/backup"]; return ok })
	if a.Silenced != "Wartung „Patchday“" || a.NotifiedAt != nil {
		t.Fatalf("alarm: %+v", a)
	}
	select {
	case m := <-got:
		t.Fatalf("während wartung gemeldet: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDependencies(t *testing.T) {
	ts, st := newAlertTestServer(t)
	c := login(t, ts)
	ctx := context.Background()
	for _, id := range []string{"router", "nas", "pi"} {
		st.UpsertHost(ctx, proto.Hello{AgentID: id, Hostname: id, IntervalSec: 10}, "x", time.Now())
	}
	meta := func(id, dep string) int {
		return postJSON(t, c, ts.URL+"/api/hosts/"+id+"/meta", ts.URL, hostMeta{DependsOn: &dep}).StatusCode
	}
	for _, tc := range []struct{ id, dep string }{
		{"nas", "nas"}, {"nas", "host:nas"}, {"nas", "host:fehlt"}, {"nas", "check:99"}, {"nas", "router"}, {"nas", "dns:router"},
	} {
		if code := meta(tc.id, tc.dep); code != http.StatusBadRequest {
			t.Errorf("%s → %q: %d, erwartet 400", tc.id, tc.dep, code)
		}
	}
	if code := meta("nas", "host:router"); code != http.StatusOK {
		t.Fatalf("nas → router: %d", code)
	}
	if code := meta("pi", "host:nas"); code != http.StatusOK {
		t.Fatalf("pi → nas: %d", code)
	}
	if code := meta("router", "host:pi"); code != http.StatusBadRequest {
		t.Errorf("schleife router → pi → nas → router akzeptiert: %d", code)
	}
	// Ohne depends_on bleibt die Abhängigkeit beim Umbenennen erhalten.
	postJSON(t, c, ts.URL+"/api/hosts/nas/meta", ts.URL, hostMeta{DisplayName: "NAS"})
	var hosts []hostView
	getJSON(t, c, ts.URL+"/api/hosts", &hosts)
	for _, h := range hosts {
		if h.ID == "nas" && (h.DependsOn != "host:router" || h.DisplayName != "NAS") {
			t.Fatalf("nas: %+v", h.Host)
		}
	}
	if code := meta("nas", ""); code != http.StatusOK {
		t.Fatalf("aufheben: %d", code)
	}
}

// Ist das Ziel ausgefallen, gilt der Host als „leise“ (Alarme nicht melden).
func TestDependencyQuiet(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(st, auth.New(st), "secret", fstest.MapFS{}, log)
	st.UpsertHost(ctx, proto.Hello{AgentID: "router", Hostname: "fritz", IntervalSec: 10}, "", time.Now())
	st.UpsertHost(ctx, proto.Hello{AgentID: "nas", Hostname: "nas", IntervalSec: 10}, "", time.Now())
	st.SetHostDependsOn(ctx, "nas", "host:router")

	s.conns["router"] = agentConn{}
	if reason, quiet := s.quiet("nas", time.Now()); quiet {
		t.Fatalf("router online, trotzdem leise: %q", reason)
	}
	delete(s.conns, "router")
	if reason, quiet := s.quiet("nas", time.Now()); !quiet || reason != "abhängig von Host router (offline)" {
		t.Fatalf("router offline: %q %v", reason, quiet)
	}
	if _, quiet := s.quiet("router", time.Now()); quiet {
		t.Fatal("router selbst ist leise")
	}
}
