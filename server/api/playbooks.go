package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Playbooks (M12a): Jeder Schritt läuft über das bestehende Job-System
// (startJob bzw. startDrain) mit denselben Prüfungen wie ein Klick in der GUI.
// Der Stand steht in SQLite; nach einem Neustart des Servers geht jeder Lauf
// dort weiter, ohne einen gestarteten Schritt doppelt auszulösen.

// Takt und Fristen (Variablen für Tests).
var (
	pbPoll          = 2 * time.Second
	pbRebootTimeout = 15 * time.Minute // nach system.reboot: bis der Host zurück ist
)

// playbookActions sind die Zugriffe des Runners auf Hosts und Agenten
// (*Server; in Tests ersetzt).
type playbookActions interface {
	// hostState liefert den Host und ob er verbunden ist.
	hostState(ctx context.Context, id string) (store.Host, bool, error)
	// startStep prüft und startet die Aktion eines Schritts als Job jobID.
	startStep(ctx context.Context, h store.Host, step playbook.Step, jobID, user, remote string) error
}

type playbooks struct {
	dir string
	act playbookActions

	mu       sync.Mutex
	ctx      context.Context // Lebensdauer des Servers; nil = RunPlaybooks läuft nicht
	files    []playbook.File
	loadErr  string
	loadedAt time.Time
	runs     map[string]*pbRun // aktive Läufe nach ID
	wg       sync.WaitGroup    // laufende execRun
}

// pbRun ist ein aktiver Lauf.
type pbRun struct {
	id, user, remote string
	pb               *playbook.Playbook
	hosts            []string
	cancel           atomic.Bool

	mu    sync.Mutex
	steps map[string][]store.PlaybookStep // je Host, nach Index
	halt  bool                            // keine weiteren Hosts starten
}

func (r *pbRun) halted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.halt
}

func (r *pbRun) setHalt() {
	r.mu.Lock()
	r.halt = true
	r.mu.Unlock()
}

// started meldet, ob auf dem Host schon ein Schritt begonnen hat.
func (r *pbRun) started(host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.steps[host] {
		if st.Status != store.StepPending {
			return true
		}
	}
	return false
}

// hostOutcome ist das Ergebnis eines Hosts in einem Lauf.
type hostOutcome int

const (
	outNotStarted  hostOutcome = iota
	outOK                      // alle Schritte ok oder übersprungen
	outErrors                  // Fehler mit on_error: continue
	outStopped                 // Fehler mit on_error: stop
	outCancelled               // abgebrochen
	outInterrupted             // nach Neustart nicht sicher fortsetzbar
	outAborted                 // Server wird beendet, Stand bleibt
)

// WithPlaybooks schaltet Playbooks aus dir frei (MAT_PLAYBOOK_DIR).
func (s *Server) WithPlaybooks(dir string) *Server {
	s.pb = &playbooks{dir: dir, act: s, runs: map[string]*pbRun{}}
	return s
}

// RunPlaybooks lädt die Playbooks und setzt Läufe fort, die vor einem Neustart
// begonnen wurden. Kehrt zurück, wenn ctx endet.
func (s *Server) RunPlaybooks(ctx context.Context) {
	if s.pb == nil {
		return
	}
	s.pb.mu.Lock()
	s.pb.ctx = ctx
	s.pb.mu.Unlock()
	if err := s.reloadPlaybooks(ctx); err != nil {
		s.log.Warn("playbooks laden", "err", err)
	}
	runs, err := s.store.ActivePlaybookRuns(ctx)
	if err != nil {
		s.log.Error("playbooks: läufe lesen", "err", err)
	}
	for _, r := range runs {
		s.resumeRun(ctx, r)
	}
	<-ctx.Done()
	s.pb.wg.Wait()
}

