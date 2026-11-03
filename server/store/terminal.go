package store

import (
	"context"
	"fmt"
	"time"
)

// Terminal-Sitzungen (M11) als Audit-Log: wer, wann, von wo, wie lange und warum
// beendet. Eingaben und Ausgaben werden nie gespeichert.
const terminalSchema = `
CREATE TABLE IF NOT EXISTS terminal_sessions (
	id         TEXT PRIMARY KEY,
	host_id    TEXT NOT NULL,
	username   TEXT NOT NULL,
	remote     TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	ended_at   INTEGER,
	reason     TEXT NOT NULL DEFAULT '',
	bytes_in   INTEGER NOT NULL DEFAULT 0,
	bytes_out  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_terminal_sessions_host ON terminal_sessions(host_id, started_at);
`

// TerminalSession ist ein Eintrag im Audit-Log der Terminal-Sitzungen.
type TerminalSession struct {
	ID        string `json:"id"`
	HostID    string `json:"host_id"`
	User      string `json:"user"`
	Remote    string `json:"remote"`
	StartedAt int64  `json:"started_at"`
	EndedAt   *int64 `json:"ended_at"` // nil = läuft
	Reason    string `json:"reason,omitempty"`
	BytesIn   int64  `json:"bytes_in"`  // Eingabe (Browser → Host)
	BytesOut  int64  `json:"bytes_out"` // Ausgabe (Host → Browser)
}

// CreateTerminalSession legt eine Sitzung beim Öffnen an.
func (s *Store) CreateTerminalSession(ctx context.Context, t TerminalSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO terminal_sessions (id, host_id, username, remote, started_at)
VALUES (?, ?, ?, ?, ?)`, t.ID, t.HostID, t.User, t.Remote, t.StartedAt)
	if err != nil {
		return fmt.Errorf("terminal-sitzung speichern: %w", err)
	}
	return nil
}

// FinishTerminalSession vermerkt Ende, Grund und Datenmenge.
func (s *Store) FinishTerminalSession(ctx context.Context, id string, end time.Time, reason string, in, out int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE terminal_sessions SET ended_at = ?, reason = ?, bytes_in = ?, bytes_out = ?
WHERE id = ? AND ended_at IS NULL`, end.Unix(), reason, in, out, id)
	if err != nil {
		return fmt.Errorf("terminal-sitzung beenden: %w", err)
	}
	return nil
}

// CloseOpenTerminalSessions beendet Sitzungen, die ein Neustart des Servers
// unterbrochen hat (Sitzungen werden nicht fortgesetzt).
func (s *Store) CloseOpenTerminalSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE terminal_sessions SET ended_at = ?, reason = 'server neu gestartet'
WHERE ended_at IS NULL`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("offene terminal-sitzungen beenden: %w", err)
	}
	return res.RowsAffected()
}

// TerminalSessions liefert die letzten Sitzungen eines Hosts, neueste zuerst.
func (s *Store) TerminalSessions(ctx context.Context, hostID string, limit int) ([]TerminalSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, host_id, username, remote, started_at, ended_at, reason, bytes_in, bytes_out
FROM terminal_sessions WHERE host_id = ? ORDER BY started_at DESC, rowid DESC LIMIT ?`, hostID, limit)
	if err != nil {
		return nil, fmt.Errorf("terminal-sitzungen lesen: %w", err)
	}
	defer rows.Close()
	out := []TerminalSession{}
	for rows.Next() {
		var t TerminalSession
		if err := rows.Scan(&t.ID, &t.HostID, &t.User, &t.Remote, &t.StartedAt, &t.EndedAt, &t.Reason, &t.BytesIn, &t.BytesOut); err != nil {
			return nil, fmt.Errorf("terminal-sitzungen lesen: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PruneTerminalSessions löscht beendete Sitzungen, die vor before begonnen haben.
func (s *Store) PruneTerminalSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM terminal_sessions WHERE started_at < ? AND ended_at IS NOT NULL`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("terminal-sitzungen bereinigen: %w", err)
	}
	return res.RowsAffected()
}
