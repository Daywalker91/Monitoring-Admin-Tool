package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Gates (M12b): Ein Gate-Schritt wartet, bis eine feste Prüfung erfüllt ist,
// höchstens bis zu seiner Frist. Erfüllt heißt: gateStable lang ununterbrochen,
// mit Daten, die nach dem Beginn des Schritts eingegangen sind – ein Stand von
// vor einem Neustart zählt nicht.

// Takt (Variablen für Tests).
var (
	gatePoll   = 15 * time.Second
	gateStable = 30 * time.Second
)

// hostGateAlerts sind die Alarme, die ein Host für host_healthy nicht haben darf
// (Dienste und Units prüft das Gate selbst, live).
var hostGateAlerts = []string{alert.KindSmart, alert.KindDisk}

// errGate ist ein Gate, das nie erfüllt werden kann (z. B. Check fehlt): Der
// Schritt schlägt sofort fehl, statt bis zur Frist zu warten.
type errGate struct{ msg string }

func (e errGate) Error() string { return e.msg }

// maxGateDetails begrenzt die Zahl der genannten Probleme in der Meldung.
const maxGateDetails = 3

// gateState gehört zu playbookActions: Ist das Gate des Schritts für den Host
// jetzt erfüllt? Daten von vor since zählen nicht (Zeit null: jeder Stand).
// detail nennt, worauf noch gewartet wird. Ein Fehler vom Typ errGate ist endgültig.
func (s *Server) gateState(ctx context.Context, step playbook.Step, hostID string, since time.Time) (bool, string, error) {
	switch step.Gate {
	case playbook.GateHostHealthy:
		return s.hostGate(hostID, since)
	case playbook.GateClusterHealthy:
		return s.clusterGate(ctx, since)
	case playbook.GateCheckOK:
		return s.checkGate(ctx, step.Check, since)
	}
	return false, "", errGate{"unbekanntes gate " + step.Gate}
}

// hostGate: verbunden, überwachte Dienste laufen, keine Unit failed, kein
// SMART- oder Platten-Alarm.
func (s *Server) hostGate(id string, since time.Time) (bool, string, error) {
	s.mu.Lock()
	ac, online := s.conns[id]
	svc, hasSvc := s.svcs[id]
	s.mu.Unlock()
	if !online {
		return false, "host nicht verbunden", nil
	}
	var problems []string
	if hasSvc { // Hosts ohne Dienstliste (z. B. Lesefehler im Agenten) prüfen nur den Rest
		if svc.at.Before(ac.since) || svc.at.Before(since) {
			return false, "warte auf aktuelle dienstliste", nil
		}
		watches, err := s.store.ServiceWatches(context.Background(), id)
		if err != nil {
			return false, "", fmt.Errorf("überwachte dienste lesen: %w", err)
		}
		for _, sv := range svc.list {
			switch {
			case sv.State == proto.SvcFailed:
				problems = append(problems, "unit "+sv.Name+" failed")
			case slices.Contains(watches, sv.Name) && sv.State != proto.SvcRunning:
				problems = append(problems, "dienst "+sv.Name+" läuft nicht")
			}
		}
		for _, w := range watches {
			if !slices.ContainsFunc(svc.list, func(sv proto.Service) bool { return sv.Name == w }) {
				problems = append(problems, "dienst "+w+" fehlt")
			}
		}
	}
	if s.alerts != nil {
		for _, a := range s.alerts.Firing(id, hostGateAlerts...) {
			problems = append(problems, "alarm "+a.Kind+" "+a.Target)
		}
	}
	return len(problems) == 0, joinProblems(problems), nil
}

// clusterGate: alle Nodes bereit, alle Workloads verfügbar, keine Pods im
// Fehlerzustand – mit denselben Ausnahmen wie die Cluster-Alarme.
func (s *Server) clusterGate(ctx context.Context, since time.Time) (bool, string, error) {
	if s.cluster == nil {
		return false, "", errGate{"k3s-ansicht ist abgeschaltet"}
	}
	snap, _ := s.cluster.Current()
	if snap == nil || snap.FetchedAt < since.Unix() {
		return false, "warte auf aktuellen cluster-stand", nil
	}
	nodes, okN := snap.NodeConds()
	work, okW := snap.WorkloadConds()
	if !okN || !okW {
		return false, "cluster-stand unvollständig", nil
	}
	filterConds(work, s.clusterIgnore(ctx), "")
	problems := append(badConds(nodes, "node "), badConds(work, "")...)
	return len(problems) == 0, joinProblems(problems), nil
}

