package checks

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const (
	maxParallel = 8               // gleichzeitig laufende Checks
	tick        = time.Second     // Auflösung des Zeitplans
	startSpread = 5 * time.Second // erste Durchläufe nach dem Start verteilen
)

// ResultFunc erhält jedes Ergebnis (z. B. für die Alarmauswertung). Sie darf
// nicht lange blockieren.
type ResultFunc func(ctx context.Context, c store.Check, r store.CheckResult)

type entry struct {
	check   store.Check
	next    time.Time
	running bool
	last    *store.CheckResult
}

// Runner führt die Checks nach ihrem Intervall aus.
type Runner struct {
	st       *store.Store
	log      *slog.Logger
	probe    func(context.Context, store.Check) store.CheckResult
	onResult ResultFunc
	now      func() time.Time

	mu      sync.Mutex
	entries map[int64]*entry
	sem     chan struct{}
}

// NewRunner erzeugt einen Runner; onResult darf nil sein.
func NewRunner(st *store.Store, log *slog.Logger, onResult ResultFunc) *Runner {
	return &Runner{st: st, log: log, probe: Probe, onResult: onResult, now: time.Now,
		entries: map[int64]*entry{}, sem: make(chan struct{}, maxParallel)}
}

// Load liest Checks und letzte Ergebnisse aus der Datenbank.
func (r *Runner) Load(ctx context.Context) error {
	last, err := r.st.LastCheckResults(ctx)
	if err != nil {
		return err
	}
	if err := r.Reload(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i := 0
	for id, e := range r.entries {
		if res, ok := last[id]; ok {
			e.last = &res
		}
		e.next = r.now().Add(time.Duration(i) * startSpread / time.Duration(max(len(r.entries), 1)))
		i++
	}
	return nil
}

// Reload übernimmt geänderte Checks aus der Datenbank. Neue und geänderte
// Checks laufen sofort, entfernte verschwinden.
func (r *Runner) Reload(ctx context.Context) error {
	list, err := r.st.Checks(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[int64]bool{}
	for _, c := range list {
		seen[c.ID] = true
		e := r.entries[c.ID]
		if e == nil {
			r.entries[c.ID] = &entry{check: c, next: r.now()}
			continue
		}
		if e.check != c {
			changedTarget := e.check.Target != c.Target || e.check.Kind != c.Kind
			e.check = c
			e.next = r.now()
			if changedTarget {
				e.last = nil // altes Ergebnis passt nicht mehr zum neuen Ziel
			}
		}
	}
	for id := range r.entries {
		if !seen[id] {
			delete(r.entries, id)
		}
	}
	return nil
}

// Last liefert das letzte Ergebnis je Check.
func (r *Runner) Last() map[int64]store.CheckResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]store.CheckResult, len(r.entries))
	for id, e := range r.entries {
		if e.last != nil {
			out[id] = *e.last
		}
	}
	return out
}

// Run führt fällige Checks aus, bis ctx endet.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		r.startDue(ctx, &wg)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Runner) startDue(ctx context.Context, wg *sync.WaitGroup) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for _, e := range r.entries {
		if !e.check.Enabled || e.running || now.Before(e.next) {
			continue
		}
		select {
		case r.sem <- struct{}{}:
		default:
			return // alle Plätze belegt; im nächsten Takt weiter
		}
		e.running = true
		e.next = now.Add(time.Duration(e.check.IntervalSec) * time.Second)
		wg.Add(1)
		go func(c store.Check) {
			defer wg.Done()
			defer func() { <-r.sem }()
			r.runOne(ctx, c)
		}(e.check)
	}
}

func (r *Runner) runOne(ctx context.Context, c store.Check) {
	res := r.probe(ctx, c)
	if ctx.Err() != nil {
		return // Server fährt herunter; kein Fehlalarm
	}
	r.mu.Lock()
	e := r.entries[c.ID]
	current := e != nil && e.check == c
	if e != nil {
		e.running = false
	}
	if current {
		e.last = &res
	}
	r.mu.Unlock()
	if !current {
		return // inzwischen geändert oder gelöscht
	}
	if err := r.st.SaveCheckResult(ctx, c.ID, res); err != nil {
		r.log.Error("check-ergebnis speichern", "check", c.ID, "err", err)
	}
	if r.onResult != nil {
		r.onResult(ctx, c, res)
	}
}
