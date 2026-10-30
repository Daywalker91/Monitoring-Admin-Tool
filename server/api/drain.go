package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// actionDrainReboot ist ein Ablauf im Server, kein Agent-Job: Node absperren,
// Pods verdrängen, Neustart über system.reboot, Rückkehr abwarten, freigeben.
const actionDrainReboot = "k3s.drain_reboot"

// Fristen und Takt des Ablaufs (Variablen für Tests).
var (
	drainTimeout  = 10 * time.Minute // Pods verdrängen (Longhorn baut ggf. Replikate um)
	returnTimeout = 15 * time.Minute // Neustart bis Agent und Node wieder da sind
	drainPoll     = 5 * time.Second
	canDrainEvery = time.Hour
)

// drainAPI sind die Cluster-Zugriffe des Ablaufs (*k8s.Client).
type drainAPI interface {
	Node(ctx context.Context, name string) (k8s.NodeState, error)
	Cordon(ctx context.Context, n k8s.NodeState, jobID string) error
	Uncordon(ctx context.Context, name, jobID string) (bool, error)
	PodsToEvict(ctx context.Context, node string) ([]k8s.EvictPod, error)
	Evict(ctx context.Context, p k8s.EvictPod) error
	CanDrain(ctx context.Context) (bool, error)
}

// drainState hält die laufenden Vorgänge (höchstens einer im Cluster).
type drainState struct {
	api     drainAPI
	ctx     context.Context // Lebensdauer des Servers; nil = RunDrain läuft nicht
	selfPod string          // eigener Pod-Name: wird als letzter verdrängt

	mu   sync.Mutex
	ok   bool                 // Rechte vorhanden (SelfSubjectAccessReview)
	runs map[string]*drainRun // nach Job-ID
}

type drainRun struct {
	op     store.NodeOp
	result chan proto.JobResult // Ergebnis des Neustart-Jobs beim Agenten
}

// WithDrain schaltet den Neustart mit drain frei (Rechte prüft RunDrain).
func (s *Server) WithDrain(api drainAPI) *Server {
	self, _ := os.Hostname() // im Pod: Pod-Name
	s.drain = &drainState{api: api, selfPod: self, runs: map[string]*drainRun{}}
	return s
}

// RunDrain prüft beim Start und stündlich die Rechte und setzt Vorgänge fort,
// die vor einem Neustart des Servers begonnen wurden.
func (s *Server) RunDrain(ctx context.Context) {
	d := s.drain
	if d == nil {
		return
	}
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
	s.checkDrainRights(ctx)
	ops, err := s.store.NodeOps(ctx)
	if err != nil {
		s.log.Error("drain: vorgänge lesen", "err", err)
	}
	for _, op := range ops {
		j, err := s.store.JobByID(ctx, op.JobID)
		if err != nil || j.FinishedAt != nil {
			s.store.DeleteNodeOp(ctx, op.JobID)
			continue
		}
		s.log.Info("drain: vorgang wird fortgesetzt", "job_id", op.JobID, "node", op.Node, "phase", op.Phase)
		s.liveStart(op.HostID, op.JobID, time.Unix(j.Deadline, 0))
		s.liveSet(op.JobID, op.Log)
		run := &drainRun{op: op, result: make(chan proto.JobResult, 1)}
		d.mu.Lock()
		d.runs[op.JobID] = run
		d.mu.Unlock()
		go s.runDrain(ctx, run)
	}
	t := time.NewTicker(canDrainEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkDrainRights(ctx)
		}
	}
}

func (s *Server) checkDrainRights(ctx context.Context) {
	ok, err := s.drain.api.CanDrain(ctx)
	if err != nil {
		s.log.Warn("drain: rechte prüfen", "err", err)
	}
	s.drain.mu.Lock()
	changed := s.drain.ok != ok
	s.drain.ok = ok
	s.drain.mu.Unlock()
	if changed {
		s.log.Info("drain vor neustart", "verfügbar", ok)
	}
}

// canDrain meldet, ob der Neustart mit drain angeboten wird.
func (s *Server) canDrain() bool {
	if s.drain == nil {
		return false
	}
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	return s.drain.ok && s.drain.ctx != nil
}

// draining liefert die Job-ID eines laufenden Vorgangs für den Host bzw.
// Node ("" = keiner).
func (s *Server) draining(hostID, node string) string {
	if s.drain == nil {
		return ""
	}
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	for id, r := range s.drain.runs {
		if (hostID != "" && r.op.HostID == hostID) || (node != "" && strings.EqualFold(r.op.Node, node)) {
			return id
		}
	}
	return ""
}

