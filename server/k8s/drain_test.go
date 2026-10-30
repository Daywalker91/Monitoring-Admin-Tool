package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeNodeAPI hält einen Node im Speicher und nimmt Merge-Patches, Evictions
// und SelfSubjectAccessReviews an.
type fakeNodeAPI struct {
	mu          sync.Mutex
	node        map[string]any
	pods        string
	evicted     []string
	blocked     map[string]bool // Pod → 429
	allowed     map[string]bool // "verb/resource[/sub]" → erlaubt
	patchStatus int
}

func newFakeNodeAPI(t *testing.T, f *fakeNodeAPI) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/api/v1/nodes/node2" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(f.node)
		case r.URL.Path == "/api/v1/nodes/node2" && r.Method == http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				t.Errorf("content-type %q", r.Header.Get("Content-Type"))
			}
			if f.patchStatus != 0 {
				w.WriteHeader(f.patchStatus)
				return
			}
			var p map[string]any
			json.Unmarshal(body, &p)
			merge(f.node, p)
			json.NewEncoder(w).Encode(f.node)
		case r.URL.Path == "/api/v1/pods" && r.URL.Query().Get("fieldSelector") == "spec.nodeName=node2":
			w.Write([]byte(f.pods))
		case strings.HasSuffix(r.URL.Path, "/eviction") && r.Method == http.MethodPost:
			parts := strings.Split(r.URL.Path, "/")
			key := parts[4] + "/" + parts[6]
			if f.blocked[key] {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			f.evicted = append(f.evicted, key)
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			var req struct {
				Spec struct {
					ResourceAttributes map[string]string `json:"resourceAttributes"`
				} `json:"spec"`
			}
			json.Unmarshal(body, &req)
			a := req.Spec.ResourceAttributes
			key := a["verb"] + "/" + a["resource"]
			if a["subresource"] != "" {
				key += "/" + a["subresource"]
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"allowed": f.allowed[key]}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL, http: srv.Client()}
}

// merge wendet einen JSON-Merge-Patch (RFC 7386) an.
func merge(dst, patch map[string]any) {
	for k, v := range patch {
		switch v := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			sub, _ := dst[k].(map[string]any)
			if sub == nil {
				sub = map[string]any{}
				dst[k] = sub
			}
			merge(sub, v)
		default:
			dst[k] = v
		}
	}
}

func readyNode() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "node2", "resourceVersion": "7"},
		"spec":     map[string]any{},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}
}

func TestCordonUncordon(t *testing.T) {
	f := &fakeNodeAPI{node: readyNode()}
	c := newFakeNodeAPI(t, f)
	ctx := context.Background()

	n, err := c.Node(ctx, "node2")
	if err != nil || !n.Ready || n.Unschedulable || n.ResourceVersion != "7" {
		t.Fatalf("node: %+v %v", n, err)
	}
	if err := c.Cordon(ctx, n, "job1"); err != nil {
		t.Fatal(err)
	}
	if n, _ = c.Node(ctx, "node2"); !n.Unschedulable || n.CordonedBy != "job1" {
		t.Fatalf("nach cordon: %+v", n)
	}
	// Fremde Job-ID gibt nicht frei.
	if ok, err := c.Uncordon(ctx, "node2", "job2"); ok || err != nil {
		t.Fatalf("fremde id: %v %v", ok, err)
	}
	if ok, err := c.Uncordon(ctx, "node2", "job1"); !ok || err != nil {
		t.Fatalf("uncordon: %v %v", ok, err)
	}
	if n, _ = c.Node(ctx, "node2"); n.Unschedulable || n.CordonedBy != "" {
		t.Fatalf("nach uncordon: %+v", n)
	}

	f.patchStatus = http.StatusConflict
	if err := c.Cordon(ctx, n, "job3"); err == nil || !strings.Contains(err.Error(), "geändert") {
		t.Fatalf("409: %v", err)
	}
	f.patchStatus = http.StatusForbidden
	if err := c.Cordon(ctx, n, "job3"); err == nil || !strings.Contains(err.Error(), "berechtigung") {
		t.Fatalf("403: %v", err)
	}
	if _, err := c.Node(ctx, "Node_2"); err == nil {
		t.Fatal("ungültiger name angenommen")
	}
}

func TestPodsToEvictAndEvict(t *testing.T) {
	f := &fakeNodeAPI{node: readyNode(), blocked: map[string]bool{"longhorn-system/instance-manager-x": true},
		pods: `{"items":[
 {"metadata":{"name":"web-1","namespace":"app","ownerReferences":[{"kind":"ReplicaSet"}]},"status":{"phase":"Running"}},
 {"metadata":{"name":"svclb-1","namespace":"kube-system","ownerReferences":[{"kind":"DaemonSet"}]},"status":{"phase":"Running"}},
 {"metadata":{"name":"static","namespace":"kube-system","annotations":{"kubernetes.io/config.mirror":"x"}},"status":{"phase":"Running"}},
 {"metadata":{"name":"job-1","namespace":"app"},"status":{"phase":"Succeeded"}},
 {"metadata":{"name":"instance-manager-x","namespace":"longhorn-system"},"status":{"phase":"Running"}},
 {"metadata":{"name":"alt","namespace":"app","deletionTimestamp":"2026-10-30T10:00:00Z"},"status":{"phase":"Running"}}]}`}
	c := newFakeNodeAPI(t, f)
	ctx := context.Background()

	pods, err := c.PodsToEvict(ctx, "node2")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range pods {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "web-1,instance-manager-x,alt" || !pods[2].Terminating || pods[0].Terminating {
		t.Fatalf("pods: %+v", pods)
	}
	if err := c.Evict(ctx, pods[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Evict(ctx, pods[1]); !errors.Is(err, ErrBlocked) {
		t.Fatalf("pdb: %v", err)
	}
	if err := c.Evict(ctx, EvictPod{Namespace: "app", Name: "../x"}); err == nil {
		t.Fatal("ungültiger name angenommen")
	}
	if strings.Join(f.evicted, ",") != "app/web-1" {
		t.Fatalf("verdrängt: %v", f.evicted)
	}
}

func TestCanDrain(t *testing.T) {
	f := &fakeNodeAPI{allowed: map[string]bool{"patch/nodes": true}}
	c := newFakeNodeAPI(t, f)
	if ok, err := c.CanDrain(context.Background()); ok || err != nil {
		t.Fatalf("ohne eviction: %v %v", ok, err)
	}
	f.allowed["create/pods/eviction"] = true
	if ok, err := c.CanDrain(context.Background()); !ok || err != nil {
		t.Fatalf("mit allen rechten: %v %v", ok, err)
	}
}
