package alert

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

type fixture struct {
	t      *testing.T
	st     *store.Store
	ev     *Evaluator
	events []Event
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, st: st, now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	f.ev = f.open()
	return f
}

func (f *fixture) open() *Evaluator {
	f.t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ev, err := New(context.Background(), f.st, func(e Event) { f.events = append(f.events, e) }, log, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	return ev
}

// observe meldet eine Messung und rückt die Uhr um step vor.
func (f *fixture) observe(m proto.Metrics, step time.Duration) {
	f.ev.Observe(context.Background(), "h1", "host-1", m, f.now)
	f.now = f.now.Add(step)
}

func (f *fixture) take() []Event {
	ev := f.events
	f.events = nil
	return ev
}

func cpu(v float64) proto.Metrics { return proto.Metrics{CPU: proto.CPU{TotalPct: v}} }

func TestFiresAfterDurationAndResolvesWithHysteresis(t *testing.T) {
	f := newFixture(t)
	// CPU: 90 % für 300 s. Messung alle 10 s.
	for i := 0; i < 30; i++ {
		f.observe(cpu(95), 10*time.Second)
	}
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("nach 290 s schon %d ereignisse", len(ev))
	}
	f.observe(cpu(97), 10*time.Second) // 300 s erreicht
	ev := f.take()
	if len(ev) != 1 || ev[0].Type != Firing || ev[0].Alert.Kind != KindCPU || ev[0].Alert.Value != 97 {
		t.Fatalf("auslösen: %+v", ev)
	}

	f.observe(cpu(99), 10*time.Second) // Spitzenwert
	f.observe(cpu(87), 10*time.Second) // unter Schwelle, aber innerhalb der Hysterese
	f.observe(cpu(91), 10*time.Second)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("flackern: %+v", ev)
	}
	f.observe(cpu(84), 10*time.Second)
	ev = f.take()
	if len(ev) != 1 || ev[0].Type != Resolved || ev[0].Alert.Peak != 99 || ev[0].Alert.ResolvedAt == nil {
		t.Fatalf("beenden: %+v", ev)
	}
	active, _ := f.st.Alerts(context.Background(), true, time.Time{}, 10)
	all, _ := f.st.Alerts(context.Background(), false, time.Time{}, 10)
	if len(active) != 0 || len(all) != 1 || all[0].Peak != 99 {
		t.Fatalf("datenbank: aktiv=%+v alle=%+v", active, all)
	}
}

func TestShortSpikeAndGapDoNotFire(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 20; i++ {
		f.observe(cpu(95), 10*time.Second)
	}
	f.observe(cpu(50), 10*time.Second) // Unterbrechung setzt die Dauer zurück
	for i := 0; i < 20; i++ {
		f.observe(cpu(95), 10*time.Second)
	}
	f.now = f.now.Add(10 * time.Minute) // Verbindungslücke
	for i := 0; i < 20; i++ {
		f.observe(cpu(95), 10*time.Second)
	}
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("unerwartete ereignisse: %+v", ev)
	}
}

func TestDiskPerMountAndRemovedTarget(t *testing.T) {
	f := newFixture(t)
	disks := func(d ...proto.DiskUsage) proto.Metrics { return proto.Metrics{Disks: d} }
	full := proto.DiskUsage{Mount: "/data", UsedPct: 95}
	ok := proto.DiskUsage{Mount: "/", UsedPct: 40}
	for i := 0; i <= 6; i++ { // 60 s
		f.observe(disks(ok, full), 10*time.Second)
	}
	ev := f.take()
	if len(ev) != 1 || ev[0].Alert.Target != "/data" {
		t.Fatalf("disk-alarm: %+v", ev)
	}
	if got := ev[0].Title(); got != "host-1: Disk /data 95,0 %" {
		t.Errorf("titel: %q", got)
	}
	f.observe(disks(ok), 10*time.Second) // Mountpoint verschwunden
	ev = f.take()
	if len(ev) != 1 || ev[0].Type != Resolved {
		t.Fatalf("entfernter mountpoint: %+v", ev)
	}
}