// handleDrainReboot startet einen Neustart mit drain (POST /api/hosts/{id}/jobs
// mit action k3s.drain_reboot).
func (s *Server) handleDrainReboot(w http.ResponseWriter, r *http.Request) {
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	fail := func(msg string) { http.Error(w, msg, http.StatusConflict) }
	switch {
	case !s.canDrain():
		fail("drain nicht verfügbar (clusterrole mat-server-drain fehlt)")
		return
	case !slices.Contains(h.Capabilities, proto.CapReboot):
		fail("host unterstützt keinen neustart (fähigkeit in agent.json freigeschaltet?)")
		return
	case !s.online(h.ID):
		fail("host ist offline")
		return
	}
	n, err := s.nodeOfHost(r.Context(), h)
	switch {
	case errors.Is(err, k8s.ErrNotFound):
		fail("kein k3s-node zu diesem host")
		return
	case err != nil:
		fail("node lesen: " + err.Error())
		return
	case !n.Ready:
		fail("node ist nicht bereit")
		return
	case n.Unschedulable:
		fail("node ist bereits abgesperrt (z. B. durch ein k3s-upgrade)")
		return
	}

	s.drain.mu.Lock()
	busy := len(s.drain.runs) > 0
	ctx := s.drain.ctx
	job := proto.JobRequest{JobID: newJobID("job"), Action: actionDrainReboot, Params: map[string]string{"node": n.Name}}
	run := &drainRun{op: store.NodeOp{JobID: job.JobID, HostID: h.ID, Node: n.Name, Phase: store.NodeOpDrain,
		BootTime: h.BootTime, PhaseUntil: s.nowFor().Add(drainTimeout).Unix()}, result: make(chan proto.JobResult, 1)}
	if !busy {
		s.drain.runs[job.JobID] = run // reserviert, bevor die Sperre fällt
	}
	s.drain.mu.Unlock()
	if busy {
		fail("es läuft bereits ein neustart mit drain")
		return
	}
	j, err := s.recordJob(r.Context(), h.ID, job, userFrom(r.Context()), r.RemoteAddr, drainTimeout+returnTimeout)
	if err == nil {
		err = s.store.SaveNodeOp(r.Context(), run.op)
	}
	if err != nil {
		s.dropRun(job.JobID)
		fail(err.Error())
		return
	}
	go s.runDrain(ctx, run)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, j)
}

// runDrain führt einen Vorgang ab seiner gespeicherten Phase aus. Endet ctx
// (Server wird beendet oder selbst verdrängt), bleibt der Stand gespeichert.
func (s *Server) runDrain(ctx context.Context, run *drainRun) {
	op := &run.op
	if op.Phase == store.NodeOpDrain {
		if err := s.drainNode(ctx, run); err != nil {
			if ctx.Err() == nil {
				s.abortDrain(run, err)
			}
			return
		}
		if err := s.startReboot(ctx, run); err != nil {
			if ctx.Err() == nil {
				s.abortDrain(run, err)
			}
			return
		}
	}
	s.awaitReturn(ctx, run)
}

