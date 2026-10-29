package checks

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestValidate(t *testing.T) {
	good := []store.Check{
		{Name: "Web", Kind: KindHTTP, Target: "https://example.org/health"},
		{Name: "NAS", Kind: KindPing, Target: "192.168.1.10"},
		{Name: "v6", Kind: KindPing, Target: "fd00::1"},
		{Name: "SSH", Kind: KindTCP, Target: "nas.lan:22", IntervalSec: 300, TimeoutSec: 5},
		{Name: "Status", Kind: KindHTTP, Target: "http://x.lan", ExpectStatus: 401},
	}
	for _, c := range good {
		if err := Validate(&c); err != nil {
			t.Errorf("%s abgelehnt: %v", c.Name, err)
		}
		if c.IntervalSec == 0 || c.TimeoutSec == 0 {
			t.Errorf("%s: keine standardwerte", c.Name)
		}
	}
	bad := []store.Check{
		{Name: "", Kind: KindHTTP, Target: "https://x"},
		{Name: "a\nb", Kind: KindHTTP, Target: "https://x"},
		{Name: "gepr\xfcft", Kind: KindHTTP, Target: "https://x"},
		{Name: "x", Kind: KindHTTP, Target: "ftp://x"},
		{Name: "x", Kind: KindHTTP, Target: "file:///etc/passwd"},
		{Name: "x", Kind: KindHTTP, Target: "https://user:pw@x"},
		{Name: "x", Kind: KindHTTP, Target: "https://x", ExpectStatus: 42},
		{Name: "x", Kind: KindPing, Target: "a b"},
		{Name: "x", Kind: KindPing, Target: "x;rm -rf /"},
		{Name: "x", Kind: KindTCP, Target: "nas.lan"},
		{Name: "x", Kind: KindTCP, Target: "nas.lan:70000"},
		{Name: "x", Kind: "shell", Target: "x"},
		{Name: "x", Kind: KindPing, Target: "x", IntervalSec: 5},
		{Name: "x", Kind: KindPing, Target: "x", IntervalSec: 30, TimeoutSec: 30},
	}
	for i, c := range bad {
		if err := Validate(&c); err == nil {
			t.Errorf("%d: ungültiger check akzeptiert: %+v", i, c)
		}
	}
	c := store.Check{Name: "x", Kind: KindHTTP, Target: "http://x", SkipVerify: true}
	if Validate(&c); c.SkipVerify {
		t.Error("skip_verify bei http gesetzt")
	}
}

func TestProbeHTTP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fehler" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("User-Agent") != userAgent {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsSrv.Close()
	notAfter := tlsSrv.Certificate().NotAfter.Unix()

	cases := []struct {
		name   string
		check  store.Check
		ok     bool
		status int
		errHas string
		cert   bool
	}{
		{"ok", store.Check{Kind: KindHTTP, Target: plain.URL}, true, 200, "", false},
		{"503", store.Check{Kind: KindHTTP, Target: plain.URL + "/fehler"}, false, 503, "status 503", false},
		{"503 erwartet", store.Check{Kind: KindHTTP, Target: plain.URL + "/fehler", ExpectStatus: 503}, true, 503, "", false},
		{"selbst signiert", store.Check{Kind: KindHTTP, Target: tlsSrv.URL}, false, 0, "nicht vertrauenswürdig", true},
		{"selbst signiert erlaubt", store.Check{Kind: KindHTTP, Target: tlsSrv.URL, SkipVerify: true}, true, 200, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check.TimeoutSec = 5
			r := Probe(context.Background(), c.check)
			if r.OK != c.ok || r.Status != c.status || !strings.Contains(r.Error, c.errHas) {
				t.Fatalf("ergebnis: %+v", r)
			}
			if c.cert != (r.CertNotAfter != nil) || (c.cert && *r.CertNotAfter != notAfter) {
				t.Fatalf("zertifikat: %v", r.CertNotAfter)
			}
			if r.OK && r.LatencyMS == nil {
				t.Fatal("keine antwortzeit")
			}
		})
	}
}

func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	r := Probe(context.Background(), store.Check{Kind: KindTCP, Target: addr, TimeoutSec: 2})
	if !r.OK || r.LatencyMS == nil {
		t.Fatalf("offener port: %+v", r)
	}
	ln.Close()
	r = Probe(context.Background(), store.Check{Kind: KindTCP, Target: addr, TimeoutSec: 2})
	if r.OK || r.Error != "verbindung abgelehnt" {
		t.Fatalf("geschlossener port: %+v", r)
	}
}

func TestRunner(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := store.Check{Name: "a", Kind: KindTCP, Target: "x:1", IntervalSec: 60, TimeoutSec: 5, Enabled: true}
	if err := st.CreateCheck(ctx, &c); err != nil {
		t.Fatal(err)
	}
	off := store.Check{Name: "b", Kind: KindTCP, Target: "y:1", IntervalSec: 60, TimeoutSec: 5}
	st.CreateCheck(ctx, &off)

	var mu sync.Mutex
	var got []int64
	done := make(chan struct{}, 10)
	r := NewRunner(st, slog.New(slog.NewTextHandler(io.Discard, nil)), func(_ context.Context, c store.Check, _ store.CheckResult) {
		mu.Lock()
		got = append(got, c.ID)
		mu.Unlock()
		done <- struct{}{}
	})
	r.probe = func(_ context.Context, c store.Check) store.CheckResult {
		return store.CheckResult{TS: time.Now().Unix(), OK: true}
	}
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	go r.Run(ctx)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("check lief nicht")
	}
	time.Sleep(1500 * time.Millisecond) // abgeschalteter Check darf auch jetzt nicht laufen
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != c.ID {
		t.Fatalf("gelaufen: %v", got)
	}
	if last := r.Last(); !last[c.ID].OK {
		t.Fatalf("letztes ergebnis: %+v", last)
	}
	if saved, _ := st.LastCheckResults(ctx); !saved[c.ID].OK {
		t.Fatalf("nicht gespeichert: %+v", saved)
	}
}

func TestProbePing(t *testing.T) {
	r := Probe(context.Background(), store.Check{Kind: KindPing, Target: "127.0.0.1", TimeoutSec: 2})
	if strings.HasPrefix(r.Error, "ping nicht möglich") {
		t.Skip("kein icmp-socket in dieser umgebung:", r.Error)
	}
	if !r.OK || r.LatencyMS == nil {
		t.Fatalf("ping 127.0.0.1: %+v", r)
	}
	r = Probe(context.Background(), store.Check{Kind: KindPing, Target: "gibt-es-nicht.invalid", TimeoutSec: 2})
	if r.OK || r.Error != "host nicht gefunden" {
		t.Fatalf("unbekannter host: %+v", r)
	}
}