func TestOffline(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	h := HostState{ID: "h1", Hostname: "host-1", LastSeen: f.now.Add(-time.Hour)}

	// Kurz nach Serverstart noch kein Alarm, auch wenn last_seen alt ist.
	f.ev.CheckOffline(ctx, []HostState{h}, f.now.Add(time.Minute))
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("alarm direkt nach start: %+v", ev)
	}
	f.ev.CheckOffline(ctx, []HostState{h}, f.now.Add(2*time.Minute))
	f.ev.CheckOffline(ctx, []HostState{h}, f.now.Add(3*time.Minute)) // kein zweites Ereignis
	ev := f.take()
	if len(ev) != 1 || ev[0].Alert.Kind != KindOffline || ev[0].Title() != "host-1 ist offline" {
		t.Fatalf("offline: %+v", ev)
	}
	if !strings.Contains(ev[0].Body(), "seit 1 h 2 min") {
		t.Errorf("text: %q", ev[0].Body())
	}

	// Nach einem Neustart bleibt der Alarm aktiv und wird nicht erneut gemeldet.
	f.ev = f.open()
	f.ev.CheckOffline(ctx, []HostState{h}, f.now.Add(10*time.Minute))
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("nach neustart erneut gemeldet: %+v", ev)
	}
	h.Online = true
	f.ev.CheckOffline(ctx, []HostState{h}, f.now.Add(11*time.Minute))
	ev = f.take()
	if len(ev) != 1 || ev[0].Type != Resolved || ev[0].Title() != "host-1 ist wieder online" {
		t.Fatalf("wieder online: %+v", ev)
	}
	// 62 min beim Auslösen + 9 min Alarmdauer
	if !strings.Contains(ev[0].Body(), "war 1 h 11 min ohne Verbindung") {
		t.Errorf("text: %q", ev[0].Body())
	}
}

func TestSetRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i := 0; i <= 30; i++ {
		f.observe(cpu(95), 10*time.Second)
	}
	if len(f.take()) != 1 {
		t.Fatal("kein alarm")
	}

	bad := []store.AlertRule{{Kind: KindCPU, Enabled: true, Threshold: 5, ForSec: 60}}
	if err := f.ev.SetRules(ctx, bad, f.now); err == nil {
		t.Error("schwelle 5 % akzeptiert")
	}
	if err := f.ev.SetRules(ctx, []store.AlertRule{{Kind: "shell"}}, f.now); err == nil {
		t.Error("unbekannter typ akzeptiert")
	}

	off := []store.AlertRule{{Kind: KindCPU, Enabled: false, Threshold: 90, ForSec: 300}}
	if err := f.ev.SetRules(ctx, off, f.now); err != nil {
		t.Fatal(err)
	}
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Resolved {
		t.Fatalf("deaktivierte regel: %+v", ev)
	}
	f.observe(cpu(99), 10*time.Second)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("deaktivierte regel löst aus: %+v", ev)
	}

	// Gespeicherte Regeln überstehen einen Neustart.
	if r := f.open().Rules(); r[1].Kind != KindCPU || r[1].Enabled {
		t.Fatalf("regeln nach neustart: %+v", r)
	}
}

func TestHostOverride(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	nas := []store.AlertRule{{Kind: KindCPU, Enabled: true, Threshold: 99, ForSec: 0}}
	if err := f.ev.SetHostRules(ctx, "h1", nas, f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.ev.SetHostRules(ctx, "h1", append(nas, nas[0]), f.now); err == nil {
		t.Error("doppelter regeltyp akzeptiert")
	}
	for i := 0; i <= 31; i++ { // global würde nach 300 s auslösen
		f.observe(cpu(95), 10*time.Second)
	}
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("host-regel ignoriert: %+v", ev)
	}
	f.observe(cpu(99.5), 10*time.Second)
	if ev := f.take(); len(ev) != 1 || ev[0].Alert.Threshold != 99 {
		t.Fatalf("host-regel: %+v", ev)
	}

	// Host-Regel deaktivieren beendet den Alarm; Neustart lädt die Regel wieder.
	off := []store.AlertRule{{Kind: KindCPU, Enabled: false, Threshold: 99, ForSec: 0}}
	f.ev.SetHostRules(ctx, "h1", off, f.now)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Resolved {
		t.Fatalf("deaktiviert: %+v", ev)
	}
	if r := f.open().HostRules("h1"); len(r) != 1 || r[0].Enabled {
		t.Fatalf("nach neustart: %+v", r)
	}
	// Leere Liste: wieder globale Regel.
	f.ev.SetHostRules(ctx, "h1", nil, f.now)
	if r := f.ev.HostRules("h1"); len(r) != 0 {
		t.Fatalf("zurückgesetzt: %+v", r)
	}
}