// drainNode sperrt den Node ab und verdrängt alle Pods bis zur Frist.
func (s *Server) drainNode(ctx context.Context, run *drainRun) error {
	op := &run.op
	n, err := s.drain.api.Node(ctx, op.Node)
	if err != nil {
		return fmt.Errorf("node lesen: %w", err)
	}
	switch {
	case n.CordonedBy == op.JobID:
		// schon abgesperrt (fortgesetzt)
	case n.Unschedulable:
		return errors.New("node wurde von anderer seite abgesperrt")
	default:
		if err := s.drain.api.Cordon(ctx, n, op.JobID); err != nil {
			return fmt.Errorf("absperren: %w", err)
		}
		s.drainLog(run, "Node %s abgesperrt (cordon)", op.Node)
	}

	evicted, blocked := map[string]bool{}, map[string]bool{}
	for {
		pods, err := s.drain.api.PodsToEvict(ctx, op.Node)
		if err != nil {
			s.log.Warn("drain: pods lesen", "node", op.Node, "err", err)
		} else if len(pods) == 0 {
			s.drainLog(run, "Alle Pods verdrängt")
			return nil
		} else {
			var self *k8s.EvictPod
			others := 0
			for i, p := range pods {
				if p.Name == s.drain.selfPod {
					self = &pods[i]
					continue
				}
				others++
				if p.Terminating {
					continue
				}
				key := p.Namespace + "/" + p.Name
				switch err := s.drain.api.Evict(ctx, p); {
				case err == nil:
					if !evicted[key] {
						evicted[key] = true
						s.drainLog(run, "Pod %s verdrängt", key)
					}
				case errors.Is(err, k8s.ErrBlocked):
					if !blocked[key] {
						blocked[key] = true
						s.drainLog(run, "Pod %s wartet auf PodDisruptionBudget", key)
					}
				case strings.Contains(err.Error(), "berechtigung"):
					return err
				default:
					s.log.Warn("drain: pod verdrängen", "pod", key, "err", err)
				}
			}
			// Der eigene Pod geht zuletzt; danach setzt der neue Pod fort.
			if self != nil && others == 0 && !self.Terminating {
				s.drainLog(run, "MAT-Server zieht auf einen anderen Node um und setzt dort fort")
				if err := s.drain.api.Evict(ctx, *self); err != nil && !errors.Is(err, k8s.ErrBlocked) {
					return fmt.Errorf("eigenen pod verdrängen: %w", err)
				}
			}
			if s.nowFor().Unix() > op.PhaseUntil {
				names := make([]string, 0, len(pods))
				for _, p := range pods {
					names = append(names, p.Namespace+"/"+p.Name)
				}
				return fmt.Errorf("drain nach %v nicht fertig, noch auf dem node: %s", drainTimeout, strings.Join(names, ", "))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainPoll):
		}
	}
}

// startReboot merkt sich die Boot-Zeit und schickt den Neustart an den Agenten.
func (s *Server) startReboot(ctx context.Context, run *drainRun) error {
	op := &run.op
	h, err := s.hostByID(ctx, op.HostID)
	if err != nil {
		return err
	}
	if !slices.Contains(h.Capabilities, proto.CapReboot) || !s.online(h.ID) {
		return errors.New("host ist offline oder kann nicht neu starten")
	}
	user := ""
	if j, err := s.store.JobByID(ctx, op.JobID); err == nil {
		user = j.User
	}
	job := proto.JobRequest{JobID: newJobID("job"), Action: proto.ActionReboot}
	s.drain.mu.Lock() // drainResult liest RebootJob
	op.Phase, op.RebootJob, op.BootTime = store.NodeOpReboot, job.JobID, h.BootTime
	op.PhaseUntil = s.nowFor().Add(returnTimeout).Unix()
	s.drain.mu.Unlock()
	s.drainLog(run, "Neustart angestoßen, warte auf Rückkehr (höchstens %v)", returnTimeout)
	if _, err := s.startJob(ctx, h.ID, job, user, "drain "+op.JobID, 0); err != nil {
		op.Phase = store.NodeOpDrain
		return fmt.Errorf("neustart senden: %w", err)
	}
	return nil
}

// awaitReturn wartet, bis der Agent mit neuer Boot-Zeit zurück und der Node
// bereit ist, und gibt ihn dann frei.
func (s *Server) awaitReturn(ctx context.Context, run *drainRun) {
	op := &run.op
	back := false
	for {
		select {
		case <-ctx.Done():
			return
		case res := <-run.result:
			if !res.OK {
				s.abortDrain(run, fmt.Errorf("neustart abgelehnt: %s", res.Error))
				return
			}
		case <-time.After(drainPoll):
		}
		h, err := s.hostByID(ctx, op.HostID)
		if err == nil && s.online(h.ID) && h.BootTime > op.BootTime {
			if !back {
				back = true
				s.drainLog(run, "Host ist wieder da, warte auf Node bereit")
			}
			if n, err := s.drain.api.Node(ctx, op.Node); err == nil && n.Ready {
				if _, err := s.drain.api.Uncordon(ctx, op.Node, op.JobID); err != nil {
					s.log.Warn("drain: freigeben", "node", op.Node, "err", err)
				} else {
					s.drainLog(run, "Node %s freigegeben (uncordon)", op.Node)
					s.finishDrain(run, true, "")
					return
				}
			}
		}
		if s.nowFor().Unix() > op.PhaseUntil {
			msg := fmt.Sprintf("host bzw. node nach %v nicht zurück – node %s bleibt abgesperrt", returnTimeout, op.Node)
			s.drainLog(run, "Abbruch: %s", msg)
			s.finishDrain(run, false, msg)
			return
		}
	}
}

