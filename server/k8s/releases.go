package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// DefaultChannelsURL liefert die k3s-Release-Kanäle (stable, latest, v1.35 …).
const DefaultChannelsURL = "https://update.k3s.io/v1-release/channels"

const (
	releasesInterval = 6 * time.Hour
	releasesRetry    = 30 * time.Minute
	releasesTimeout  = 20 * time.Second
	maxChannelsBody  = 1 << 20
)

var (
	channelID  = regexp.MustCompile(`^(stable|latest|v\d+\.\d+)$`)
	k3sVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+\+k3s\d+$`)
)

// Releases hält die neuesten k3s-Versionen je Kanal. Es wird nur gelesen und
// angezeigt; aktualisiert wird der Cluster außerhalb dieses Tools.
type Releases struct {
	url    string
	client *http.Client
	log    *slog.Logger

	mu       sync.Mutex
	channels map[string]string
	err      string
}

// NewReleases erzeugt den Abruf für url (leer = DefaultChannelsURL).
func NewReleases(url string, log *slog.Logger) *Releases {
	if url == "" {
		url = DefaultChannelsURL
	}
	return &Releases{url: url, client: &http.Client{Timeout: releasesTimeout}, log: log}
}

// Run ruft sofort und danach alle 6 Stunden ab (nach einem Fehler früher).
func (r *Releases) Run(ctx context.Context) {
	for {
		wait := releasesInterval
		if err := r.Fetch(ctx); err != nil {
			r.log.Warn("k3s-versionen abrufen", "err", err)
			wait = releasesRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Fetch ruft die Kanäle einmal ab.
func (r *Releases) Fetch(ctx context.Context) error {
	channels, err := r.fetch(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.err = err.Error()
		return err
	}
	r.channels, r.err = channels, ""
	return nil
}

func (r *Releases) fetch(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("abruf: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("abruf: http %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID     string `json:"id"`
			Latest string `json:"latest"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxChannelsBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("antwort lesen: %w", err)
	}
	channels := map[string]string{}
	for _, c := range body.Data {
		if channelID.MatchString(c.ID) && k3sVersion.MatchString(c.Latest) {
			channels[c.ID] = c.Latest
		}
	}
	if channels["stable"] == "" {
		return nil, fmt.Errorf("antwort ohne kanal stable")
	}
	return channels, nil
}

// Channels liefert eine Kopie der Kanäle (nil = noch nicht abgerufen) und den
// letzten Fehler.
func (r *Releases) Channels() (map[string]string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channels == nil {
		return nil, r.err
	}
	out := make(map[string]string, len(r.channels))
	for k, v := range r.channels {
		out[k] = v
	}
	return out, r.err
}
