// Package store speichert Hosts, den letzten Messstand und eine kompakte
// Historie in SQLite (modernc.org/sqlite, ohne CGO).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

const schema = `
CREATE TABLE IF NOT EXISTS hosts (
	id               TEXT PRIMARY KEY,
	hostname         TEXT NOT NULL,
	os               TEXT NOT NULL DEFAULT '',
	platform         TEXT NOT NULL DEFAULT '',
	platform_version TEXT NOT NULL DEFAULT '',
	kernel_version   TEXT NOT NULL DEFAULT '',
	arch             TEXT NOT NULL DEFAULT '',
	agent_version    TEXT NOT NULL DEFAULT '',
	cpu_cores        INTEGER NOT NULL DEFAULT 0,
	boot_time        INTEGER NOT NULL DEFAULT 0,
	interval_sec     INTEGER NOT NULL DEFAULT 0,
	capabilities     TEXT NOT NULL DEFAULT '[]',
	remote_addr      TEXT NOT NULL DEFAULT '',
	first_seen       INTEGER NOT NULL,
	last_seen        INTEGER NOT NULL,
	last_metrics     TEXT,
	last_processes   TEXT,
	display_name     TEXT NOT NULL DEFAULT '',
	group_name       TEXT NOT NULL DEFAULT '',
	last_services    TEXT,
	last_services_at INTEGER,
	last_updates     TEXT,
	updates_summary  TEXT,
	log_sources      TEXT NOT NULL DEFAULT '[]',
	last_smart       TEXT
);

-- Kompakte Historie: nur Kennzahlen, kein vollständiges JSON (spart Platz).
CREATE TABLE IF NOT EXISTS metrics_history (
	host_id      TEXT NOT NULL,
	ts           INTEGER NOT NULL,
	cpu_pct      REAL NOT NULL,
	load1        REAL NOT NULL,
	mem_pct      REAL NOT NULL,
	swap_pct     REAL NOT NULL,
	net_rx_bps   REAL NOT NULL,
	net_tx_bps   REAL NOT NULL,
	disk_max_pct REAL NOT NULL,
	cpu_temp     REAL
);
CREATE INDEX IF NOT EXISTS idx_history_host_ts ON metrics_history(host_id, ts);
CREATE INDEX IF NOT EXISTS idx_history_ts ON metrics_history(ts);
`

// Store kapselt die Datenbank.
type Store struct {
	db *sql.DB
}

// Open öffnet (bzw. erstellt) die Datenbank unter path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("datenverzeichnis anlegen: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("datenbank öffnen: %w", err)
	}
	// SQLite verträgt nur einen Schreiber; eine Verbindung vermeidet SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + usersSchema + alertsSchema + checksSchema + jobsSchema + servicesSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema anlegen: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("datenbank migrieren: %w", err)
	}
	return &Store{db: db}, nil
}

// Close schließt die Datenbank.
func (s *Store) Close() error { return s.db.Close() }

