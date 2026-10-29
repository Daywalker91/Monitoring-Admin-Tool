package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const (
	alertCheckInterval = 30 * time.Second
	alertHistorySpan   = 7 * 24 * time.Hour
	alertListLimit     = 500
)

// WithAlerts aktiviert die Alarmauswertung. d verschickt Testnachrichten und
// liefert die Namen der konfigurierten Kanäle.
func (s *Server) WithAlerts(ev *alert.Evaluator, d *notify.Dispatcher) *Server {
	s.alerts = ev
	s.notifier = d
	ev.SetQuiet(s.quiet)
	return s
}

// RunAlerts prüft regelmäßig, ob Hosts offline sind, und wertet die
// Dienstlisten neu aus. Ohne Alarmauswertung kehrt
// die Funktion sofort zurück.
func (s *Server) RunAlerts(ctx context.Context) {
	if s.alerts == nil {
		return
	}
	s.alerts.Run(ctx, alertCheckInterval, func(ctx context.Context) ([]alert.HostState, error) {
		s.reevaluateServices(ctx)
		return s.hostStates(ctx)
	})
}

func (s *Server) hostStates(ctx context.Context) ([]alert.HostState, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]alert.HostState, len(hosts))
	for i, h := range hosts {
		out[i] = alert.HostState{ID: h.ID, Hostname: h.Name(), Online: s.online(h.ID),
			LastSeen: time.Unix(h.LastSeen, 0)}
	}
	return out, nil
}

// maxAckComment begrenzt den Kommentar einer Quittierung (Zeichen).
const maxAckComment = 256

