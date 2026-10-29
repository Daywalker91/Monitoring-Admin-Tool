package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const checksSchema = `
-- Externe Checks (M6): HTTP(S), Ping, TCP. Angelegt in der GUI.
CREATE TABLE IF NOT EXISTS checks (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT NOT NULL,
	kind          TEXT NOT NULL,
	target        TEXT NOT NULL,
	interval_sec  INTEGER NOT NULL,
	timeout_sec   INTEGER NOT NULL,
	expect_status INTEGER NOT NULL DEFAULT 0,
	skip_verify   INTEGER NOT NULL DEFAULT 0,
	enabled       INTEGER NOT NULL DEFAULT 1,
	created_at    INTEGER NOT NULL
);

-- Ergebnisse je Durchlauf (Retention wie die Host-Historie).
CREATE TABLE IF NOT EXISTS check_results (
	check_id       INTEGER NOT NULL,
	ts             INTEGER NOT NULL,
	ok             INTEGER NOT NULL,
	latency_ms     REAL,
	status         INTEGER NOT NULL DEFAULT 0,
	cert_not_after INTEGER,
	error          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_check_results ON check_results(check_id, ts);
CREATE INDEX IF NOT EXISTS idx_check_results_ts ON check_results(ts);
`

// Check ist ein externer Check.
type Check struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`   // "http", "ping" oder "tcp"
	Target       string `json:"target"` // URL bzw. Host oder Host:Port
	IntervalSec  int    `json:"interval_sec"`
	TimeoutSec   int    `json:"timeout_sec"`
	ExpectStatus int    `json:"expect_status"` // nur http; 0 = 200–399
	SkipVerify   bool   `json:"skip_verify"`   // nur https: Zertifikat nicht prüfen (selbst signiert)
	Enabled      bool   `json:"enabled"`
	CreatedAt    int64  `json:"created_at"`
}

// CheckResult ist das Ergebnis eines Durchlaufs.
type CheckResult struct {
	TS           int64    `json:"ts"`
	OK           bool     `json:"ok"`
	LatencyMS    *float64 `json:"latency_ms"`               // nil = keine Antwort
	Status       int      `json:"status,omitempty"`         // HTTP-Status
	CertNotAfter *int64   `json:"cert_not_after,omitempty"` // Ablauf des Zertifikats (https)
	Error        string   `json:"error,omitempty"`
}

const checkCols = `id, name, kind, target, interval_sec, timeout_sec, expect_status, skip_verify, enabled, created_at`

func scanCheck(sc interface{ Scan(...any) error }) (Check, error) {
	var c Check
	err := sc.Scan(&c.ID, &c.Name, &c.Kind, &c.Target, &c.IntervalSec, &c.TimeoutSec, &c.ExpectStatus,
		&c.SkipVerify, &c.Enabled, &c.CreatedAt)
	return c, err
}

// Checks liefert alle Checks, sortiert nach Name.
func (s *Store) Checks(ctx context.Context) ([]Check, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+checkCols+` FROM checks ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("checks lesen: %w", err)
	}
	defer rows.Close()
	out := []Check{}
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CheckByID liefert einen Check oder ErrNotFound.
func (s *Store) CheckByID(ctx context.Context, id int64) (Check, error) {
	c, err := scanCheck(s.db.QueryRowContext(ctx, `SELECT `+checkCols+` FROM checks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Check{}, ErrNotFound
	}
	if err != nil {
		return Check{}, fmt.Errorf("check lesen: %w", err)
	}
	return c, nil
}

// CountChecks liefert die Zahl der Checks.
func (s *Store) CountChecks(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checks`).Scan(&n); err != nil {
		return 0, fmt.Errorf("checks zählen: %w", err)
	}
	return n, nil
}

// CreateCheck legt einen Check an und setzt c.ID.
func (s *Store) CreateCheck(ctx context.Context, c *Check) error {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO checks (name, kind, target, interval_sec, timeout_sec, expect_status, skip_verify, enabled, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Kind, c.Target, c.IntervalSec, c.TimeoutSec, c.ExpectStatus, c.SkipVerify, c.Enabled, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("check anlegen: %w", err)
	}
	c.ID, err = res.LastInsertId()
	return err
}

// UpdateCheck ändert einen Check (ohne created_at).
func (s *Store) UpdateCheck(ctx context.Context, c Check) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE checks SET name = ?, kind = ?, target = ?, interval_sec = ?, timeout_sec = ?, expect_status = ?,
                  skip_verify = ?, enabled = ?
WHERE id = ?`,
		c.Name, c.Kind, c.Target, c.IntervalSec, c.TimeoutSec, c.ExpectStatus, c.SkipVerify, c.Enabled, c.ID)
	if err != nil {
		return fmt.Errorf("check ändern: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCheck entfernt einen Check samt Ergebnissen und Alarmen (alarmID ist
// die Kennung, unter der seine Alarme geführt werden).
func (s *Store) DeleteCheck(ctx context.Context, id int64, alarmID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM checks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("check löschen: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM check_results WHERE check_id = ?`, id); err != nil {
		return fmt.Errorf("check-ergebnisse löschen: %w", err)
	}
	for _, table := range []string{"alerts", "alert_overrides", "host_mutes"} { // feste Liste
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE host_id = ?`, alarmID); err != nil {
			return fmt.Errorf("%s löschen: %w", table, err)
		}
	}
	return tx.Commit()
}

// SaveCheckResult speichert das Ergebnis eines Durchlaufs.
func (s *Store) SaveCheckResult(ctx context.Context, id int64, r CheckResult) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO check_results (check_id, ts, ok, latency_ms, status, cert_not_after, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, r.TS, r.OK, r.LatencyMS, r.Status, r.CertNotAfter, r.Error)
	if err != nil {
		return fmt.Errorf("check-ergebnis speichern: %w", err)
	}
	return nil
}

// LastCheckResults liefert das jeweils letzte Ergebnis je Check.
func (s *Store) LastCheckResults(ctx context.Context) (map[int64]CheckResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT r.check_id, r.ts, r.ok, r.latency_ms, r.status, r.cert_not_after, r.error
FROM check_results r
JOIN (SELECT check_id, MAX(ts) AS ts FROM check_results GROUP BY check_id) l
  ON l.check_id = r.check_id AND l.ts = r.ts`)
	if err != nil {
		return nil, fmt.Errorf("letzte check-ergebnisse lesen: %w", err)
	}
	defer rows.Close()
	out := map[int64]CheckResult{}
	for rows.Next() {
		var id int64
		var r CheckResult
		if err := rows.Scan(&id, &r.TS, &r.OK, &r.LatencyMS, &r.Status, &r.CertNotAfter, &r.Error); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// CheckPoint ist ein Punkt im Verlauf eines Checks.
type CheckPoint struct {
	TS        int64    `json:"ts"`
	LatencyMS *float64 `json:"latency_ms"` // Mittelwert erfolgreicher Durchläufe; nil = keiner
	OKPct     float64  `json:"ok_pct"`     // Anteil erfolgreicher Durchläufe in Prozent
}

// CheckHistory liefert den Verlauf eines Checks ab since, verdichtet auf bucket.
func (s *Store) CheckHistory(ctx context.Context, id int64, since time.Time, bucket time.Duration) ([]CheckPoint, error) {
	b := max(int64(bucket/time.Second), 1)
	rows, err := s.db.QueryContext(ctx, `
SELECT (ts / ?1) * ?1 AS t, AVG(CASE WHEN ok THEN latency_ms END), AVG(ok) * 100
FROM check_results WHERE check_id = ?2 AND ts >= ?3
GROUP BY t ORDER BY t`, b, id, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("check-verlauf lesen: %w", err)
	}
	defer rows.Close()
	out := []CheckPoint{}
	for rows.Next() {
		var p CheckPoint
		if err := rows.Scan(&p.TS, &p.LatencyMS, &p.OKPct); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CheckUptime liefert je Check den Anteil erfolgreicher Durchläufe seit since in Prozent.
func (s *Store) CheckUptime(ctx context.Context, since time.Time) (map[int64]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT check_id, AVG(ok) * 100 FROM check_results WHERE ts >= ? GROUP BY check_id`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("verfügbarkeit lesen: %w", err)
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var pct float64
		if err := rows.Scan(&id, &pct); err != nil {
			return nil, err
		}
		out[id] = pct
	}
	return out, rows.Err()
}
