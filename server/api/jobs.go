package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const (
	// jobGrace: so lange wartet der Server über das Zeitlimit der Aktion hinaus
	// auf eine Rückmeldung (Übertragung, Nachlieferung nach kurzer Trennung).
	jobGrace = 2 * time.Minute
	jobsList = 50
)

type jobRequest struct {
	Action string            `json:"action"`
	Params map[string]string `json:"params"`
}

// handleCreateJob startet eine Admin-Aktion auf einem Host. Erlaubt sind nur
// Aktionen aus der Whitelist (proto), deren Fähigkeit der Host gemeldet hat.
// Das Self-Update hat einen eigenen Ablauf (/update).
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var req jobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	if req.Action == proto.ActionAgentUpdate {
		http.Error(w, "agent-update bitte über die update-funktion starten", http.StatusBadRequest)
		return
	}
	job := proto.JobRequest{JobID: newJobID("job"), Action: req.Action, Params: req.Params}
	if err := job.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	if !slices.Contains(h.Capabilities, proto.ActionCapability[job.Action]) {
		http.Error(w, "host unterstützt diese aktion nicht (fähigkeit in agent.json freigeschaltet?)", http.StatusConflict)
		return
	}
	if job.Action == proto.ActionLogRead && !slices.Contains(h.LogSources, job.Params["source"]) {
		http.Error(w, "log-quelle ist auf diesem host nicht freigegeben", http.StatusBadRequest)
		return
	}
	if !s.online(h.ID) {
		http.Error(w, "host ist offline", http.StatusConflict)
		return
	}
	j, err := s.startJob(r.Context(), h.ID, job, userFrom(r.Context()), r.RemoteAddr, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, j)
}

// findHost sucht den Host aus dem Pfad; sonst Antwort 404 bzw. 500.
func (s *Server) findHost(w http.ResponseWriter, r *http.Request) (store.Host, bool) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.log.Error("hosts lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return store.Host{}, false
	}
	id := r.PathValue("id")
	i := slices.IndexFunc(hosts, func(h store.Host) bool { return h.ID == id })
	if i < 0 {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return store.Host{}, false
	}
	return hosts[i], true
}

// recordJob legt den Job im Audit-Log an. extra verlängert die Deadline (z. B.
// um die Vorbereitung eines Updates).
func (s *Server) recordJob(ctx context.Context, hostID string, job proto.JobRequest, user, remote string, extra time.Duration) (store.Job, error) {
	now := s.nowFor()
	j := store.Job{ID: job.JobID, HostID: hostID, Action: job.Action, Params: job.Params, User: user, Remote: remote,
		CreatedAt: now.Unix(), Deadline: now.Add(proto.JobTimeout(job.Action) + jobGrace + extra).Unix()}
	if err := s.store.CreateJob(ctx, j); err != nil {
		s.log.Error("job speichern", "err", err)
		return j, errors.New("interner fehler")
	}
	s.log.Info("job gestartet", "agent_id", hostID, "job_id", job.JobID, "action", job.Action,
		"params", job.Params, "user", user, "remote", remote)
	return j, nil
}

// startJob speichert den Job und schickt ihn an den Agenten.
func (s *Server) startJob(ctx context.Context, hostID string, job proto.JobRequest, user, remote string, extra time.Duration) (store.Job, error) {
	j, err := s.recordJob(ctx, hostID, job, user, remote, extra)
	if err != nil {
		return j, err
	}
	if err := s.sendJob(ctx, hostID, job); err != nil {
		s.failJob(hostID, job.JobID, err)
		return j, err
	}
	return j, nil
}

// failJob beendet einen Job, der den Agenten nicht erreicht hat.
func (s *Server) failJob(hostID, jobID string, cause error) {
	if _, err := s.store.FinishJob(context.Background(), jobID, hostID, s.nowFor(), false, 1, "", cause.Error()); err != nil {
		s.log.Error("job beenden", "err", err)
	}
}

// jobResult verarbeitet die Rückmeldung eines Agenten.
func (s *Server) jobResult(id string, res proto.JobResult) {
	found, err := s.store.FinishJob(context.Background(), res.JobID, id, s.nowFor(), res.OK, res.ExitCode, res.Output, res.Error)
	if err != nil {
		s.log.Error("job-ergebnis speichern", "err", err)
	}
	if s.updateResult(id, res) {
		return
	}
	if !found {
		s.log.Warn("job-ergebnis ohne passenden job", "agent_id", id, "job_id", res.JobID)
		return
	}
	if res.OK {
		s.log.Info("job erledigt", "agent_id", id, "job_id", res.JobID)
	} else {
		s.log.Warn("job fehlgeschlagen", "agent_id", id, "job_id", res.JobID, "err", res.Error)
	}
}

// handleHostJobs liefert die letzten Jobs eines Hosts (ohne Ausgabe).
func (s *Server) handleHostJobs(w http.ResponseWriter, r *http.Request) {
	s.expireJobs(r.Context())
	jobs, err := s.store.Jobs(r.Context(), r.PathValue("id"), jobsList)
	if err != nil {
		s.log.Error("jobs lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, jobs)
}

// handleJob liefert einen Job samt Ausgabe.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	s.expireJobs(r.Context())
	j, err := s.store.JobByID(r.Context(), r.PathValue("job"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "job nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("job lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, j)
}

func (s *Server) expireJobs(ctx context.Context) {
	if _, err := s.store.ExpireJobs(ctx, s.nowFor()); err != nil {
		s.log.Error("jobs ablaufen lassen", "err", err)
	}
}
