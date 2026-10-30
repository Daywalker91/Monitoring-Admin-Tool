// Package heartbeat meldet einem externen Wächter (z. B. ipv64.net-Healthcheck,
// healthchecks.io) regelmäßig, dass der Server läuft. Bleibt die Meldung aus –
// Server abgestürzt, Router oder Internet weg –, alarmiert der Wächter von
// außen. MAT selbst kann in diesem Fall nichts mehr melden.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultInterval hält bei ipv64.net (1024 Pings/Tag) Abstand zum Kontingent.
	DefaultInterval = 3 * time.Minute
	minInterval     = 30 * time.Second
	timeout         = 10 * time.Second
	maxPayload      = 200
	// defaultStartDelay gibt den Agenten nach einem Neustart Zeit zum Verbinden,
	// sonst meldet die erste Status-Zeile „0/N Hosts online“.
	defaultStartDelay = time.Minute
)

// StatusFunc liefert einen kurzen Status für das Event-Log des Wächters. Ein
// Fehler unterdrückt die Meldung: Ein Server ohne Datenbank gilt nicht als lebendig.
type StatusFunc func(ctx context.Context) (string, error)

// Pinger ruft die URL des Wächters im festen Abstand auf.
type Pinger struct {
	url      string
	token    string
	interval time.Duration
	delay    time.Duration // Wartezeit vor der ersten Meldung
	payload  bool
	client   *http.Client
	log      *slog.Logger
}

// FromEnv liest die Konfiguration. Ohne MAT_HEARTBEAT_URL ist der Heartbeat aus (nil).
//
//	MAT_HEARTBEAT_URL       URL des Wächters, z. B. https://ipv64.net/health.php
//	MAT_HEARTBEAT_TOKEN     optional: Token, wird als Authorization: Bearer gesendet
//	MAT_HEARTBEAT_INTERVAL  Abstand, Standard 3m, mindestens 30s
//	MAT_HEARTBEAT_PAYLOAD   off: nur GET ohne Status (Standard: POST mit Feld payload)
//
// URL und Token werden nie geloggt, beide wirken wie ein Passwort.
func FromEnv(getenv func(string) string, log *slog.Logger) (p *Pinger, warnings []string, err error) {
	raw := getenv("MAT_HEARTBEAT_URL")
	if raw == "" {
		if getenv("MAT_HEARTBEAT_TOKEN") != "" {
			return nil, nil, fmt.Errorf("MAT_HEARTBEAT_TOKEN ohne MAT_HEARTBEAT_URL gesetzt")
		}
		return nil, nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, nil, fmt.Errorf("MAT_HEARTBEAT_URL muss mit https:// beginnen")
	}
	if u.User != nil {
		return nil, nil, fmt.Errorf("MAT_HEARTBEAT_URL darf keine zugangsdaten enthalten (MAT_HEARTBEAT_TOKEN nutzen)")
	}
	if u.Scheme == "http" {
		warnings = append(warnings, "MAT_HEARTBEAT_URL ist unverschlüsselt (http) – nur im LAN verwenden")
	}
	interval := DefaultInterval
	if v := getenv("MAT_HEARTBEAT_INTERVAL"); v != "" {
		if interval, err = time.ParseDuration(v); err != nil {
			return nil, nil, fmt.Errorf("MAT_HEARTBEAT_INTERVAL: %w", err)
		}
		if interval < minInterval {
			return nil, nil, fmt.Errorf("MAT_HEARTBEAT_INTERVAL muss mindestens %s sein", minInterval)
		}
	}
	payload := true
	switch strings.ToLower(getenv("MAT_HEARTBEAT_PAYLOAD")) {
	case "", "on":
	case "off":
		payload = false
	default:
		return nil, nil, fmt.Errorf("MAT_HEARTBEAT_PAYLOAD muss on oder off sein")
	}
	return &Pinger{
		url: raw, token: getenv("MAT_HEARTBEAT_TOKEN"), interval: interval, delay: defaultStartDelay, payload: payload,
		// Keine Weiterleitungen: Das Token soll nicht an einen anderen Host gehen.
		client: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		log: log,
	}, warnings, nil
}

// Host liefert nur den Hostnamen des Wächters (für das Start-Log).
func (p *Pinger) Host() string {
	if u, err := url.Parse(p.url); err == nil {
		return u.Hostname()
	}
	return ""
}

// Interval liefert den Abstand der Meldungen.
func (p *Pinger) Interval() time.Duration { return p.interval }

// Run meldet nach der Startverzögerung und danach im Abstand interval, bis ctx
// endet. Fehlschläge werden nur geloggt; der nächste Takt ist der neue Versuch.
func (p *Pinger) Run(ctx context.Context, status StatusFunc) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(p.delay):
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()
	failing := false
	for {
		err := p.beat(ctx, status)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			p.log.Warn("heartbeat fehlgeschlagen", "host", p.Host(), "err", err)
			failing = true
		case failing:
			p.log.Info("heartbeat wieder ok", "host", p.Host())
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Pinger) beat(ctx context.Context, status StatusFunc) error {
	text, err := status(ctx)
	if err != nil {
		return fmt.Errorf("server nicht bereit, keine meldung: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var req *http.Request
	if p.payload && text != "" {
		form := url.Values{"payload": {truncate(text, maxPayload)}}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, p.url, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	}
	if err != nil {
		return fmt.Errorf("anfrage bauen: %w", err)
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	req.Header.Set("User-Agent", "mat-server")
	resp, err := p.client.Do(req)
	if err != nil {
		// url.Error enthält die URL samt Token-Parameter – nicht weitergeben.
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			err = uerr.Err
		}
		return fmt.Errorf("senden: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("HTTP %d – token falsch?", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("HTTP 429 – kontingent erschöpft, MAT_HEARTBEAT_INTERVAL erhöhen")
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// truncate kürzt auf höchstens n Bytes, ohne ein UTF-8-Zeichen zu zerschneiden.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
