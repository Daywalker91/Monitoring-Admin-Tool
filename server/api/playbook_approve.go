package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Freigabe-Schritte (M12c): Der Lauf hält an, bis ein angemeldeter Benutzer in
// der GUI freigibt oder ablehnt, höchstens bis zur Frist des Schritts. Mit once
// gilt eine Entscheidung für alle Hosts des Laufs. Wer entschieden hat, steht in
// der Meldung des Schritts und im Log.

const (
	msgAwaitApproval = "wartet auf freigabe"
	msgApproved      = "freigegeben von "
	msgRejected      = "abgelehnt von "
	msgForAll        = " (für alle hosts)"
)

// approval ist eine Entscheidung über einen Freigabe-Schritt.
type approval struct {
	ok   bool
	user string
}

func approvalKey(host string, idx int, once bool) string {
	if once {
		host = "*"
	}
	return host + "/" + strconv.Itoa(idx)
}

// decision liefert die Entscheidung für den Schritt idx auf host.
func (r *pbRun) decision(host string, idx int, once bool) (approval, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.approvals[approvalKey(host, idx, once)]
	return a, ok
}

// decide hält eine Entscheidung fest; eine bestehende bleibt (erste gewinnt).
func (r *pbRun) decide(host string, idx int, once bool, a approval) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.approvals == nil {
		r.approvals = map[string]approval{}
	}
	k := approvalKey(host, idx, once)
	if _, done := r.approvals[k]; done {
		return false
	}
	r.approvals[k] = a
	return true
}

// restoreApproval stellt nach einem Neustart eine once-Entscheidung aus dem
// gespeicherten Schritt wieder her, damit die übrigen Hosts nicht erneut fragen.
func (r *pbRun) restoreApproval(step playbook.Step, st store.PlaybookStep) {
	if !step.IsApprove() || !step.Once {
		return
	}
	msg := strings.TrimSuffix(st.Message, msgForAll)
	switch {
	case st.Status == store.StepOK && strings.HasPrefix(msg, msgApproved):
		r.decide(st.HostID, st.Index, true, approval{true, strings.TrimPrefix(msg, msgApproved)})
	case st.Status == store.StepFailed && strings.HasPrefix(msg, msgRejected):
		r.decide(st.HostID, st.Index, true, approval{false, strings.TrimPrefix(msg, msgRejected)})
	}
}

// askApproval meldet, dass ein Lauf auf eine Freigabe wartet (bei once nur einmal).
func (s *Server) askApproval(run *pbRun, step playbook.Step, st store.PlaybookStep) {
	if _, done := run.decision(st.HostID, st.Index, step.Once); done {
		return
	}
	run.mu.Lock()
	if step.Once {
		if run.asked == nil {
			run.asked = map[int]bool{}
		}
		if run.asked[st.Index] {
			run.mu.Unlock()
			return
		}
		run.asked[st.Index] = true
	}
	run.mu.Unlock()
	host := st.HostID
	if step.Once {
		host = "alle hosts"
	}
	s.log.Info("playbook wartet auf freigabe", "run_id", run.id, "playbook", run.pb.Name, "schritt", step.ID, "host", host)
	if s.notifier != nil {
		s.notifier.Enqueue(notify.Message{Title: "Playbook " + run.pb.Name + " wartet auf Freigabe",
			Body: step.Approve + " (" + host + ", Frist " + step.StepTimeout().String() + ")", High: true, Tag: "raised_hand"})
	}
}

// execApprove wartet auf die Entscheidung, höchstens bis st.Until.
func (s *Server) execApprove(ctx context.Context, run *pbRun, step playbook.Step, st store.PlaybookStep,
	finish func(bool, string) (store.PlaybookStep, bool)) (store.PlaybookStep, bool) {
	for {
		if run.cancel.Load() {
			st.Status, st.Message, st.FinishedAt = store.StepSkipped, "abgebrochen", s.nowFor().Unix()
			s.saveStep(run, st)
			return st, false
		}
		if a, ok := run.decision(st.HostID, st.Index, step.Once); ok {
			msg := msgRejected
			if a.ok {
				msg = msgApproved
			}
			msg += a.user
			if step.Once {
				msg += msgForAll
			}
			return finish(a.ok, msg)
		}
		if s.nowFor().Unix() >= st.Until {
			return finish(false, "keine freigabe nach "+step.StepTimeout().String())
		}
		if !sleepCtx(ctx, pbPoll) {
			return st, false
		}
	}
}

// approveRequest ist der Inhalt von POST /api/playbook-runs/{id}/approve.
type approveRequest struct {
	Host string `json:"host"`
	Step int    `json:"step"` // Index des Schritts
	OK   bool   `json:"ok"`   // true = freigeben, false = ablehnen
}

func (s *Server) handleApprovePlaybookStep(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if !s.requirePlaybooks(w) {
		return
	}
	var req approveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	id, user := r.PathValue("id"), userFrom(r.Context())
	s.pb.mu.Lock()
	run := s.pb.runs[id]
	s.pb.mu.Unlock()
	if run == nil {
		http.Error(w, "kein laufender lauf mit dieser id", http.StatusNotFound)
		return
	}
	if req.Step < 0 || req.Step >= len(run.pb.Steps) || !run.pb.Steps[req.Step].IsApprove() || !slices.Contains(run.hosts, req.Host) {
		http.Error(w, "kein freigabe-schritt", http.StatusBadRequest)
		return
	}
	step := run.pb.Steps[req.Step]
	if st := run.step(req.Host, req.Step); st.Status != store.StepRunning {
		http.Error(w, "schritt wartet nicht auf freigabe", http.StatusConflict)
		return
	}
	if !run.decide(req.Host, req.Step, step.Once, approval{req.OK, user}) {
		http.Error(w, "schon entschieden", http.StatusConflict)
		return
	}
	s.log.Info("playbook-freigabe", "run_id", id, "playbook", run.pb.Name, "schritt", step.ID, "host", req.Host,
		"once", step.Once, "freigegeben", req.OK, "user", user, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}
