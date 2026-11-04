package collector

import (
	"context"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

// Unter Windows liefert gopsutil als "Load" nur die gemittelte Warteschlange
// (Threads, die auf einen Kern warten). Auf Rechnern mit vielen Kernen ist die
// fast immer 0. Dazu kommen hier die belegten Kerne, wie Linux sie mitzählt.
// Anders als unter Linux fehlen Prozesse, die auf Platte oder Netz warten.
var winLoad struct {
	once sync.Once
	mu   sync.Mutex
	avg  busyAvg
}

// platformLoad ergänzt die Werte von gopsutil um die belegten Kerne. Der erste
// Aufruf startet die Messung; ctx (Lebensdauer des Agenten, siehe Prime)
// beendet sie.
func platformLoad(ctx context.Context, l1, l5, l15 float64) (float64, float64, float64) {
	winLoad.once.Do(func() { go sampleBusy(ctx) })
	winLoad.mu.Lock()
	defer winLoad.mu.Unlock()
	a := winLoad.avg
	return l1 + a.l1, l5 + a.l5, l15 + a.l15
}

func sampleBusy(ctx context.Context) {
	t := time.NewTicker(loadSample)
	defer t.Stop()
	for {
		if times, err := cpu.TimesWithContext(ctx, false); err == nil && len(times) > 0 {
			total := times[0].Total()
			winLoad.mu.Lock()
			winLoad.avg.add(total-times[0].Idle, time.Now())
			winLoad.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
