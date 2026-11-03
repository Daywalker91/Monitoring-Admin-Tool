package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// fakeAgents ersetzt Hosts und Agenten: Jeder gestartete Schritt wird als Job
// gespeichert und nach delay mit dem Ergebnis von result beendet.
type fakeAgents struct {
	st     *store.Store
	delay  time.Duration
	result func(host string, step playbook.Step) (ok bool, exit int)
	gate   func(host string, step playbook.Step) (ok bool, detail string, err error)

	mu       sync.Mutex
	offline  map[string]bool
	boot     map[string]int64
	starts   []string // "host/schritt"
	running  int
	maxConc  int
	hold     bool     // Jobs nicht beenden (Server-Neustart simulieren)
	reboots  []string // Hosts, deren Boot-Zeit nach einem Neustart steigt
	startErr error
}

func newFakeAgents(st *store.Store) *fakeAgents {
	return &fakeAgents{st: st, delay: 20 * time.Millisecond, offline: map[string]bool{}, boot: map[string]int64{},
		result: func(string, playbook.Step) (bool, int) { return true, 0 },
		gate:   func(string, playbook.Step) (bool, string, error) { return true, "", nil }}
}

func (f *fakeAgents) gateState(_ context.Context, step playbook.Step, host string, _ time.Time) (bool, string, error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	return gate(host, step)
}

func (f *fakeAgents) hostState(_ context.Context, id string) (store.Host, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return store.Host{ID: id, BootTime: f.boot[id]}, !f.offline[id], nil
}

func (f *fakeAgents) startStep(ctx context.Context, h store.Host, step playbook.Step, jobID, user, remote string) error {
	f.mu.Lock()
	f.starts = append(f.starts, h.ID+"/"+step.ID)
	if f.startErr != nil {
		f.mu.Unlock()
		return f.startErr
	}
	f.running++
	f.maxConc = max(f.maxConc, f.running)
	hold := f.hold
	f.mu.Unlock()
	now := time.Now()
	if err := f.st.CreateJob(ctx, store.Job{ID: jobID, HostID: h.ID, Action: step.Action, User: user, Remote: remote,
		CreatedAt: now.Unix(), Deadline: now.Add(time.Hour).Unix()}); err != nil {
		return err
	}
	go func() {
		time.Sleep(f.delay)
		f.mu.Lock()
		f.running--
		if step.Action == proto.ActionReboot {
			f.boot[h.ID] += 100
		}
		f.mu.Unlock()
		if hold {
			return
		}
		ok, exit := f.result(h.ID, step)
		f.st.FinishJob(context.Background(), jobID, h.ID, time.Now(), ok, exit, "", map[bool]string{false: "fehler"}[ok])
	}()
	return nil
}

func (f *fakeAgents) started() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.starts, " ")
}

