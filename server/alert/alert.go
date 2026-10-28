// Package alert wertet Schwellwert-Regeln aus und führt aktive Alarme.
//
// Ein Alarm löst erst aus, wenn die Bedingung über die Mindestdauer der Regel
// ununterbrochen erfüllt ist. Er endet, wenn der Wert um Hysteresis unter die
// Schwelle fällt (verhindert Flackern um den Schwellwert). Pro Alarm gibt es
// genau zwei Ereignisse: Auslösen und Ende.
package alert

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Regeltypen. Die Liste ist fest; frei formulierbare Ausdrücke gibt es nicht.
const (
	KindOffline = "offline" // Host nicht verbunden
	KindCPU     = "cpu"     // CPU-Auslastung gesamt
	KindMem     = "mem"     // RAM belegt
	KindSwap    = "swap"    // Swap belegt
	KindDisk    = "disk"    // Speicher je Mountpoint
)

const (
	// Hysteresis in Prozentpunkten: ein Alarm endet erst unterhalb Schwelle − Hysteresis.
	Hysteresis = 5.0
	// pendingGap: Liegen zwischen zwei Messungen mehr als pendingGap, beginnt die
	// Mindestdauer neu (z. B. nach einem Verbindungsabbruch).
	pendingGap = 2 * time.Minute
)

// DefaultRules sind die Standardwerte, solange nichts anderes gespeichert ist.
var DefaultRules = []store.AlertRule{
	{Kind: KindOffline, Enabled: true, ForSec: 120},
	{Kind: KindCPU, Enabled: true, Threshold: 90, ForSec: 300},
	{Kind: KindMem, Enabled: true, Threshold: 90, ForSec: 300},
	{Kind: KindSwap, Enabled: true, Threshold: 80, ForSec: 600},
	{Kind: KindDisk, Enabled: true, Threshold: 90, ForSec: 60},
}

// ValidateRule prüft eine Regel auf erlaubte Werte.
func ValidateRule(r store.AlertRule) error {
	if !knownKind(r.Kind) {
		return fmt.Errorf("unbekannter regeltyp %q", r.Kind)
	}
	if r.ForSec < 0 || r.ForSec > 86400 {
		return fmt.Errorf("%s: dauer muss zwischen 0 und 86400 s liegen", r.Kind)
	}
	if r.Kind == KindOffline {
		if r.ForSec < 30 {
			return fmt.Errorf("offline: dauer muss mindestens 30 s sein")
		}
		return nil
	}
	if r.Threshold < 10 || r.Threshold > 100 {
		return fmt.Errorf("%s: schwelle muss zwischen 10 und 100 %% liegen", r.Kind)
	}
	return nil
}

func knownKind(k string) bool {
	for _, d := range DefaultRules {
		if d.Kind == k {
			return true
		}
	}
	return false
}

// EventType unterscheidet Auslösen und Ende eines Alarms.
type EventType string

const (
	Firing   EventType = "firing"
	Resolved EventType = "resolved"
)

// Event meldet eine Zustandsänderung eines Alarms.
type Event struct {
	Type  EventType
	Alert store.Alert
	Rule  store.AlertRule
	At    time.Time
}

// HostState ist der Verbindungsstand eines Hosts für die Offline-Prüfung.
type HostState struct {
	ID       string
	Hostname string
	Online   bool
	LastSeen time.Time
}

type key struct{ host, kind, target string }

type pending struct {
	since, last time.Time
	value, peak float64
}

// Evaluator hält Regeln, wartende Bedingungen und aktive Alarme.
type Evaluator struct {
	st      *store.Store
	notify  func(Event)
	log     *slog.Logger
	started time.Time // Offline-Dauer zählt frühestens ab Serverstart

	mu      sync.Mutex
	rules   map[string]store.AlertRule
	pending map[key]*pending
	firing  map[key]*store.Alert
}

