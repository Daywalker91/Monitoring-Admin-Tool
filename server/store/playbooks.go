package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Playbook-Läufe (M12a). Der Lauf hält eine Kopie des Playbooks (source), damit
// eine geänderte Datei einen laufenden Lauf nicht beeinflusst. Nach einem
// Neustart des Servers geht es mit diesem Stand weiter (wie node_ops).
const playbooksSchema = `
CREATE TABLE IF NOT EXISTS playbook_runs (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	source      TEXT NOT NULL,
	username    TEXT NOT NULL DEFAULT '',
	remote      TEXT NOT NULL DEFAULT '',
	hosts       TEXT NOT NULL DEFAULT '[]',
	status      TEXT NOT NULL,
	cancel      INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	finished_at INTEGER,
	message     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_playbook_runs_created ON playbook_runs(created_at);
CREATE TABLE IF NOT EXISTS playbook_steps (
	run_id      TEXT NOT NULL REFERENCES playbook_runs(id) ON DELETE CASCADE,
	host_id     TEXT NOT NULL,
	idx         INTEGER NOT NULL,
	step_id     TEXT NOT NULL,
	status      TEXT NOT NULL,
	job_id      TEXT NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	ok          INTEGER NOT NULL DEFAULT 0,
	exit_code   INTEGER NOT NULL DEFAULT 0,
	message     TEXT NOT NULL DEFAULT '',
	until       INTEGER NOT NULL DEFAULT 0,
	boot_time   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, host_id, idx)
);
-- Zeitpläne (M12c): letzter behandelter Termin und Pause je Playbook-Name.
CREATE TABLE IF NOT EXISTS playbook_schedules (
	name     TEXT PRIMARY KEY,
	last_due INTEGER NOT NULL DEFAULT 0,
	paused   INTEGER NOT NULL DEFAULT 0
);
`

// Status eines Laufs.
const (
	RunRunning     = "running"
	RunOK          = "ok"
	RunFailed      = "failed"      // fertig, aber mit Fehlern (on_error: continue)
	RunStopped     = "stopped"     // angehalten nach einem Fehler (on_error: stop)
	RunCancelled   = "cancelled"   // vom Benutzer abgebrochen
	RunInterrupted = "interrupted" // nach Neustart nicht sicher fortsetzbar
)

// Status eines Schritts auf einem Host.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepOK      = "ok"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// PlaybookRun ist ein Lauf; Steps nur bei Einzelabfrage und aktiven Läufen.
type PlaybookRun struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Source     string         `json:"-"`
	User       string         `json:"user"`
	Remote     string         `json:"-"`
	Hosts      []string       `json:"hosts"`
	Status     string         `json:"status"`
	Cancel     bool           `json:"cancel"`
	CreatedAt  int64          `json:"created_at"`
	FinishedAt *int64         `json:"finished_at"`
	Message    string         `json:"message,omitempty"`
	Steps      []PlaybookStep `json:"steps,omitempty"`
}

// PlaybookStep ist der Stand eines Schritts auf einem Host.
type PlaybookStep struct {
	RunID      string `json:"-"`
	HostID     string `json:"host_id"`
	Index      int    `json:"index"`
	StepID     string `json:"step_id"`
	Status     string `json:"status"`
	JobID      string `json:"job_id,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exit_code"`
	Message    string `json:"message,omitempty"`
	Until      int64  `json:"until,omitempty"` // Wartezeit bzw. Frist für die Rückkehr nach einem Neustart
	BootTime   int64  `json:"-"`               // Boot-Zeit vor einem Neustart
}

// CreatePlaybookRun legt einen Lauf samt aller Schritte (Status pending) an.
func (s *Store) CreatePlaybookRun(ctx context.Context, r PlaybookRun, steps []PlaybookStep) error {
	hosts, err := json.Marshal(r.Hosts)
	if err != nil {
		return fmt.Errorf("lauf-hosts: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lauf anlegen: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO playbook_runs (id, name, source, username, remote, hosts, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Name, r.Source, r.User, r.Remote, string(hosts), r.Status, r.CreatedAt); err != nil {
		return fmt.Errorf("lauf anlegen: %w", err)
	}
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, `INSERT INTO playbook_steps (run_id, host_id, idx, step_id, status) VALUES (?, ?, ?, ?, ?)`,
			r.ID, st.HostID, st.Index, st.StepID, st.Status); err != nil {
			return fmt.Errorf("lauf-schritt anlegen: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lauf anlegen: %w", err)
	}
	return nil
}

// SavePlaybookStep schreibt den Stand eines Schritts fort.
func (s *Store) SavePlaybookStep(ctx context.Context, st PlaybookStep) error {
	_, err := s.db.ExecContext(ctx, `UPDATE playbook_steps SET status = ?, job_id = ?, started_at = ?, finished_at = ?,
		ok = ?, exit_code = ?, message = ?, until = ?, boot_time = ? WHERE run_id = ? AND host_id = ? AND idx = ?`,
		st.Status, st.JobID, st.StartedAt, st.FinishedAt, st.OK, st.ExitCode, st.Message, st.Until, st.BootTime,
		st.RunID, st.HostID, st.Index)
	if err != nil {
		return fmt.Errorf("lauf-schritt speichern: %w", err)
	}
	return nil
}

