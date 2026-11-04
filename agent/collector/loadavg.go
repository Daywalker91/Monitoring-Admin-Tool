package collector

import (
	"math"
	"time"
)

// loadSample: so oft fließt ein Messwert in die Mittelwerte ein (wie Linux und gopsutil).
const loadSample = 5 * time.Second

// busyAvg mittelt die Zahl belegter Kerne exponentiell über 1, 5 und 15
// Minuten, so wie der Linux-Kernel die Load. Windows kennt keine Load; zusammen
// mit der Warteschlange (gopsutil) ergibt das einen vergleichbaren Wert.
type busyAvg struct {
	l1, l5, l15 float64
	busy        float64 // letzter Zählerstand (CPU-Sekunden aller Kerne)
	at          time.Time
	primed      bool
}

// add nimmt einen neuen Zählerstand auf: busy sind die CPU-Sekunden aller
// Kerne ohne Leerlauf seit dem Start, at der Zeitpunkt der Messung.
func (b *busyAvg) add(busy float64, at time.Time) {
	if !b.primed {
		b.busy, b.at, b.primed = busy, at, true
		return
	}
	dt := at.Sub(b.at).Seconds()
	dBusy := busy - b.busy
	b.busy, b.at = busy, at
	if dt <= 0 || dBusy < 0 {
		return // Uhr oder Zähler zurückgesprungen
	}
	cores := dBusy / dt // CPU-Sekunden je Sekunde = belegte Kerne
	ewma := func(old float64, window time.Duration) float64 {
		f := math.Exp(-dt / window.Seconds())
		return old*f + cores*(1-f)
	}
	b.l1, b.l5, b.l15 = ewma(b.l1, time.Minute), ewma(b.l5, 5*time.Minute), ewma(b.l15, 15*time.Minute)
}
