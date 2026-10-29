package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMaintenanceActive(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("keine zeitzonen-daten")
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Sonntag 23:00 für 2 h: reicht in den Montag hinein (2026-11-01 ist ein Sonntag).
	weekly := Maintenance{Kind: MaintWeekly, Weekdays: 1 << 6, StartMin: 23 * 60, DurationMin: 120, Enabled: true}
	for s, want := range map[string]bool{
		"2026-11-01 22:59": false, "2026-11-01 23:00": true, "2026-11-02 00:30": true,
		"2026-11-02 01:00": false, "2026-11-08 23:30": true, "2026-11-03 23:30": false,
	} {
		if _, ok := weekly.ActiveAt(at(s), loc); ok != want {
			t.Errorf("wöchentlich %s: %v, erwartet %v", s, ok, want)
		}
	}
	if end, _ := weekly.ActiveAt(at("2026-11-02 00:30"), loc); !end.Equal(at("2026-11-02 01:00")) {
		t.Errorf("ende: %v", end)
	}
	if next := weekly.NextStart(at("2026-11-02 00:30"), loc); !next.Equal(at("2026-11-08 23:00")) {
		t.Errorf("nächster beginn: %v", next)
	}

	once := Maintenance{Kind: MaintOnce, Start: at("2026-11-05 10:00").Unix(), DurationMin: 30, Enabled: true}
	if _, ok := once.ActiveAt(at("2026-11-05 10:15"), loc); !ok {
		t.Error("einmalig: nicht aktiv")
	}
	if _, ok := once.ActiveAt(at("2026-11-05 10:30"), loc); ok {
		t.Error("einmalig: nach ende aktiv")
	}
	once.Enabled = false
	if _, ok := once.ActiveAt(at("2026-11-05 10:15"), loc); ok {
		t.Error("abgeschaltet aktiv")
	}
}

func TestMaintenanceStore(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	bad := []Maintenance{
		{Name: "", Scope: ScopeAll, Kind: MaintOnce, Start: 1, DurationMin: 10},
		{Name: "x", Scope: "welt", Kind: MaintOnce, Start: 1, DurationMin: 10},
		{Name: "x", Scope: ScopeHost, Kind: MaintOnce, Start: 1, DurationMin: 10},
		{Name: "x", Scope: ScopeAll, Kind: MaintWeekly, Weekdays: 0, DurationMin: 10},
		{Name: "x", Scope: ScopeAll, Kind: MaintWeekly, Weekdays: 1, StartMin: 1440, DurationMin: 10},
		{Name: "x", Scope: ScopeAll, Kind: MaintOnce, Start: 1, DurationMin: 8 * 24 * 60},
	}
	for i, m := range bad {
		if m.Validate() == nil {
			t.Errorf("%d: ungültiges fenster akzeptiert", i)
		}
	}
	m := Maintenance{Name: "Patchday", Scope: ScopeGroup, Target: "k3s", Kind: MaintWeekly, Weekdays: 64, StartMin: 180,
		DurationMin: 120, Enabled: true, CreatedAt: 1}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMaintenance(ctx, &m); err != nil || m.ID == 0 {
		t.Fatalf("anlegen: %v", err)
	}
	m.DurationMin = 60
	if err := st.SaveMaintenance(ctx, &m); err != nil {
		t.Fatal(err)
	}
	list, _ := st.MaintenanceWindows(ctx)
	if len(list) != 1 || list[0].DurationMin != 60 || list[0].Target != "k3s" {
		t.Fatalf("liste: %+v", list)
	}
	if err := st.DeleteMaintenance(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteMaintenance(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("doppelt gelöscht: %v", err)
	}
}
