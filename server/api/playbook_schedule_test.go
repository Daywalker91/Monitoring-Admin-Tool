package api

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestPlaybookSchedule(t *testing.T) {
	old := scheduleTick
	scheduleTick = time.Hour // nur der erste Durchlauf beim Start, den Rest ruft der Test auf
	t.Cleanup(func() { scheduleTick = old })
	book := "name: nachts\ntargets: [n1]\nschedule: daily 03:00\nsteps: [{id: a, wait: 1s}]\n"
	s, _, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(), map[string]string{"n.yaml": book})
	ctx := context.Background()

	// Der erste Durchlauf merkt sich nur den letzten Termin (nicht rückwirkend).
	var states map[string]store.ScheduleState
	for i := 0; ; i++ {
		var err error
		if states, err = s.store.PlaybookSchedules(ctx); err != nil {
			t.Fatal(err)
		}
		if states["nachts"].LastDue != 0 {
			break
		}
		if i > 500 {
			t.Fatal("zeitplan wird nicht erkannt")
		}
		time.Sleep(time.Millisecond)
	}
	sch, _ := playbook.ParseSchedule("daily 03:00")
	base := sch.Prev(time.Now().In(time.Local))
	if states["nachts"].LastDue != base.Unix() {
		t.Fatalf("letzter termin %v, erwartet %v", time.Unix(states["nachts"].LastDue, 0), base)
	}
	runs := func() []store.PlaybookRun {
		r, err := s.store.PlaybookRuns(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if len(runs()) != 0 {
		t.Fatal("neuer zeitplan darf nicht sofort starten")
	}

	var offset atomic.Int64
	s.nowFor = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	at := func(t time.Time) { offset.Store(int64(time.Until(t))) }

	// Nächster Termin: startet einmal, als Benutzer „zeitplan“.
	at(base.AddDate(0, 0, 1).Add(time.Minute))
	s.runSchedules(ctx)
	s.runSchedules(ctx)
	if r := runs(); len(r) != 1 || r[0].User != scheduleUser {
		t.Fatalf("läufe: %+v", r)
	}
	waitRun(t, s, runs()[0].ID)

	// Pausiert: Termin fällt aus.
	if err := s.store.SetSchedulePaused(ctx, "nachts", true); err != nil {
		t.Fatal(err)
	}
	at(base.AddDate(0, 0, 2).Add(time.Minute))
	s.runSchedules(ctx)
	if len(runs()) != 1 {
		t.Fatal("pausierter zeitplan hat gestartet")
	}
	views := s.scheduleViews(ctx)
	if v := views["nachts"]; !v.Paused || v.Next != base.AddDate(0, 0, 3).Unix() {
		t.Errorf("ansicht: %+v", v)
	}

	// Verpasst (Server länger als scheduleGrace aus): kein Start.
	s.store.SetSchedulePaused(ctx, "nachts", false)
	at(base.AddDate(0, 0, 3).Add(2 * time.Hour))
	s.runSchedules(ctx)
	if len(runs()) != 1 {
		t.Fatal("verpasster termin hat gestartet")
	}
	states, _ = s.store.PlaybookSchedules(ctx)
	if states["nachts"].LastDue != base.AddDate(0, 0, 3).Unix() {
		t.Errorf("verpasster termin nicht vermerkt: %v", time.Unix(states["nachts"].LastDue, 0))
	}
}
