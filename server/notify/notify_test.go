package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNtfy(t *testing.T) {
	var got map[string]any
	var auth, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer ts.Close()

	n, insecure, err := NewNtfy(ts.URL+"/sub/alarme", "tk_geheim")
	if err != nil || !insecure {
		t.Fatalf("NewNtfy: %v insecure=%v", err, insecure)
	}
	if err := n.Send(context.Background(), Message{Title: "PC: CPU 95 %", Body: "Text", High: true, Tag: "warning"}); err != nil {
		t.Fatal(err)
	}
	if path != "/sub/" || auth != "Bearer tk_geheim" || got["topic"] != "alarme" || got["title"] != "PC: CPU 95 %" ||
		got["priority"] != float64(4) {
		t.Fatalf("anfrage: path=%q auth=%q body=%v", path, auth, got)
	}
}

func TestNtfyErrors(t *testing.T) {
	for _, u := range []string{"ftp://x/t", "https://x", "https://x/", "https://u:p@x/t", "https://x/t?a=1", "https://x/t%20b"} {
		if _, _, err := NewNtfy(u, ""); err == nil {
			t.Errorf("%q akzeptiert", u)
		}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer ts.Close()
	n, _, _ := NewNtfy(ts.URL+"/t", "")
	if err := n.Send(context.Background(), Message{}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("fehler: %v", err)
	}
}

// fakeSMTP ist ein minimaler SMTP-Server für Tests.
type fakeSMTP struct {
	ln       net.Listener
	tlsCfg   *tls.Config // nil = kein STARTTLS
	mu       sync.Mutex
	data     string
	authed   bool
	usedTLS  bool
	rcpts    []string
	finished chan struct{}
}

func newFakeSMTP(t *testing.T, tlsCfg *tls.Config) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, tlsCfg: tlsCfg, finished: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	defer close(f.finished)
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r, w := bufio.NewReader(conn), conn
	reply := func(s string) { io.WriteString(w, s+"\r\n") }
	reply("220 fake")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.Fields(line + " x")[0])
		switch cmd {
		case "EHLO":
			if f.tlsCfg != nil && !f.usedTLS {
				reply("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				reply("250-fake\r\n250 AUTH PLAIN")
			}
		case "STARTTLS":
			reply("220 go")
			tc := tls.Server(conn, f.tlsCfg)
			if tc.Handshake() != nil {
				return
			}
			conn, r, w = tc, bufio.NewReader(tc), tc
			f.usedTLS = true
		case "AUTH":
			f.authed = true
			reply("235 ok")
		case "MAIL":
			reply("250 ok")
		case "RCPT":
			f.rcpts = append(f.rcpts, strings.TrimSpace(line))
			reply("250 ok")
		case "DATA":
			reply("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			reply("250 ok")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("500 ?")
		}
	}
}

// selfSigned erzeugt ein Zertifikat für 127.0.0.1.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestSMTPStartTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	f := newFakeSMTP(t, &tls.Config{Certificates: []tls.Certificate{cert}})
	s, err := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: f.port(), Mode: SMTPStartTLS, Username: "u", Password: "p",
		From: "Monitor <mon@example.lan>", To: []string{"a@example.lan", " b@example.lan"}, rootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	m := sanitize(Message{Title: "PC-Büro: CPU 95 %\r\nBcc: x@evil", Body: "Zeile 1\nZeile 2 – äöü", High: true})
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	<-f.finished
	if !f.usedTLS || !f.authed || len(f.rcpts) != 2 {
		t.Fatalf("tls=%v auth=%v rcpts=%v", f.usedTLS, f.authed, f.rcpts)
	}
	header, body, _ := strings.Cut(f.data, "\r\n\r\n")
	if strings.Contains(header, "\r\nBcc:") || !strings.Contains(header, "Subject: =?utf-8?q?[Alarm]") {
		t.Fatalf("header: %q", header)
	}
	dec, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if !strings.Contains(string(dec), "Zeile 2 – äöü") {
		t.Fatalf("text: %q", dec)
	}
}

