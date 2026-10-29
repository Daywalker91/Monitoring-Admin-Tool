package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const jobsSchema = `
-- Jobs an Agenten (M7), zugleich Audit-Log: wer hat wann was an welchem Host ausgelöst.
-- finished_at = NULL: läuft noch. exit_code = -1: keine Rückmeldung bis deadline
-- (ein spätes Ergebnis darf das noch überschreiben).
CREATE TABLE IF NOT EXISTS jobs (
	id          TEXT PRIMARY KEY,
	host_id     TEXT NOT NULL,
	action      TEXT NOT NULL,
	params      TEXT NOT NULL DEFAULT '{}',
	username    TEXT NOT NULL DEFAULT '',
	remote      TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	deadline    INTEGER NOT NULL,
	finished_at INTEGER,
	ok          INTEGER NOT NULL DEFAULT 0,
	exit_code   INTEGER NOT NULL DEFAULT 0,
	output      TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_jobs_host ON jobs(host_id, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_open ON jobs(deadline) WHERE finished_at IS NULL;
`

// ExitNoResponse markiert Jobs ohne Rückmeldung bis zur Deadline.
const ExitNoResponse = -1

// Job ist ein Auftrag an einen Agenten samt Ergebnis.
type Job struct {
	ID         string            `json:"id"`
	HostID     string            `json:"host_id"`
	Action     string            `json:"action"`
	Params     map[string]string `json:"params,omitempty"`
	User       string            `json:"user"`
	Remote     string            `json:"-"`
	CreatedAt  int64             `json:"created_at"`
	Deadline   int64             `json:"deadline"`
	FinishedAt *int64            `json:"finished_at"` // nil = läuft
	OK         bool              `json:"ok"`
	ExitCode   int               `json:"exit_code"`
	Output     string            `json:"output,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// CreateJob legt einen laufenden Job an.
func (s *Store) CreateJob(ctx context.Context, j Job) error {
	params, err := json.Marshal(j.Params)
	if err != nil {
		return fmt.Errorf("job-parameter: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO jobs (id, host_id, action, params, username, remote, created_at, deadline)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, j.ID, j.HostID, j.Action, string(params), j.User, j.Remote, j.CreatedAt, j.Deadline)
	if err != nil {
		return fmt.Errorf("job anlegen: %w", err)
	}
	return nil
}

// FinishJob speichert das Ergebnis eines Jobs von hostID. Bereits beendete Jobs
// bleiben unverändert, außer sie sind nur wegen fehlender Rückmeldung beendet.
// found meldet, ob ein passender Job aktualisiert wurde.
func (s *Store) FinishJob(ctx context.Context, id, hostID string, at time.Time, ok bool, exitCode int, output, errMsg string) (found bool, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET finished_at = ?, ok = ?, exit_code = ?, output = ?, error = ?
		WHERE id = ? AND host_id = ? AND (finished_at IS NULL OR exit_code = ?)`,
		at.Unix(), ok, exitCode, output, errMsg, id, hostID, ExitNoResponse)
	if err != nil {
		return false, fmt.Errorf("job beenden: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ExpireJobs beendet laufende Jobs, deren Deadline vor now liegt.
func (s *Store) ExpireJobs(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET finished_at = ?, exit_code = ?, error = 'keine rückmeldung vom agenten'
		WHERE finished_at IS NULL AND deadline < ?`, now.Unix(), ExitNoResponse, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("jobs ablaufen lassen: %w", err)
	}
	return res.RowsAffected()
}

// PruneJobs löscht Jobs, die vor before angelegt wurden.
func (s *Store) PruneJobs(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE created_at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("jobs bereinigen: %w", err)
	}
	return res.RowsAffected()
}

// ClearJobOutputs leert die Ausgabe von Jobs der Aktion action, die vor before
// angelegt wurden. Der Eintrag selbst bleibt als Nachweis (z. B. wer welches Log gelesen hat).
func (s *Store) ClearJobOutputs(ctx context.Context, action string, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET output = '' WHERE action = ? AND created_at < ? AND output != ''`,
		action, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("job-ausgaben leeren: %w", err)
	}
	return res.RowsAffected()
}

const jobColumns = `id, host_id, action, params, username, created_at, deadline, finished_at, ok, exit_code, error`

func scanJob(sc interface{ Scan(...any) error }, extra ...any) (Job, error) {
	var j Job
	var params string
	var finished sql.NullInt64
	dest := append([]any{&j.ID, &j.HostID, &j.Action, &params, &j.User, &j.CreatedAt, &j.Deadline,
		&finished, &j.OK, &j.ExitCode, &j.Error}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return j, err
	}
	if finished.Valid {
		j.FinishedAt = &finished.Int64
	}
	if err := json.Unmarshal([]byte(params), &j.Params); err != nil {
		return j, fmt.Errorf("job-parameter lesen: %w", err)
	}
	return j, nil
}

// Jobs liefert die neuesten Jobs eines Hosts (ohne Ausgabe), neueste zuerst.
func (s *Store) Jobs(ctx context.Context, hostID string, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE host_id = ?
		ORDER BY created_at DESC, rowid DESC LIMIT ?`, hostID, limit)
	if err != nil {
		return nil, fmt.Errorf("jobs lesen: %w", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobByID liefert einen Job samt Ausgabe oder ErrNotFound.
func (s *Store) JobByID(ctx context.Context, id string) (Job, error) {
	var output string
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+`, output FROM jobs WHERE id = ?`, id), &output)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("job lesen: %w", err)
	}
	j.Output = output
	return j, nil
}
