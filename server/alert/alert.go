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
	"slices"
	"strconv"
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
	KindTemp    = "temp"    // CPU-Temperatur in °C

	KindCheckDown = "check_down" // externer Check schlägt fehl
	KindCheckCert = "check_cert" // Zertifikat eines HTTPS-Checks läuft in weniger als Threshold Tagen ab
)

// HostKinds sind die Regeltypen, die sich je Host anpassen lassen.
var HostKinds = []string{KindOffline, KindCPU, KindMem, KindSwap, KindDisk, KindTemp}

const (
	// Hysteresis in Prozentpunkten (bei "temp" in °C): ein Alarm endet erst
	// unterhalb Schwelle − Hysteresis.
	Hysteresis = 5.0
	// pendingGap: Liegen zwischen zwei Messungen mehr als pendingGap, beginnt die
	// Mindestdauer neu (z. B. nach einem Verbindungsabbruch).
	pendingGap = 2 * time.Minute

	settingReminder  = "reminder_hours"
	maxReminderHours = 168
)

// DefaultRules sind die Standardwerte, solange nichts anderes gespeichert ist.
var DefaultRules = []store.AlertRule{
	{Kind: KindOffline, Enabled: true, ForSec: 120},
	{Kind: KindCPU, Enabled: true, Threshold: 90, ForSec: 300},
	{Kind: KindMem, Enabled: true, Threshold: 90, ForSec: 300},
	{Kind: KindSwap, Enabled: true, Threshold: 80, ForSec: 600},
	{Kind: KindDisk, Enabled: true, Threshold: 90, ForSec: 60},
	{Kind: KindTemp, Enabled: true, Threshold: 80, ForSec: 300},
	{Kind: KindCheckDown, Enabled: true, ForSec: 120},
	{Kind: KindCheckCert, Enabled: true, Threshold: 14},
}