// newPlaybookServer legt einen Server mit Store, Hosts (ID → Gruppe) und den
// Playbooks (Dateiname → YAML) an und startet RunPlaybooks.
func newPlaybookServer(t *testing.T, dbPath string, hosts map[string]string, books map[string]string) (*Server, *fakeAgents, func()) {
	t.Helper()
	oldPoll, oldWait, oldGatePoll, oldStable := pbPoll, pbOnlineWait, gatePoll, gateStable
	pbPoll, pbOnlineWait, gatePoll, gateStable = 10*time.Millisecond, 200*time.Millisecond, 10*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { pbPoll, pbOnlineWait, gatePoll, gateStable = oldPoll, oldWait, oldGatePoll, oldStable })

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	for id, group := range hosts {
		if err := st.UpsertHost(ctx, proto.Hello{AgentID: id, Hostname: id}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		st.SetHostMeta(ctx, id, "", group)
	}
	dir := t.TempDir()
	for name, src := range books {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := New(st, nil, "token", nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithPlaybooks(dir)
	f := newFakeAgents(st)
	s.pb.act = f
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.RunPlaybooks(runCtx); close(done) }()
	stop := func() { cancel(); <-done } // wie ein Ende des Servers
	t.Cleanup(stop)
	for i := 0; ; i++ {
		s.pb.mu.Lock()
		ready := s.pb.ctx != nil && !s.pb.loadedAt.IsZero()
		s.pb.mu.Unlock()
		if ready {
			break
		}
		if i > 500 {
			t.Fatal("RunPlaybooks startet nicht")
		}
		time.Sleep(time.Millisecond)
	}
	return s, f, stop
}

func waitRun(t *testing.T, s *Server, id string) store.PlaybookRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := s.store.PlaybookRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.FinishedAt != nil {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("lauf endet nicht: %+v", r)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stepStates liefert "host/schritt=status" in Reihenfolge.
func stepStates(r store.PlaybookRun) string {
	var out []string
	for _, st := range r.Steps {
		out = append(out, st.HostID+"/"+st.StepID+"="+st.Status)
	}
	return strings.Join(out, " ")
}

const ampBook = `name: amp
targets: [gs]
steps:
  - id: pruefen
    action: command.run
    params: {name: amp-update-check}
    ok_exit_codes: [0, 10]
  - id: update
    action: command.run
    params: {name: amp-update}
    when: {step: pruefen, exit_code: 0}
  - id: pause
    wait: 50ms
  - id: danach
    action: service.restart
    params: {name: amp}
    when: {step: update, ok: true}
`

func TestPlaybookWhen(t *testing.T) {
	for _, c := range []struct {
		exit       int
		want, runs string
	}{
		{10, "gs/pruefen=ok gs/update=skipped gs/pause=ok gs/danach=skipped", "gs/pruefen"},
		{0, "gs/pruefen=ok gs/update=ok gs/pause=ok gs/danach=ok", "gs/pruefen gs/update gs/danach"},
	} {
		s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), map[string]string{"gs": ""}, map[string]string{"amp.yaml": ampBook})
		f.result = func(_ string, step playbook.Step) (bool, int) {
			if step.ID == "pruefen" {
				return c.exit == 0, c.exit
			}
			return true, 0
		}
		run, err := s.startPlaybook(context.Background(), "amp", "admin", "test")
		if err != nil {
			t.Fatal(err)
		}
		r := waitRun(t, s, run.ID)
		if r.Status != store.RunOK || stepStates(r) != c.want || f.started() != c.runs {
			t.Errorf("exit %d: %s\n%s\nstarts: %s", c.exit, r.Status, stepStates(r), f.started())
		}
		if r.Steps[0].ExitCode != c.exit {
			t.Errorf("exit-code %d", r.Steps[0].ExitCode)
		}
	}
}

const patchBook = `name: patchen
targets: [k3s]
mode: %s
max_parallel: %d
on_error: %s
steps:
  - id: refresh
    action: packages.refresh
  - id: upgrade
    action: packages.refresh
`

func threeHosts() map[string]string { return map[string]string{"n1": "k3s", "n2": "k3s", "n3": "k3s"} }

func TestPlaybookSerialStop(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(),
		map[string]string{"p.yaml": fmt.Sprintf(patchBook, "serial", 0, "stop")})
	f.result = func(host string, step playbook.Step) (bool, int) { return !(host == "n2" && step.ID == "refresh"), 1 }
	run, err := s.startPlaybook(context.Background(), "patchen", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, s, run.ID)
	want := "n1/refresh=ok n1/upgrade=ok n2/refresh=failed n2/upgrade=skipped n3/refresh=skipped n3/upgrade=skipped"
	if r.Status != store.RunStopped || stepStates(r) != want || f.started() != "n1/refresh n1/upgrade n2/refresh" {
		t.Fatalf("%s: %s\n%s", r.Status, stepStates(r), f.started())
	}
	if r.Steps[2].Message != "fehler" || r.Steps[5].Message != "nicht ausgeführt" {
		t.Errorf("meldungen: %q %q", r.Steps[2].Message, r.Steps[5].Message)
	}
}

func TestPlaybookContinue(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(),
		map[string]string{"p.yaml": fmt.Sprintf(patchBook, "serial", 0, "continue")})
	f.result = func(host string, step playbook.Step) (bool, int) { return !(host == "n2" && step.ID == "refresh"), 1 }
	run, _ := s.startPlaybook(context.Background(), "patchen", "admin", "")
	r := waitRun(t, s, run.ID)
	want := "n1/refresh=ok n1/upgrade=ok n2/refresh=failed n2/upgrade=ok n3/refresh=ok n3/upgrade=ok"
	if r.Status != store.RunFailed || stepStates(r) != want {
		t.Fatalf("%s: %s", r.Status, stepStates(r))
	}
}

