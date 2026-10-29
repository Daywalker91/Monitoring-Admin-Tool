package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\"b\\c\nd"); got != `a\"b\\c\nd` {
		t.Fatalf("escapeLabel = %q", got)
	}
}

func TestMetricsDisabled(t *testing.T) {
	ts := newTestServer(t)
	if code := status(t, newClient(t), "GET", ts.URL+"/metrics", ""); code != http.StatusNotFound {
		t.Fatalf("ohne token: %d, erwartet 404", code)
	}
}

func TestMetrics(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(st, auth.New(st), "secret", fstest.MapFS{}, log).WithMetricsToken("prom-token")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Ein verbundener Linux-Host und ein Offline-Host.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st.UpsertHost(ctx, proto.Hello{AgentID: "alt", Hostname: "alt", OS: "linux", IntervalSec: 10}, "", time.Now())
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for _, msg := range []struct {
		typ string
		v   any
	}{
		{proto.TypeHello, proto.Hello{AgentID: "pi-1", Hostname: "pi \"eins\"", OS: "linux", IntervalSec: 10}},
		{proto.TypeMetrics, proto.Metrics{
			CPU:     proto.CPU{TotalPct: 12.5, PerCore: []float64{10, 15}, Load1: 0.5},
			Mem:     proto.Memory{Total: 1000, Used: 400},
			Net:     []proto.NetIface{{Name: "eth0", RxBps: 100}},
			Disks:   []proto.DiskUsage{{Mount: "/", FSType: "ext4", Total: 2000, Used: 500}},
			CPUTemp: func() *float64 { v := 47.5; return &v }(),
			Temps:   []proto.Temp{{Name: "cpu-thermal", Celsius: 47.5}},
		}},
	} {
		data, _ := proto.Encode(msg.typ, msg.v)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			t.Fatal(err)
		}
	}

	c := newClient(t)
	if code := status(t, c, "GET", ts.URL+"/metrics", ""); code != http.StatusUnauthorized {
		t.Fatalf("ohne bearer: %d, erwartet 401", code)
	}

	var body string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer prom-token")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body = string(b)
		if strings.Contains(body, "mat_cpu_usage_percent") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, want := range []string{
		`mat_host_up{host="pi-1",hostname="pi \"eins\""} 1`,
		`mat_host_up{host="alt",hostname="alt"} 0`,
		`mat_cpu_usage_percent{host="pi-1"} 12.5`,
		`mat_cpu_core_usage_percent{host="pi-1",core="1"} 15`,
		`mat_load1{host="pi-1"} 0.5`,
		`mat_memory_used_bytes{host="pi-1"} 400`,
		`mat_network_receive_bytes_per_second{host="pi-1",interface="eth0"} 100`,
		`mat_filesystem_used_bytes{host="pi-1",mountpoint="/",fstype="ext4"} 500`,
		"# TYPE mat_network_receive_errors_total counter",
		`mat_cpu_temperature_celsius{host="pi-1"} 47.5`,
		`mat_sensor_temperature_celsius{host="pi-1",sensor="cpu-thermal"} 47.5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fehlt: %s", want)
		}
	}
	if strings.Contains(body, `mat_cpu_usage_percent{host="alt"}`) {
		t.Error("offline-host liefert messwerte")
	}
	if strings.Count(body, "# HELP mat_host_up ") != 1 {
		t.Error("HELP-zeile doppelt oder fehlend")
	}
}