// resumeRun setzt einen gespeicherten Lauf fort.
func (s *Server) resumeRun(ctx context.Context, r store.PlaybookRun) {
	p, err := playbook.Parse([]byte(r.Source))
	if err == nil {
		err = p.Validate()
	}
	if err != nil {
		s.finishRun(r.ID, r.Name, store.RunInterrupted, "kopie des playbooks nicht lesbar: "+err.Error())
		return
	}
	run := &pbRun{id: r.ID, user: r.User, pb: p, hosts: r.Hosts, steps: map[string][]store.PlaybookStep{}}
	run.cancel.Store(r.Cancel)
	for _, st := range r.Steps {
		run.steps[st.HostID] = append(run.steps[st.HostID], st)
	}
	for _, h := range r.Hosts {
		if len(run.steps[h]) != len(p.Steps) {
			s.finishRun(r.ID, r.Name, store.RunInterrupted, "gespeicherte schritte passen nicht zum playbook")
			return
		}
		// Ein Host, der vor dem Neustart angehalten hat, hält auch jetzt die übrigen an.
		for i, st := range run.steps[h] {
			if st.Status == store.StepFailed && p.EffectiveOnError(p.Steps[i]) == playbook.OnErrorStop {
				run.halt = true
			}
		}
	}
	s.log.Info("playbook-lauf wird fortgesetzt", "run_id", r.ID, "playbook", r.Name)
	s.pb.mu.Lock()
	s.pb.runs[r.ID] = run
	s.pb.wg.Add(1)
	s.pb.mu.Unlock()
	go s.execRun(ctx, run)
}

// reloadPlaybooks liest den Ordner neu.
func (s *Server) reloadPlaybooks(ctx context.Context) error {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("hosts lesen: %w", err)
	}
	files, err := playbook.LoadDir(s.pb.dir, pbHosts(hosts))
	s.pb.mu.Lock()
	defer s.pb.mu.Unlock()
	s.pb.loadedAt = s.nowFor()
	if err != nil {
		s.pb.files, s.pb.loadErr = nil, err.Error()
		return err
	}
	s.pb.files, s.pb.loadErr = files, ""
	bad := 0
	for _, f := range files {
		if !f.OK() {
			bad++
		}
	}
	s.log.Info("playbooks geladen", "dateien", len(files), "fehlerhaft", bad)
	return nil
}

func pbHosts(hosts []store.Host) []playbook.Host {
	out := make([]playbook.Host, len(hosts))
	for i, h := range hosts {
		out[i] = playbook.Host{ID: h.ID, DisplayName: h.DisplayName, Group: h.Group}
	}
	return out
}

// playbookFile liefert ein startbares Playbook nach Namen.
func (s *Server) playbookFile(name string) (*playbook.Playbook, error) {
	s.pb.mu.Lock()
	defer s.pb.mu.Unlock()
	for _, f := range s.pb.files {
		if f.Playbook != nil && f.Playbook.Name == name {
			if !f.OK() {
				return nil, fmt.Errorf("playbook ist fehlerhaft: %s", f.Err)
			}
			return f.Playbook, nil
		}
	}
	return nil, store.ErrNotFound
}

// resolvePlaybook löst die Ziele mit dem aktuellen Host-Stand auf.
func (s *Server) resolvePlaybook(ctx context.Context, p *playbook.Playbook) ([]string, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("hosts lesen: %w", err)
	}
	return p.Resolve(pbHosts(hosts))
}

// startPlaybook prüft die Sperren und startet einen Lauf.
func (s *Server) startPlaybook(ctx context.Context, name, user, remote string) (store.PlaybookRun, error) {
	p, err := s.playbookFile(name)
	if err != nil {
		return store.PlaybookRun{}, err
	}
	hosts, err := s.resolvePlaybook(ctx, p)
	if err != nil {
		return store.PlaybookRun{}, err
	}
	src, err := playbook.Marshal(p)
	if err != nil {
		return store.PlaybookRun{}, err
	}
	open, err := s.store.OpenJobs(ctx)
	if err != nil {
		return store.PlaybookRun{}, err
	}

	s.pb.mu.Lock()
	defer s.pb.mu.Unlock()
	if s.pb.ctx == nil || s.pb.ctx.Err() != nil {
		return store.PlaybookRun{}, errors.New("playbooks laufen nicht (server startet oder endet)")
	}
	for _, r := range s.pb.runs {
		if r.pb.Name == name {
			return store.PlaybookRun{}, errors.New("dieses playbook läuft bereits")
		}
		for _, h := range hosts {
			if slices.Contains(r.hosts, h) {
				return store.PlaybookRun{}, fmt.Errorf("host %s ist schon in einem anderen lauf (%s)", h, r.pb.Name)
			}
		}
	}
	if s.drainBusy() {
		return store.PlaybookRun{}, errors.New("es läuft ein neustart mit drain")
	}
	for _, j := range open {
		if slices.Contains(hosts, j.HostID) && !proto.ReadOnly(j.Action) {
			return store.PlaybookRun{}, fmt.Errorf("auf host %s läuft noch ein job (%s)", j.HostID, j.Action)
		}
	}

	run := &pbRun{id: newJobID("pb"), user: user, remote: remote, pb: p, hosts: hosts, steps: map[string][]store.PlaybookStep{}}
	rec := store.PlaybookRun{ID: run.id, Name: p.Name, Source: string(src), User: user, Remote: remote, Hosts: hosts,
		Status: store.RunRunning, CreatedAt: s.nowFor().Unix()}
	var all []store.PlaybookStep
	for _, h := range hosts {
		for i, st := range p.Steps {
			ps := store.PlaybookStep{RunID: run.id, HostID: h, Index: i, StepID: st.ID, Status: store.StepPending}
			run.steps[h] = append(run.steps[h], ps)
			all = append(all, ps)
		}
	}
	if err := s.store.CreatePlaybookRun(ctx, rec, all); err != nil {
		s.log.Error("playbook-lauf anlegen", "err", err)
		return store.PlaybookRun{}, errors.New("interner fehler")
	}
	s.pb.runs[run.id] = run
	s.pb.wg.Add(1)
	s.log.Info("playbook gestartet", "run_id", run.id, "playbook", p.Name, "hosts", hosts, "user", user, "remote", remote)
	go s.execRun(s.pb.ctx, run)
	return rec, nil
}

