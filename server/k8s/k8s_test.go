package k8s

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI liefert feste Antworten je Pfad; fehlende Pfade ergeben 404.
func fakeAPI(t *testing.T, routes map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("nur GET erwartet, kam %s", r.Method)
		}
		key := r.URL.Path
		if r.URL.RawQuery != "" && strings.HasSuffix(key, "/log") {
			key += "?" + r.URL.Query().Get("container") + "," + r.URL.Query().Get("tailLines")
		}
		body, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if body == "403" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL, http: srv.Client()}
}

const (
	nodesJSON = `{"items":[
 {"metadata":{"name":"node1","creationTimestamp":"2026-01-01T00:00:00Z","labels":{"node-role.kubernetes.io/control-plane":"true"}},
  "status":{"capacity":{"cpu":"4","memory":"8Gi"},"conditions":[{"type":"Ready","status":"True"}],
  "addresses":[{"type":"InternalIP","address":"192.168.4.20"}],"nodeInfo":{"kubeletVersion":"v1.35.5+k3s1","architecture":"arm64","osImage":"Armbian"}}},
 {"metadata":{"name":"node2","creationTimestamp":"2026-01-01T00:00:00Z"},
  "status":{"capacity":{"cpu":"8","memory":"8000000Ki"},"conditions":[{"type":"Ready","status":"Unknown","message":"Kubelet stopped posting node status."}]}}]}`
	nodeMetricsJSON = `{"items":[{"metadata":{"name":"node1"},"usage":{"cpu":"500m","memory":"2Gi"}}]}`
	deployJSON      = `{"items":[
 {"metadata":{"name":"mat-server","namespace":"managment"},"spec":{"replicas":1},"status":{"readyReplicas":1}},
 {"metadata":{"name":"otbr","namespace":"smarthome"},"spec":{"replicas":1},"status":{}},
 {"metadata":{"name":"aus","namespace":"x"},"spec":{"replicas":0},"status":{}}]}`
	dsJSON   = `{"items":[{"metadata":{"name":"svclb-traefik","namespace":"kube-system"},"status":{"desiredNumberScheduled":5,"numberReady":4}}]}`
	podsJSON = `{"items":[
 {"metadata":{"name":"mat-server-abc","namespace":"managment","ownerReferences":[{"kind":"ReplicaSet"}]},
  "spec":{"nodeName":"node1","containers":[{"name":"mat-server"}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"mat-server","ready":true,"restartCount":2,"state":{"running":{}}}]}},
 {"metadata":{"name":"kaputt","namespace":"default"},
  "spec":{"nodeName":"node2","containers":[{"name":"a"},{"name":"b"}]},
  "status":{"phase":"Running","containerStatuses":[{"name":"a","ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}},{"name":"b","ready":true}]}}]}`
	appsJSON = `{"items":[
 {"metadata":{"name":"mat-server","namespace":"argocd"},"spec":{"project":"managment","source":{"path":"Managment/mat-server"},"destination":{"namespace":"managment"}},
  "status":{"sync":{"status":"Synced","revision":"0c7b8b5abcdef"},"health":{"status":"Healthy"}}},
 {"metadata":{"name":"otbr","namespace":"argocd"},"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Degraded"}}}]}`
)