// handleAck quittiert einen aktiven Alarm (POST, {"comment": "…"}) oder hebt
// die Quittierung auf (DELETE). Quittierte Alarme bekommen keine Erinnerungen.
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if s.alerts == nil {
		http.Error(w, "alarme deaktiviert", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "ungültige id", http.StatusBadRequest)
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	ack := r.Method == http.MethodPost
	if ack {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, "ungültige anfrage", http.StatusBadRequest)
			return
		}
		var ok bool
		if req.Comment, ok = validText(req.Comment, maxAckComment); !ok {
			http.Error(w, "kommentar: höchstens 256 zeichen, keine steuerzeichen", http.StatusBadRequest)
			return
		}
	}
	user := userFrom(r.Context())
	a, err := s.alerts.Ack(r.Context(), id, ack, user, req.Comment, s.nowFor())
	switch {
	case errors.Is(err, alert.ErrNotActive):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		s.log.Error("quittieren", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.log.Info("alarm quittiert", "alert_id", id, "ack", ack, "user", user)
	writeJSON(w, a)
}

// handleAlerts liefert aktive Alarme, mit ?all=1 zusätzlich die beendeten der letzten 7 Tage.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	alerts, err := s.store.Alerts(r.Context(), !all, s.nowFor().Add(-alertHistorySpan), alertListLimit)
	if err != nil {
		s.log.Error("alarme lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, alerts)
}

type alertRulesView struct {
	Rules         []store.AlertRule `json:"rules"`
	Channels      []string          `json:"channels"`
	Hysteresis    float64           `json:"hysteresis"`
	ReminderHours int               `json:"reminder_hours"` // 0 = aus
}

func (s *Server) rulesView() alertRulesView {
	return alertRulesView{Rules: s.alerts.Rules(), Channels: s.channels(), Hysteresis: alert.Hysteresis,
		ReminderHours: int(s.alerts.Reminder() / time.Hour)}
}

func (s *Server) handleGetAlertRules(w http.ResponseWriter, _ *http.Request) {
	if s.alerts == nil {
		http.Error(w, "alarme deaktiviert", http.StatusNotFound)
		return
	}
	writeJSON(w, s.rulesView())
}

func (s *Server) channels() []string {
	if s.notifier == nil {
		return []string{}
	}
	return s.notifier.Channels()
}

func (s *Server) handleSetAlertRules(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if s.alerts == nil {
		http.Error(w, "alarme deaktiviert", http.StatusNotFound)
		return
	}
	var req struct {
		Rules         []store.AlertRule `json:"rules"`
		ReminderHours *int              `json:"reminder_hours"`
	}
	if !decodeJSON(w, r, &req) || len(req.Rules) == 0 || len(req.Rules) > len(alert.DefaultRules) {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	if req.ReminderHours != nil {
		if err := s.alerts.SetReminder(r.Context(), *req.ReminderHours); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := s.alerts.SetRules(r.Context(), req.Rules, s.nowFor()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("alarmregeln geändert", "user", userFrom(r.Context()))
	writeJSON(w, s.rulesView())
}

// handleTestAlert schickt eine Testnachricht über alle Kanäle und meldet das Ergebnis je Kanal.
func (s *Server) handleTestAlert(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if s.notifier == nil || len(s.notifier.Channels()) == 0 {
		http.Error(w, "keine benachrichtigungskanäle konfiguriert", http.StatusConflict)
		return
	}
	user := userFrom(r.Context())
	s.log.Info("testnachricht angefordert", "user", user)
	results := s.notifier.Test(r.Context(), notify.Message{
		Title: "MAT: Testnachricht",
		Body:  "Diese Nachricht wurde von " + user + " in der GUI angefordert. Benachrichtigungen funktionieren.",
		Tag:   "white_check_mark",
	})
	writeJSON(w, results)
}

// NotifyEvent wandelt ein Alarmereignis in eine Meldung für die Kanäle.
func NotifyEvent(d *notify.Dispatcher) func(alert.Event) {
	return func(e alert.Event) {
		tag, high := "white_check_mark", false
		switch e.Type {
		case alert.Firing:
			tag, high = "rotating_light", true
		case alert.Reminder:
			tag, high = "bell", true
		}
		d.Enqueue(notify.Message{Title: e.Title(), Body: e.Body(), High: high, Tag: tag})
	}
}

// decodeJSON liest einen kleinen JSON-Body mit unbekannten Feldern als Fehler.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

// knownHost antwortet mit 404 bzw. 500 und liefert false, wenn der Host fehlt.
func (s *Server) knownHost(w http.ResponseWriter, r *http.Request) bool {
	if s.alerts == nil {
		http.Error(w, "alarme deaktiviert", http.StatusNotFound)
		return false
	}
	ok, err := s.store.HostExists(r.Context(), r.PathValue("id"))
	if err != nil {
		s.log.Error("host prüfen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return false
	}
	if !ok {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
	}
	return ok
}

// hostDefaults liefert die globalen Regeln der Typen, die je Host gelten.
func (s *Server) hostDefaults() []store.AlertRule {
	out := []store.AlertRule{}
	for _, r := range s.alerts.Rules() {
		if slices.Contains(alert.HostKinds, r.Kind) {
			out = append(out, r)
		}
	}
	return out
}

type hostRulesView struct {
	Rules    []store.AlertRule `json:"rules"`    // abweichende Regeln des Hosts
	Defaults []store.AlertRule `json:"defaults"` // globale Regeln
}

func (s *Server) handleGetHostRules(w http.ResponseWriter, r *http.Request) {
	if !s.knownHost(w, r) {
		return
	}
	writeJSON(w, hostRulesView{Rules: s.alerts.HostRules(r.PathValue("id")), Defaults: s.hostDefaults()})
}

func (s *Server) handleSetHostRules(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if !s.knownHost(w, r) {
		return
	}
	var req struct {
		Rules []store.AlertRule `json:"rules"`
	}
	if !decodeJSON(w, r, &req) || len(req.Rules) > len(alert.DefaultRules) {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	if err := s.alerts.SetHostRules(r.Context(), id, req.Rules, s.nowFor()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("host-alarmregeln geändert", "agent_id", id, "rules", len(req.Rules), "user", userFrom(r.Context()))
	writeJSON(w, hostRulesView{Rules: s.alerts.HostRules(id), Defaults: s.hostDefaults()})
}

// handleMute schaltet einen Host stumm (POST {"minutes": N}, 0 = unbefristet)
// oder hebt die Stummschaltung auf (DELETE).
func (s *Server) handleMute(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if !s.knownHost(w, r) {
		return
	}
	var until time.Time
	if r.Method == http.MethodPost {
		var req struct {
			Minutes int `json:"minutes"`
		}
		if !decodeJSON(w, r, &req) || req.Minutes < 0 || req.Minutes > 30*24*60 {
			http.Error(w, "minutes muss zwischen 0 (unbefristet) und 43200 liegen", http.StatusBadRequest)
			return
		}
		until = alert.MuteForever
		if req.Minutes > 0 {
			until = s.nowFor().Add(time.Duration(req.Minutes) * time.Minute)
		}
	}
	id := r.PathValue("id")
	if err := s.alerts.Mute(r.Context(), id, until); err != nil {
		s.log.Error("stummschalten", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.log.Info("stummschaltung geändert", "agent_id", id, "until", until, "user", userFrom(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}