// badConds liefert die Ziele mit Problem, sortiert.
func badConds(conds map[string]k8s.Cond, prefix string) []string {
	var bad []string
	for target, c := range conds {
		switch {
		case !c.Bad:
		case c.Detail == "":
			bad = append(bad, prefix+target)
		default:
			bad = append(bad, prefix+target+" ("+c.Detail+")")
		}
	}
	slices.Sort(bad)
	return bad
}

// checkGate: der externe Check mit diesem Namen meldet ok.
func (s *Server) checkGate(ctx context.Context, name string, since time.Time) (bool, string, error) {
	if s.checks == nil {
		return false, "", errGate{"externe checks sind abgeschaltet"}
	}
	c, err := s.checkByName(ctx, name)
	if err != nil {
		return false, "", err
	}
	r, ok := s.checks.Last()[c.ID]
	switch {
	case !ok || r.TS < since.Unix():
		return false, "warte auf neues ergebnis von " + c.Name, nil
	case !r.OK:
		msg := "check " + c.Name + " schlägt fehl"
		if r.Error != "" {
			msg += ": " + r.Error
		}
		return false, msg, nil
	}
	return true, "", nil
}

// checkByName sucht einen aktiven Check nach Namen (ohne Groß-/Kleinschreibung).
func (s *Server) checkByName(ctx context.Context, name string) (store.Check, error) {
	list, err := s.store.Checks(ctx)
	if err != nil {
		return store.Check{}, fmt.Errorf("checks lesen: %w", err)
	}
	var found []store.Check
	for _, c := range list {
		if strings.EqualFold(c.Name, name) {
			found = append(found, c)
		}
	}
	switch {
	case len(found) == 0:
		return store.Check{}, errGate{"check " + name + " nicht gefunden"}
	case len(found) > 1:
		return store.Check{}, errGate{"check-name " + name + " ist nicht eindeutig"}
	case !found[0].Enabled:
		return store.Check{}, errGate{"check " + name + " ist abgeschaltet"}
	}
	return found[0], nil
}

func joinProblems(p []string) string {
	if len(p) > maxGateDetails {
		return strings.Join(p[:maxGateDetails], ", ") + fmt.Sprintf(" und %d weitere", len(p)-maxGateDetails)
	}
	return strings.Join(p, ", ")
}

// execGate wartet, bis das Gate gateStable lang erfüllt ist, höchstens bis
// st.Until. Ein Abbruch des Laufs beendet das Warten (Schritt übersprungen).
func (s *Server) execGate(ctx context.Context, run *pbRun, step playbook.Step, st store.PlaybookStep,
	finish func(bool, string) (store.PlaybookStep, bool)) (store.PlaybookStep, bool) {
	since := time.Unix(st.StartedAt, 0)
	var okSince time.Time
	for {
		if run.cancel.Load() {
			st.Status, st.Message, st.FinishedAt = store.StepSkipped, "abgebrochen", s.nowFor().Unix()
			s.saveStep(run, st)
			return st, false
		}
		ok, detail, err := s.pb.act.gateState(ctx, step, st.HostID, since)
		now := s.nowFor()
		var eg errGate
		switch {
		case errors.As(err, &eg):
			return finish(false, eg.msg)
		case err != nil:
			s.log.Warn("gate prüfen", "run_id", run.id, "host", st.HostID, "err", err)
			ok, detail = false, "prüfung fehlgeschlagen"
		}
		if ok {
			if okSince.IsZero() {
				okSince = now
			}
			if now.Sub(okSince) >= gateStable {
				return finish(true, "erfüllt")
			}
			detail = "erfüllt, prüfe ob stabil"
		} else {
			okSince = time.Time{}
		}
		if now.Unix() >= st.Until {
			return finish(false, fmt.Sprintf("nach %v nicht erfüllt: %s", step.GateTimeout(), detail))
		}
		if detail != st.Message {
			st.Message = detail
			s.saveStep(run, st)
		}
		if !sleepCtx(ctx, min(gatePoll, time.Duration(st.Until-now.Unix())*time.Second)) {
			return st, false
		}
	}
}
