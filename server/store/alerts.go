package store

import (
	"context"
	"database/sql"
	"errors"
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
	resolved_at INTEGER,
	name        TEXT NOT NULL DEFAULT '',
	detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_alerts_open ON alerts(resolved_at) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_alerts_started ON alerts(started_at);

-- Abweichende Regeln je Host; fehlende Einträge = globale Regel.
CREATE TABLE IF NOT EXISTS alert_overrides (
	host_id   TEXT NOT NULL,
	kind      TEXT NOT NULL,
	enabled   INTEGER NOT NULL,
	threshold REAL NOT NULL,
	for_sec   INTEGER NOT NULL,
	PRIMARY KEY (host_id, kind)
);

-- Stummgeschaltete Hosts (Wartung): keine Benachrichtigungen bis until (Unix-Sekunden).
CREATE TABLE IF NOT EXISTS host_mutes (
	host_id TEXT PRIMARY KEY,
	until   INTEGER NOT NULL
);

-- Einfache Einstellungen der Alarmauswertung (z. B. reminder_hours).
CREATE TABLE IF NOT EXISTS alert_settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// migrations ergänzt Spalten, die nach M3 hinzugekommen sind.
var migrations = []struct{ table, column, def, fill string }{
	// Zeitpunkt der letzten Benachrichtigung; NULL = nie gemeldet (z. B. stummgeschaltet).
	// Bestehende Alarme gelten als gemeldet, damit sie nach dem Update nicht nachgemeldet werden.
	{"alerts", "notified_at", "INTEGER", "UPDATE alerts SET notified_at = started_at"},
	// CPU-Temperatur in °C (M6); NULL = Host ohne Sensor.
	{"metrics_history", "cpu_temp", "REAL", ""},
	// Alarme ohne Host (Checks, Cluster, M6): Anzeigename und Zusatzinfo (z. B. Fehlermeldung).
	{"alerts", "name", "TEXT NOT NULL DEFAULT ''", ""},
	{"alerts", "detail", "TEXT NOT NULL DEFAULT ''", ""},
	// Anzeigename und Gruppe eines Hosts, in der GUI gesetzt; leer = Hostname bzw. ohne Gruppe.
	{"hosts", "display_name", "TEXT NOT NULL DEFAULT ''", ""},
	{"hosts", "group_name", "TEXT NOT NULL DEFAULT ''", ""},
	// Letzte Dienstliste (M7, JSON) und wann sie kam.
	{"hosts", "last_services", "TEXT", ""},
	{"hosts", "last_services_at", "INTEGER", ""},
	// Update-Stand (M7): vollständige Nachricht und Zusammenfassung für die Host-Liste (JSON).
	{"hosts", "last_updates", "TEXT", ""},
	{"hosts", "updates_summary", "TEXT", ""},
}

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
	HostID     string  `json:"host_id"`          // bei Checks "check:<id>", beim Cluster "k8s"
	Hostname   string  `json:"hostname"`         // Hostname bzw. Name des Checks
	Detail     string  `json:"detail,omitempty"` // Zusatzinfo, z. B. Fehlermeldung eines Checks
	Kind       string  `json:"kind"`
	Target     string  `json:"target"` // z. B. Mountpoint bei "disk"
	Threshold  float64 `json:"threshold"`
	Value      float64 `json:"value"` // Wert beim Auslösen
	Peak       float64 `json:"peak"`  // Spitzenwert während des Alarms
	StartedAt  int64   `json:"started_at"`
	ResolvedAt *int64  `json:"resolved_at"` // nil = aktiv
	NotifiedAt *int64  `json:"notified_at"` // nil = nicht benachrichtigt
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
INSERT INTO alerts (host_id, kind, target, threshold, value, peak, started_at, notified_at, name, detail)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.HostID, a.Kind, a.Target, a.Threshold, a.Value, a.Peak, a.StartedAt, a.NotifiedAt, a.Hostname, a.Detail)
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
SELECT a.id, a.host_id, COALESCE(h.hostname, NULLIF(a.name, ''), a.host_id), a.kind, a.target, a.threshold,
       a.value, a.peak, a.started_at, a.resolved_at, a.notified_at, a.detail
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
		var resolved, notified sql.NullInt64
		if err := rows.Scan(&a.ID, &a.HostID, &a.Hostname, &a.Kind, &a.Target, &a.Threshold, &a.Value,
			&a.Peak, &a.StartedAt, &resolved, &notified, &a.Detail); err != nil {
			return nil, err
		}
		if resolved.Valid {
			a.ResolvedAt = &resolved.Int64
		}
		if notified.Valid {
			a.NotifiedAt = &notified.Int64
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

// SetAlertNotified speichert den Zeitpunkt der letzten Benachrichtigung.
func (s *Store) SetAlertNotified(ctx context.Context, id int64, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE alerts SET notified_at = ? WHERE id = ?`, at.Unix(), id); err != nil {
		return fmt.Errorf("benachrichtigung speichern: %w", err)
	}
	return nil
}

// AlertOverrides liefert alle abweichenden Regeln, gruppiert nach Host.
func (s *Store) AlertOverrides(ctx context.Context) (map[string][]AlertRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_id, kind, enabled, threshold, for_sec FROM alert_overrides ORDER BY host_id, kind`)
	if err != nil {
		return nil, fmt.Errorf("host-regeln lesen: %w", err)
	}
	defer rows.Close()
	out := map[string][]AlertRule{}
	for rows.Next() {
		var host string
		var r AlertRule
		if err := rows.Scan(&host, &r.Kind, &r.Enabled, &r.Threshold, &r.ForSec); err != nil {
			return nil, err
		}
		out[host] = append(out[host], r)
	}
	return out, rows.Err()
}

