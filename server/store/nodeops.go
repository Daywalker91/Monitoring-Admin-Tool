package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Laufende Neustarts mit drain. Der Server kann währenddessen selbst
// verdrängt werden; mit diesem Stand macht er nach dem Start weiter.
const nodeOpsSchema = `
CREATE TABLE IF NOT EXISTS node_ops (
	job_id      TEXT PRIMARY KEY,
	host_id     TEXT NOT NULL,
	node        TEXT NOT NULL,
	phase       TEXT NOT NULL,
	reboot_job  TEXT NOT NULL DEFAULT '',
	boot_time   INTEGER NOT NULL DEFAULT 0,
	phase_until INTEGER NOT NULL DEFAULT 0,
	log         TEXT NOT NULL DEFAULT ''
);
`

// Phasen eines Neustarts mit drain.
const (
	NodeOpDrain  = "drain"  // abgesperrt, Pods werden verdrängt
	NodeOpReboot = "reboot" // Neustart angestoßen, warten auf Rückkehr
)

// NodeOp ist ein laufender Neustart mit drain.
type NodeOp struct {
	JobID      string
	HostID     string
	Node       string
	Phase      string
	RebootJob  string // Job-ID des Neustarts beim Agenten
	BootTime   int64  // Boot-Zeit des Hosts vor dem Neustart
	PhaseUntil int64  // Frist der aktuellen Phase (Unix-Sekunden)
	Log        string
}

// SaveNodeOp legt einen Vorgang an oder schreibt seinen Stand fort.
func (s *Store) SaveNodeOp(ctx context.Context, op NodeOp) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO node_ops (job_id, host_id, node, phase, reboot_job, boot_time, phase_until, log)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(job_id) DO UPDATE SET phase = excluded.phase, reboot_job = excluded.reboot_job,
	boot_time = excluded.boot_time, phase_until = excluded.phase_until, log = excluded.log`,
		op.JobID, op.HostID, op.Node, op.Phase, op.RebootJob, op.BootTime, op.PhaseUntil, op.Log)
	if err != nil {
		return fmt.Errorf("node-vorgang speichern: %w", err)
	}
	return nil
}

// NodeOps liefert alle laufenden Vorgänge.
func (s *Store) NodeOps(ctx context.Context) ([]NodeOp, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, host_id, node, phase, reboot_job, boot_time, phase_until, log
FROM node_ops ORDER BY job_id`)
	if err != nil {
		return nil, fmt.Errorf("node-vorgänge lesen: %w", err)
	}
	defer rows.Close()
	var out []NodeOp
	for rows.Next() {
		var op NodeOp
		if err := rows.Scan(&op.JobID, &op.HostID, &op.Node, &op.Phase, &op.RebootJob, &op.BootTime, &op.PhaseUntil, &op.Log); err != nil {
			return nil, fmt.Errorf("node-vorgänge lesen: %w", err)
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// DeleteNodeOp entfernt einen abgeschlossenen Vorgang.
func (s *Store) DeleteNodeOp(ctx context.Context, jobID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM node_ops WHERE job_id = ?`, jobID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("node-vorgang löschen: %w", err)
	}
	return nil
}
