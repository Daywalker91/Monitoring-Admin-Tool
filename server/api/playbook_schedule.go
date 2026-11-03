package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Zeitpläne (M12c): Ein Playbook mit schedule startet zu seinen Terminen als
// Benutzer „zeitplan“. Einschalten geht nur über die Datei, in der GUI lässt
// sich ein Zeitplan nur pausieren. Termine gelten in der Zeitzone des Servers
// (TZ, wie die Wartungsfenster).

const scheduleUser = "zeitplan"

// Takt (Variablen für Tests).
var (
	scheduleTick = 30 * time.Second
	// scheduleGrace: War der Server zum Termin aus, startet der Lauf noch,
	// wenn er innerhalb dieser Zeit zurück ist; sonst fällt der Termin aus.
	scheduleGrace = time.Hour
)

// schedules liefert die gültigen Playbooks mit Zeitplan.
func (s *Server) schedules() map[string]*playbook.Schedule {
	s.pb.mu.Lock()
	defer s.pb.mu.Unlock()
	out := map[string]*playbook.Schedule{}
	for _, f := range s.pb.files {
		if f.OK() && f.Playbook.Schedule != "" {
			if sch, err := playbook.ParseSchedule(f.Playbook.Schedule); err == nil {
				out[f.Playbook.Name] = sch
			}
		}
	}
	return out
}

// runSchedules startet fällige Termine. Jeder Termin wird vor dem Start als
// erledigt gespeichert und so höchstens einmal ausgelöst.
func (s *Server) runSchedules(ctx context.Context) {
	scheds := s.schedules()
	if len(scheds) == 0 {
		return
	}
	states, err := s.store.PlaybookSchedules(ctx)
	if err != nil {
		s.log.Error("zeitpläne lesen", "err", err)
		return
	}
	now := s.nowFor().In(time.Local)
	for name, sch := range scheds {
		due := sch.Prev(now)
		st := states[name]
		if due.IsZero() || due.Unix() <= st.LastDue {
			continue
		}
		if err := s.store.SetScheduleDue(ctx, name, due.Unix()); err != nil {
			s.log.Error("zeitplan speichern", "playbook", name, "err", err)
			continue
		}
		log := s.log.With("playbook", name, "termin", due.Format(time.DateTime))
		switch {
		case st.LastDue == 0:
			// Neuer Zeitplan: erst ab dem nächsten Termin, nicht rückwirkend.
			log.Info("zeitplan erkannt", "nächster", sch.Next(now).Format(time.DateTime))
		case now.Sub(due) > scheduleGrace:
			log.Warn("zeitplan: termin verpasst (server war nicht erreichbar)")
			s.notifySchedule(name, "Termin "+due.Format("02.01. 15:04")+" verpasst: Server war nicht erreichbar.")
		case st.Paused:
			log.Info("zeitplan pausiert, termin übersprungen")
		default:
			run, err := s.startPlaybook(ctx, name, scheduleUser, scheduleUser)
			if err != nil {
				log.Warn("zeitplan: playbook nicht gestartet", "err", err)
				s.notifySchedule(name, "Nicht gestartet: "+err.Error())
				continue
			}
			log.Info("zeitplan: playbook gestartet", "run_id", run.ID)
		}
	}
}

func (s *Server) notifySchedule(name, body string) {
	if s.notifier != nil {
		s.notifier.Enqueue(notify.Message{Title: "Zeitplan " + name, Body: body, High: true, Tag: "warning"})
	}
}

// scheduleView ist der Zeitplan eines Playbooks in der Liste.
type scheduleView struct {
	Next   int64 `json:"next,omitempty"` // nächster Termin (Unix)
	Paused bool  `json:"paused"`
}

// scheduleViews liefert Pause und nächsten Termin je Playbook mit Zeitplan.
func (s *Server) scheduleViews(ctx context.Context) map[string]scheduleView {
	scheds := s.schedules()
	out := make(map[string]scheduleView, len(scheds))
	if len(scheds) == 0 {
		return out
	}
	states, err := s.store.PlaybookSchedules(ctx)
	if err != nil {
		s.log.Error("zeitpläne lesen", "err", err)
	}
	now := s.nowFor().In(time.Local)
	for name, sch := range scheds {
		v := scheduleView{Paused: states[name].Paused}
		if next := sch.Next(now); !next.IsZero() {
			v.Next = next.Unix()
		}
		out[name] = v
	}
	return out
}

// handlePauseSchedule pausiert den Zeitplan eines Playbooks oder setzt ihn fort.
func (s *Server) handlePauseSchedule(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if !s.requirePlaybooks(w) {
		return
	}
	var req struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	p, err := s.playbookFile(name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "playbook nicht gefunden", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case p.Schedule == "":
		http.Error(w, "playbook hat keinen zeitplan", http.StatusConflict)
		return
	}
	if err := s.store.SetSchedulePaused(r.Context(), name, req.Paused); err != nil {
		s.log.Error("zeitplan pausieren", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.log.Info("zeitplan geändert", "playbook", name, "pausiert", req.Paused, "user", userFrom(r.Context()), "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}
