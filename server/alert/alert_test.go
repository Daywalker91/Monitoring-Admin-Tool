package alert

import (
	"context"
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