// SetAlertOverrides ersetzt die abweichenden Regeln eines Hosts (leer = alle global).
func (s *Store) SetAlertOverrides(ctx context.Context, hostID string, rules []AlertRule) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM alert_overrides WHERE host_id = ?`, hostID); err != nil {
		return fmt.Errorf("host-regeln löschen: %w", err)
	}
	for _, r := range rules {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO alert_overrides (host_id, kind, enabled, threshold, for_sec) VALUES (?, ?, ?, ?, ?)`,
			hostID, r.Kind, r.Enabled, r.Threshold, r.ForSec); err != nil {
			return fmt.Errorf("host-regel speichern: %w", err)
		}
	}
	return tx.Commit()
}

// HostMutes liefert die Stummschaltungen (Host → Ende).
func (s *Store) HostMutes(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host_id, until FROM host_mutes`)
	if err != nil {
		return nil, fmt.Errorf("stummschaltungen lesen: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var host string
		var until int64
		if err := rows.Scan(&host, &until); err != nil {
			return nil, err
		}
		out[host] = time.Unix(until, 0)
	}
	return out, rows.Err()
}

// SetHostMute schaltet einen Host bis until stumm; ein Nullwert hebt die Stummschaltung auf.
func (s *Store) SetHostMute(ctx context.Context, hostID string, until time.Time) error {
	var err error
	if until.IsZero() {
		_, err = s.db.ExecContext(ctx, `DELETE FROM host_mutes WHERE host_id = ?`, hostID)
	} else {
		_, err = s.db.ExecContext(ctx, `
INSERT INTO host_mutes (host_id, until) VALUES (?, ?)
ON CONFLICT(host_id) DO UPDATE SET until = excluded.until`, hostID, until.Unix())
	}
	if err != nil {
		return fmt.Errorf("stummschaltung speichern: %w", err)
	}
	return nil
}

// AlertSetting liest eine Einstellung; ok ist false, wenn sie nicht gesetzt ist.
func (s *Store) AlertSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM alert_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("einstellung lesen: %w", err)
	}
	return value, true, nil
}

// SetAlertSetting speichert eine Einstellung.
func (s *Store) SetAlertSetting(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO alert_settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("einstellung speichern: %w", err)
	}
	return nil
}

// HostExists meldet, ob ein Host bekannt ist.
func (s *Store) HostExists(ctx context.Context, hostID string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hosts WHERE id = ?`, hostID).Scan(&n); err != nil {
		return false, fmt.Errorf("host prüfen: %w", err)
	}
	return n > 0, nil
}

// migrate ergänzt fehlende Spalten in bestehenden Datenbanken.
func migrate(db *sql.DB) error {
	for _, m := range migrations {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		// Tabellen- und Spaltennamen stammen aus der festen Liste oben, nicht aus Eingaben.
		if _, err := db.Exec(`ALTER TABLE ` + m.table + ` ADD COLUMN ` + m.column + ` ` + m.def); err != nil {
			return fmt.Errorf("spalte %s.%s ergänzen: %w", m.table, m.column, err)
		}
		if m.fill != "" {
			if _, err := db.Exec(m.fill); err != nil {
				return fmt.Errorf("spalte %s.%s füllen: %w", m.table, m.column, err)
			}
		}
	}
	return nil
}