// cancelPlaybook merkt den Abbruch vor: Der Lauf endet nach dem aktuellen
// Schritt, laufende Jobs laufen zu Ende.
func (s *Server) cancelPlaybook(ctx context.Context, id string) (bool, error) {
	found, err := s.store.CancelPlaybookRun(ctx, id)
	if err != nil || !found {
		return found, err
	}
	s.pb.mu.Lock()
	if r := s.pb.runs[id]; r != nil {
		r.cancel.Store(true)
	}
	s.pb.mu.Unlock()
	return true, nil
}

// execRun führt die Hosts eines Laufs aus: serial einer nach dem anderen,
// parallel bis max_parallel gleichzeitig. Nach einem Fehler mit stop (oder
// einem Abbruch) startet kein weiterer Host; laufende machen weiter bis zum
// Ende ihres Schritts.
func (s *Server) execRun(ctx context.Context, run *pbRun) {
	defer s.pb.wg.Done()
	sem := make(chan struct{}, run.pb.Parallel(len(run.hosts)))
	outcomes := make([]hostOutcome, len(run.hosts))
	var wg sync.WaitGroup
loop:
	for i, h := range run.hosts {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		if (run.halted() || run.cancel.Load()) && !run.started(h) {
			<-sem
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			outcomes[i] = s.runHost(ctx, run, h)
			if outcomes[i] == outStopped || outcomes[i] == outInterrupted {
				run.setHalt()
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return // Server wird beendet; der Lauf geht nach dem Start weiter
	}
	status, msg := runStatus(outcomes, run.cancel.Load())
	s.finishRun(run.id, run.pb.Name, status, msg)
}

// runStatus fasst die Ergebnisse der Hosts zusammen.
func runStatus(outcomes []hostOutcome, cancelled bool) (string, string) {
	count := map[hostOutcome]int{}
	for _, o := range outcomes {
		count[o]++
	}
	parts := []string{fmt.Sprintf("%d hosts", len(outcomes))}
	for _, c := range []struct {
		o    hostOutcome
		text string
	}{{outOK, "ok"}, {outErrors, "mit fehlern"}, {outStopped, "angehalten"}, {outCancelled, "abgebrochen"},
		{outInterrupted, "unterbrochen"}, {outNotStarted, "nicht gestartet"}} {
		if count[c.o] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count[c.o], c.text))
		}
	}
	msg := strings.Join(parts, ", ")
	switch {
	case count[outInterrupted] > 0:
		return store.RunInterrupted, msg
	case count[outStopped] > 0:
		return store.RunStopped, msg
	case cancelled:
		return store.RunCancelled, msg
	case count[outErrors] > 0:
		return store.RunFailed, msg
	}
	return store.RunOK, msg
}

