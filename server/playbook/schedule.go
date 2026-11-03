package playbook

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Zeitplan (M12c) in einem eigenen, kleinen Format statt cron – cron verknüpft
// Tag im Monat und Wochentag mit ODER, „erster Sonntag im Monat“ geht dort nicht:
//
//	daily 04:30
//	weekly sun 03:00          (auch mehrere Tage: weekly sat,sun 03:00)
//	monthly 15 02:00          (Tag 1–28)
//	monthly first sun 03:00   (first, second, third, fourth, last)
//
// Die Uhrzeit gilt in der Zeitzone des Servers (MAT_TZ). Fällt sie in die
// Zeitumstellung, verschiebt sie sich wie bei time.Date.
type Schedule struct {
	kind   string // daily, weekly, monthly
	days   []time.Weekday
	dom    int // Tag im Monat (monthly); 0 = nth/weekday
	nth    int // 1–4, -1 = letzter
	hh, mm int
	source string
}

var weekdays = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

var nths = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "last": -1}

// maxScanDays begrenzt die Suche nach dem nächsten bzw. letzten Termin.
const maxScanDays = 400

// ParseSchedule liest einen Zeitplan.
func ParseSchedule(text string) (*Schedule, error) {
	f := strings.Fields(strings.ToLower(text))
	if len(f) < 2 || len(text) > 64 {
		return nil, errors.New(`schedule: z. B. "daily 04:30", "weekly sun 03:00", "monthly first sun 03:00"`)
	}
	s := &Schedule{kind: f[0], source: text}
	var err error
	if s.hh, s.mm, err = parseClock(f[len(f)-1]); err != nil {
		return nil, err
	}
	args := f[1 : len(f)-1]
	switch {
	case s.kind == "daily" && len(args) == 0:
	case s.kind == "weekly" && len(args) == 1:
		for _, d := range strings.Split(args[0], ",") {
			wd, ok := weekdays[d]
			if !ok {
				return nil, fmt.Errorf("schedule: unbekannter wochentag %q (mon … sun)", d)
			}
			s.days = append(s.days, wd)
		}
	case s.kind == "monthly" && len(args) == 1:
		if s.dom, err = strconv.Atoi(args[0]); err != nil || s.dom < 1 || s.dom > 28 {
			return nil, errors.New("schedule: tag im monat 1–28")
		}
	case s.kind == "monthly" && len(args) == 2:
		n, ok := nths[args[0]]
		wd, ok2 := weekdays[args[1]]
		if !ok || !ok2 {
			return nil, errors.New("schedule: z. B. monthly first sun 03:00 (first, second, third, fourth, last)")
		}
		s.nth, s.days = n, []time.Weekday{wd}
	default:
		return nil, errors.New(`schedule: daily, weekly <tage> oder monthly <tag | first … last wochentag>, dann die uhrzeit`)
	}
	return s, nil
}

func parseClock(v string) (int, int, error) {
	h, m, ok := strings.Cut(v, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || len(m) != 2 || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("schedule: uhrzeit %q, erwartet hh:mm", v)
	}
	return hh, mm, nil
}

// String liefert den Zeitplan wie in der Datei.
func (s *Schedule) String() string { return s.source }

// matches meldet, ob am Tag d (in seiner Zone) ein Termin liegt.
func (s *Schedule) matches(d time.Time) bool {
	switch s.kind {
	case "daily":
		return true
	case "weekly":
		for _, wd := range s.days {
			if d.Weekday() == wd {
				return true
			}
		}
		return false
	}
	if s.dom > 0 {
		return d.Day() == s.dom
	}
	if d.Weekday() != s.days[0] {
		return false
	}
	if s.nth == -1 {
		return d.AddDate(0, 0, 7).Month() != d.Month()
	}
	return (d.Day()-1)/7+1 == s.nth
}

// at liefert den Termin am Kalendertag von d.
func (s *Schedule) at(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), s.hh, s.mm, 0, 0, d.Location())
}

// Next liefert den ersten Termin nach t (in der Zone von t); null, wenn keiner
// in Reichweite liegt.
func (s *Schedule) Next(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	for range maxScanDays {
		if s.matches(d) {
			if at := s.at(d); at.After(t) {
				return at
			}
		}
		d = d.AddDate(0, 0, 1)
	}
	return time.Time{}
}

// Prev liefert den letzten Termin bis einschließlich t; null, wenn keiner in
// Reichweite liegt.
func (s *Schedule) Prev(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	for range maxScanDays {
		if s.matches(d) {
			if at := s.at(d); !at.After(t) {
				return at
			}
		}
		d = d.AddDate(0, 0, -1)
	}
	return time.Time{}
}