// FinishPlaybookRun beendet einen Lauf. Nicht begonnene Schritte werden als
// übersprungen markiert.
func (s *Store) FinishPlaybookRun(ctx context.Context, id, status, message string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lauf beenden: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE playbook_runs SET status = ?, message = ?, finished_at = ? WHERE id = ?`,
		status, message, at.Unix(), id); err != nil {
		return fmt.Errorf("lauf beenden: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE playbook_steps SET status = ?, message = 'nicht ausgeführt'
		WHERE run_id = ? AND status = ?`, StepSkipped, id, StepPending); err != nil {
		return fmt.Errorf("lauf beenden: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lauf beenden: %w", err)
	}
	return nil
}

// CancelPlaybookRun merkt den Abbruch eines laufenden Laufs vor. found ist
// false, wenn es keinen laufenden Lauf mit dieser ID gibt.
func (s *Store) CancelPlaybookRun(ctx context.Context, id string) (found bool, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE playbook_runs SET cancel = 1 WHERE id = ? AND finished_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("lauf abbrechen: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const runColumns = `id, name, source, username, hosts, status, cancel, created_at, finished_at, message`

func scanRun(sc interface{ Scan(...any) error }) (PlaybookRun, error) {
	var r PlaybookRun
	var hosts string
	var finished sql.NullInt64
	if err := sc.Scan(&r.ID, &r.Name, &r.Source, &r.User, &hosts, &r.Status, &r.Cancel, &r.CreatedAt, &finished, &r.Message); err != nil {
		return r, err
	}
	if finished.Valid {
		r.FinishedAt = &finished.Int64
	}
	if err := json.Unmarshal([]byte(hosts), &r.Hosts); err != nil {
		return r, fmt.Errorf("lauf-hosts lesen: %w", err)
	}
	return r, nil
}

// PlaybookRun liefert einen Lauf samt Schritten oder ErrNotFound.
func (s *Store) PlaybookRun(ctx context.Context, id string) (PlaybookRun, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM playbook_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("lauf lesen: %w", err)
	}
	r.Steps, err = s.playbookSteps(ctx, id)
	return r, err
}

// PlaybookRuns liefert die neuesten Läufe ohne Schritte, neueste zuerst.
func (s *Store) PlaybookRuns(ctx context.Context, limit int) ([]PlaybookRun, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM playbook_runs ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
}

// ActivePlaybookRuns liefert alle nicht beendeten Läufe samt Schritten.
func (s *Store) ActivePlaybookRuns(ctx context.Context) ([]PlaybookRun, error) {
	runs, err := s.queryRuns(ctx, `SELECT `+runColumns+` FROM playbook_runs WHERE finished_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	for i := range runs {
		if runs[i].Steps, err = s.playbookSteps(ctx, runs[i].ID); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func (s *Store) queryRuns(ctx context.Context, query string, args ...any) ([]PlaybookRun, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("läufe lesen: %w", err)
	}
	defer rows.Close()
	out := []PlaybookRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("läufe lesen: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) playbookSteps(ctx context.Context, runID string) ([]PlaybookStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host_id, idx, step_id, status, job_id, started_at, finished_at, ok, exit_code,
		message, until, boot_time FROM playbook_steps WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, fmt.Errorf("lauf-schritte lesen: %w", err)
	}
	defer rows.Close()
	var out []PlaybookStep
	for rows.Next() {
		st := PlaybookStep{RunID: runID}
		if err := rows.Scan(&st.HostID, &st.Index, &st.StepID, &st.Status, &st.JobID, &st.StartedAt, &st.FinishedAt,
			&st.OK, &st.ExitCode, &st.Message, &st.Until, &st.BootTime); err != nil {
			return nil, fmt.Errorf("lauf-schritte lesen: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// PrunePlaybookRuns löscht beendete Läufe, die vor before angelegt wurden.
func (s *Store) PrunePlaybookRuns(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM playbook_runs WHERE created_at < ? AND finished_at IS NOT NULL`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("läufe bereinigen: %w", err)
	}
	return res.RowsAffected()
}

// OpenJobs liefert alle laufenden Jobs (ohne Ausgabe).
func (s *Store) OpenJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE finished_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("offene jobs lesen: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ScheduleState ist der Stand des Zeitplans eines Playbooks.
type ScheduleState struct {
	LastDue int64 // letzter behandelter Termin (Unix); 0 = noch keiner
	Paused  bool
}

// PlaybookSchedules liefert den Stand aller Zeitpläne nach Playbook-Name.
func (s *Store) PlaybookSchedules(ctx context.Context) (map[string]ScheduleState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, last_due, paused FROM playbook_schedules`)
	if err != nil {
		return nil, fmt.Errorf("zeitpläne lesen: %w", err)
	}
	defer rows.Close()
	out := map[string]ScheduleState{}
	for rows.Next() {
		var name string
		var st ScheduleState
		if err := rows.Scan(&name, &st.LastDue, &st.Paused); err != nil {
			return nil, fmt.Errorf("zeitpläne lesen: %w", err)
		}
		out[name] = st
	}
	return out, rows.Err()
}

// SetScheduleDue merkt sich den zuletzt behandelten Termin.
func (s *Store) SetScheduleDue(ctx context.Context, name string, due int64) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO playbook_schedules (name, last_due) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET last_due = excluded.last_due`, name, due); err != nil {
		return fmt.Errorf("zeitplan speichern: %w", err)
	}
	return nil
}

// SetSchedulePaused pausiert einen Zeitplan oder setzt ihn fort.
func (s *Store) SetSchedulePaused(ctx context.Context, name string, paused bool) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO playbook_schedules (name, paused) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET paused = excluded.paused`, name, paused); err != nil {
		return fmt.Errorf("zeitplan speichern: %w", err)
	}
	return nil
}
