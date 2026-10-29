package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// svcSummary fasst die Dienstliste eines Hosts für die Host-Liste zusammen.
type svcSummary struct {
	Total  int `json:"total"`
	Failed int `json:"failed"` // systemd-Units im Zustand failed
}

// svcState ist die letzte Dienstliste eines Hosts (nur im Speicher; nach einem
// Serverneustart ab der nächsten Meldung). Sie wird regelmäßig neu
// ausgewertet, damit die Mindestdauer eines Alarms abläuft, auch wenn der
// Agent eine unveränderte Liste nur alle paar Minuten schickt.
type svcState struct {
	sum      svcSummary
	list     []proto.Service
	hostname string // gemeldeter Hostname (Anzeigename kommt aus s.names)
}

// observeServices speichert eine Dienstliste und wertet die Alarme aus.
// hostname ist der vom Agenten gemeldete Name.
func (s *Server) observeServices(ctx context.Context, id, hostname string, svcs proto.Services, raw []byte, now time.Time) {
	if err := s.store.SaveServices(ctx, id, raw, now); err != nil {
		s.log.Error("dienste speichern", "agent_id", id, "err", err)
	}
	sum := svcSummary{Total: len(svcs.List)}
	for _, sv := range svcs.List {
		if sv.State == proto.SvcFailed {
			sum.Failed++
		}
	}
	s.mu.Lock()
	s.svcs[id] = svcState{sum: sum, list: svcs.List, hostname: hostname}
	s.mu.Unlock()
	s.evaluateServices(ctx, id, s.hostName(id, hostname), svcs.List, now)
}

// reevaluateServices wertet die Dienstlisten aller verbundenen Hosts neu aus.
func (s *Server) reevaluateServices(ctx context.Context) {
	s.mu.Lock()
	states := make(map[string]svcState, len(s.svcs))
	for id, st := range s.svcs {
		if _, online := s.conns[id]; online {
			states[id] = st
		}
	}
	s.mu.Unlock()
	now := s.nowFor()
	for id, st := range states {
		s.evaluateServices(ctx, id, s.hostName(id, st.hostname), st.list, now)
	}
}

// evaluateServices prüft überwachte Dienste (Alarm "service") und fehlgeschlagene
// systemd-Units (Alarm "unit_failed").
func (s *Server) evaluateServices(ctx context.Context, id, hostname string, list []proto.Service, now time.Time) {
	if s.alerts == nil {
		return
	}
	watches, err := s.store.ServiceWatches(ctx, id)
	if err != nil {
		s.log.Error("überwachte dienste lesen", "err", err)
		return
	}
	byName := make(map[string]proto.Service, len(list))
	failed := map[string]alert.Condition{}
	for _, sv := range list {
		byName[sv.Name] = sv
		if sv.State == proto.SvcFailed {
			failed[sv.Name] = alert.Condition{Bad: true, Detail: sv.Detail}
		}
	}
	watched := make(map[string]alert.Condition, len(watches))
	for _, name := range watches {
		if sv, ok := byName[name]; ok {
			watched[name] = alert.Condition{Bad: sv.State != proto.SvcRunning, Detail: sv.Detail}
		} else {
			watched[name] = alert.Condition{Bad: true, Detail: "nicht vorhanden"}
		}
	}
	s.alerts.ObserveSet(ctx, id, hostname, alert.KindService, watched, now)
	s.alerts.ObserveSet(ctx, id, hostname, alert.KindUnitFailed, failed, now)
}

func (s *Server) servicesSummary(id string) *svcSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.svcs[id]; ok {
		return &st.sum
	}
	return nil
}

// servicesView ist die Antwort von GET /api/hosts/{id}/services.
type servicesView struct {
	Updated int64           `json:"updated"` // 0 = noch keine Liste
	List    []proto.Service `json:"list"`
	Watches []string        `json:"watches"`
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	raw, at, err := s.store.Services(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("dienste lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	v := servicesView{Updated: at, List: []proto.Service{}}
	if raw != nil {
		var svcs proto.Services
		if err := json.Unmarshal(raw, &svcs); err != nil {
			s.log.Error("gespeicherte dienste lesen", "err", err)
		} else if svcs.List != nil {
			v.List = svcs.List
		}
	}
	if v.Watches, err = s.store.ServiceWatches(r.Context(), id); err != nil {
		s.log.Error("überwachte dienste lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, v)
}

type watchRequest struct {
	Name  string `json:"name"`
	Watch bool   `json:"watch"`
}

// handleServiceWatch schaltet die Überwachung eines Dienstes ein oder aus und
// wertet die Alarme sofort mit der letzten Liste neu aus.
func (s *Server) handleServiceWatch(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var req watchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	if !proto.ValidServiceName(req.Name) {
		http.Error(w, "ungültiger dienstname", http.StatusBadRequest)
		return
	}
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	if err := s.store.SetServiceWatch(r.Context(), h.ID, req.Name, req.Watch); err != nil {
		s.log.Error("überwachung speichern", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.log.Info("dienstüberwachung geändert", "agent_id", h.ID, "service", req.Name, "watch", req.Watch,
		"user", userFrom(r.Context()))
	if raw, _, err := s.store.Services(r.Context(), h.ID); err == nil && raw != nil {
		var svcs proto.Services
		if json.Unmarshal(raw, &svcs) == nil {
			s.evaluateServices(r.Context(), h.ID, h.Name(), svcs.List, s.nowFor())
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdates liefert den letzten Update-Stand eines Hosts samt Paketliste
// (null = noch keiner).
func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	raw, err := s.store.Updates(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("update-stand lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	if raw == nil {
		raw = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}