// finishRun schließt einen Lauf ab und meldet das Ergebnis.
func (s *Server) finishRun(id, name, status, msg string) {
	ctx := context.Background()
	if err := s.store.FinishPlaybookRun(ctx, id, status, msg, s.nowFor()); err != nil {
		s.log.Error("playbook-lauf beenden", "err", err)
	}
	s.pb.mu.Lock()
	delete(s.pb.runs, id)
	s.pb.mu.Unlock()
	s.log.Info("playbook-lauf beendet", "run_id", id, "playbook", name, "status", status, "ergebnis", msg)
	if s.notifier != nil {
		m := notify.Message{Title: "Playbook " + name + ": " + runStatusText[status], Body: msg, Tag: "white_check_mark"}
		if status != store.RunOK {
			m.High, m.Tag = true, "warning"
		}
		s.notifier.Enqueue(m)
	}
}

var runStatusText = map[string]string{
	store.RunOK: "erfolgreich", store.RunFailed: "mit Fehlern", store.RunStopped: "angehalten",
	store.RunCancelled: "abgebrochen", store.RunInterrupted: "unterbrochen",
}

// runHost führt die Schritte eines Hosts der Reihe nach aus. Bereits beendete
// Schritte (Fortsetzung nach Neustart) werden nur ausgewertet.
func (s *Server) runHost(ctx context.Context, run *pbRun, host string) hostOutcome {
	p := run.pb
	results := map[string]playbook.Result{}
	failed := false
	for i, step := range p.Steps {
		st := run.step(host, i)
		if st.Status == store.StepPending {
			if run.cancel.Load() {
				return outCancelled
			}
			if w := step.When; w != nil && !w.Match(results[w.Step]) {
				st.Status, st.Message = store.StepSkipped, "bedingung nicht erfüllt"
				s.saveStep(run, st)
			}
		}
		if st.Status == store.StepPending || st.Status == store.StepRunning {
			var interrupted bool
			st, interrupted = s.execStep(ctx, run, step, st)
			switch {
			case ctx.Err() != nil:
				return outAborted
			case interrupted:
				return outInterrupted
			case st.Status == store.StepSkipped: // Wartezeit abgebrochen
				return outCancelled
			}
		}
		results[step.ID] = playbook.Result{Skipped: st.Status == store.StepSkipped, OK: st.OK, ExitCode: st.ExitCode}
		if st.Status == store.StepFailed {
			if p.EffectiveOnError(step) == playbook.OnErrorStop {
				return outStopped
			}
			failed = true
		}
	}
	if failed {
		return outErrors
	}
	return outOK
}

func (r *pbRun) step(host string, i int) store.PlaybookStep {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steps[host][i]
}

// saveStep speichert den Stand eines Schritts (Speicher und Datenbank).
func (s *Server) saveStep(run *pbRun, st store.PlaybookStep) {
	run.mu.Lock()
	run.steps[st.HostID][st.Index] = st
	run.mu.Unlock()
	if err := s.store.SavePlaybookStep(context.Background(), st); err != nil {
		s.log.Error("playbook-schritt speichern", "run_id", run.id, "err", err)
	}
}

// execStep führt einen Schritt aus oder setzt ihn fort. interrupted meldet,
// dass er nach einem Neustart nicht sicher fortgesetzt werden kann. Endet ctx,
// bleibt der Stand gespeichert.
func (s *Server) execStep(ctx context.Context, run *pbRun, step playbook.Step, st store.PlaybookStep) (store.PlaybookStep, bool) {
	finish := func(ok bool, msg string) (store.PlaybookStep, bool) {
		st.Status, st.OK, st.Message, st.FinishedAt = store.StepFailed, ok, msg, s.nowFor().Unix()
		if ok {
			st.Status = store.StepOK
		}
		s.saveStep(run, st)
		return st, false
	}

	if st.Status == store.StepPending {
		st.Status, st.StartedAt = store.StepRunning, s.nowFor().Unix()
		if step.IsWait() {
			st.Until = s.nowFor().Add(time.Duration(step.Wait)).Unix()
			s.saveStep(run, st)
		} else {
			h, online, err := s.pb.act.hostState(ctx, st.HostID)
			switch {
			case err != nil:
				return finish(false, "host nicht gefunden")
			case !online:
				return finish(false, "host ist offline")
			}
			st.JobID, st.BootTime = newJobID("job"), h.BootTime
			s.saveStep(run, st) // Job-ID vor dem Senden speichern: nie doppelt auslösen
			if err := s.pb.act.startStep(ctx, h, step, st.JobID, run.user, "playbook "+run.id); err != nil {
				return finish(false, err.Error())
			}
		}
	}

	if step.IsWait() {
		for s.nowFor().Unix() < st.Until {
			if run.cancel.Load() {
				st.Status, st.Message, st.FinishedAt = store.StepSkipped, "abgebrochen", s.nowFor().Unix()
				s.saveStep(run, st)
				return st, false
			}
			if !sleepCtx(ctx, min(pbPoll, time.Duration(st.Until-s.nowFor().Unix())*time.Second)) {
				return st, false
			}
		}
		return finish(true, "")
	}

	if st.JobID == "" {
		return s.interruptStep(run, st, "schritt lief ohne job-id")
	}
	j, err := s.awaitJob(ctx, st.JobID)
	switch {
	case ctx.Err() != nil:
		return st, false
	case errors.Is(err, store.ErrNotFound):
		return s.interruptStep(run, st, "job "+st.JobID+" fehlt nach neustart des servers")
	case err != nil:
		return s.interruptStep(run, st, "job lesen: "+err.Error())
	}
	st.ExitCode = j.ExitCode
	if !step.Succeeded(j.OK, j.ExitCode) {
		msg := j.Error
		if msg == "" {
			msg = fmt.Sprintf("exit-code %d", j.ExitCode)
		}
		return finish(false, msg)
	}
	if step.Action == proto.ActionReboot {
		return s.awaitReboot(ctx, run, st, finish)
	}
	return finish(true, "")
}