// New lädt Regeln und offene Alarme aus der Datenbank. notify wird für jedes
// Ereignis aufgerufen und darf nicht blockieren.
func New(ctx context.Context, st *store.Store, notify func(Event), log *slog.Logger, now time.Time) (*Evaluator, error) {
	e := &Evaluator{st: st, notify: notify, log: log, started: now,
		rules: map[string]store.AlertRule{}, pending: map[key]*pending{}, firing: map[key]*store.Alert{}}
	for _, r := range DefaultRules {
		e.rules[r.Kind] = r
	}
	saved, err := st.AlertRules(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range saved {
		if ValidateRule(r) == nil {
			e.rules[r.Kind] = r
		}
	}
	open, err := st.Alerts(ctx, true, time.Time{}, 10000)
	if err != nil {
		return nil, err
	}
	for i := range open {
		a := open[i]
		e.firing[key{a.HostID, a.Kind, a.Target}] = &a
	}
	return e, nil
}

// Rules liefert die aktuellen Regeln in fester Reihenfolge.
func (e *Evaluator) Rules() []store.AlertRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]store.AlertRule, 0, len(DefaultRules))
	for _, d := range DefaultRules {
		out = append(out, e.rules[d.Kind])
	}
	return out
}

// SetRules prüft und speichert Regeln. Alarme deaktivierter Regeln werden beendet.
func (e *Evaluator) SetRules(ctx context.Context, rules []store.AlertRule, now time.Time) error {
	for _, r := range rules {
		if err := ValidateRule(r); err != nil {
			return err
		}
	}
	if err := e.st.SaveAlertRules(ctx, rules); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rules {
		e.rules[r.Kind] = r
		if r.Enabled {
			continue
		}
		for k := range e.pending {
			if k.kind == r.Kind {
				delete(e.pending, k)
			}
		}
		for k := range e.firing {
			if k.kind == r.Kind {
				e.resolve(ctx, k, now)
			}
		}
	}
	return nil
}

// Observe wertet eine Metrik-Nachricht eines Hosts aus.
func (e *Evaluator) Observe(ctx context.Context, hostID, hostname string, m proto.Metrics, now time.Time) {
	values := map[key]float64{
		{hostID, KindCPU, ""}: m.CPU.TotalPct,
		{hostID, KindMem, ""}: m.Mem.UsedPct,
	}
	if m.Swap.Total > 0 {
		values[key{hostID, KindSwap, ""}] = m.Swap.UsedPct
	}
	for _, d := range m.Disks {
		values[key{hostID, KindDisk, d.Mount}] = d.UsedPct
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range values {
		rule := e.rules[k.kind]
		if !rule.Enabled {
			continue
		}
		e.check(ctx, k, hostname, rule, v, now)
	}
	// Ziele, die nicht mehr gemeldet werden (Mountpoint entfernt, Swap aus), beenden.
	for k := range e.pending {
		if _, ok := values[k]; !ok && k.host == hostID && k.kind != KindOffline {
			delete(e.pending, k)
		}
	}
	for k := range e.firing {
		if _, ok := values[k]; !ok && k.host == hostID && k.kind != KindOffline {
			e.resolve(ctx, k, now)
		}
	}
}

// check aktualisiert den Zustand eines einzelnen Messwerts. e.mu ist gesperrt.
func (e *Evaluator) check(ctx context.Context, k key, hostname string, rule store.AlertRule, v float64, now time.Time) {
	if a := e.firing[k]; a != nil {
		a.Hostname = hostname
		if v < rule.Threshold-Hysteresis {
			e.resolve(ctx, k, now)
		} else if v > a.Peak {
			a.Peak = v
			if err := e.st.UpdateAlertPeak(ctx, a.ID, v); err != nil {
				e.log.Error("alarm aktualisieren", "err", err)
			}
		}
		return
	}
	if v < rule.Threshold {
		delete(e.pending, k)
		return
	}
	p := e.pending[k]
	if p == nil || now.Sub(p.last) > pendingGap {
		p = &pending{since: now, value: v}
		e.pending[k] = p
	}
	p.last = now
	p.peak = max(p.peak, v)
	if now.Sub(p.since) >= time.Duration(rule.ForSec)*time.Second {
		delete(e.pending, k)
		e.fire(ctx, k, hostname, rule, v, p.peak, now)
	}
}

// CheckOffline prüft den Verbindungsstand aller Hosts.
func (e *Evaluator) CheckOffline(ctx context.Context, hosts []HostState, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rule := e.rules[KindOffline]
	for _, h := range hosts {
		k := key{h.ID, KindOffline, ""}
		if h.Online {
			if a := e.firing[k]; a != nil {
				a.Hostname = h.Hostname
				e.resolve(ctx, k, now)
			}
			continue
		}
		if !rule.Enabled || e.firing[k] != nil {
			continue
		}
		last := h.LastSeen
		if last.Before(e.started) {
			last = e.started // nach einem Serverneustart haben Agenten Zeit, sich neu zu verbinden
		}
		if offline := now.Sub(last); offline >= time.Duration(rule.ForSec)*time.Second {
			// Wert ist die Zeit ohne Verbindung in Minuten, gemessen ab der letzten Meldung.
			mins := now.Sub(h.LastSeen).Minutes()
			e.fire(ctx, k, h.Hostname, rule, mins, mins, now)
		}
	}
}

// Forget verwirft alles zu einem Host (nach dem Löschen; die Datenbank
// entfernt seine Alarme selbst).
func (e *Evaluator) Forget(hostID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := range e.pending {
		if k.host == hostID {
			delete(e.pending, k)
		}
	}
	for k := range e.firing {
		if k.host == hostID {
			delete(e.firing, k)
		}
	}
}

// ActiveCount liefert die Zahl aktiver Alarme je Host und Regeltyp.
func (e *Evaluator) ActiveCount() map[string]map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]map[string]int{}
	for k := range e.firing {
		if out[k.host] == nil {
			out[k.host] = map[string]int{}
		}
		out[k.host][k.kind]++
	}
	return out
}

