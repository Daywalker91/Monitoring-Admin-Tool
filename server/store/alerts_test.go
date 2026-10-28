package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func TestAlertRules(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	rules := []AlertRule{{Kind: "cpu", Enabled: true, Threshold: 90, ForSec: 300}}
	if err := s.SaveAlertRules(ctx, rules); err != nil {
		t.Fatal(err)
	}
	rules[0].Threshold = 80
	if err := s.SaveAlertRules(ctx, rules); err != nil {
		t.Fatal(err)
	}
	got, err := s.AlertRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != rules[0] {
		t.Fatalf("regeln: %+v", got)
	}
}

func TestAlertLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	h := proto.Hello{AgentID: "pi-1", Hostname: "pi", IntervalSec: 10}
	if err := s.UpsertHost(ctx, h, "10.0.0.5:1", now); err != nil {
		t.Fatal(err)
	}

	old := &Alert{HostID: "pi-1", Kind: "cpu", Threshold: 90, Value: 95, Peak: 95,
		StartedAt: now.Add(-10 * 24 * time.Hour).Unix()}
	open := &Alert{HostID: "pi-1", Kind: "disk", Target: "/", Threshold: 90, Value: 91, Peak: 91,
		StartedAt: now.Unix()}
	for _, a := range []*Alert{old, open} {
		if err := s.OpenAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ResolveAlert(ctx, old.ID, now.Add(-9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAlertPeak(ctx, open.ID, 97); err != nil {
		t.Fatal(err)
	}

	active, err := s.Alerts(ctx, true, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != open.ID || active[0].Peak != 97 || active[0].Hostname != "pi" ||
		active[0].ResolvedAt != nil {
		t.Fatalf("aktive alarme: %+v", active)
	}
	all, _ := s.Alerts(ctx, false, time.Time{}, 100)
	if len(all) != 2 || all[0].ID != open.ID {
		t.Fatalf("alle alarme (offene zuerst): %+v", all)
	}

	if _, err := s.Prune(ctx, now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, _ = s.Alerts(ctx, false, time.Time{}, 100)
	if len(all) != 1 {
		t.Fatalf("nach prune: %d alarme, erwartet 1 (offene bleiben)", len(all))
	}

	if _, err := s.DeleteHost(ctx, "pi-1"); err != nil {
		t.Fatal(err)
	}
	all, _ = s.Alerts(ctx, false, time.Time{}, 100)
	if len(all) != 0 {
		t.Fatalf("nach host löschen: %d alarme", len(all))
	}
}

func TestAlertOverridesMutesSettings(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	s.UpsertHost(ctx, proto.Hello{AgentID: "nas", Hostname: "nas", IntervalSec: 10}, "x", now)

	rules := []AlertRule{{Kind: "disk", Enabled: true, Threshold: 95, ForSec: 60}, {Kind: "offline", Enabled: false, ForSec: 120}}
	if err := s.SetAlertOverrides(ctx, "nas", rules); err != nil {
		t.Fatal(err)
	}
	got, _ := s.AlertOverrides(ctx)
	if len(got["nas"]) != 2 || got["nas"][0].Kind != "disk" || got["nas"][1].Enabled {
		t.Fatalf("overrides: %+v", got)
	}
	s.SetAlertOverrides(ctx, "nas", nil)
	if got, _ := s.AlertOverrides(ctx); len(got) != 0 {
		t.Fatalf("nach leeren: %+v", got)
	}

	until := now.Add(time.Hour).Truncate(time.Second)
	s.SetHostMute(ctx, "nas", until)
	if m, _ := s.HostMutes(ctx); !m["nas"].Equal(until) {
		t.Fatalf("mute: %+v", m)
	}

	if _, ok, _ := s.AlertSetting(ctx, "reminder_hours"); ok {
		t.Fatal("einstellung ohne wert vorhanden")
	}
	s.SetAlertSetting(ctx, "reminder_hours", "4")
	if v, ok, _ := s.AlertSetting(ctx, "reminder_hours"); !ok || v != "4" {
		t.Fatalf("einstellung: %q %v", v, ok)
	}

	if exists, _ := s.HostExists(ctx, "nas"); !exists {
		t.Fatal("host nicht gefunden")
	}
	s.SetAlertOverrides(ctx, "nas", rules)
	s.DeleteHost(ctx, "nas")
	if m, _ := s.HostMutes(ctx); len(m) != 0 {
		t.Fatalf("mute nach host löschen: %+v", m)
	}
	if got, _ := s.AlertOverrides(ctx); len(got) != 0 {
		t.Fatalf("overrides nach host löschen: %+v", got)
	}
}

func TestMigrateNotifiedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	// Tabelle im Stand von M3, ohne notified_at.
	if _, err := db.Exec(`CREATE TABLE alerts (id INTEGER PRIMARY KEY AUTOINCREMENT, host_id TEXT NOT NULL,
		kind TEXT NOT NULL, target TEXT NOT NULL DEFAULT '', threshold REAL NOT NULL, value REAL NOT NULL,
		peak REAL NOT NULL, started_at INTEGER NOT NULL, resolved_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO alerts (host_id, kind, threshold, value, peak, started_at) VALUES ('h', 'cpu', 90, 95, 95, 1)`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	alerts, err := s.Alerts(ctx, true, time.Time{}, 10)
	if err != nil || len(alerts) != 1 || alerts[0].NotifiedAt == nil || *alerts[0].NotifiedAt != 1 {
		t.Fatalf("nach migration: %+v %v", alerts, err)
	}
	if err := s.SetAlertNotified(ctx, alerts[0].ID, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	alerts, _ = s.Alerts(ctx, true, time.Time{}, 10)
	if alerts[0].NotifiedAt == nil || *alerts[0].NotifiedAt != 100 {
		t.Fatalf("notified_at: %+v", alerts[0])
	}
}