// abortDrain bricht vor dem Neustart ab: Node freigeben, kein Neustart.
func (s *Server) abortDrain(run *drainRun, cause error) {
	op := &run.op
	s.drainLog(run, "Abbruch: %v", cause)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if freed, err := s.drain.api.Uncordon(ctx, op.Node, op.JobID); err != nil {
		s.drainLog(run, "Freigeben fehlgeschlagen: %v – node %s bitte von Hand freigeben", err, op.Node)
	} else if freed {
		s.drainLog(run, "Node %s wieder freigegeben, kein Neustart", op.Node)
	}
	s.finishDrain(run, false, cause.Error())
}

// finishDrain schließt den Job ab und meldet Fehlschläge.
func (s *Server) finishDrain(run *drainRun, ok bool, errText string) {
	op := run.op
	ctx := context.Background()
	s.liveEnd(op.JobID)
	exit := 0
	if !ok {
		exit = 1
	}
	if _, err := s.store.FinishJob(ctx, op.JobID, op.HostID, s.nowFor(), ok, exit, op.Log, errText); err != nil {
		s.log.Error("drain: job beenden", "err", err)
	}
	if err := s.store.DeleteNodeOp(ctx, op.JobID); err != nil {
		s.log.Error("drain: vorgang löschen", "err", err)
	}
	s.dropRun(op.JobID)
	if ok {
		s.log.Info("neustart mit drain erledigt", "agent_id", op.HostID, "node", op.Node, "job_id", op.JobID)
		return
	}
	s.log.Warn("neustart mit drain fehlgeschlagen", "agent_id", op.HostID, "node", op.Node, "job_id", op.JobID, "err", errText)
	if s.notifier != nil {
		name, _ := s.store.HostDisplayName(ctx, op.HostID)
		if name == "" {
			name = op.HostID
		}
		s.notifier.Enqueue(notify.Message{Title: "Neustart mit drain fehlgeschlagen: " + name, Body: errText,
			High: true, Tag: "warning"})
	}
}

func (s *Server) dropRun(jobID string) {
	s.drain.mu.Lock()
	delete(s.drain.runs, jobID)
	s.drain.mu.Unlock()
}

// drainLog hängt eine Zeile an die Ausgabe an und speichert den Stand.
func (s *Server) drainLog(run *drainRun, format string, args ...any) {
	op := &run.op
	line := s.nowFor().In(s.loc).Format("15:04:05") + "  " + fmt.Sprintf(format, args...) + "\n"
	op.Log += line
	if over := len(op.Log) - proto.MaxJobOutputLen; over > 0 {
		for over < len(op.Log) && !utf8.RuneStart(op.Log[over]) {
			over++
		}
		op.Log = op.Log[over:]
	}
	s.liveSet(op.JobID, op.Log)
	if err := s.store.SaveNodeOp(context.Background(), *op); err != nil {
		s.log.Error("drain: stand speichern", "err", err)
	}
}

// drainResult reicht das Ergebnis des Neustart-Jobs an den Vorgang weiter.
func (s *Server) drainResult(res proto.JobResult) {
	if s.drain == nil {
		return
	}
	s.drain.mu.Lock()
	defer s.drain.mu.Unlock()
	for _, r := range s.drain.runs {
		if r.op.RebootJob == res.JobID {
			select {
			case r.result <- res:
			default:
			}
		}
	}
}

// nodeOfHost sucht den Node zum Host wie matchNode (Hostname, ohne Domain,
// Agent-ID), aber direkt in der API statt im letzten Stand.
func (s *Server) nodeOfHost(ctx context.Context, h store.Host) (k8s.NodeState, error) {
	host := strings.ToLower(h.Hostname)
	short, _, _ := strings.Cut(host, ".")
	for _, name := range []string{host, short, strings.ToLower(h.ID)} {
		if !k8s.ValidName(name) {
			continue
		}
		n, err := s.drain.api.Node(ctx, name)
		if !errors.Is(err, k8s.ErrNotFound) {
			return n, err
		}
	}
	return k8s.NodeState{}, k8s.ErrNotFound
}

// hostByID liest einen Host.
func (s *Server) hostByID(ctx context.Context, id string) (store.Host, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return store.Host{}, err
	}
	for _, h := range hosts {
		if h.ID == id {
			return h, nil
		}
	}
	return store.Host{}, errors.New("host nicht gefunden")
}
