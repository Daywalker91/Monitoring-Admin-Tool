// Package checks führt externe Checks aus: HTTP(S) mit Statuscode und
// Zertifikatsablauf, Ping (ICMP) und TCP-Port. Die Checks legt der Nutzer in
// der GUI an; Ergebnisse landen in SQLite und in der Alarmauswertung.
package checks

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Arten von Checks.
const (
	KindHTTP = "http" // URL abrufen, Status prüfen, bei https Zertifikatsablauf
	KindPing = "ping" // ICMP-Echo
	KindTCP  = "tcp"  // TCP-Verbindung zu Host:Port
)

// Grenzen für Eingaben.
const (
	MaxChecks       = 100
	maxNameLen      = 64
	maxTargetLen    = 2048
	minInterval     = 30
	maxInterval     = 3600
	maxTimeout      = 30
	defaultInterval = 60
	defaultTimeout  = 10
	maxRedirects    = 5
	maxBodyBytes    = 64 << 10 // so viel wird vom Inhalt gelesen (und verworfen)
	userAgent       = "Monitoring-Admin-Tool-Check"
)

// AlarmID ist die Kennung, unter der die Alarme eines Checks geführt werden.
func AlarmID(id int64) string { return "check:" + strconv.FormatInt(id, 10) }

// Validate prüft einen Check und setzt Standardwerte. Die Werte stammen aus der
// GUI und gelten als nicht vertrauenswürdig.
func Validate(c *store.Check) error {
	c.Name = strings.TrimSpace(c.Name)
	c.Target = strings.TrimSpace(c.Target)
	if c.Name == "" || len(c.Name) > maxNameLen || !utf8.ValidString(c.Name) || strings.IndexFunc(c.Name, unicode.IsControl) >= 0 {
		return fmt.Errorf("name: 1–%d zeichen, keine steuerzeichen", maxNameLen)
	}
	if c.Target == "" || len(c.Target) > maxTargetLen || !utf8.ValidString(c.Target) ||
		strings.IndexFunc(c.Target, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fmt.Errorf("ziel fehlt oder enthält leerzeichen")
	}
	switch c.Kind {
	case KindHTTP:
		u, err := url.Parse(c.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("url muss mit http:// oder https:// beginnen")
		}
		if u.User != nil {
			return fmt.Errorf("zugangsdaten in der url sind nicht erlaubt")
		}
		if c.ExpectStatus != 0 && (c.ExpectStatus < 100 || c.ExpectStatus > 599) {
			return fmt.Errorf("erwarteter status muss zwischen 100 und 599 liegen (0 = 200–399)")
		}
		if u.Scheme != "https" {
			c.SkipVerify = false
		}
	case KindPing:
		if !validHost(c.Target) {
			return fmt.Errorf("ungültiger host")
		}
		c.ExpectStatus, c.SkipVerify = 0, false
	case KindTCP:
		host, port, err := net.SplitHostPort(c.Target)
		if p, perr := strconv.Atoi(port); err != nil || perr != nil || p < 1 || p > 65535 || !validHost(host) {
			return fmt.Errorf("ziel im format host:port angeben")
		}
		c.ExpectStatus, c.SkipVerify = 0, false
	default:
		return fmt.Errorf("unbekannte art %q", c.Kind)
	}
	if c.IntervalSec == 0 {
		c.IntervalSec = defaultInterval
	}
	if c.TimeoutSec == 0 {
		c.TimeoutSec = defaultTimeout
	}
	if c.IntervalSec < minInterval || c.IntervalSec > maxInterval {
		return fmt.Errorf("intervall muss zwischen %d und %d s liegen", minInterval, maxInterval)
	}
	if c.TimeoutSec < 1 || c.TimeoutSec > maxTimeout || c.TimeoutSec >= c.IntervalSec {
		return fmt.Errorf("timeout muss zwischen 1 und %d s liegen und kürzer als das intervall sein", maxTimeout)
	}
	return nil
}

// validHost erlaubt Hostnamen und IP-Adressen (auch IPv6 ohne Klammern).
func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}

