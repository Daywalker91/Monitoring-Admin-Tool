package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const servicesSchema = `
-- Überwachte Dienste je Host (M7): läuft ein Dienst nicht, gibt es Alarm "service".
CREATE TABLE IF NOT EXISTS service_watches (
	host_id TEXT NOT NULL,
	name    TEXT NOT NULL,
	PRIMARY KEY (host_id, name)
);
`

// SaveServices speichert die letzte Dienstliste eines Hosts (vollständiges JSON).
func (s *Store) SaveServices(ctx context.Context, hostID string, raw []byte, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET last_services = ?, last_services_at = ? WHERE id = ?`,
		string(raw), now.Unix(), hostID)
	if err != nil {
		return fmt.Errorf("dienste speichern: %w", err)
	}
	return nil
}

// Services liefert die letzte Dienstliste (nil = noch keine) und ihren Zeitpunkt.
func (s *Store) Services(ctx context.Context, hostID string) (json.RawMessage, int64, error) {
	var raw sql.NullString
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT last_services, last_services_at FROM hosts WHERE id = ?`, hostID).Scan(&raw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("dienste lesen: %w", err)
	}
	if !raw.Valid {
		return nil, 0, nil
	}
	return json.RawMessage(raw.String), at.Int64, nil
}

// ServiceWatches liefert die überwachten Dienste eines Hosts, sortiert.
func (s *Store) ServiceWatches(ctx context.Context, hostID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM service_watches WHERE host_id = ? ORDER BY name`, hostID)
	if err != nil {
		return nil, fmt.Errorf("überwachte dienste lesen: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetServiceWatch schaltet die Überwachung eines Dienstes ein oder aus.
func (s *Store) SetServiceWatch(ctx context.Context, hostID, name string, watch bool) error {
	var err error
	if watch {
		_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO service_watches (host_id, name) VALUES (?, ?)`, hostID, name)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM service_watches WHERE host_id = ? AND name = ?`, hostID, name)
	}
	if err != nil {
		return fmt.Errorf("überwachung speichern: %w", err)
	}
	return nil
}
