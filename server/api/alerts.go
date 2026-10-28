package api

import (
	"context"
	"encoding/json"
	"net/http"
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
	return s
}

// RunAlerts prüft regelmäßig, ob Hosts offline sind. Ohne Alarmauswertung kehrt
// die Funktion sofort zurück.
func (s *Server) RunAlerts(ctx context.Context) {
	if s.alerts == nil {
		return
	}
	s.alerts.Run(ctx, alertCheckInterval, s.hostStates)
}

func (s *Server) hostStates(ctx context.Context) ([]alert.HostState, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]alert.HostState, len(hosts))
	for i, h := range hosts {
		out[i] = alert.HostState{ID: h.ID, Hostname: h.Hostname, Online: s.online(h.ID),
			LastSeen: time.Unix(h.LastSeen, 0)}
	}
	return out, nil
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
	Rules      []store.AlertRule `json:"rules"`
	Channels   []string          `json:"channels"`
	Hysteresis float64           `json:"hysteresis"`
}

func (s *Server) handleGetAlertRules(w http.ResponseWriter, _ *http.Request) {
	if s.alerts == nil {
		http.Error(w, "alarme deaktiviert", http.StatusNotFound)
		return
	}
	writeJSON(w, alertRulesView{Rules: s.alerts.Rules(), Channels: s.channels(), Hysteresis: alert.Hysteresis})
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
		Rules []store.AlertRule `json:"rules"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.Rules) == 0 || len(req.Rules) > len(alert.DefaultRules) {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	if err := s.alerts.SetRules(r.Context(), req.Rules, s.nowFor()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("alarmregeln geändert", "user", userFrom(r.Context()))
	writeJSON(w, alertRulesView{Rules: s.alerts.Rules(), Channels: s.channels(), Hysteresis: alert.Hysteresis})
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
		Title: "Homelab Monitor: Testnachricht",
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
