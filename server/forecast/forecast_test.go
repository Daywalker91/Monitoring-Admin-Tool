package forecast

import (
	"math"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const gb = 1 << 30

// series erzeugt stündliche Messpunkte über hours Stunden bis now; used(h)
// liefert die Belegung h Stunden nach dem ersten Punkt.
func series(now time.Time, hours int, total uint64, used func(h int) uint64) []store.DiskSample {
	var out []store.DiskSample
	start := now.Add(-time.Duration(hours) * time.Hour)
	for h := 0; h <= hours; h++ {
		out = append(out, store.DiskSample{TS: start.Add(time.Duration(h) * time.Hour).Unix(), Used: used(h), Total: total})
	}
	return out
}

func TestDaysUntilFull(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// 1 GB pro Tag, 50 von 100 GB belegt am Ende → 50 Tage.
	grow := series(now, 72, 100*gb, func(h int) uint64 { return 47*gb + uint64(h)*gb/24 })
	if d, ok := DaysUntilFull(grow, now); !ok || math.Abs(d-50) > 0.1 {
		t.Errorf("wachsend: %v %v, erwartet 50", d, ok)
	}

	cases := []struct {
		name    string
		samples []store.DiskSample
	}{
		{"gleichbleibend", series(now, 72, 100*gb, func(int) uint64 { return 50 * gb })},
		{"schrumpfend", series(now, 72, 100*gb, func(h int) uint64 { return 60*gb - uint64(h)*gb/24 })},
		{"zu kurz", series(now, 12, 100*gb, func(h int) uint64 { return 50*gb + uint64(h)*gb })},
		{"leer", nil},
	}
	for _, c := range cases {
		if d, ok := DaysUntilFull(c.samples, now); ok {
			t.Errorf("%s: %v, erwartet keine prognose", c.name, d)
		}
	}

	// Punkte vor dem Fenster zählen nicht: früher stark gewachsen, jetzt konstant.
	old := series(now.Add(-10*24*time.Hour), 48, 100*gb, func(h int) uint64 { return uint64(h) * gb })
	flat := series(now, 48, 100*gb, func(int) uint64 { return 48 * gb })
	if d, ok := DaysUntilFull(append(old, flat...), now); ok {
		t.Errorf("altes wachstum gezählt: %v", d)
	}

	full := series(now, 48, 100*gb, func(h int) uint64 { return 52*gb + uint64(h)*gb })
	if d, ok := DaysUntilFull(full, now); !ok || d != 0 {
		t.Errorf("voll: %v %v, erwartet 0", d, ok)
	}
}
