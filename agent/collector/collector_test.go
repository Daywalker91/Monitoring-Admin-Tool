package collector

import (
	"context"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/net"
)

func TestRate(t *testing.T) {
	cases := []struct {
		prev, cur uint64
		secs      float64
		want      float64
	}{
		{100, 1100, 10, 100},
		{100, 100, 10, 0},
		{500, 100, 10, 0}, // Zähler-Reset
		{0, 100, 0, 0},    // kein Zeitabstand
	}
	for _, c := range cases {
		if got := rate(c.prev, c.cur, c.secs); got != c.want {
			t.Errorf("rate(%d,%d,%v) = %v, erwartet %v", c.prev, c.cur, c.secs, got, c.want)
		}
	}
}

func TestNetRates(t *testing.T) {
	c := New()
	t0 := time.Now()
	first := c.netRates([]net.IOCountersStat{
		{Name: "eth0", BytesRecv: 1000, BytesSent: 500, PacketsRecv: 10, PacketsSent: 5},
		{Name: "lo", BytesRecv: 9999, BytesSent: 9999},
		{Name: "idle0"},
	}, t0)
	if len(first) != 1 || first[0].Name != "eth0" || first[0].RxBps != 0 {
		t.Fatalf("erste Messung: %+v", first)
	}

	second := c.netRates([]net.IOCountersStat{
		{Name: "eth0", BytesRecv: 3000, BytesSent: 1500, PacketsRecv: 30, PacketsSent: 15},
	}, t0.Add(10*time.Second))
	got := second[0]
	if got.RxBps != 200 || got.TxBps != 100 || got.RxPps != 2 || got.TxPps != 1 {
		t.Fatalf("zweite Messung: %+v", got)
	}
}

// TestCollectLive prüft nur, dass auf dem Testrechner sinnvolle Werte entstehen.
func TestCollectLive(t *testing.T) {
	ctx := context.Background()
	c := New()
	c.Prime(ctx)

	m, err := c.Metrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mem.Total == 0 {
		t.Error("RAM-Gesamtgröße ist 0")
	}
	if len(m.CPU.PerCore) == 0 {
		t.Error("keine Werte pro Kern")
	}

	p, err := c.Processes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.TopMem) == 0 || len(p.TopMem) > 10 {
		t.Errorf("TopMem hat %d Einträge", len(p.TopMem))
	}

	h := HostInfo(ctx)
	if h.Hostname == "" || h.CPUCores == 0 {
		t.Errorf("HostInfo unvollständig: %+v", h)
	}
}
