package store

import (
	"context"
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