func TestPlaybookParallel(t *testing.T) {
	hosts := map[string]string{}
	for i := range 5 {
		hosts[fmt.Sprintf("n%d", i)] = "k3s"
	}
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), hosts,
		map[string]string{"p.yaml": fmt.Sprintf(patchBook, "parallel", 2, "stop")})
	f.delay = 50 * time.Millisecond
	run, _ := s.startPlaybook(context.Background(), "patchen", "admin", "")
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunOK || f.maxConc != 2 {
		t.Fatalf("%s, gleichzeitig %d: %s", r.Status, f.maxConc, stepStates(r))
	}
}

// TestPlaybookParallelStop: Nach einem Fehler mit stop startet kein weiterer Host.
func TestPlaybookParallelStop(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(),
		map[string]string{"p.yaml": fmt.Sprintf(patchBook, "parallel", 2, "stop")})
	f.result = func(host string, step playbook.Step) (bool, int) { return host != "n1", 1 }
	run, _ := s.startPlaybook(context.Background(), "patchen", "admin", "")
	r := waitRun(t, s, run.ID)
	// Reihenfolge der Hosts: n1, n2, n3 (Gruppe in Listenreihenfolge).
	if r.Status != store.RunStopped || strings.Contains(f.started(), "n3") ||
		!strings.Contains(stepStates(r), "n3/refresh=skipped") {
		t.Fatalf("%s: %s\n%s", r.Status, stepStates(r), f.started())
	}
}

func TestPlaybookOfflineAndStartError(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), map[string]string{"n1": "k3s"},
		map[string]string{"p.yaml": fmt.Sprintf(patchBook, "serial", 0, "stop")})
	f.mu.Lock()
	f.offline["n1"] = true
	f.mu.Unlock()
	run, _ := s.startPlaybook(context.Background(), "patchen", "admin", "")
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunStopped || !strings.HasPrefix(r.Steps[0].Message, "host ist offline") || f.started() != "" {
		t.Fatalf("%s: %+v", r.Status, r.Steps[0])
	}

	// Kommt der Host innerhalb von pbOnlineWait zurück (z. B. Agent nach einem
	// Neustart des Servers), läuft der Schritt.
	run, _ = s.startPlaybook(context.Background(), "patchen", "admin", "")
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	f.offline["n1"] = false
	f.mu.Unlock()
	if r = waitRun(t, s, run.ID); r.Status != store.RunOK {
		t.Fatalf("host zurück: %s %+v", r.Status, r.Steps)
	}

	f.startErr = errors.New("befehl ist auf diesem host nicht freigegeben")
	run, _ = s.startPlaybook(context.Background(), "patchen", "admin", "")
	r = waitRun(t, s, run.ID)
	if r.Status != store.RunStopped || r.Steps[0].Message != f.startErr.Error() {
		t.Fatalf("%s: %+v", r.Status, r.Steps[0])
	}
}

func TestPlaybookLocks(t *testing.T) {
	other := "name: andere\ntargets: [n2]\nsteps: [{id: a, action: packages.refresh}]\n"
	free := "name: frei\ntargets: [n9]\nsteps: [{id: a, action: packages.refresh}]\n"
	broken := "name: kaputt\ntargets: [n1]\nsteps: [{id: a, action: shell.run}]\n"
	hosts := threeHosts()
	hosts["n9"] = ""
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), hosts, map[string]string{
		"p.yaml": fmt.Sprintf(patchBook, "serial", 0, "stop"), "o.yaml": other, "f.yaml": free, "k.yaml": broken})
	f.hold = true
	ctx := context.Background()
	if _, err := s.startPlaybook(ctx, "patchen", "admin", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.startPlaybook(ctx, "patchen", "admin", ""); err == nil || !strings.Contains(err.Error(), "läuft bereits") {
		t.Errorf("doppelstart: %v", err)
	}
	if _, err := s.startPlaybook(ctx, "andere", "admin", ""); err == nil || !strings.Contains(err.Error(), "anderen lauf") {
		t.Errorf("host-sperre: %v", err)
	}
	if _, err := s.startPlaybook(ctx, "kaputt", "admin", ""); err == nil || !strings.Contains(err.Error(), "fehlerhaft") {
		t.Errorf("fehlerhaftes playbook: %v", err)
	}
	if _, err := s.startPlaybook(ctx, "fehlt", "admin", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unbekannt: %v", err)
	}
	// Offener ändernder Job auf dem Host blockiert, ein lesender nicht.
	s.store.CreateJob(ctx, store.Job{ID: "j-log", HostID: "n9", Action: proto.ActionLogRead, CreatedAt: 1, Deadline: time.Now().Add(time.Hour).Unix()})
	s.store.CreateJob(ctx, store.Job{ID: "j-svc", HostID: "n9", Action: proto.ActionServiceRestart, CreatedAt: 1, Deadline: time.Now().Add(time.Hour).Unix()})
	if _, err := s.startPlaybook(ctx, "frei", "admin", ""); err == nil || !strings.Contains(err.Error(), "service.restart") {
		t.Errorf("offener job: %v", err)
	}
	s.store.FinishJob(ctx, "j-svc", "n9", time.Now(), true, 0, "", "")
	if _, err := s.startPlaybook(ctx, "frei", "admin", ""); err != nil {
		t.Errorf("nur lesender job offen: %v", err)
	}
}