func TestSMTPRequiresStartTLS(t *testing.T) {
	f := newFakeSMTP(t, nil) // bietet kein STARTTLS an
	s, err := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: f.port(), Mode: SMTPStartTLS,
		From: "mon@example.lan", To: []string{"a@example.lan"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), Message{Title: "x"}); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("ohne starttls gesendet: %v", err)
	}
}

func TestSMTPConfigValidation(t *testing.T) {
	base := SMTPConfig{Host: "mail.example.lan", Port: 587, Mode: SMTPStartTLS, From: "m@x.lan", To: []string{"a@x.lan"}}
	cases := map[string]func(*SMTPConfig){
		"klartext extern": func(c *SMTPConfig) { c.Mode = SMTPNone },
		"modus":           func(c *SMTPConfig) { c.Mode = "ssl" },
		"absender":        func(c *SMTPConfig) { c.From = "kein@" },
		"empfänger":       func(c *SMTPConfig) { c.To = nil },
		"nur passwort":    func(c *SMTPConfig) { c.Password = "p" },
		"port":            func(c *SMTPConfig) { c.Port = 0 },
	}
	for name, mod := range cases {
		c := base
		mod(&c)
		if _, err := NewSMTP(c); err == nil {
			t.Errorf("%s: akzeptiert", name)
		}
	}
	local := base
	local.Host, local.Mode = "127.0.0.1", SMTPNone
	if _, err := NewSMTP(local); err != nil {
		t.Errorf("klartext zu localhost: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	chans, _, err := FromEnv(env(nil))
	if err != nil || len(chans) != 0 {
		t.Fatalf("leer: %v %v", chans, err)
	}
	chans, warn, err := FromEnv(env(map[string]string{
		"MAT_NTFY_URL": "http://ntfy.lan/alarme", "MAT_SMTP_HOST": "mail.lan", "MAT_SMTP_TLS": "tls",
		"MAT_SMTP_FROM": "m@x.lan", "MAT_SMTP_TO": "a@x.lan,b@x.lan"}))
	if err != nil || len(chans) != 2 || len(warn) != 1 {
		t.Fatalf("beide: %v %v %v", chans, warn, err)
	}
	if s := chans[1].(*SMTP); s.cfg.Port != 465 || len(s.to) != 2 {
		t.Fatalf("smtp: %+v", s.cfg)
	}
	if _, _, err := FromEnv(env(map[string]string{"MAT_NTFY_TOKEN": "x"})); err == nil {
		t.Error("token ohne url akzeptiert")
	}
}

type fakeChannel struct {
	mu    sync.Mutex
	fails int
	got   []Message
	sent  chan struct{}
}

func (f *fakeChannel) Name() string { return "fake" }
func (f *fakeChannel) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("kaputt")
	}
	f.got = append(f.got, m)
	f.sent <- struct{}{}
	return nil
}

func TestDispatcherRetries(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = old })

	ch := &fakeChannel{fails: 2, sent: make(chan struct{}, 1)}
	d := NewDispatcher(slog.New(slog.NewTextHandler(io.Discard, nil)), ch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.Enqueue(Message{Title: "a\x00b"})
	select {
	case <-ch.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("nicht zugestellt")
	}
	if ch.got[0].Title != "a b" {
		t.Errorf("steuerzeichen nicht entfernt: %q", ch.got[0].Title)
	}

	res := d.Test(ctx, Message{Title: "test"})
	if len(res) != 1 || !res[0].OK || res[0].Channel != "fake" {
		t.Fatalf("test: %+v", res)
	}
}

func TestSanitizeLimit(t *testing.T) {
	m := sanitize(Message{Title: strings.Repeat("ä", 150)})
	if len(m.Title) > 210 || !strings.HasSuffix(m.Title, "…") {
		t.Fatalf("länge %d: %q", len(m.Title), m.Title)
	}
}
