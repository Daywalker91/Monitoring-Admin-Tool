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
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// fakeDrain ist ein Cluster mit einem Node "a1" im Speicher.
type fakeDrain struct {
	mu      sync.Mutex
	allowed bool
	node    k8s.NodeState
	pods    []k8s.EvictPod
	blocked map[string]bool // Pod-Name → PDB blockiert
	unblock string          // verdrängter Pod, der alle PDB-Blockaden löst (Longhorn-Engine)
	log     []string
}

func (f *fakeDrain) Node(_ context.Context, name string) (k8s.NodeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != f.node.Name {
		return k8s.NodeState{}, k8s.ErrNotFound
	}
	return f.node, nil
}

func (f *fakeDrain) Cordon(_ context.Context, _ k8s.NodeState, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.node.Unschedulable, f.node.CordonedBy = true, jobID
	f.log = append(f.log, "cordon")
	return nil
}

func (f *fakeDrain) Uncordon(_ context.Context, _, jobID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.node.CordonedBy != jobID {
		return false, nil
	}
	f.node.Unschedulable, f.node.CordonedBy = false, ""
	f.log = append(f.log, "uncordon")
	return true, nil
}

func (f *fakeDrain) PodsToEvict(context.Context, string) ([]k8s.EvictPod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]k8s.EvictPod(nil), f.pods...), nil
}

func (f *fakeDrain) Evict(_ context.Context, p k8s.EvictPod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked[p.Name] {
		return k8s.ErrBlocked
	}
	for i, q := range f.pods {
		if q.Name == p.Name {
			f.pods = append(f.pods[:i], f.pods[i+1:]...)
			break
		}
	}
	f.log = append(f.log, "evict "+p.Name)
	if p.Name == f.unblock {
		f.blocked = nil
	}
	return nil
}

func (f *fakeDrain) CanDrain(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowed, nil
}

func (f *fakeDrain) set(fn func(f *fakeDrain)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeDrain) events() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, ",")
}