func TestPlaybookCancelDuringWait(t *testing.T) {
	book := "name: warten\ntargets: [n1, n2]\nsteps:\n  - {id: a, action: packages.refresh}\n  - {id: w, wait: 1h}\n  - {id: b, action: packages.refresh}\n"
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{"w.yaml": book})
	ctx := context.Background()
	run, _ := s.startPlaybook(ctx, "warten", "admin", "")
	for !strings.Contains(stepStatesOf(t, s, run.ID), "n1/w=running") {
		time.Sleep(5 * time.Millisecond)
	}
	if ok, err := s.cancelPlaybook(ctx, run.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	r := waitRun(t, s, run.ID)
	want := "n1/a=ok n1/w=skipped n1/b=skipped n2/a=skipped n2/w=skipped n2/b=skipped"
	if r.Status != store.RunCancelled || stepStates(r) != want || f.started() != "n1/a" {
		t.Fatalf("%s: %s / %s", r.Status, stepStates(r), f.started())
	}
	if ok, _ := s.cancelPlaybook(ctx, run.ID); ok {
		t.Error("beendeter lauf abbrechbar")
	}
}

func stepStatesOf(t *testing.T, s *Server, id string) string {
	r, err := s.store.PlaybookRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return stepStates(r)
}

// TestPlaybookResume simuliert einen Neustart des Servers mitten im Schritt:
// Der neue Server löst den Schritt nicht erneut aus, sondern wartet auf den Job.
func TestPlaybookResume(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mat.db")
	books := map[string]string{"p.yaml": fmt.Sprintf(patchBook, "serial", 0, "stop")}
	s1, f1, stop1 := newPlaybookServer(t, db, map[string]string{"n1": "k3s", "n2": "k3s"}, books)
	f1.hold = true
	run, err := s1.startPlaybook(context.Background(), "patchen", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	for f1.started() != "n1/refresh" {
		time.Sleep(5 * time.Millisecond)
	}
	stop1() // Server endet, der Job auf n1 läuft weiter

	s2, f2, _ := newPlaybookServer(t, db, nil, books)
	r, _ := s2.store.PlaybookRun(context.Background(), run.ID)
	if r.Steps[0].Status != store.StepRunning || r.FinishedAt != nil {
		t.Fatalf("stand nach neustart: %s", stepStates(r))
	}
	// Das Ergebnis des alten Jobs kommt erst jetzt an.
	s2.store.FinishJob(context.Background(), r.Steps[0].JobID, "n1", time.Now(), true, 0, "", "")
	r = waitRun(t, s2, run.ID)
	if r.Status != store.RunOK || f2.started() != "n1/upgrade n2/refresh n2/upgrade" {
		t.Fatalf("%s: %s / %s", r.Status, stepStates(r), f2.started())
	}
}

// TestPlaybookResumeMissingJob: Fehlt der Job eines laufenden Schritts, endet
// der Lauf als unterbrochen, statt zu raten.
func TestPlaybookResumeMissingJob(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mat.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, _ := playbook.Parse([]byte(fmt.Sprintf(patchBook, "serial", 0, "stop")))
	src, _ := playbook.Marshal(p)
	st.CreatePlaybookRun(ctx, store.PlaybookRun{ID: "pb-x", Name: "patchen", Source: string(src), Hosts: []string{"n1"},
		Status: store.RunRunning, CreatedAt: time.Now().Unix()}, []store.PlaybookStep{
		{HostID: "n1", Index: 0, StepID: "refresh", Status: store.StepPending},
		{HostID: "n1", Index: 1, StepID: "upgrade", Status: store.StepPending}})
	st.SavePlaybookStep(ctx, store.PlaybookStep{RunID: "pb-x", HostID: "n1", Index: 0, StepID: "refresh",
		Status: store.StepRunning, JobID: "job-weg"})
	st.Close()

	s, f, _ := newPlaybookServer(t, db, nil, nil)
	r := waitRun(t, s, "pb-x")
	if r.Status != store.RunInterrupted || !strings.Contains(r.Steps[0].Message, "job-weg") || f.started() != "" {
		t.Fatalf("%s: %+v / %s", r.Status, r.Steps, f.started())
	}
}

func TestPlaybookRebootWaitsForReturn(t *testing.T) {
	book := "name: neustart\ntargets: [n1]\nsteps:\n  - {id: boot, action: system.reboot}\n  - {id: danach, action: packages.refresh}\n"
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{"r.yaml": book})
	f.boot["n1"] = 1000
	run, _ := s.startPlaybook(context.Background(), "neustart", "admin", "")
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunOK || r.Steps[0].Message != "host nach neustart zurück" || f.started() != "n1/boot n1/danach" {
		t.Fatalf("%s: %+v / %s", r.Status, r.Steps, f.started())
	}

	// Kommt der Host nicht zurück, schlägt der Schritt nach der Frist fehl.
	old := pbRebootTimeout
	pbRebootTimeout = 0
	t.Cleanup(func() { pbRebootTimeout = old })
	f.mu.Lock()
	f.offline["n1"] = false
	f.mu.Unlock()
	f.result = func(string, playbook.Step) (bool, int) { return true, 0 }
	// Boot-Zeit bleibt gleich: Neustart-Job meldet ok, aber der Host kommt nicht neu hoch.
	f.delay = 0
	s.pb.act = &noReboot{f}
	run, _ = s.startPlaybook(context.Background(), "neustart", "admin", "")
	r = waitRun(t, s, run.ID)
	if r.Status != store.RunStopped || !strings.Contains(r.Steps[0].Message, "nicht zurück") {
		t.Fatalf("%s: %+v", r.Status, r.Steps)
	}
}