// Probe führt einen Check einmal aus.
func Probe(ctx context.Context, c store.Check) store.CheckResult {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	var r store.CheckResult
	switch c.Kind {
	case KindHTTP:
		r = probeHTTP(ctx, c)
	case KindTCP:
		r = probeTCP(ctx, c.Target)
	case KindPing:
		r = probePing(ctx, c.Target)
	default:
		r.Error = "unbekannte art"
	}
	r.TS = start.Unix()
	if r.Error != "" && ctx.Err() != nil {
		r.Error = fmt.Sprintf("keine antwort nach %d s", c.TimeoutSec)
	}
	return r
}

func probeHTTP(ctx context.Context, c store.Check) store.CheckResult {
	var r store.CheckResult
	transport := &http.Transport{
		Proxy:               nil,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: time.Duration(c.TimeoutSec) * time.Second,
		// SkipVerify nur auf ausdrücklichen Wunsch (selbst signierte Dienste im LAN);
		// der Ablauf des Zertifikats wird trotzdem ausgewertet.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.SkipVerify}, //nolint:gosec
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("mehr als %d weiterleitungen", maxRedirects)
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Target, nil)
	if err != nil {
		r.Error = "ungültige url"
		return r
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		r.Error = shortError(err)
		// Abgelaufenes oder unbekanntes Zertifikat: Ablauf trotzdem melden.
		var inv x509.CertificateInvalidError
		var unk x509.UnknownAuthorityError
		var host x509.HostnameError
		switch {
		case errors.As(err, &inv) && inv.Cert != nil:
			r.CertNotAfter = ptr(inv.Cert.NotAfter.Unix())
		case errors.As(err, &unk) && unk.Cert != nil:
			r.CertNotAfter = ptr(unk.Cert.NotAfter.Unix())
		case errors.As(err, &host) && host.Certificate != nil:
			r.CertNotAfter = ptr(host.Certificate.NotAfter.Unix())
		}
		return r
	}
	latency := float64(time.Since(start).Microseconds()) / 1000
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	resp.Body.Close()
	r.LatencyMS = &latency
	r.Status = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.CertNotAfter = ptr(resp.TLS.PeerCertificates[0].NotAfter.Unix())
	}
	if c.ExpectStatus == 0 {
		r.OK = resp.StatusCode >= 200 && resp.StatusCode < 400
	} else {
		r.OK = resp.StatusCode == c.ExpectStatus
	}
	if !r.OK {
		r.Error = "status " + strconv.Itoa(resp.StatusCode)
	}
	return r
}

func probeTCP(ctx context.Context, target string) store.CheckResult {
	var r store.CheckResult
	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		r.Error = shortError(err)
		return r
	}
	latency := float64(time.Since(start).Microseconds()) / 1000
	conn.Close()
	r.OK, r.LatencyMS = true, &latency
	return r
}

// shortError übersetzt häufige Fehler in kurze deutsche Meldungen; sonst
// bleibt der Kern der Originalmeldung (ohne wiederholte URL).
func shortError(err error) string {
	var dns *net.DNSError
	var inv x509.CertificateInvalidError
	var unk x509.UnknownAuthorityError
	var host x509.HostnameError
	var ne net.Error
	switch {
	case errors.As(err, &dns) && dns.IsNotFound:
		return "host nicht gefunden"
	case errors.As(err, &inv) && inv.Reason == x509.Expired:
		return "zertifikat abgelaufen"
	case errors.As(err, &inv):
		return "zertifikat ungültig: " + inv.Error()
	case errors.As(err, &unk):
		return "zertifikat nicht vertrauenswürdig (selbst signiert?)"
	case errors.As(err, &host):
		return "zertifikat passt nicht zum namen"
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused"):
		return "verbindung abgelehnt"
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		return "ziel nicht erreichbar"
	case errors.As(err, &ne) && ne.Timeout():
		return "keine antwort (zeitüberschreitung)"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func ptr[T any](v T) *T { return &v }
