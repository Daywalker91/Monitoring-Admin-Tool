package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const alertsSchema = `
-- Schwellwerte je Regeltyp. Fehlende Typen ergänzt der Server mit Standardwerten.
CREATE TABLE IF NOT EXISTS alert_rules (
	kind      TEXT PRIMARY KEY,
	enabled   INTEGER NOT NULL,
	threshold REAL NOT NULL,
	for_sec   INTEGER NOT NULL
);

-- Alarmverlauf. Offene Alarme haben resolved_at = NULL.
CREATE TABLE IF NOT EXISTS alerts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	host_id     TEXT NOT NULL,
	kind        TEXT NOT NULL,
	target      TEXT NOT NULL DEFAULT '',
	threshold   REAL NOT NULL,
	value       REAL NOT NULL,
	peak        REAL NOT NULL,
	started_at  INTEGER NOT NULL,
	resolved_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_alerts_open ON alerts(resolved_at) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_alerts_started ON alerts(started_at);
`

// AlertRule ist der Schwellwert eines Regeltyps.
type AlertRule struct {
	Kind      string  `json:"kind"`
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"` // Prozent; bei "offline" ohne Bedeutung
	ForSec    int     `json:"for_sec"`   // Mindestdauer, bevor der Alarm auslöst
}

// Alert ist ein ausgelöster Alarm.
type Alert struct {
	ID         int64   `json:"id"`
	HostID     string  `json:"host_id"`
	Hostname   string  `json:"hostname"`
	Kind       string  `json:"kind"`
	Target     string  `json:"target"` // z. B. Mountpoint bei "disk"
	Threshold  float64 `json:"threshold"`
	Value      float64 `json:"value"` // Wert beim Auslösen
	Peak       float64 `json:"peak"`  // Spitzenwert während des Alarms
	StartedAt  int64   `json:"started_at"`
	ResolvedAt *int64  `json:"resolved_at"` // nil = aktiv
}

// AlertRules liefert die gespeicherten Regeln.
func (s *Store) AlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, enabled, threshold, for_sec FROM alert_rules ORDER BY kind`)
	if err != nil {
		return nil, fmt.Errorf("alarmregeln lesen: %w", err)
	}
	defer rows.Close()
	rules := []AlertRule{}
	for rows.Next() {
		var r AlertRule
		if err := rows.Scan(&r.Kind, &r.Enabled, &r.Threshold, &r.ForSec); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// SaveAlertRules speichert die Regeln (anlegen oder überschreiben).
func (s *Store) SaveAlertRules(ctx context.Context, rules []AlertRule) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rules {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO alert_rules (kind, enabled, threshold, for_sec) VALUES (?, ?, ?, ?)
ON CONFLICT(kind) DO UPDATE SET enabled = excluded.enabled, threshold = excluded.threshold,
	for_sec = excluded.for_sec`, r.Kind, r.Enabled, r.Threshold, r.ForSec); err != nil {
			return fmt.Errorf("alarmregel speichern: %w", err)
		}
	}
	return tx.Commit()
}

// OpenAlert legt einen aktiven Alarm an und setzt a.ID.
func (s *Store) OpenAlert(ctx context.Context, a *Alert) error {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO alerts (host_id, kind, target, threshold, value, peak, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.HostID, a.Kind, a.Target, a.Threshold, a.Value, a.Peak, a.StartedAt)
	if err != nil {
		return fmt.Errorf("alarm speichern: %w", err)
	}
	a.ID, err = res.LastInsertId()
	return err
}

// UpdateAlertPeak speichert einen neuen Spitzenwert.
func (s *Store) UpdateAlertPeak(ctx context.Context, id int64, peak float64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE alerts SET peak = ? WHERE id = ?`, peak, id); err != nil {
		return fmt.Errorf("spitzenwert speichern: %w", err)
	}
	return nil
}

// ResolveAlert beendet einen Alarm.
func (s *Store) ResolveAlert(ctx context.Context, id int64, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE alerts SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL`,
		at.Unix(), id); err != nil {
		return fmt.Errorf("alarm beenden: %w", err)
	}
	return nil
}

// Alerts liefert Alarme, neueste zuerst. activeOnly beschränkt auf offene
// Alarme, sonst kommen offene sowie seit since beendete Alarme.
func (s *Store) Alerts(ctx context.Context, activeOnly bool, since time.Time, limit int) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id, a.host_id, COALESCE(h.hostname, a.host_id), a.kind, a.target, a.threshold, a.value, a.peak,
       a.started_at, a.resolved_at
FROM alerts a LEFT JOIN hosts h ON h.id = a.host_id
WHERE a.resolved_at IS NULL OR (?1 = 0 AND a.resolved_at >= ?2)
ORDER BY a.resolved_at IS NOT NULL, a.started_at DESC, a.id DESC
LIMIT ?3`, activeOnly, since.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("alarme lesen: %w", err)
	}
	defer rows.Close()
	alerts := []Alert{}
	for rows.Next() {
		var a Alert
		var resolved sql.NullInt64
		if err := rows.Scan(&a.ID, &a.HostID, &a.Hostname, &a.Kind, &a.Target, &a.Threshold, &a.Value,
			&a.Peak, &a.StartedAt, &resolved); err != nil {
			return nil, err
		}
		if resolved.Valid {
			a.ResolvedAt = &resolved.Int64
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}
