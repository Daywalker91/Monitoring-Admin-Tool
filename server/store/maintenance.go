package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const maintenanceSchema = `
-- Wartungsfenster (M7): Alarme werden erfasst, aber nicht gemeldet.
-- kind "once": start (Unix-Sekunden) + duration_min.
-- kind "weekly": weekdays (Bit 0 = Montag … Bit 6 = Sonntag), start_min (Minute
-- des Tages, Ortszeit des Servers) + duration_min.
CREATE TABLE IF NOT EXISTS maintenance (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	scope        TEXT NOT NULL,
	target       TEXT NOT NULL DEFAULT '',
	kind         TEXT NOT NULL,
	start        INTEGER NOT NULL DEFAULT 0,
	weekdays     INTEGER NOT NULL DEFAULT 0,
	start_min    INTEGER NOT NULL DEFAULT 0,
	duration_min INTEGER NOT NULL,
	enabled      INTEGER NOT NULL DEFAULT 1,
	created_at   INTEGER NOT NULL
);
`

// Bereiche und Arten eines Wartungsfensters.
const (
	ScopeAll   = "all"   // alle Hosts, Checks und der Cluster
	ScopeHost  = "host"  // Target = Agent-ID
	ScopeGroup = "group" // Target = Gruppenname

	MaintOnce   = "once"
	MaintWeekly = "weekly"
)

// Maintenance ist ein Wartungsfenster.
type Maintenance struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Target      string `json:"target"`
	Kind        string `json:"kind"`
	Start       int64  `json:"start"`     // nur once
	Weekdays    int    `json:"weekdays"`  // nur weekly
	StartMin    int    `json:"start_min"` // nur weekly
	DurationMin int    `json:"duration_min"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   int64  `json:"created_at"`
}

// Validate prüft ein Wartungsfenster (ohne Existenz von Host bzw. Gruppe).
func (m *Maintenance) Validate() error {
	if m.Name == "" {
		return errors.New("name fehlt")
	}
	switch m.Scope {
	case ScopeAll:
		m.Target = ""
	case ScopeHost, ScopeGroup:
		if m.Target == "" {
			return errors.New("host bzw. gruppe fehlt")
		}
	default:
		return fmt.Errorf("unbekannter bereich %q", m.Scope)
	}
	switch m.Kind {
	case MaintOnce:
		if m.Start <= 0 || m.DurationMin < 1 || m.DurationMin > 7*24*60 {
			return errors.New("einmalig: beginn und dauer (1 min bis 7 tage) angeben")
		}
		m.Weekdays, m.StartMin = 0, 0
	case MaintWeekly:
		if m.Weekdays < 1 || m.Weekdays > 127 || m.StartMin < 0 || m.StartMin >= 24*60 || m.DurationMin < 1 || m.DurationMin > 24*60 {
			return errors.New("wöchentlich: wochentage, uhrzeit und dauer (1 min bis 24 h) angeben")
		}
		m.Start = 0
	default:
		return fmt.Errorf("unbekannte art %q", m.Kind)
	}
	return nil
}

// ActiveAt meldet, ob das Fenster zu now gilt, und bis wann. Wöchentliche
// Fenster gelten in der Zeitzone loc und dürfen über Mitternacht reichen.
func (m Maintenance) ActiveAt(now time.Time, loc *time.Location) (end time.Time, ok bool) {
	if !m.Enabled {
		return time.Time{}, false
	}
	dur := time.Duration(m.DurationMin) * time.Minute
	if m.Kind == MaintOnce {
		start := time.Unix(m.Start, 0)
		return start.Add(dur), !now.Before(start) && now.Before(start.Add(dur))
	}
	local := now.In(loc)
	for back := 0; back <= 1; back++ {
		day := local.AddDate(0, 0, -back)
		start := time.Date(day.Year(), day.Month(), day.Day(), m.StartMin/60, m.StartMin%60, 0, 0, loc)
		if m.Weekdays&(1<<weekdayBit(start.Weekday())) != 0 && !now.Before(start) && now.Before(start.Add(dur)) {
			return start.Add(dur), true
		}
	}
	return time.Time{}, false
}

// NextStart liefert den nächsten Beginn ab now (Nullwert = keiner mehr).
func (m Maintenance) NextStart(now time.Time, loc *time.Location) time.Time {
	if !m.Enabled {
		return time.Time{}
	}
	if m.Kind == MaintOnce {
		if start := time.Unix(m.Start, 0); start.After(now) {
			return start
		}
		return time.Time{}
	}
	local := now.In(loc)
	for ahead := 0; ahead <= 7; ahead++ {
		day := local.AddDate(0, 0, ahead)
		start := time.Date(day.Year(), day.Month(), day.Day(), m.StartMin/60, m.StartMin%60, 0, 0, loc)
		if start.After(now) && m.Weekdays&(1<<weekdayBit(start.Weekday())) != 0 {
			return start
		}
	}
	return time.Time{}
}

// weekdayBit: Montag = 0 … Sonntag = 6.
func weekdayBit(d time.Weekday) int { return (int(d) + 6) % 7 }

// MaintenanceWindows liefert alle Wartungsfenster.
func (s *Store) MaintenanceWindows(ctx context.Context) ([]Maintenance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, scope, target, kind, start, weekdays, start_min, duration_min,
		enabled, created_at FROM maintenance ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("wartungsfenster lesen: %w", err)
	}
	defer rows.Close()
	out := []Maintenance{}
	for rows.Next() {
		var m Maintenance
		if err := rows.Scan(&m.ID, &m.Name, &m.Scope, &m.Target, &m.Kind, &m.Start, &m.Weekdays, &m.StartMin,
			&m.DurationMin, &m.Enabled, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveMaintenance legt ein Fenster an (ID 0) oder ändert es.
func (s *Store) SaveMaintenance(ctx context.Context, m *Maintenance) error {
	if m.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO maintenance (name, scope, target, kind, start, weekdays, start_min,
			duration_min, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.Name, m.Scope, m.Target, m.Kind, m.Start, m.Weekdays, m.StartMin, m.DurationMin, m.Enabled, m.CreatedAt)
		if err != nil {
			return fmt.Errorf("wartungsfenster anlegen: %w", err)
		}
		m.ID, err = res.LastInsertId()
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE maintenance SET name = ?, scope = ?, target = ?, kind = ?, start = ?,
		weekdays = ?, start_min = ?, duration_min = ?, enabled = ? WHERE id = ?`,
		m.Name, m.Scope, m.Target, m.Kind, m.Start, m.Weekdays, m.StartMin, m.DurationMin, m.Enabled, m.ID)
	if err != nil {
		return fmt.Errorf("wartungsfenster ändern: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMaintenance löscht ein Fenster.
func (s *Store) DeleteMaintenance(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM maintenance WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("wartungsfenster löschen: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HostGroup liefert die Gruppe eines Hosts ("" = keine oder unbekannter Host).
func (s *Store) HostGroup(ctx context.Context, hostID string) (string, error) {
	var g string
	err := s.db.QueryRowContext(ctx, `SELECT group_name FROM hosts WHERE id = ?`, hostID).Scan(&g)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("gruppe lesen: %w", err)
	}
	return g, nil
}