// UpsertHost legt einen Host an oder aktualisiert seine Stammdaten.
func (s *Store) UpsertHost(ctx context.Context, h proto.Hello, remoteAddr string, now time.Time) error {
	caps, err := json.Marshal(h.Capabilities)
	if err != nil {
		return err
	}
	sources, err := json.Marshal(h.LogSources)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO hosts (id, hostname, os, platform, platform_version, kernel_version, arch, agent_version,
                   cpu_cores, boot_time, interval_sec, capabilities, remote_addr, first_seen, last_seen, log_sources)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	hostname = excluded.hostname, os = excluded.os, platform = excluded.platform,
	platform_version = excluded.platform_version, kernel_version = excluded.kernel_version,
	arch = excluded.arch, agent_version = excluded.agent_version, cpu_cores = excluded.cpu_cores,
	boot_time = excluded.boot_time, interval_sec = excluded.interval_sec,
	capabilities = excluded.capabilities, remote_addr = excluded.remote_addr, last_seen = excluded.last_seen,
	log_sources = excluded.log_sources`,
		h.AgentID, h.Hostname, h.OS, h.Platform, h.PlatformVer, h.KernelVer, h.Arch, h.AgentVersion,
		h.CPUCores, h.BootTime, h.IntervalSec, string(caps), remoteAddr, now.Unix(), now.Unix(), string(sources))
	if err != nil {
		return fmt.Errorf("host speichern: %w", err)
	}
	return nil
}

// SaveMetrics speichert den letzten Stand (vollständiges JSON) und einen Historienpunkt.
func (s *Store) SaveMetrics(ctx context.Context, hostID string, m proto.Metrics, raw []byte, now time.Time) error {
	var rx, tx, diskMax float64
	for _, n := range m.Net {
		rx += n.RxBps
		tx += n.TxBps
	}
	for _, d := range m.Disks {
		diskMax = max(diskMax, d.UsedPct)
	}

	tx2, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx2.Rollback()

	if _, err := tx2.ExecContext(ctx, `UPDATE hosts SET last_metrics = ?, last_seen = ? WHERE id = ?`,
		string(raw), now.Unix(), hostID); err != nil {
		return fmt.Errorf("letzten stand speichern: %w", err)
	}
	if _, err := tx2.ExecContext(ctx, `
INSERT INTO metrics_history (host_id, ts, cpu_pct, load1, mem_pct, swap_pct, net_rx_bps, net_tx_bps, disk_max_pct, cpu_temp)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hostID, now.Unix(), m.CPU.TotalPct, m.CPU.Load1, m.Mem.UsedPct, m.Swap.UsedPct, rx, tx, diskMax, m.CPUTemp); err != nil {
		return fmt.Errorf("historie speichern: %w", err)
	}
	return tx2.Commit()
}

