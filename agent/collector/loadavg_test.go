package collector

import (
	"math"
	"testing"
	"time"
)

func TestBusyAvg(t *testing.T) {
	var b busyAvg
	at := time.Unix(1000, 0)
	b.add(0, at) // erste Messung: nur Ausgangswert
	if b.l1 != 0 {
		t.Fatalf("erste messung %v", b.l1)
	}
	// 2 Kerne dauerhaft belegt: 10 CPU-Sekunden je 5 s. Nach 15 min ist die
	// 1-min-Load praktisch 2, die 15-min-Load bei 1 - 1/e ≈ 63 %.
	busy := 0.0
	for range 180 {
		at = at.Add(5 * time.Second)
		busy += 10
		b.add(busy, at)
	}
	if math.Abs(b.l1-2) > 0.01 || math.Abs(b.l15-2*(1-math.Exp(-1))) > 0.05 || b.l5 <= b.l15 {
		t.Fatalf("l1=%v l5=%v l15=%v", b.l1, b.l5, b.l15)
	}
	// Zähler zurückgesprungen (z. B. Überlauf): Wert bleibt, kein Absturz ins Negative.
	l1 := b.l1
	b.add(0, at.Add(5*time.Second))
	if b.l1 != l1 {
		t.Fatalf("rücksprung änderte die load: %v", b.l1)
	}
}
