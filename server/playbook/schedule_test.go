package playbook

import (
	"strings"
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	for _, ok := range []string{"daily 04:30", "weekly sun 03:00", "weekly sat,SUN 23:59", "monthly 15 02:00",
		"monthly first sun 03:00", "monthly last fri 00:00"} {
		if _, err := ParseSchedule(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "daily", "daily 4:5", "daily 24:00", "daily 03:60", "weekly 03:00", "weekly son 03:00",
		"monthly 29 03:00", "monthly 0 03:00", "monthly fifth sun 03:00", "monthly first 03:00", "hourly 03:00",
		"daily x 03:00", "0 3 * * 0", strings.Repeat("daily ", 20) + "03:00"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q: fehler erwartet", bad)
		}
	}
}

func TestScheduleNextPrev(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("keine zeitzonen-daten:", err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, berlin)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const f = "2006-01-02 15:04"
	for _, c := range []struct{ sched, now, next, prev string }{
		{"daily 04:30", "2026-11-03 10:00", "2026-11-04 04:30", "2026-11-03 04:30"},
		{"daily 04:30", "2026-11-03 04:30", "2026-11-04 04:30", "2026-11-03 04:30"},
		// 2026-11-03 ist ein Dienstag.
		{"weekly sun 03:00", "2026-11-03 10:00", "2026-11-08 03:00", "2026-11-01 03:00"},
		{"weekly tue,sat 03:00", "2026-11-03 10:00", "2026-11-07 03:00", "2026-11-03 03:00"},
		{"monthly 15 02:00", "2026-11-03 10:00", "2026-11-15 02:00", "2026-10-15 02:00"},
		{"monthly first sun 03:00", "2026-11-03 10:00", "2026-12-06 03:00", "2026-11-01 03:00"},
		{"monthly last fri 22:00", "2026-11-03 10:00", "2026-11-27 22:00", "2026-10-30 22:00"},
		{"monthly second mon 01:00", "2026-11-03 10:00", "2026-11-09 01:00", "2026-10-12 01:00"},
		// Zeitumstellung: 2027-03-28 02:30 gibt es in Berlin nicht, time.Date macht 03:30 daraus.
		{"daily 02:30", "2027-03-28 00:00", "2027-03-28 03:30", "2027-03-27 02:30"},
	} {
		s, err := ParseSchedule(c.sched)
		if err != nil {
			t.Fatal(err)
		}
		now := at(c.now)
		if got := s.Next(now).Format(f); got != c.next {
			t.Errorf("%s ab %s: next %s, erwartet %s", c.sched, c.now, got, c.next)
		}
		if got := s.Prev(now).Format(f); got != c.prev {
			t.Errorf("%s ab %s: prev %s, erwartet %s", c.sched, c.now, got, c.prev)
		}
	}
}
