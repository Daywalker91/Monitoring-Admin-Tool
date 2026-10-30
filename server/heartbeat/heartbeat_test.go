package heartbeat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnv(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p, _, err := FromEnv(env(nil), log)
	if err != nil || p != nil {
		t.Fatalf("ohne URL: p=%v err=%v, erwartet aus", p, err)
	}
	p, warn, err := FromEnv(env(map[string]string{"MAT_HEARTBEAT_URL": "https://ipv64.net/health.php"}), log)
	if err != nil || len(warn) != 0 {
		t.Fatalf("gültige URL: %v %v", err, warn)
	}
	if p.Interval() != DefaultInterval || !p.payload || p.Host() != "ipv64.net" {
		t.Fatalf("standardwerte: %+v", p)
	}
	if _, warn, _ = FromEnv(env(map[string]string{"MAT_HEARTBEAT_URL": "http://lan/x"}), log); len(warn) != 1 {
		t.Fatalf("http ohne warnung")
	}
	for name, m := range map[string]map[string]string{
		"token ohne url":   {"MAT_HEARTBEAT_TOKEN": "x"},
		"schema":           {"MAT_HEARTBEAT_URL": "ftp://x/y"},
		"zugangsdaten":     {"MAT_HEARTBEAT_URL": "https://a:b@x/y"},
		"intervall kaputt": {"MAT_HEARTBEAT_URL": "https://x/y", "MAT_HEARTBEAT_INTERVAL": "drei"},
		"intervall kurz":   {"MAT_HEARTBEAT_URL": "https://x/y", "MAT_HEARTBEAT_INTERVAL": "10s"},
		"payload":          {"MAT_HEARTBEAT_URL": "https://x/y", "MAT_HEARTBEAT_PAYLOAD": "vielleicht"},
	} {
		if _, _, err := FromEnv(env(m), log); err == nil {
			t.Errorf("%s: kein fehler", name)
		}
	}
}

type request struct {
	method, auth, payload string
}

// fakeGuard nimmt Meldungen an und antwortet mit status.
func fakeGuard(t *testing.T, status int) (*httptest.Server, func() []request) {
	var mu sync.Mutex
	var reqs []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		reqs = append(reqs, request{r.Method, r.Header.Get("Authorization"), r.PostForm.Get("payload")})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return append([]request(nil), reqs...)
	}
}

func newPinger(t *testing.T, url string, extra map[string]string, out io.Writer) *Pinger {
	m := map[string]string{"MAT_HEARTBEAT_URL": url, "MAT_HEARTBEAT_TOKEN": "geheim123"}
	for k, v := range extra {
		m[k] = v
	}
	p, _, err := FromEnv(env(m), slog.New(slog.NewTextHandler(out, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p.delay = 0
	return p
}

func okStatus(context.Context) (string, error) { return "MAT 1.0 · 2/3 Hosts online", nil }

func TestBeatPayload(t *testing.T) {
	srv, reqs := fakeGuard(t, http.StatusOK)
	p := newPinger(t, srv.URL, nil, io.Discard)
	if err := p.beat(context.Background(), okStatus); err != nil {
		t.Fatal(err)
	}
	got := reqs()
	if len(got) != 1 || got[0].method != http.MethodPost || got[0].auth != "Bearer geheim123" ||
		got[0].payload != "MAT 1.0 · 2/3 Hosts online" {
		t.Fatalf("anfrage: %+v", got)
	}
}

func TestBeatPlainGET(t *testing.T) {
	srv, reqs := fakeGuard(t, http.StatusOK)
	p := newPinger(t, srv.URL, map[string]string{"MAT_HEARTBEAT_PAYLOAD": "off"}, io.Discard)
	if err := p.beat(context.Background(), okStatus); err != nil {
		t.Fatal(err)
	}
	if got := reqs(); len(got) != 1 || got[0].method != http.MethodGet {
		t.Fatalf("anfrage: %+v", got)
	}
}

func TestBeatErrors(t *testing.T) {
	for status, want := range map[int]string{401: "token", 429: "kontingent", 500: "HTTP 500"} {
		srv, _ := fakeGuard(t, status)
		p := newPinger(t, srv.URL, nil, io.Discard)
		if err := p.beat(context.Background(), okStatus); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: err=%v, erwartet %q", status, err, want)
		}
	}
	// Server nicht bereit: keine Meldung.
	srv, reqs := fakeGuard(t, http.StatusOK)
	p := newPinger(t, srv.URL, nil, io.Discard)
	bad := func(context.Context) (string, error) { return "", errors.New("db weg") }
	if err := p.beat(context.Background(), bad); err == nil || len(reqs()) != 0 {
		t.Fatalf("trotz fehler gemeldet: err=%v reqs=%d", err, len(reqs()))
	}
}

// syncBuffer ist ein bytes.Buffer, den Run und der Test gleichzeitig nutzen dürfen.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Weder URL (mit Token-Parameter) noch Token dürfen im Log landen.
func TestNoSecretsInLog(t *testing.T) {
	var buf syncBuffer
	p := newPinger(t, "http://127.0.0.1:1/health.php?token=urlgeheim", nil, &buf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, okStatus); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "heartbeat fehlgeschlagen") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	log := buf.String()
	if !strings.Contains(log, "heartbeat fehlgeschlagen") {
		t.Fatalf("kein fehler geloggt: %s", log)
	}
	if strings.Contains(log, "urlgeheim") || strings.Contains(log, "geheim123") {
		t.Fatalf("geheimnis im log: %s", log)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("aä", 2); got != "a" {
		t.Fatalf("truncate: %q", got)
	}
}

// Vor Ablauf der Startverzögerung geht nichts raus (Agenten verbinden sich noch).
func TestRunWaitsForDelay(t *testing.T) {
	srv, reqs := fakeGuard(t, http.StatusOK)
	p := newPinger(t, srv.URL, nil, io.Discard)
	p.delay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p.Run(ctx, okStatus)
	if n := len(reqs()); n != 0 {
		t.Fatalf("%d meldungen vor ablauf der verzögerung", n)
	}
}
