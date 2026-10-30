package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Belegung je Mountpoint, stündlich, für die Prognose „Platte voll“. Die
// allgemeine Historie kennt nur die höchste Belegung aller Mountpoints.
const disksSchema = `
CREATE TABLE IF NOT EXISTS disk_history (
	host_id TEXT NOT NULL,
	mount   TEXT NOT NULL,
	ts      INTEGER NOT NULL,
	used    INTEGER NOT NULL,
	total   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_disk_history_host_ts ON disk_history(host_id, ts);
CREATE INDEX IF NOT EXISTS idx_disk_history_ts ON disk_history(ts);
`

// DiskSample ist ein Messpunkt eines Mountpoints (Bytes).
type DiskSample struct {
	TS    int64
	Used  uint64
	Total uint64
}

// SaveDiskSamples speichert die Belegung aller Mountpoints eines Hosts.
func (s *Store) SaveDiskSamples(ctx context.Context, hostID string, disks []proto.DiskUsage, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, d := range disks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO disk_history (host_id, mount, ts, used, total) VALUES (?, ?, ?, ?, ?)`,
			hostID, d.Mount, now.Unix(), int64(d.Used), int64(d.Total)); err != nil {
			return fmt.Errorf("disk-historie speichern: %w", err)
		}
	}
	return tx.Commit()
}

// LastDiskSample liefert den Zeitpunkt des letzten Messpunkts eines Hosts
// (Nullzeit, wenn es keinen gibt).
func (s *Store) LastDiskSample(ctx context.Context, hostID string) (time.Time, error) {
	var ts *int64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(ts) FROM disk_history WHERE host_id = ?`, hostID).Scan(&ts); err != nil {
		return time.Time{}, fmt.Errorf("disk-historie lesen: %w", err)
	}
	if ts == nil {
		return time.Time{}, nil
	}
	return time.Unix(*ts, 0), nil
}

// DiskHistory liefert die Messpunkte eines Hosts seit since je Mountpoint,
// zeitlich aufsteigend.
func (s *Store) DiskHistory(ctx context.Context, hostID string, since time.Time) (map[string][]DiskSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT mount, ts, used, total FROM disk_history
WHERE host_id = ? AND ts >= ? ORDER BY ts`, hostID, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("disk-historie lesen: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]DiskSample)
	for rows.Next() {
		var mount string
		var used, total int64
		var d DiskSample
		if err := rows.Scan(&mount, &d.TS, &used, &total); err != nil {
			return nil, err
		}
		d.Used, d.Total = uint64(used), uint64(total)
		out[mount] = append(out[mount], d)
	}
	return out, rows.Err()
}

// PruneDiskHistory löscht Messpunkte, die älter als before sind.
func (s *Store) PruneDiskHistory(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM disk_history WHERE ts < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("disk-historie bereinigen: %w", err)
	}
	return res.RowsAffected()
}