// fire legt einen Alarm an und meldet ihn. e.mu ist gesperrt.
func (e *Evaluator) fire(ctx context.Context, k key, hostname string, rule store.AlertRule, v, peak float64, now time.Time) {
	a := &store.Alert{HostID: k.host, Hostname: hostname, Kind: k.kind, Target: k.target,
		Threshold: rule.Threshold, Value: v, Peak: peak, StartedAt: now.Unix()}
	if err := e.st.OpenAlert(ctx, a); err != nil {
		e.log.Error("alarm speichern", "err", err)
		return // ohne Datensatz kein Alarm; nächste Messung versucht es erneut
	}
	e.firing[k] = a
	e.log.Info("alarm ausgelöst", "host", k.host, "kind", k.kind, "target", k.target, "value", v)
	e.notify(Event{Type: Firing, Alert: *a, Rule: rule, At: now})
}

// resolve beendet einen aktiven Alarm und meldet das. e.mu ist gesperrt.
func (e *Evaluator) resolve(ctx context.Context, k key, now time.Time) {
	a := e.firing[k]
	if a == nil {
		return
	}
	if err := e.st.ResolveAlert(ctx, a.ID, now); err != nil {
		e.log.Error("alarm beenden", "err", err)
		return
	}
	delete(e.firing, k)
	ts := now.Unix()
	a.ResolvedAt = &ts
	e.log.Info("alarm beendet", "host", k.host, "kind", k.kind, "target", k.target)
	e.notify(Event{Type: Resolved, Alert: *a, Rule: e.rules[k.kind], At: now})
}

// Run prüft alle interval den Verbindungsstand der Hosts.
func (e *Evaluator) Run(ctx context.Context, interval time.Duration, hosts func(context.Context) ([]HostState, error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			hs, err := hosts(ctx)
			if err != nil {
				e.log.Error("hosts für alarmprüfung lesen", "err", err)
				continue
			}
			e.CheckOffline(ctx, hs, now)
		}
	}
}