func TestMute(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rules := []store.AlertRule{{Kind: KindCPU, Enabled: true, Threshold: 90, ForSec: 0}}
	f.ev.SetRules(ctx, rules, f.now)

	f.ev.Mute(ctx, "h1", f.now.Add(time.Hour))
	f.observe(cpu(95), 10*time.Second)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("stumm, trotzdem gemeldet: %+v", ev)
	}
	active, _ := f.st.Alerts(ctx, true, time.Time{}, 10)
	if len(active) != 1 || active[0].NotifiedAt != nil {
		t.Fatalf("alarm wird erfasst, aber nicht gemeldet: %+v", active)
	}
	f.ev.Tick(ctx, f.now)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("tick während stummschaltung: %+v", ev)
	}

	// Nach Ende der Stummschaltung wird der noch aktive Alarm nachgemeldet.
	f.now = f.now.Add(time.Hour)
	f.ev.Tick(ctx, f.now)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Firing {
		t.Fatalf("nachmeldung: %+v", ev)
	}

	// Ein während der Stummschaltung ausgelöster und beendeter Alarm bleibt ganz still.
	f.ev.Mute(ctx, "h1", MuteForever)
	f.observe(cpu(50), 10*time.Second) // beendet den gemeldeten Alarm – stumm, also keine Meldung
	f.observe(cpu(95), 10*time.Second)
	f.observe(cpu(50), 10*time.Second)
	f.ev.Mute(ctx, "h1", time.Time{})
	f.ev.Tick(ctx, f.now)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("stumme alarme gemeldet: %+v", ev)
	}
	if _, muted := f.ev.MutedUntil("h1", f.now); muted {
		t.Fatal("stummschaltung nicht aufgehoben")
	}
}

func TestReminder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.ev.SetRules(ctx, []store.AlertRule{{Kind: KindCPU, Enabled: true, Threshold: 90, ForSec: 0}}, f.now)
	if err := f.ev.SetReminder(ctx, 200); err == nil {
		t.Error("200 h akzeptiert")
	}
	f.ev.SetReminder(ctx, 2)
	f.observe(cpu(95), 10*time.Second)
	f.take()

	f.ev.Tick(ctx, f.now.Add(time.Hour))
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("zu frühe erinnerung: %+v", ev)
	}
	f.ev.Tick(ctx, f.now.Add(2*time.Hour))
	ev := f.take()
	if len(ev) != 1 || ev[0].Type != Reminder || !strings.HasPrefix(ev[0].Title(), "Erinnerung: host-1: CPU") ||
		!strings.Contains(ev[0].Body(), "Alarm besteht seit 2 h") {
		t.Fatalf("erinnerung: %+v %q", ev, ev[0].Body())
	}
	f.ev.Tick(ctx, f.now.Add(3*time.Hour))
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("erinnerung zu oft: %+v", ev)
	}
	if f.open().Reminder() != 2*time.Hour {
		t.Fatal("erinnerung nach neustart verloren")
	}
}

func TestTempAlert(t *testing.T) {
	f := newFixture(t)
	temp := func(v float64) proto.Metrics { return proto.Metrics{CPUTemp: &v} }
	// Standard: 80 °C für 300 s.
	for i := 0; i <= 30; i++ {
		f.observe(temp(82.5), 10*time.Second)
	}
	ev := f.take()
	if len(ev) != 1 || ev[0].Alert.Kind != KindTemp || ev[0].Title() != "host-1: CPU-Temperatur 82,5 °C" {
		t.Fatalf("auslösen: %+v", ev)
	}
	if body := ev[0].Body(); !strings.Contains(body, "über 80,0 °C") || strings.Contains(body, "%") {
		t.Fatalf("text: %s", body)
	}
	f.observe(temp(76), 10*time.Second) // innerhalb der Hysterese von 5 °C
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("flackern: %+v", ev)
	}
	f.observe(temp(74), 10*time.Second)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Resolved || !strings.Contains(ev[0].Body(), "unter 75,0 °C") {
		t.Fatalf("ende: %+v", ev)
	}
	// Host ohne Sensor: kein Alarm, keine wartende Bedingung.
	f.observe(proto.Metrics{}, 10*time.Second)
	if len(f.ev.pending) != 0 {
		t.Fatalf("wartend ohne sensor: %+v", f.ev.pending)
	}
	if ValidateRule(store.AlertRule{Kind: KindTemp, Enabled: true, Threshold: 25}) == nil ||
		ValidateRule(store.AlertRule{Kind: KindTemp, Enabled: true, Threshold: 110}) != nil {
		t.Fatal("grenzen der temperaturschwelle")
	}
}