// interruptStep beendet einen Schritt, der nicht sicher fortgesetzt werden kann.
func (s *Server) interruptStep(run *pbRun, st store.PlaybookStep, msg string) (store.PlaybookStep, bool) {
	st.Status, st.Message, st.FinishedAt = store.StepFailed, "unterbrochen: "+msg, s.nowFor().Unix()
	s.saveStep(run, st)
	return st, true
}

// awaitReboot wartet nach einem Neustart, bis der Host mit neuer Boot-Zeit
// wieder verbunden ist; sonst würde der nächste Schritt sofort scheitern.
func (s *Server) awaitReboot(ctx context.Context, run *pbRun, st store.PlaybookStep,
	finish func(bool, string) (store.PlaybookStep, bool)) (store.PlaybookStep, bool) {
	if st.Until == 0 {
		st.Until, st.Message = s.nowFor().Add(pbRebootTimeout).Unix(), "warte auf rückkehr nach neustart"
		s.saveStep(run, st)
	}
	for {
		if h, online, err := s.pb.act.hostState(ctx, st.HostID); err == nil && online && h.BootTime > st.BootTime {
			return finish(true, "host nach neustart zurück")
		}
		if s.nowFor().Unix() > st.Until {
			return finish(false, fmt.Sprintf("host nach %v nicht zurück", pbRebootTimeout))
		}
		if !sleepCtx(ctx, pbPoll) {
			return st, false
		}
	}
}

// awaitJob wartet, bis der Job ein Ergebnis hat (oder abgelaufen ist).
func (s *Server) awaitJob(ctx context.Context, id string) (store.Job, error) {
	for {
		s.expireJobs(ctx)
		j, err := s.store.JobByID(ctx, id)
		if err != nil || j.FinishedAt != nil {
			return j, err
		}
		if !sleepCtx(ctx, pbPoll) {
			return j, ctx.Err()
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// hostState gehört zu playbookActions.
func (s *Server) hostState(ctx context.Context, id string) (store.Host, bool, error) {
	h, err := s.hostByID(ctx, id)
	return h, err == nil && s.online(id), err
}

// startStep gehört zu playbookActions: dieselben Prüfungen wie ein Job aus
// der GUI, die Parameter werden noch einmal gegen die Whitelist geprüft.
func (s *Server) startStep(ctx context.Context, h store.Host, step playbook.Step, jobID, user, remote string) error {
	if step.Action == playbook.ActionDrainReboot {
		_, err := s.startDrain(ctx, h, jobID, user, remote)
		return err
	}
	job := proto.JobRequest{JobID: jobID, Action: step.Action, Params: step.Params}
	if err := job.Validate(); err != nil || job.Action == proto.ActionAgentUpdate {
		return fmt.Errorf("aktion abgelehnt: %v", err)
	}
	if err := s.checkJob(h, job); err != nil {
		return err
	}
	_, err := s.startJob(ctx, h.ID, job, user, remote, 0)
	return err
}
