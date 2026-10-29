package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
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
