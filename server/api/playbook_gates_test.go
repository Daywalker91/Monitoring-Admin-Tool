package api

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/checks"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const gateBook = `name: gates
targets: [n1, n2]
steps:
  - {id: vorher, action: packages.refresh}
  - {id: gesund, gate: cluster_healthy, timeout: 1s}
  - {id: danach, action: packages.refresh}
`

// TestPlaybookGate: Das Gate wartet, bis die Prüfung stabil erfüllt ist.
func TestPlaybookGate(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{"g.yaml": gateBook})
	var calls atomic.Int32
	f.gate = func(host string, step playbook.Step) (bool, string, error) {
		if step.Gate != playbook.GateClusterHealthy {
			t.Errorf("gate %q", step.Gate)
		}
		if calls.Add(1) < 3 {
			return false, "node n1 nicht bereit", nil
		}
		return true, "", nil
	}
	run, err := s.startPlaybook(context.Background(), "gates", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunOK || f.started() != "n1/vorher n1/danach n2/vorher n2/danach" || r.Steps[1].Message != "erfüllt" {
		t.Fatalf("%s: %s / %s / %q", r.Status, stepStates(r), f.started(), r.Steps[1].Message)
	}
}

// TestPlaybookGateTimeout: Nach der Frist schlägt das Gate fehl, on_error: stop hält an.
func TestPlaybookGateTimeout(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{"g.yaml": gateBook})
	f.gate = func(string, playbook.Step) (bool, string, error) { return false, "Deployment a/b (0/1 bereit)", nil }
	run, _ := s.startPlaybook(context.Background(), "gates", "admin", "")
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunStopped || f.started() != "n1/vorher" ||
		r.Steps[1].Message != "nach 1s nicht erfüllt: Deployment a/b (0/1 bereit)" {
		t.Fatalf("%s: %s / %s / %q", r.Status, stepStates(r), f.started(), r.Steps[1].Message)
	}
}

// TestPlaybookGateFatal: Ein Gate, das nie erfüllt werden kann, scheitert sofort.
func TestPlaybookGateFatal(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{"g.yaml": gateBook})
	f.gate = func(string, playbook.Step) (bool, string, error) { return false, "", errGate{"keine k3s-anbindung"} }
	start := time.Now()
	run, _ := s.startPlaybook(context.Background(), "gates", "admin", "")
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunStopped || r.Steps[1].Message != "keine k3s-anbindung" || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("%s: %q nach %v", r.Status, r.Steps[1].Message, time.Since(start))
	}
}

// TestPlaybookGateCancel: Ein Abbruch beendet das Warten.
func TestPlaybookGateCancel(t *testing.T) {
	book := strings.Replace(gateBook, "timeout: 1s", "timeout: 1h", 1)
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{"g.yaml": book})
	waiting := make(chan struct{}, 1)
	f.gate = func(string, playbook.Step) (bool, string, error) {
		select {
		case waiting <- struct{}{}:
		default:
		}
		return false, "wartet", nil
	}
	run, _ := s.startPlaybook(context.Background(), "gates", "admin", "")
	<-waiting
	if _, err := s.cancelPlaybook(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunCancelled || r.Steps[1].Status != store.StepSkipped || f.started() != "n1/vorher" {
		t.Fatalf("%s: %s / %s", r.Status, stepStates(r), f.started())
	}
}

// TestHostGate prüft host_healthy mit echten Dienstlisten.
func TestHostGate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	st.UpsertHost(ctx, proto.Hello{AgentID: "h1", Hostname: "h1"}, "", time.Now())
	st.SetServiceWatch(ctx, "h1", "nginx.service", true)
	s := New(st, nil, "token", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t0 := time.Now()

	gate := func(since time.Time) (bool, string) {
		ok, detail, err := s.gateState(ctx, playbook.Step{Gate: playbook.GateHostHealthy}, "h1", since)
		if err != nil {
			t.Fatal(err)
		}
		return ok, detail
	}
	if ok, d := gate(t0); ok || d != "host nicht verbunden" {
		t.Fatalf("offline: %v %q", ok, d)
	}
	s.conns["h1"] = agentConn{since: t0}
	setSvcs := func(at time.Time, list ...proto.Service) {
		s.mu.Lock()
		s.svcs["h1"] = svcState{list: list, at: at}
		s.mu.Unlock()
	}
	setSvcs(t0.Add(-time.Minute), proto.Service{Name: "nginx.service", State: proto.SvcRunning})
	if ok, d := gate(t0); ok || d != "warte auf aktuelle dienstliste" {
		t.Fatalf("alte liste: %v %q", ok, d)
	}
	setSvcs(t0.Add(time.Second), proto.Service{Name: "nginx.service", State: proto.SvcStopped},
		proto.Service{Name: "x.service", State: proto.SvcFailed})
	if ok, d := gate(t0); ok || d != "dienst nginx.service läuft nicht, unit x.service failed" {
		t.Fatalf("probleme: %v %q", ok, d)
	}
	setSvcs(t0.Add(time.Second), proto.Service{Name: "nginx.service", State: proto.SvcRunning})
	if ok, d := gate(t0); !ok || d != "" {
		t.Fatalf("gesund: %v %q", ok, d)
	}
	setSvcs(t0.Add(time.Second))
	if ok, d := gate(t0); ok || d != "dienst nginx.service fehlt" {
		t.Fatalf("fehlt: %v %q", ok, d)
	}
}

// TestCheckGate prüft check_ok: Name, Abschaltung und frisches Ergebnis.
func TestCheckGate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cr := checks.NewRunner(st, log, nil)
	s := New(st, nil, "token", nil, log).WithChecks(cr)
	c := store.Check{Name: "Nextcloud", Kind: "tcp", Target: "127.0.0.1:1", IntervalSec: 60, TimeoutSec: 5, Enabled: true}
	if err := s.store.CreateCheck(ctx, &c); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	gate := func(name string) (bool, string, error) {
		return s.gateState(ctx, playbook.Step{Gate: playbook.GateCheckOK, Check: name}, "", t0)
	}
	if _, _, err := gate("gibtsnicht"); err == nil || !strings.Contains(err.Error(), "nicht gefunden") {
		t.Fatalf("unbekannt: %v", err)
	}
	if err := cr.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, d, err := gate("nextcloud"); ok || err != nil || d != "warte auf neues ergebnis von Nextcloud" {
		t.Fatalf("ohne ergebnis: %v %q %v", ok, d, err)
	}
	setLast := func(r store.CheckResult) {
		if err := st.SaveCheckResult(ctx, c.ID, r); err != nil {
			t.Fatal(err)
		}
		if err := cr.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}
	setLast(store.CheckResult{TS: t0.Unix(), OK: false, Error: "verbindung abgelehnt"})
	if ok, d, _ := gate("nextcloud"); ok || d != "check Nextcloud schlägt fehl: verbindung abgelehnt" {
		t.Fatalf("fehler: %v %q", ok, d)
	}
	setLast(store.CheckResult{TS: t0.Unix() + 1, OK: true})
	if ok, _, err := gate("NEXTCLOUD"); !ok || err != nil {
		t.Fatalf("ok: %v %v", ok, err)
	}
}