func TestCheckAlerts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	obs := func(ok bool, certDays *float64) {
		f.ev.ObserveCheck(ctx, "check:7", "Webseite", ok, "status 503 (https://x)", certDays, f.now)
		f.now = f.now.Add(time.Minute)
	}
	days := func(v float64) *float64 { return &v }

	// Standard: 2 min fehlgeschlagen.
	obs(false, nil)
	obs(false, nil)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("zu früh: %+v", ev)
	}
	obs(false, nil)
	ev := f.take()
	if len(ev) != 1 || ev[0].Alert.Kind != KindCheckDown || ev[0].Title() != "Webseite ist nicht erreichbar" ||
		!strings.Contains(ev[0].Body(), "status 503 (https://x)") || !strings.Contains(ev[0].Body(), "Check: Webseite") {
		t.Fatalf("auslösen: %+v %s", ev, ev[0].Body())
	}
	obs(true, days(30))
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Resolved || ev[0].Title() != "Webseite ist wieder erreichbar" {
		t.Fatalf("ende: %+v", ev)
	}

	// Zertifikat: Standard 14 Tage, sofort.
	obs(true, days(9.5))
	ev = f.take()
	if len(ev) != 1 || ev[0].Alert.Kind != KindCheckCert || ev[0].Title() != "Webseite: Zertifikat läuft in 9 Tagen ab" {
		t.Fatalf("zertifikat: %+v", ev)
	}
	obs(false, nil) // Fehlschlag ohne Zertifikatsinfo ändert den Zertifikatsalarm nicht
	if f.ev.firing[key{"check:7", KindCheckCert, ""}] == nil {
		t.Fatal("zertifikatsalarm ohne neue info beendet")
	}
	obs(true, days(89))
	ev = f.take()
	if len(ev) != 1 || ev[0].Type != Resolved || ev[0].Alert.Kind != KindCheckCert {
		t.Fatalf("zertifikat erneuert: %+v", ev)
	}

	// Abschalten beendet laufende Alarme.
	for i := 0; i < 3; i++ {
		obs(false, nil)
	}
	f.take()
	f.ev.Clear(ctx, "check:7", f.now)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Resolved || len(f.ev.firing) != 0 {
		t.Fatalf("clear: %+v", ev)
	}
	if f.ev.SetHostRules(ctx, "h1", []store.AlertRule{{Kind: KindCheckDown, Enabled: true}}, f.now) == nil {
		t.Fatal("check-regel als host-regel akzeptiert")
	}
}

// Metrik-Nachrichten dürfen Dienst-Alarme desselben Hosts nicht beenden.
func TestMetricsKeepServiceAlerts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := map[string]Condition{"backup": {Bad: true, Detail: "inactive/dead"}}
	f.ev.ObserveSet(ctx, "h1", "host-1", KindService, bad, f.now)
	f.observe(cpu(10), 30*time.Second)
	f.ev.ObserveSet(ctx, "h1", "host-1", KindService, bad, f.now)
	f.observe(cpu(10), 31*time.Second)
	f.ev.ObserveSet(ctx, "h1", "host-1", KindService, bad, f.now)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Firing || ev[0].Alert.Target != "backup" {
		t.Fatalf("auslösen trotz metriken: %+v", ev)
	}
	f.observe(cpu(10), time.Second)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("metrik hat dienst-alarm beendet: %+v", ev)
	}
}

// Weitere Gründe (Wartung, Abhängigkeit) wirken wie die Stummschaltung.
func TestQuiet(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.ev.SetRules(ctx, []store.AlertRule{{Kind: KindCPU, Enabled: true, Threshold: 90, ForSec: 0}}, f.now)
	quiet := true
	f.ev.SetQuiet(func(host string, _ time.Time) (string, bool) { return "Wartung „Patchday“", quiet && host == "h1" })

	f.observe(cpu(95), 10*time.Second)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("während wartung gemeldet: %+v", ev)
	}
	active, _ := f.st.Alerts(ctx, true, time.Time{}, 10)
	if len(active) != 1 || active[0].Silenced != "Wartung „Patchday“" || active[0].NotifiedAt != nil {
		t.Fatalf("erfasst mit grund: %+v", active)
	}
	quiet = false
	f.ev.Tick(ctx, f.now)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Firing {
		t.Fatalf("nachmeldung nach wartung: %+v", ev)
	}
}