func TestFetch(t *testing.T) {
	c := fakeAPI(t, map[string]string{
		"/api/v1/nodes":                           nodesJSON,
		"/apis/metrics.k8s.io/v1beta1/nodes":      nodeMetricsJSON,
		"/apis/apps/v1/deployments":               deployJSON,
		"/apis/apps/v1/statefulsets":              `{"items":[]}`,
		"/apis/apps/v1/daemonsets":                dsJSON,
		"/api/v1/pods":                            podsJSON,
		"/apis/argoproj.io/v1alpha1/applications": appsJSON,
	})
	s, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Errors) != 0 {
		t.Fatalf("teilfehler: %v", s.Errors)
	}
	n1, n2 := s.Nodes[0], s.Nodes[1]
	if !n1.Ready || n1.Roles[0] != "control-plane" || n1.IP != "192.168.4.20" || *n1.CPUPct != 12.5 || *n1.MemPct != 25 || n1.Pods != 1 {
		t.Fatalf("node1: %+v", n1)
	}
	if n2.Ready || n2.CPUPct != nil || n2.MemBytes != 8000000*1024 || !strings.Contains(n2.Reason, "stopped") {
		t.Fatalf("node2: %+v", n2)
	}
	if len(s.Workloads) != 4 || len(s.Pods) != 2 || !s.HasArgo || len(s.Apps) != 2 || s.Apps[0].Revision != "0c7b8b5a" {
		t.Fatalf("snapshot: %+v", s)
	}
	if p := s.Pods[0]; p.Name != "kaputt" || p.Reason != "CrashLoopBackOff" || p.Ready != 1 || p.Total != 2 || p.Restarts != 7 {
		t.Fatalf("pod: %+v", p)
	}

	nodes, ok := s.NodeConds()
	if !ok || nodes["node1"].Bad || !nodes["node2"].Bad {
		t.Fatalf("node-bedingungen: %+v", nodes)
	}
	wl, ok := s.WorkloadConds()
	if !ok || wl["Deployment managment/mat-server"].Bad || !wl["Deployment smarthome/otbr"].Bad ||
		wl["Deployment x/aus"].Bad || !wl["DaemonSet kube-system/svclb-traefik"].Bad ||
		wl["Pod default/kaputt"].Detail != "CrashLoopBackOff, 7 Neustarts" {
		t.Fatalf("workload-bedingungen: %+v", wl)
	}
	apps, ok := s.AppConds()
	if !ok || apps["mat-server"].Bad || !apps["otbr"].Bad {
		t.Fatalf("app-bedingungen: %+v", apps)
	}
}

func TestFetchPartial(t *testing.T) {
	// Ohne metrics-server und ArgoCD, StatefulSets verboten.
	c := fakeAPI(t, map[string]string{
		"/api/v1/nodes":              nodesJSON,
		"/apis/apps/v1/deployments":  deployJSON,
		"/apis/apps/v1/statefulsets": "403",
		"/apis/apps/v1/daemonsets":   dsJSON,
		"/api/v1/pods":               podsJSON,
	})
	s, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.HasArgo || len(s.Errors) != 2 || !strings.Contains(strings.Join(s.Errors, ";"), "keine berechtigung") {
		t.Fatalf("teilfehler: %v", s.Errors)
	}
	if _, ok := s.WorkloadConds(); ok {
		t.Fatal("unvollständige workloads als vollständig gemeldet")
	}
	if _, ok := s.AppConds(); !ok {
		t.Fatal("ohne argocd sollen apps als vollständig (leer) gelten")
	}
	if _, err := fakeAPI(t, map[string]string{}).Fetch(context.Background()); err == nil {
		t.Fatal("ohne nodes kein fehler")
	}
}

func TestLogs(t *testing.T) {
	c := fakeAPI(t, map[string]string{"/api/v1/namespaces/managment/pods/mat-server-abc/log?mat-server,100": "zeile 1\nzeile 2\n"})
	w := NewWatcher(c, nil, nil)
	text, err := w.Logs(context.Background(), "managment", "mat-server-abc", "mat-server", 100, false)
	if err != nil || text != "zeile 1\nzeile 2\n" {
		t.Fatalf("logs: %q %v", text, err)
	}
	for _, bad := range [][3]string{{"../x", "p", ""}, {"ns", "p/../../x", ""}, {"ns", "p", "a b"}, {"NS", "p", ""}} {
		if _, err := w.Logs(context.Background(), bad[0], bad[1], bad[2], 10, false); err == nil {
			t.Errorf("ungültiger name akzeptiert: %v", bad)
		}
	}
}

func TestParseQuantities(t *testing.T) {
	for in, want := range map[string]float64{"4": 4, "250m": 0.25, "1500000000n": 1.5, "100u": 0.0001} {
		if got := parseCPU(in); got != want {
			t.Errorf("parseCPU(%q) = %v", in, got)
		}
	}
	for in, want := range map[string]int64{"1Ki": 1024, "2Gi": 2 << 30, "512M": 512e6, "1000": 1000} {
		if got := parseMem(in); got != want {
			t.Errorf("parseMem(%q) = %v", in, got)
		}
	}
	if !ValidName("svclb-traefik-639b84de") || ValidName("-x") || ValidName("x.") || ValidName(strings.Repeat("a", 254)) {
		t.Error("ValidName")
	}
}
