package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/checks"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const uptimeSpan = 24 * time.Hour

// WithChecks aktiviert die externen Checks.
func (s *Server) WithChecks(r *checks.Runner) *Server {
	s.checks = r
	return s
}

// CheckResult leitet ein Check-Ergebnis an die Alarmauswertung weiter.
func (s *Server) CheckResult(ctx context.Context, c store.Check, r store.CheckResult) {
	if s.alerts == nil {
		return
	}
	detail := r.Error + " (" + c.Target + ")"
	var certDays *float64
	if r.CertNotAfter != nil {
		d := time.Unix(*r.CertNotAfter, 0).Sub(time.Unix(r.TS, 0)).Hours() / 24
		certDays = &d
	}
	s.alerts.ObserveCheck(ctx, checks.AlarmID(c.ID), c.Name, r.OK, detail, certDays, s.nowFor())
}

// checkView ist ein Check mit letztem Ergebnis für die GUI.
type checkView struct {
	store.Check
	Last      *store.CheckResult `json:"last"`       // nil = noch nicht gelaufen
	Uptime24h *float64           `json:"uptime_24h"` // Anteil erfolgreicher Durchläufe in Prozent
	Alerts    int                `json:"alerts"`     // aktive Alarme
}

func (s *Server) handleChecks(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Checks(r.Context())
	if err != nil {
		s.log.Error("checks lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	uptime, err := s.store.CheckUptime(r.Context(), s.nowFor().Add(-uptimeSpan))
	if err != nil {
		s.log.Error("verfügbarkeit lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	last := s.checks.Last()
	var active map[string]map[string]int
	if s.alerts != nil {
		active = s.alerts.ActiveCount()
	}
	out := make([]checkView, len(list))
	for i, c := range list {
		v := checkView{Check: c}
		if res, ok := last[c.ID]; ok {
			v.Last = &res
		}
		if u, ok := uptime[c.ID]; ok {
			v.Uptime24h = &u
		}
		for _, n := range active[checks.AlarmID(c.ID)] {
			v.Alerts += n
		}
		out[i] = v
	}
	writeJSON(w, out)
}

// checkInput sind die Felder, die die GUI setzen darf.
type checkInput struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Target       string `json:"target"`
	IntervalSec  int    `json:"interval_sec"`
	TimeoutSec   int    `json:"timeout_sec"`
	ExpectStatus int    `json:"expect_status"`
	SkipVerify   bool   `json:"skip_verify"`
	Enabled      bool   `json:"enabled"`
}

func (in checkInput) check() store.Check {
	return store.Check{Name: in.Name, Kind: in.Kind, Target: in.Target, IntervalSec: in.IntervalSec,
		TimeoutSec: in.TimeoutSec, ExpectStatus: in.ExpectStatus, SkipVerify: in.SkipVerify, Enabled: in.Enabled}
}

func (s *Server) handleCreateCheck(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var in checkInput
	if !decodeJSON(w, r, &in) {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	c := in.check()
	if err := checks.Validate(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n, err := s.store.CountChecks(r.Context())
	if err != nil {
		s.log.Error("checks zählen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	if n >= checks.MaxChecks {
		http.Error(w, "höchstens "+strconv.Itoa(checks.MaxChecks)+" checks", http.StatusConflict)
		return
	}
	c.CreatedAt = s.nowFor().Unix()
	if err := s.store.CreateCheck(r.Context(), &c); err != nil {
		s.log.Error("check anlegen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.reloadChecks(r.Context())
	s.log.Info("check angelegt", "check", c.ID, "kind", c.Kind, "user", userFrom(r.Context()))
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, c)
}

func (s *Server) handleEditCheck(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	old, ok := s.checkFromPath(w, r)
	if !ok {
		return
	}
	var in checkInput
	if !decodeJSON(w, r, &in) {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	c := in.check()
	if err := checks.Validate(&c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.ID, c.CreatedAt = old.ID, old.CreatedAt
	if err := s.store.UpdateCheck(r.Context(), c); err != nil {
		s.log.Error("check ändern", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	// Abgeschaltet oder neues Ziel: bisherige Alarme passen nicht mehr.
	if s.alerts != nil && (!c.Enabled || c.Kind != old.Kind || c.Target != old.Target) {
		s.alerts.Clear(r.Context(), checks.AlarmID(c.ID), s.nowFor())
	}
	s.reloadChecks(r.Context())
	s.log.Info("check geändert", "check", c.ID, "user", userFrom(r.Context()))
	writeJSON(w, c)
}

func (s *Server) handleDeleteCheck(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	c, ok := s.checkFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteCheck(r.Context(), c.ID, checks.AlarmID(c.ID)); err != nil {
		s.log.Error("check löschen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	if s.alerts != nil {
		s.alerts.Forget(checks.AlarmID(c.ID))
	}
	s.reloadChecks(r.Context())
	s.log.Info("check gelöscht", "check", c.ID, "user", userFrom(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCheckHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := s.checkFromPath(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	hours, ok := intParam(q.Get("hours"), 24, 1, 24*7)
	if !ok {
		http.Error(w, "hours muss zwischen 1 und 168 liegen", http.StatusBadRequest)
		return
	}
	maxPoints, ok := intParam(q.Get("points"), 0, 0, 2000)
	if !ok {
		http.Error(w, "points muss zwischen 0 und 2000 liegen", http.StatusBadRequest)
		return
	}
	span := time.Duration(hours) * time.Hour
	var bucket time.Duration
	if maxPoints > 0 {
		bucket = (span + time.Duration(maxPoints) - 1) / time.Duration(maxPoints)
	}
	points, err := s.store.CheckHistory(r.Context(), c.ID, s.nowFor().Add(-span), bucket)
	if err != nil {
		s.log.Error("check-verlauf lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, points)
}

// checkFromPath liest den Check aus {id}; bei Fehlern ist die Antwort geschrieben.
func (s *Server) checkFromPath(w http.ResponseWriter, r *http.Request) (store.Check, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "ungültige id", http.StatusBadRequest)
		return store.Check{}, false
	}
	c, err := s.store.CheckByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "check nicht gefunden", http.StatusNotFound)
		return store.Check{}, false
	}
	if err != nil {
		s.log.Error("check lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return store.Check{}, false
	}
	return c, true
}

func (s *Server) reloadChecks(ctx context.Context) {
	if err := s.checks.Reload(ctx); err != nil {
		s.log.Error("checks neu laden", "err", err)
	}
}