// ValidateRule prüft eine Regel auf erlaubte Werte.
func ValidateRule(r store.AlertRule) error {
	if !knownKind(r.Kind) {
		return fmt.Errorf("unbekannter regeltyp %q", r.Kind)
	}
	if r.ForSec < 0 || r.ForSec > 86400 {
		return fmt.Errorf("%s: dauer muss zwischen 0 und 86400 s liegen", r.Kind)
	}
	switch r.Kind {
	case KindOffline:
		if r.ForSec < 30 {
			return fmt.Errorf("offline: dauer muss mindestens 30 s sein")
		}
		return nil
	case KindCheckDown:
		return nil
	case KindCheckCert:
		if r.Threshold < 1 || r.Threshold > 365 {
			return fmt.Errorf("check_cert: schwelle muss zwischen 1 und 365 tagen liegen")
		}
		return nil
	}
	if r.Kind == KindTemp {
		if r.Threshold < 30 || r.Threshold > 120 {
			return fmt.Errorf("temp: schwelle muss zwischen 30 und 120 °C liegen")
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

// Reminder erinnert an einen weiterhin aktiven Alarm.
const Reminder EventType = "reminder"

// MuteForever ist das Ende einer unbefristeten Stummschaltung.
var MuteForever = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

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

	mu        sync.Mutex
	rules     map[string]store.AlertRule            // globale Regeln je Typ
	overrides map[string]map[string]store.AlertRule // Host → Typ → abweichende Regel
	muted     map[string]time.Time                  // Host → Ende der Stummschaltung
	reminder  time.Duration                         // 0 = keine Erinnerungen
	pending   map[key]*pending
	firing    map[key]*store.Alert
}

// New lädt Regeln und offene Alarme aus der Datenbank. notify wird für jedes
// Ereignis aufgerufen und darf nicht blockieren.
func New(ctx context.Context, st *store.Store, notify func(Event), log *slog.Logger, now time.Time) (*Evaluator, error) {
	e := &Evaluator{st: st, notify: notify, log: log, started: now,
		rules: map[string]store.AlertRule{}, overrides: map[string]map[string]store.AlertRule{},
		pending: map[key]*pending{}, firing: map[key]*store.Alert{}}
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
	overrides, err := st.AlertOverrides(ctx)
	if err != nil {
		return nil, err
	}
	for host, rules := range overrides {
		for _, r := range rules {
			if ValidateRule(r) == nil {
				e.setOverride(host, r)
			}
		}
	}
	if e.muted, err = st.HostMutes(ctx); err != nil {
		return nil, err
	}
	if v, ok, err := st.AlertSetting(ctx, settingReminder); err != nil {
		return nil, err
	} else if ok {
		if h, err := strconv.Atoi(v); err == nil && h > 0 && h <= maxReminderHours {
			e.reminder = time.Duration(h) * time.Hour
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
	}
	e.applyRules(ctx, now)
	return nil
}

// HostRules liefert die abweichenden Regeln eines Hosts in fester Reihenfolge.
func (e *Evaluator) HostRules(hostID string) []store.AlertRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []store.AlertRule{}
	for _, kind := range HostKinds {
		if r, ok := e.overrides[hostID][kind]; ok {
			out = append(out, r)
		}
	}
	return out
}

// SetHostRules ersetzt die abweichenden Regeln eines Hosts; nicht genannte
// Typen folgen wieder der globalen Regel.
func (e *Evaluator) SetHostRules(ctx context.Context, hostID string, rules []store.AlertRule, now time.Time) error {
	seen := map[string]bool{}
	for _, r := range rules {
		if err := ValidateRule(r); err != nil {
			return err
		}
		if !slices.Contains(HostKinds, r.Kind) {
			return fmt.Errorf("regeltyp %q gilt nicht je host", r.Kind)
		}
		if seen[r.Kind] {
			return fmt.Errorf("regeltyp %q doppelt", r.Kind)
		}
		seen[r.Kind] = true
	}
	if err := e.st.SetAlertOverrides(ctx, hostID, rules); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.overrides, hostID)
	for _, r := range rules {
		e.setOverride(hostID, r)
	}
	// Schwellen können sich geändert haben: wartende Bedingungen neu beginnen.
	for k := range e.pending {
		if k.host == hostID {
			delete(e.pending, k)
		}
	}
	e.applyRules(ctx, now)
	return nil
}

func (e *Evaluator) setOverride(hostID string, r store.AlertRule) {
	if e.overrides[hostID] == nil {
		e.overrides[hostID] = map[string]store.AlertRule{}
	}
	e.overrides[hostID][r.Kind] = r
}

// ruleFor liefert die für einen Host geltende Regel. e.mu ist gesperrt.
func (e *Evaluator) ruleFor(hostID, kind string) store.AlertRule {
	if r, ok := e.overrides[hostID][kind]; ok {
		return r
	}
	return e.rules[kind]
}

// applyRules beendet Alarme und wartende Bedingungen, deren Regel jetzt
// deaktiviert ist. e.mu ist gesperrt.
func (e *Evaluator) applyRules(ctx context.Context, now time.Time) {
	for k := range e.pending {
		if !e.ruleFor(k.host, k.kind).Enabled {
			delete(e.pending, k)
		}
	}
	for k := range e.firing {
		if !e.ruleFor(k.host, k.kind).Enabled {
			e.resolve(ctx, k, now)
		}
	}
}

// Mute schaltet die Benachrichtigungen eines Hosts bis until ab (MuteForever =
// unbefristet, Nullwert = aufheben). Alarme werden weiter erfasst.
func (e *Evaluator) Mute(ctx context.Context, hostID string, until time.Time) error {
	if err := e.st.SetHostMute(ctx, hostID, until); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if until.IsZero() {
		delete(e.muted, hostID)
	} else {
		e.muted[hostID] = until
	}
	return nil
}

// MutedUntil liefert das Ende der Stummschaltung, falls der Host zu now stumm ist.
func (e *Evaluator) MutedUntil(hostID string, now time.Time) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mutedUntil(hostID, now)
}

func (e *Evaluator) mutedUntil(hostID string, now time.Time) (time.Time, bool) {
	until, ok := e.muted[hostID]
	return until, ok && until.After(now)
}

// Reminder liefert den Erinnerungsabstand (0 = aus).
func (e *Evaluator) Reminder() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reminder
}