func newDrainTestServer(t *testing.T, f *fakeDrain, setup ...func(*store.Store)) (*httptest.Server, *Server) {
	t.Helper()
	oldPoll, oldDrain, oldReturn, oldMove := drainPoll, drainTimeout, returnTimeout, selfMoveWait
	drainPoll, drainTimeout, returnTimeout, selfMoveWait = 10*time.Millisecond, 2*time.Second, 3*time.Second, 50*time.Millisecond
	t.Cleanup(func() { drainPoll, drainTimeout, returnTimeout, selfMoveWait = oldPoll, oldDrain, oldReturn, oldMove })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword(testPassword)
	st.SetUser(ctx, "admin", hash, time.Now())
	for _, fn := range setup {
		fn(st)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(st, auth.New(st), "secret", fstest.MapFS{}, log).WithDrain(f)
	go srv.RunDrain(ctx)
	waitFor(t, "drain bereit", func() bool { return srv.canDrain() == f.allowed && srv.drainReady() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// drainReady meldet, ob RunDrain läuft (nur für Tests).
func (s *Server) drainReady() bool {
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	return s.drain.ctx != nil
}

// bootAgent verbindet einen Agenten "a1" mit Neustart-Fähigkeit und Boot-Zeit.
func bootAgent(t *testing.T, ctx context.Context, ts *httptest.Server, boot int64) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	data, _ := proto.Encode(proto.TypeHello, proto.Hello{AgentID: "a1", Hostname: "a1.lan", IntervalSec: 10, AgentVersion: "v1.4.0",
		OS: "linux", Arch: "arm64", BootTime: boot, Capabilities: []proto.Capability{proto.CapMetrics, proto.CapReboot}})
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	return conn
}

func waitJob(t *testing.T, c *http.Client, ts *httptest.Server, id string) store.Job {
	t.Helper()
	var j store.Job
	waitFor(t, "job "+id+" fertig", func() bool {
		getJSON(t, c, ts.URL+"/api/jobs/"+id, &j)
		return j.FinishedAt != nil
	})
	return j
}

func startDrain(t *testing.T, c *http.Client, ts *httptest.Server) (int, store.Job) {
	t.Helper()
	resp := postJSON(t, c, ts.URL+"/api/hosts/a1/jobs", ts.URL, jobRequest{Action: actionDrainReboot})
	var j store.Job
	json.NewDecoder(resp.Body).Decode(&j)
	return resp.StatusCode, j
}

func TestDrainReboot(t *testing.T) {
	f := &fakeDrain{allowed: true, node: k8s.NodeState{Name: "a1", Ready: true},
		pods:    []k8s.EvictPod{{Namespace: "app", Name: "web"}, {Namespace: "longhorn-system", Name: "im"}},
		blocked: map[string]bool{"im": true}}
	ts, _ := newDrainTestServer(t, f)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := bootAgent(t, ctx, ts, 100)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })

	code, j := startDrain(t, c, ts)
	if code != http.StatusAccepted || j.Action != actionDrainReboot || j.Params["node"] != "a1" || j.User != "admin" {
		t.Fatalf("start: %d %+v", code, j)
	}
	if code, _ := startDrain(t, c, ts); code != http.StatusConflict {
		t.Fatalf("zweiter drain: %d", code)
	}
	waitHost(t, c, ts, "draining", func(h hostView) bool { return h.Draining == j.ID })

	// Longhorn gibt nach einer Weile frei.
	time.Sleep(50 * time.Millisecond)
	f.set(func(f *fakeDrain) { f.blocked = nil })

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	var reboot proto.JobRequest
	json.Unmarshal(data, &env)
	json.Unmarshal(env.Data, &reboot)
	if reboot.Action != proto.ActionReboot {
		t.Fatalf("beim agenten: %s", data)
	}
	if ev := f.events(); ev != "cordon,evict web,evict im" {
		t.Fatalf("vor dem neustart: %s", ev)
	}
	res, _ := proto.Encode(proto.TypeJobResult, proto.JobResult{JobID: reboot.JobID, OK: true, Output: "neustart in 5 s"})
	conn.Write(ctx, websocket.MessageText, res)
	conn.Close(websocket.StatusNormalClosure, "")

	// Gleiche Boot-Zeit zählt nicht als Neustart.
	conn = bootAgent(t, ctx, ts, 100)
	time.Sleep(100 * time.Millisecond)
	if ev := f.events(); strings.Contains(ev, "uncordon") {
		t.Fatalf("ohne neue boot-zeit freigegeben: %s", ev)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	bootAgent(t, ctx, ts, 200)

	done := waitJob(t, c, ts, j.ID)
	if !done.OK || !strings.Contains(done.Output, "freigegeben (uncordon)") || !strings.Contains(done.Output, "wartet auf PodDisruptionBudget") {
		t.Fatalf("ergebnis: %+v", done)
	}
	if ev := f.events(); !strings.HasSuffix(ev, ",uncordon") {
		t.Fatalf("ablauf: %s", ev)
	}
	waitHost(t, c, ts, "nicht mehr draining", func(h hostView) bool { return h.Draining == "" })
}

// Bleibt ein Pod blockiert, wird abgebrochen: Node frei, kein Neustart.
func TestDrainAbort(t *testing.T) {
	f := &fakeDrain{allowed: true, node: k8s.NodeState{Name: "a1", Ready: true},
		pods: []k8s.EvictPod{{Namespace: "app", Name: "fest"}}, blocked: map[string]bool{"fest": true}}
	ts, _ := newDrainTestServer(t, f)
	drainTimeout = 200 * time.Millisecond
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := bootAgent(t, ctx, ts, 100)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })

	code, j := startDrain(t, c, ts)
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	done := waitJob(t, c, ts, j.ID)
	if done.OK || !strings.Contains(done.Error, "app/fest") || !strings.Contains(done.Output, "kein Neustart") {
		t.Fatalf("ergebnis: %+v", done)
	}
	if ev := f.events(); ev != "cordon,uncordon" {
		t.Fatalf("ablauf: %s", ev)
	}
	rctx, rcancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer rcancel()
	if _, data, err := conn.Read(rctx); err == nil {
		t.Fatalf("trotz abbruch an den agenten: %s", data)
	}
}