// noReboot meldet Neustarts als erledigt, ohne dass die Boot-Zeit steigt.
type noReboot struct{ *fakeAgents }

func (n *noReboot) startStep(ctx context.Context, h store.Host, step playbook.Step, jobID, user, remote string) error {
	now := time.Now()
	n.st.CreateJob(ctx, store.Job{ID: jobID, HostID: h.ID, Action: step.Action, CreatedAt: now.Unix(), Deadline: now.Add(time.Hour).Unix()})
	_, err := n.st.FinishJob(ctx, jobID, h.ID, now, true, 0, "", "")
	return err
}

func TestRunStatus(t *testing.T) {
	for _, c := range []struct {
		out       []hostOutcome
		cancelled bool
		want      string
	}{
		{[]hostOutcome{outOK, outOK}, false, store.RunOK},
		{[]hostOutcome{outOK, outErrors}, false, store.RunFailed},
		{[]hostOutcome{outErrors, outStopped, outNotStarted}, false, store.RunStopped},
		{[]hostOutcome{outOK, outCancelled, outNotStarted}, true, store.RunCancelled},
		{[]hostOutcome{outStopped, outInterrupted}, false, store.RunInterrupted},
	} {
		if got, msg := runStatus(c.out, c.cancelled); got != c.want {
			t.Errorf("%v: %s (%s)", c.out, got, msg)
		}
	}
	if _, msg := runStatus([]hostOutcome{outOK, outOK, outStopped}, false); msg != "3 Hosts, 2 ok, 1 angehalten" {
		t.Errorf("meldung %q", msg)
	}
}
