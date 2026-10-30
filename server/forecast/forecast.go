// Package forecast schätzt, wann ein Mountpoint voll ist: lineare Regression
// der Belegung über die letzten Tage, fortgeschrieben ab dem letzten Messwert.
package forecast

import (
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const (
	// Window ist der Zeitraum, aus dem der Trend berechnet wird.
	Window = 7 * 24 * time.Hour
	// minSpan: so lange müssen Messpunkte mindestens reichen, sonst ist der Trend Zufall.
	minSpan = 24 * time.Hour
	// minSamples: Mindestzahl Messpunkte (stündlich gemessen).
	minSamples = 6
)

// DaysUntilFull liefert, in wie vielen Tagen der Mountpoint bei gleichem
// Trend voll ist. ok = false, wenn die Daten nicht reichen oder die Belegung
// nicht wächst. samples sind zeitlich aufsteigend; ältere als Window vor now
// werden ignoriert.
func DaysUntilFull(samples []store.DiskSample, now time.Time) (days float64, ok bool) {
	from := now.Add(-Window).Unix()
	for len(samples) > 0 && samples[0].TS < from {
		samples = samples[1:]
	}
	if len(samples) < minSamples {
		return 0, false
	}
	first, last := samples[0], samples[len(samples)-1]
	if time.Duration(last.TS-first.TS)*time.Second < minSpan {
		return 0, false
	}

	// Steigung (Bytes pro Sekunde) per kleinste Quadrate; Zeit relativ zum
	// ersten Punkt, damit die Summen klein bleiben.
	var n, sx, sy, sxx, sxy float64
	for _, s := range samples {
		x := float64(s.TS - first.TS)
		y := float64(s.Used)
		n++
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, false
	}
	slope := (n*sxy - sx*sy) / den
	if slope <= 0 {
		return 0, false
	}
	if last.Used >= last.Total {
		return 0, true
	}
	free := float64(last.Total - last.Used)
	return free / slope / 86400, true
}
