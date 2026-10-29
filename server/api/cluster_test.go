package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestClusterAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes":
			w.Write([]byte(`{"items":[{"metadata":{"name":"node1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`))
		case "/apis/apps/v1/deployments":
			w.Write([]byte(`{"items":[{"metadata":{"name":"otbr","namespace":"smarthome"},"spec":{"replicas":1},"status":{}}]}`))
		case "/apis/apps/v1/daemonsets":
			w.Write([]byte(`{"items":[{"metadata":{"name":"svclb-x","namespace":"kube-system"},"status":{"desiredNumberScheduled":5}}]}`))
		case "/apis/apps/v1/statefulsets", "/api/v1/pods":
			w.Write([]byte(`{"items":[]}`))
		case "/api/v1/namespaces/smarthome/pods/otbr-1/log":
			w.Write([]byte("<script>alert(1)</script>\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer kube.Close()
	t.Setenv("MAT_K8S_API", kube.URL)

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword(testPassword)
	st.SetUser(ctx, "admin", hash, time.Now())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ev, _ := alert.New(ctx, st, func(alert.Event) {}, log, time.Now())
	// Workload-Alarme ohne Mindestdauer, damit der erste Abruf auslöst.
	rules := ev.Rules()
	for i := range rules {
		if rules[i].Kind == alert.KindK8sWorkload {
			rules[i].ForSec = 0
		}
	}
	if err := ev.SetRules(ctx, rules, time.Now()); err != nil {
		t.Fatal(err)
	}

	kc, err := k8s.FromEnv()
	if err != nil || kc == nil {
		t.Fatalf("client: %v", err)
	}
	srv := New(st, auth.New(st), "secret", fstest.MapFS{}, log).WithAlerts(ev, nil)
	watcher := k8s.NewWatcher(kc, log, srv.ClusterSnapshot)
	srv.WithCluster(watcher)
	go watcher.Run(ctx)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := login(t, ts)

	var view struct {
		Nodes     []k8s.Node     `json:"nodes"`
		Workloads []k8s.Workload `json:"workloads"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(view.Nodes) == 0 {
		resp, _ := c.Get(ts.URL + "/api/cluster")
		json.NewDecoder(resp.Body).Decode(&view)
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if len(view.Nodes) != 1 || len(view.Workloads) != 2 {
		t.Fatalf("cluster: %+v", view)
	}

	active := func() []store.Alert {
		resp, _ := c.Get(ts.URL + "/api/alerts")
		var a []store.Alert
		json.NewDecoder(resp.Body).Decode(&a)
		resp.Body.Close()
		return a
	}
	// svclb ist standardmäßig ausgenommen, otbr nicht.
	if a := active(); len(a) != 1 || a[0].Target != "Deployment smarthome/otbr" || a[0].Hostname != "Cluster" {
		t.Fatalf("alarme: %+v", a)
	}

	if r := postJSON(t, c, ts.URL+"/api/cluster/ignore", ts.URL, ignoreView{Patterns: []string{"[unvollständig"}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("ungültiges muster: %d", r.StatusCode)
	}
	if r := postJSON(t, c, ts.URL+"/api/cluster/ignore", ts.URL, ignoreView{Patterns: []string{"kube-system/svclb-*", "smarthome/*"}}); r.StatusCode != http.StatusOK {
		t.Fatalf("ausnahmen speichern: %d", r.StatusCode)
	}
	if a := active(); len(a) != 0 {
		t.Fatalf("alarm trotz ausnahme aktiv: %+v", a)
	}

	resp, _ := c.Get(ts.URL + "/api/cluster/logs?ns=smarthome&pod=otbr-1&lines=50")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || string(body) != "<script>alert(1)</script>\n" {
		t.Fatalf("logs: %d %v %q", resp.StatusCode, resp.Header, body)
	}
	for _, q := range []string{"ns=../x&pod=p", "ns=a&pod=p&lines=99999", "ns=a&pod=p&container=a%2Fb"} {
		if code := status(t, c, "GET", ts.URL+"/api/cluster/logs?"+q, ""); code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, code)
		}
	}
	if code := status(t, c, "GET", ts.URL+"/api/cluster/logs?ns=a&pod=fehlt", ""); code != http.StatusNotFound {
		t.Errorf("unbekannter pod: %d", code)
	}
}