func TestAck(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.ev.SetRules(ctx, []store.AlertRule{{Kind: KindCPU, Enabled: true, Threshold: 90, ForSec: 0}}, f.now)
	f.ev.SetReminder(ctx, 1)
	f.observe(cpu(95), 10*time.Second)
	f.take()
	active, _ := f.st.Alerts(ctx, true, time.Time{}, 10)
	a, err := f.ev.Ack(ctx, active[0].ID, true, "admin", "Lüfter getauscht", f.now)
	if err != nil || a.AckedAt == nil || a.AckedBy != "admin" {
		t.Fatalf("quittieren: %+v %v", a, err)
	}
	f.now = f.now.Add(2 * time.Hour)
	f.ev.Tick(ctx, f.now)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("erinnerung trotz quittierung: %+v", ev)
	}
	// Nach dem Neustart des Servers bleibt die Quittierung erhalten.
	f.ev = f.open()
	f.ev.SetReminder(ctx, 1)
	f.ev.Tick(ctx, f.now)
	if ev := f.take(); len(ev) != 0 {
		t.Fatalf("erinnerung nach neustart: %+v", ev)
	}
	if _, err := f.ev.Ack(ctx, active[0].ID, false, "", "", f.now); err != nil {
		t.Fatal(err)
	}
	f.ev.Tick(ctx, f.now)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Reminder {
		t.Fatalf("erinnerung nach aufheben: %+v", ev)
	}
	if _, err := f.ev.Ack(ctx, 9999, true, "admin", "", f.now); !errors.Is(err, ErrNotActive) {
		t.Errorf("unbekannter alarm: %v", err)
	}
	// Ende wird trotz Quittierung gemeldet (Beginn war gemeldet).
	f.observe(cpu(10), 10*time.Second)
	if ev := f.take(); len(ev) != 1 || ev[0].Type != Resolved {
		t.Fatalf("ende: %+v", ev)
	}
}

func TestDiskForecast(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	obs := func(days map[string]float64) []Event {
		f.ev.ObserveForecast(ctx, "h1", "host-1", days, f.now)
		f.now = f.now.Add(time.Hour)
		return f.take()
	}

	// Standard: unter 7 Tagen sofort.
	if ev := obs(map[string]float64{"/": 30, "/var": 8}); len(ev) != 0 {
		t.Fatalf("zu früh: %+v", ev)
	}
	ev := obs(map[string]float64{"/": 30, "/var": 5.4})
	if len(ev) != 1 || ev[0].Alert.Target != "/var" || ev[0].Title() != "host-1: Disk /var in ~5 Tagen voll" ||
		!strings.Contains(ev[0].Body(), "Warnschwelle 7 Tage") {
		t.Fatalf("auslösen: %+v", ev)
	}
	// Hysterese: endet erst über 7 × 1,5 = 10,5 Tagen.
	if ev := obs(map[string]float64{"/": 30, "/var": 9}); len(ev) != 0 {
		t.Fatalf("flackert: %+v", ev)
	}
	if ev := obs(map[string]float64{"/": 30, "/var": 11}); len(ev) != 1 || ev[0].Type != Resolved ||
		ev[0].Title() != "host-1: Disk /var – Prognose wieder unkritisch" {
		t.Fatalf("ende: %+v", ev)
	}
	// Wächst ein Mountpoint nicht mehr (fehlt in der Prognose), endet sein Alarm.
	obs(map[string]float64{"/": 0.5})
	if ev := obs(map[string]float64{}); len(ev) != 1 || ev[0].Type != Resolved || ev[0].Alert.Target != "/" {
		t.Fatalf("ohne prognose: %+v", ev)
	}

	// Je Host anpassbar, Schwelle in Tagen.
	if err := f.ev.SetHostRules(ctx, "h1", []store.AlertRule{{Kind: KindDiskForecast, Enabled: true, Threshold: 30}}, f.now); err != nil {
		t.Fatal(err)
	}
	if ev := obs(map[string]float64{"/": 20}); len(ev) != 1 || ev[0].Alert.Threshold != 30 {
		t.Fatalf("host-schwelle: %+v", ev)
	}
	if ValidateRule(store.AlertRule{Kind: KindDiskForecast, Threshold: 400}) == nil {
		t.Error("schwelle 400 tage akzeptiert")
	}
	if fullIn(0) != "ist voll" || fullIn(0.3) != "in weniger als 1 Tag voll" || fullIn(1.9) != "in ~1 Tag voll" {
		t.Error("fullIn")
	}
}
