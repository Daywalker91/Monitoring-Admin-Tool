package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestHostLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()

	h := proto.Hello{AgentID: "pi-1", Hostname: "pi", OS: "linux", Arch: "arm", IntervalSec: 10,
		Capabilities: []proto.Capability{proto.CapMetrics}}
	if err := s.UpsertHost(ctx, h, "10.0.0.5:1234", now); err != nil {
		t.Fatal(err)
	}
	h.AgentVersion = "1.1"
	if err := s.UpsertHost(ctx, h, "10.0.0.5:1235", now); err != nil {
		t.Fatal(err)
	}

	m := proto.Metrics{
		CPU:   proto.CPU{TotalPct: 42},
		Mem:   proto.Memory{UsedPct: 50},
		Net:   []proto.NetIface{{Name: "eth0", RxBps: 100, TxBps: 10}, {Name: "wlan0", RxBps: 50}},
		Disks: []proto.DiskUsage{{Mount: "/", UsedPct: 30}, {Mount: "/data", UsedPct: 80}},
	}
	raw, _ := json.Marshal(m)
	if err := s.SaveMetrics(ctx, "pi-1", m, raw, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProcesses(ctx, "pi-1", []byte(`{"top_cpu":[]}`), now); err != nil {
		t.Fatal(err)
	}

	hosts, err := s.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].AgentVersion != "1.1" || len(hosts[0].Capabilities) != 1 {
		t.Fatalf("unerwartete hosts: %+v", hosts)
	}
	var got proto.Metrics
	if err := json.Unmarshal(hosts[0].Metrics, &got); err != nil || got.CPU.TotalPct != 42 {
		t.Fatalf("letzter stand falsch: %s (%v)", hosts[0].Metrics, err)
	}

	hist, err := s.History(ctx, "pi-1", now.Add(-time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].NetRxBps != 150 || hist[0].DiskMaxPct != 80 {
		t.Fatalf("historie falsch: %+v", hist)
	}

	if found, err := s.DeleteHost(ctx, "pi-1"); err != nil || !found {
		t.Fatalf("DeleteHost: found=%v err=%v", found, err)
	}
	if found, _ := s.DeleteHost(ctx, "pi-1"); found {
		t.Fatal("zweites DeleteHost meldet found")
	}
	hosts, _ = s.ListHosts(ctx)
	hist, _ = s.History(ctx, "pi-1", time.Time{}, 0)
	if len(hosts) != 0 || len(hist) != 0 {
		t.Fatalf("nach dem Löschen: %d hosts, %d historie", len(hosts), len(hist))
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	if err := s.UpsertHost(ctx, proto.Hello{AgentID: "a", Hostname: "a", IntervalSec: 10}, "", now); err != nil {
		t.Fatal(err)
	}
	for _, age := range []time.Duration{8 * 24 * time.Hour, 6 * 24 * time.Hour, 0} {
		if err := s.SaveMetrics(ctx, "a", proto.Metrics{}, []byte("{}"), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Prune(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("gelöscht: %d, erwartet 1", n)
	}
	hist, _ := s.History(ctx, "a", time.Time{}, 0)
	if len(hist) != 2 {
		t.Fatalf("verbleibend: %d, erwartet 2", len(hist))
	}
}

func TestHistoryBuckets(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	base := time.Unix(1_800_000_000, 0) // durch 60 teilbar → definierte Fenstergrenzen
	s.UpsertHost(ctx, proto.Hello{AgentID: "a", Hostname: "a", IntervalSec: 10}, "", base)
	// 12 Punkte im 10-s-Abstand mit CPU 0, 10, …, 110 → zwei 60-s-Fenster.
	for i := 0; i < 12; i++ {
		m := proto.Metrics{CPU: proto.CPU{TotalPct: float64(i * 10)}}
		if err := s.SaveMetrics(ctx, "a", m, []byte("{}"), base.Add(time.Duration(i)*10*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := s.History(ctx, "a", base, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("fenster: %d, erwartet 2", len(hist))
	}
	if hist[0].TS != base.Unix() || hist[0].CPUPct != 25 || hist[1].CPUPct != 85 {
		t.Fatalf("mittelwerte falsch: %+v", hist)
	}
}
