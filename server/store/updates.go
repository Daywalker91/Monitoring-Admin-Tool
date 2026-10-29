package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// UpdateSummary ist der Update-Stand eines Hosts ohne Paketliste (für die Host-Liste).
type UpdateSummary struct {
	Manager        string `json:"manager,omitempty"`
	Upgradable     int    `json:"upgradable"`
	Security       int    `json:"security"`
	RebootRequired bool   `json:"reboot_required"`
	Error          string `json:"error,omitempty"`
	At             int64  `json:"at"` // Eingang beim Server
}

// SaveUpdates speichert den Update-Stand: die Zusammenfassung für die Host-Liste
// und die vollständige Nachricht (Paketliste) für die Details.
func (s *Store) SaveUpdates(ctx context.Context, hostID string, u proto.Updates, raw []byte, now time.Time) error {
	sum, err := json.Marshal(UpdateSummary{Manager: u.Manager, Upgradable: u.Upgradable, Security: u.Security,
		RebootRequired: u.RebootRequired, Error: u.Error, At: now.Unix()})
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE hosts SET last_updates = ?, updates_summary = ? WHERE id = ?`,
		string(raw), string(sum), hostID); err != nil {
		return fmt.Errorf("update-stand speichern: %w", err)
	}
	return nil
}

// Updates liefert die letzte Updates-Nachricht eines Hosts (nil = noch keine).
func (s *Store) Updates(ctx context.Context, hostID string) (json.RawMessage, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT last_updates FROM hosts WHERE id = ?`, hostID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update-stand lesen: %w", err)
	}
	if !raw.Valid {
		return nil, nil
	}
	return json.RawMessage(raw.String), nil
}
