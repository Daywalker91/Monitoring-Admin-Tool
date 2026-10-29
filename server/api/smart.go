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

// observeSmart speichert einen SMART-Stand und wertet den Alarm "smart" je
// Datenträger aus. Ist der Scan fehlgeschlagen (Error), bleiben bestehende
// Alarme unverändert – ein vorübergehender Fehler beendet sie nicht.
func (s *Server) observeSmart(ctx context.Context, id, hostname string, sm proto.Smart, raw []byte, now time.Time) {
	if err := s.store.SaveSmart(ctx, id, raw); err != nil {
		s.log.Error("smart speichern", "agent_id", id, "err", err)
	}
	if s.alerts == nil || sm.Error != "" {
		return
	}
	conds := make(map[string]alert.Condition, len(sm.Disks))
	for _, d := range sm.Disks {
		problem := d.Problem()
		conds[d.Device] = alert.Condition{Bad: problem != "", Detail: problem}
	}
	s.alerts.ObserveSet(ctx, id, hostname, alert.KindSmart, conds, now)
}

// handleSmart liefert den letzten SMART-Stand eines Hosts (null = noch keiner).
func (s *Server) handleSmart(w http.ResponseWriter, r *http.Request) {
	raw, err := s.store.Smart(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("smart lesen", "err", err)
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
