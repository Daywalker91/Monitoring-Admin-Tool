// Package config lädt die Agent-Konfiguration aus einer JSON-Datei und Flags.
//
// Reihenfolge (spätere gewinnt): Standardwerte → Datei (-config) → Umgebung → Flags.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Config ist die Agent-Konfiguration.
type Config struct {
	ServerURL          string `json:"server_url"` // z. B. ws://server:8080/ws/agent
	Token              string `json:"token"`
	AgentID            string `json:"agent_id"` // Standard: Hostname
	IntervalSec        int    `json:"interval_sec"`
	ProcessIntervalSec int    `json:"process_interval_sec"`
	LogLevel           string `json:"log_level"` // debug, info, warn, error
	// CAFile ist ein PEM-Zertifikat, dem bei wss:// vertraut wird (z. B. das
	// selbst signierte Server-Zertifikat). Leer = System-Zertifikate.
	CAFile string `json:"ca_file"`
	// Jobs schaltet ändernde Admin-Aktionen frei, z. B. ["services", "packages",
	// "reboot"]. Nur in der Datei, nicht per Flag: ohne Eintrag meldet der Agent
	// diese Fähigkeiten nicht, und der Server kann sie nicht auslösen.
	Jobs []proto.Capability `json:"jobs"`
	// LogSources gibt Logs zum Lesen frei, z. B. ["journal:sshd", "journal:kernel",
	// "eventlog:System"]. Ohne Eintrag kann der Server keine Logs lesen.
	LogSources []string `json:"log_sources"`
}

// jobCapabilities sind die Fähigkeiten, die sich über Jobs freischalten lassen.
var jobCapabilities = []proto.Capability{proto.CapServices, proto.CapPackages, proto.CapReboot}

// Defaults liefert die Standardkonfiguration.
func Defaults() Config {
	return Config{IntervalSec: 10, ProcessIntervalSec: 30, LogLevel: "info"}
}

// DefaultPath ist der feste Ort der Konfigurationsdatei, den Dienst-Installation
// und Install-Skript verwenden.
func DefaultPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "mat-agent", "agent.json")
	}
	return "/etc/mat-agent/agent.json"
}

// Load liest Datei, Umgebung (MAT_AGENT_TOKEN) und Flags ein. Ohne -config und
// -server wird DefaultPath gelesen, falls die Datei existiert.
func Load(args []string) (Config, error) {
	cfg := Defaults()

	fs := flag.NewFlagSet("mat-agent", flag.ContinueOnError)
	path := fs.String("config", "", "Pfad zur JSON-Konfigurationsdatei")
	server := fs.String("server", "", "Server-URL, z. B. ws://server:8080/ws/agent")
	token := fs.String("token", "", "Agent-Token (besser: MAT_AGENT_TOKEN oder Config-Datei)")
	id := fs.String("id", "", "Agent-ID (Standard: Hostname)")
	interval := fs.Int("interval", 0, "Messintervall in Sekunden (10–15 empfohlen)")
	procInterval := fs.Int("process-interval", 0, "Intervall für Top-Prozesse in Sekunden")
	logLevel := fs.String("log-level", "", "Log-Level: debug, info, warn, error")
	caFile := fs.String("ca", "", "PEM-Zertifikat, dem bei wss:// vertraut wird (selbst signierter Server)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	if *path == "" && *server == "" {
		if _, err := os.Stat(DefaultPath()); err == nil {
			*path = DefaultPath()
		}
	}
	if *path != "" {
		raw, err := os.ReadFile(*path)
		if err != nil {
			return cfg, fmt.Errorf("config-datei lesen: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("config-datei %s: %w", *path, err)
		}
	}
	if v := os.Getenv("MAT_AGENT_TOKEN"); v != "" {
		cfg.Token = v
	}

	setString(&cfg.ServerURL, *server)
	setString(&cfg.Token, *token)
	setString(&cfg.AgentID, *id)
	setString(&cfg.LogLevel, *logLevel)
	setString(&cfg.CAFile, *caFile)
	if *interval > 0 {
		cfg.IntervalSec = *interval
	}
	if *procInterval > 0 {
		cfg.ProcessIntervalSec = *procInterval
	}
	return cfg, cfg.Validate()
}

// Validate prüft die Konfiguration.
func (c *Config) Validate() error {
	if c.ServerURL == "" {
		return errors.New("server_url fehlt (-server oder Config-Datei)")
	}
	u, err := url.Parse(c.ServerURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("server_url muss mit ws:// oder wss:// beginnen: %q", c.ServerURL)
	}
	if c.Token == "" {
		return errors.New("token fehlt (MAT_AGENT_TOKEN, -token oder Config-Datei)")
	}
	if c.IntervalSec < 5 || c.IntervalSec > 3600 {
		return fmt.Errorf("interval_sec muss zwischen 5 und 3600 liegen, ist %d", c.IntervalSec)
	}
	if c.ProcessIntervalSec < c.IntervalSec || c.ProcessIntervalSec > 3600 {
		return fmt.Errorf("process_interval_sec muss zwischen interval_sec und 3600 liegen, ist %d", c.ProcessIntervalSec)
	}
	if c.CAFile != "" && u.Scheme != "wss" {
		return errors.New("ca_file ist nur mit wss:// sinnvoll")
	}
	for i, j := range c.Jobs {
		if !slices.Contains(jobCapabilities, j) {
			return fmt.Errorf("jobs: unbekannte fähigkeit %q (erlaubt: %v)", j, jobCapabilities)
		}
		if slices.Contains(c.Jobs[:i], j) {
			return fmt.Errorf("jobs: %q doppelt", j)
		}
	}
	if !proto.ValidLogSources(c.LogSources) {
		return errors.New(`log_sources: ungültige oder doppelte quelle (erlaubt: "journal", "journal:<unit>", "journal:kernel", "eventlog:<name>")`)
	}
	return nil
}

// Allows meldet, ob eine Admin-Fähigkeit in der Konfiguration freigeschaltet ist.
func (c *Config) Allows(capability proto.Capability) bool {
	return slices.Contains(c.Jobs, capability)
}

// TLSConfig liefert die TLS-Einstellungen für die Verbindung. Ohne CAFile
// werden die System-Zertifikate verwendet (Rückgabe nil).
func (c *Config) TLSConfig() (*tls.Config, error) {
	if c.CAFile == "" {
		return nil, nil
	}
	pemData, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("ca_file lesen: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("ca_file %s enthält kein gültiges PEM-Zertifikat", c.CAFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// Insecure meldet, ob unverschlüsselt (ws://) zu einem entfernten Host verbunden wird.
func (c *Config) Insecure() bool {
	u, err := url.Parse(c.ServerURL)
	if err != nil || u.Scheme != "ws" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// SanitizeID wandelt einen Hostnamen in eine gültige Agent-ID um.
func SanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func setString(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