// SaveProcesses speichert die letzten Top-Prozesse.
func (s *Store) SaveProcesses(ctx context.Context, hostID string, raw []byte, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE hosts SET last_processes = ?, last_seen = ? WHERE id = ?`,
		string(raw), now.Unix(), hostID)
	if err != nil {
		return fmt.Errorf("prozesse speichern: %w", err)
	}
	return nil
}

// Host ist ein Eintrag der Host-Liste.
type Host struct {
	ID              string             `json:"id"`
	Hostname        string             `json:"hostname"`
	OS              string             `json:"os"`
	Platform        string             `json:"platform"`
	PlatformVersion string             `json:"platform_version"`
	KernelVersion   string             `json:"kernel_version"`
	Arch            string             `json:"arch"`
	AgentVersion    string             `json:"agent_version"`
	CPUCores        int                `json:"cpu_cores"`
	BootTime        int64              `json:"boot_time"`
	IntervalSec     int                `json:"interval_sec"`
	Capabilities    []proto.Capability `json:"capabilities"`
	RemoteAddr      string             `json:"remote_addr"`
	FirstSeen       int64              `json:"first_seen"`
	LastSeen        int64              `json:"last_seen"`
	Metrics         json.RawMessage    `json:"metrics"`
	Processes       json.RawMessage    `json:"processes"`
	DisplayName     string             `json:"display_name"` // leer = Hostname
	Group           string             `json:"group"`        // leer = ohne Gruppe
	Updates         *UpdateSummary     `json:"updates"`      // nil = noch kein Update-Stand
	LogSources      []string           `json:"log_sources"`  // lesbare Logs (agent.json)
}

// Name liefert den Anzeigenamen, ersatzweise den gemeldeten Hostnamen.
func (h Host) Name() string {
	if h.DisplayName != "" {
		return h.DisplayName
	}
	return h.Hostname
}

// ListHosts liefert alle Hosts mit dem letzten Stand, sortiert nach Hostname.
func (s *Store) ListHosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, hostname, os, platform, platform_version, kernel_version, arch, agent_version, cpu_cores,
       boot_time, interval_sec, capabilities, remote_addr, first_seen, last_seen,
       COALESCE(last_metrics, 'null'), COALESCE(last_processes, 'null'), display_name, group_name,
       COALESCE(updates_summary, 'null'), log_sources
FROM hosts ORDER BY CASE display_name WHEN '' THEN hostname ELSE display_name END COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("hosts lesen: %w", err)
	}
	defer rows.Close()

	hosts := []Host{}
	for rows.Next() {
		var h Host
		var caps, metrics, procs, updates, sources string
		if err := rows.Scan(&h.ID, &h.Hostname, &h.OS, &h.Platform, &h.PlatformVersion, &h.KernelVersion,
			&h.Arch, &h.AgentVersion, &h.CPUCores, &h.BootTime, &h.IntervalSec, &caps, &h.RemoteAddr,
			&h.FirstSeen, &h.LastSeen, &metrics, &procs, &h.DisplayName, &h.Group, &updates, &sources); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(caps), &h.Capabilities); err != nil {
			h.Capabilities = nil
		}
		h.Metrics = json.RawMessage(metrics)
		h.Processes = json.RawMessage(procs)
		if err := json.Unmarshal([]byte(sources), &h.LogSources); err != nil {
			h.LogSources = nil
		}
		if updates != "null" && json.Unmarshal([]byte(updates), &h.Updates) != nil {
			h.Updates = nil
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}

// SetHostMeta setzt Anzeigename und Gruppe eines Hosts (leer = zurücksetzen).
// found meldet, ob es den Host gibt.
func (s *Store) SetHostMeta(ctx context.Context, hostID, displayName, group string) (found bool, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE hosts SET display_name = ?, group_name = ? WHERE id = ?`,
		displayName, group, hostID)
	if err != nil {
		return false, fmt.Errorf("host-angaben speichern: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// HostDisplayName liefert den Anzeigenamen eines Hosts (leer = keiner gesetzt).
func (s *Store) HostDisplayName(ctx context.Context, hostID string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT display_name FROM hosts WHERE id = ?`, hostID).Scan(&name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("anzeigename lesen: %w", err)
	}
	return name, nil
}

// HistoryPoint ist ein Punkt der kompakten Historie.
type HistoryPoint struct {
	TS         int64    `json:"ts"`
	CPUPct     float64  `json:"cpu_pct"`
	Load1      float64  `json:"load1"`
	MemPct     float64  `json:"mem_pct"`
	SwapPct    float64  `json:"swap_pct"`
	NetRxBps   float64  `json:"net_rx_bps"`
	NetTxBps   float64  `json:"net_tx_bps"`
	DiskMaxPct float64  `json:"disk_max_pct"`
	CPUTemp    *float64 `json:"cpu_temp"` // °C; nil = kein Sensor
}

// History liefert die Historie eines Hosts ab since. Bei bucket > 1 s werden
// die Punkte zu Mittelwerten je Zeitfenster verdichtet; ts ist dann der Beginn
// des Fensters.
func (s *Store) History(ctx context.Context, hostID string, since time.Time, bucket time.Duration) ([]HistoryPoint, error) {
	b := int64(bucket / time.Second)
	if b < 1 {
		b = 1
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT (ts / ?1) * ?1 AS t, AVG(cpu_pct), AVG(load1), AVG(mem_pct), AVG(swap_pct),
       AVG(net_rx_bps), AVG(net_tx_bps), MAX(disk_max_pct), AVG(cpu_temp)
FROM metrics_history WHERE host_id = ?2 AND ts >= ?3
GROUP BY t ORDER BY t`, b, hostID, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("historie lesen: %w", err)
	}
	defer rows.Close()

	points := []HistoryPoint{}
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.TS, &p.CPUPct, &p.Load1, &p.MemPct, &p.SwapPct,
			&p.NetRxBps, &p.NetTxBps, &p.DiskMaxPct, &p.CPUTemp); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}

// DeleteHost entfernt einen Host samt Historie und Alarmen. found meldet, ob es ihn gab.
func (s *Store) DeleteHost(ctx context.Context, hostID string) (found bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM metrics_history WHERE host_id = ?`, hostID); err != nil {
		return false, fmt.Errorf("historie löschen: %w", err)
	}
	for _, table := range []string{"alerts", "alert_overrides", "host_mutes", "jobs", "service_watches"} { // feste Liste
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE host_id = ?`, hostID); err != nil {
			return false, fmt.Errorf("%s löschen: %w", table, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM hosts WHERE id = ?`, hostID)
	if err != nil {
		return false, fmt.Errorf("host löschen: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// Prune löscht Historienpunkte und beendete Alarme, die älter als before sind,
// und liefert die Anzahl der gelöschten Historienpunkte.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM metrics_history WHERE ts < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("historie bereinigen: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE resolved_at < ?`, before.Unix()); err != nil {
		return 0, fmt.Errorf("alarme bereinigen: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM check_results WHERE ts < ?`, before.Unix()); err != nil {
		return 0, fmt.Errorf("check-ergebnisse bereinigen: %w", err)
	}
	return res.RowsAffected()
}
