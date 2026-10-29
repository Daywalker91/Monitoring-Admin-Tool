package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// LoadMaintenance lädt die Wartungsfenster (beim Start und nach Änderungen).
func (s *Server) LoadMaintenance(ctx context.Context) error {
	list, err := s.store.MaintenanceWindows(ctx)
	if err != nil {
		return err
	}
	s.maintMu.Lock()
	s.maint = list
	s.maintMu.Unlock()
	return nil
}

// maintenanceFor liefert den Namen eines aktiven Wartungsfensters für id ("" =
// keines). Hosts erfasst ein Fenster über Bereich, Host oder Gruppe; Checks und
// der Cluster nur über "alle".
func (s *Server) maintenanceFor(id string, now time.Time) string {
	s.maintMu.Lock()
	list := s.maint
	s.maintMu.Unlock()
	var group *string
	for _, m := range list {
		if _, ok := m.ActiveAt(now, s.loc); !ok {
			continue
		}
		switch m.Scope {
		case store.ScopeAll:
			return m.Name
		case store.ScopeHost:
			if m.Target == id {
				return m.Name
			}
		case store.ScopeGroup:
			if group == nil {
				g, err := s.store.HostGroup(context.Background(), id)
				if err != nil {
					s.log.Error("gruppe für wartungsfenster", "err", err)
				}
				group = &g
			}
			if *group != "" && strings.EqualFold(*group, m.Target) {
				return m.Name
			}
		}
	}
	return ""
}

// quiet ist der Grund, Alarme von id gerade nicht zu melden (für die Alarm-Engine).
func (s *Server) quiet(id string, now time.Time) (string, bool) {
	if name := s.maintenanceFor(id, now); name != "" {
		return "Wartung „" + name + "“", true
	}
	return s.dependencyDown(id)
}

// maintenanceView ergänzt ein Fenster um seinen aktuellen Stand.
type maintenanceView struct {
	store.Maintenance
	Active    bool  `json:"active"`
	EndsAt    int64 `json:"ends_at,omitempty"`
	NextStart int64 `json:"next_start,omitempty"`
}

func (s *Server) handleMaintenanceList(w http.ResponseWriter, _ *http.Request) {
	s.maintMu.Lock()
	list := s.maint
	s.maintMu.Unlock()
	now := s.nowFor()
	out := make([]maintenanceView, len(list))
	for i, m := range list {
		out[i] = maintenanceView{Maintenance: m}
		if end, ok := m.ActiveAt(now, s.loc); ok {
			out[i].Active, out[i].EndsAt = true, end.Unix()
		}
		if next := m.NextStart(now, s.loc); !next.IsZero() {
			out[i].NextStart = next.Unix()
		}
	}
	writeJSON(w, out)
}

// handleMaintenanceSave legt ein Fenster an (POST) oder ändert es (PUT /{id}).
func (s *Server) handleMaintenanceSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var m store.Maintenance
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&m); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	m.ID = 0
	if r.Method == http.MethodPut {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "ungültige id", http.StatusBadRequest)
			return
		}
		m.ID = id
	}
	var ok1, ok2 bool
	m.Name, ok1 = validMeta(m.Name)
	m.Target, ok2 = validMeta(m.Target)
	if !ok1 || !ok2 {
		http.Error(w, "name und ziel: höchstens 64 zeichen, keine steuerzeichen", http.StatusBadRequest)
		return
	}
	if err := m.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if m.Scope == store.ScopeHost {
		if exists, err := s.store.HostExists(r.Context(), m.Target); err != nil || !exists {
			http.Error(w, "host nicht gefunden", http.StatusBadRequest)
			return
		}
	}
	m.CreatedAt = s.nowFor().Unix()
	err := s.store.SaveMaintenance(r.Context(), &m)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "wartungsfenster nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("wartungsfenster speichern", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.reloadMaintenance(r.Context())
	s.log.Info("wartungsfenster gespeichert", "id", m.ID, "name", m.Name, "scope", m.Scope, "target", m.Target,
		"user", userFrom(r.Context()))
	writeJSON(w, m)
}

func (s *Server) handleMaintenanceDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "ungültige id", http.StatusBadRequest)
		return
	}
	err = s.store.DeleteMaintenance(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "wartungsfenster nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("wartungsfenster löschen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.reloadMaintenance(r.Context())
	s.log.Info("wartungsfenster gelöscht", "id", id, "user", userFrom(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reloadMaintenance(ctx context.Context) {
	if err := s.LoadMaintenance(ctx); err != nil {
		s.log.Error("wartungsfenster laden", "err", err)
	}
}

// maxDependencyDepth begrenzt die Kette Host → Host → … bei der Prüfung auf Schleifen.
const maxDependencyDepth = 16

// checkDependency prüft eine Abhängigkeit: Ziel existiert, ist nicht der Host
// selbst und erzeugt keine Schleife.
func (s *Server) checkDependency(ctx context.Context, id, dep string) error {
	if dep == "" {
		return nil
	}
	kind, target, ok := strings.Cut(dep, ":")
	switch {
	case !ok || target == "":
		return errors.New(`abhängigkeit: "host:<id>" oder "check:<id>"`)
	case kind == "check":
		n, err := strconv.ParseInt(target, 10, 64)
		if err != nil {
			return errors.New("abhängigkeit: ungültige check-id")
		}
		if _, err := s.store.CheckByID(ctx, n); err != nil {
			return errors.New("abhängigkeit: check nicht gefunden")
		}
		return nil
	case kind != "host":
		return errors.New(`abhängigkeit: "host:<id>" oder "check:<id>"`)
	}
	for depth := 0; depth < maxDependencyDepth; depth++ {
		if target == id {
			return errors.New("abhängigkeit: ergibt eine schleife")
		}
		exists, err := s.store.HostExists(ctx, target)
		if err != nil || !exists {
			return errors.New("abhängigkeit: host nicht gefunden")
		}
		next, err := s.store.HostDependsOn(ctx, target)
		if err != nil {
			return err
		}
		k, t, _ := strings.Cut(next, ":")
		if k != "host" {
			return nil
		}
		target = t
	}
	return errors.New("abhängigkeit: kette zu lang")
}

// dependencyDown meldet, ob das Ziel der Abhängigkeit von id ausgefallen ist:
// ein Host ohne Verbindung bzw. ein Check, dessen letzter Lauf fehlschlug.
func (s *Server) dependencyDown(id string) (string, bool) {
	dep, err := s.store.HostDependsOn(context.Background(), id)
	if err != nil {
		s.log.Error("abhängigkeit lesen", "err", err)
		return "", false
	}
	kind, target, _ := strings.Cut(dep, ":")
	switch kind {
	case "host":
		if !s.online(target) {
			return "abhängig von Host " + s.hostName(target, target) + " (offline)", true
		}
	case "check":
		n, err := strconv.ParseInt(target, 10, 64)
		if err != nil || s.checks == nil {
			return "", false
		}
		if r, ok := s.checks.Last()[n]; ok && !r.OK {
			name := target
			if c, err := s.store.CheckByID(context.Background(), n); err == nil {
				name = c.Name
			}
			return "abhängig von Check " + name + " (fehlgeschlagen)", true
		}
	}
	return "", false
}