// SetReminder speichert den Erinnerungsabstand in ganzen Stunden (0 = aus).
func (e *Evaluator) SetReminder(ctx context.Context, hours int) error {
	if hours < 0 || hours > maxReminderHours {
		return fmt.Errorf("erinnerung muss zwischen 0 und %d stunden liegen", maxReminderHours)
	}
	if err := e.st.SetAlertSetting(ctx, settingReminder, strconv.Itoa(hours)); err != nil {
		return err
	}
	e.mu.Lock()
	e.reminder = time.Duration(hours) * time.Hour
	e.mu.Unlock()
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
	if m.CPUTemp != nil {
		values[key{hostID, KindTemp, ""}] = *m.CPUTemp
	}
	for _, d := range m.Disks {
		values[key{hostID, KindDisk, d.Mount}] = d.UsedPct
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range values {
		rule := e.ruleFor(hostID, k.kind)
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

// ObserveCheck wertet das Ergebnis eines externen Checks aus. id ist die
// Alarm-Kennung des Checks (checks.AlarmID), detail die Fehlermeldung samt
// Ziel. certDays ist die Restlaufzeit des Zertifikats in Tagen (nil = kein
// HTTPS oder nicht ermittelbar; dann bleibt der Zertifikatsalarm unverändert).
func (e *Evaluator) ObserveCheck(ctx context.Context, id, name string, ok bool, detail string, certDays *float64, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rule := e.ruleFor(id, KindCheckDown); rule.Enabled {
		e.condition(ctx, key{id, KindCheckDown, ""}, name, detail, rule, !ok, 1, now)
	}
	if rule := e.ruleFor(id, KindCheckCert); rule.Enabled && certDays != nil {
		e.condition(ctx, key{id, KindCheckCert, ""}, name, "", rule, *certDays < rule.Threshold, *certDays, now)
	}
}

// condition führt einen Alarm, dessen Bedingung ja/nein ist (ohne Hysterese):
// auslösen, wenn cond die Mindestdauer lang gilt, beenden, sobald sie nicht
// mehr gilt. e.mu ist gesperrt.
func (e *Evaluator) condition(ctx context.Context, k key, name, detail string, rule store.AlertRule, cond bool, v float64, now time.Time) {
	if a := e.firing[k]; a != nil {
		a.Hostname = name
		if !cond {
			e.resolve(ctx, k, now)
		}
		return
	}
	if !cond {
		delete(e.pending, k)
		return
	}
	p := e.pending[k]
	if p == nil {
		p = &pending{since: now, value: v}
		e.pending[k] = p
	}
	p.last = now
	if now.Sub(p.since) >= time.Duration(rule.ForSec)*time.Second {
		delete(e.pending, k)
		e.fireDetail(ctx, k, name, detail, rule, v, v, now)
	}
}

// Clear beendet alle Alarme und wartenden Bedingungen einer Kennung (z. B.
// eines abgeschalteten Checks); das Ende wird wie üblich gemeldet.
func (e *Evaluator) Clear(ctx context.Context, id string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := range e.pending {
		if k.host == id {
			delete(e.pending, k)
		}
	}
	for k := range e.firing {
		if k.host == id {
			e.resolve(ctx, k, now)
		}
	}
}

// CheckOffline prüft den Verbindungsstand aller Hosts.
func (e *Evaluator) CheckOffline(ctx context.Context, hosts []HostState, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, h := range hosts {
		rule := e.ruleFor(h.ID, KindOffline)
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
	delete(e.overrides, hostID)
	delete(e.muted, hostID)
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
	e.fireDetail(ctx, k, hostname, "", rule, v, peak, now)
}

// fireDetail legt einen Alarm mit Zusatzinfo an und meldet ihn. e.mu ist gesperrt.
func (e *Evaluator) fireDetail(ctx context.Context, k key, hostname, detail string, rule store.AlertRule, v, peak float64, now time.Time) {
	a := &store.Alert{HostID: k.host, Hostname: hostname, Detail: detail, Kind: k.kind, Target: k.target,
		Threshold: rule.Threshold, Value: v, Peak: peak, StartedAt: now.Unix()}
	_, muted := e.mutedUntil(k.host, now)
	if !muted {
		ts := now.Unix()
		a.NotifiedAt = &ts
	}
	if err := e.st.OpenAlert(ctx, a); err != nil {
		e.log.Error("alarm speichern", "err", err)
		return // ohne Datensatz kein Alarm; nächste Messung versucht es erneut
	}
	e.firing[k] = a
	e.log.Info("alarm ausgelöst", "host", k.host, "kind", k.kind, "target", k.target, "value", v, "muted", muted)
	if !muted {
		e.notify(Event{Type: Firing, Alert: *a, Rule: rule, At: now})
	}
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
	// Ende nur melden, wenn der Beginn gemeldet wurde und der Host nicht stumm ist.
	if _, muted := e.mutedUntil(k.host, now); a.NotifiedAt != nil && !muted {
		e.notify(Event{Type: Resolved, Alert: *a, Rule: e.ruleFor(k.host, k.kind), At: now})
	}
}

// Tick meldet Alarme nach, die während einer Stummschaltung ausgelöst haben
// und noch aktiv sind, und verschickt fällige Erinnerungen.
func (e *Evaluator) Tick(ctx context.Context, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, a := range e.firing {
		if _, muted := e.mutedUntil(k.host, now); muted {
			continue
		}
		typ := Firing
		if a.NotifiedAt != nil {
			if e.reminder == 0 || now.Sub(time.Unix(*a.NotifiedAt, 0)) < e.reminder {
				continue
			}
			typ = Reminder
		}
		if err := e.st.SetAlertNotified(ctx, a.ID, now); err != nil {
			e.log.Error("benachrichtigung speichern", "err", err)
			continue
		}
		ts := now.Unix()
		a.NotifiedAt = &ts
		e.notify(Event{Type: typ, Alert: *a, Rule: e.ruleFor(k.host, k.kind), At: now})
	}
}

// Run prüft alle interval den Verbindungsstand der Hosts, meldet Alarme nach
// dem Ende einer Stummschaltung nach und verschickt Erinnerungen.
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
			e.Tick(ctx, now)
		}
	}
}