// Der Server liegt auf einem Longhorn-Volume: Dessen Engine hält das PDB des
// Instance-Managers, bis der Server-Pod weg ist. Der Pod muss deshalb vorziehen.
func TestDrainSelfMovesWhenBlocked(t *testing.T) {
	f := &fakeDrain{allowed: true, node: k8s.NodeState{Name: "a1", Ready: true},
		pods:    []k8s.EvictPod{{Namespace: "longhorn-system", Name: "instance-manager"}, {Namespace: "managment", Name: "mat-server"}},
		blocked: map[string]bool{"instance-manager": true}, unblock: "mat-server"}
	ts, srv := newDrainTestServer(t, f)
	srv.drain.selfPod = "mat-server"
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bootAgent(t, ctx, ts, 100)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })

	code, _ := startDrain(t, c, ts)
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	waitFor(t, "pods weg", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.pods) == 0
	})
	if ev := f.events(); !strings.Contains(ev, "evict mat-server,evict instance-manager") {
		t.Fatalf("ablauf: %s", ev)
	}
}

// Solange andere Pods noch verdrängt werden, bleibt der eigene Pod.
func TestDrainSelfStaysWhileProgress(t *testing.T) {
	f := &fakeDrain{allowed: true, node: k8s.NodeState{Name: "a1", Ready: true},
		pods: []k8s.EvictPod{{Namespace: "app", Name: "beendet", Terminating: true},
			{Namespace: "longhorn-system", Name: "instance-manager"}, {Namespace: "managment", Name: "mat-server"}},
		blocked: map[string]bool{"instance-manager": true}}
	ts, srv := newDrainTestServer(t, f)
	srv.drain.selfPod = "mat-server"
	drainTimeout = 300 * time.Millisecond
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bootAgent(t, ctx, ts, 100)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })

	code, j := startDrain(t, c, ts)
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	if done := waitJob(t, c, ts, j.ID); done.OK {
		t.Fatalf("ergebnis: %+v", done)
	}
	if ev := f.events(); strings.Contains(ev, "mat-server") {
		t.Fatalf("eigener pod trotz laufender verdrängung umgezogen: %s", ev)
	}
}

func TestDrainRefused(t *testing.T) {
	f := &fakeDrain{node: k8s.NodeState{Name: "a1", Ready: true}}
	ts, srv := newDrainTestServer(t, f)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bootAgent(t, ctx, ts, 100)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })

	if code, _ := startDrain(t, c, ts); code != http.StatusConflict {
		t.Fatalf("ohne rechte: %d", code)
	}
	f.set(func(f *fakeDrain) { f.allowed = true })
	srv.checkDrainRights(ctx)
	f.set(func(f *fakeDrain) { f.node.Unschedulable = true })
	if code, _ := startDrain(t, c, ts); code != http.StatusConflict {
		t.Fatalf("schon abgesperrt: %d", code)
	}
	f.set(func(f *fakeDrain) { f.node = k8s.NodeState{Name: "anderer", Ready: true} })
	if code, _ := startDrain(t, c, ts); code != http.StatusConflict {
		t.Fatalf("ohne node: %d", code)
	}
	if ev := f.events(); ev != "" {
		t.Fatalf("trotz ablehnung: %s", ev)
	}
}

// Nach einem Neustart des Servers (z. B. selbst verdrängt) geht es weiter.
func TestDrainResume(t *testing.T) {
	f := &fakeDrain{allowed: true, node: k8s.NodeState{Name: "a1", Ready: true, Unschedulable: true, CordonedBy: "job-alt"}}
	ts, _ := newDrainTestServer(t, f, func(st *store.Store) {
		ctx := context.Background()
		now := time.Now()
		st.CreateJob(ctx, store.Job{ID: "job-alt", HostID: "a1", Action: actionDrainReboot, User: "admin",
			CreatedAt: now.Unix(), Deadline: now.Add(time.Hour).Unix()})
		st.SaveNodeOp(ctx, store.NodeOp{JobID: "job-alt", HostID: "a1", Node: "a1", Phase: store.NodeOpReboot,
			RebootJob: "job-r", BootTime: 100, PhaseUntil: now.Add(time.Hour).Unix(), Log: "10:00:00  Neustart angestoßen\n"})
	})
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bootAgent(t, ctx, ts, 200)

	done := waitJob(t, c, ts, "job-alt")
	if !done.OK || !strings.HasPrefix(done.Output, "10:00:00  Neustart angestoßen") || !strings.Contains(done.Output, "freigegeben") {
		t.Fatalf("ergebnis: %+v", done)
	}
	if ev := f.events(); ev != "uncordon" {
		t.Fatalf("ablauf: %s", ev)
	}
}
