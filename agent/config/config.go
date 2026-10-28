// Package config lädt die Agent-Konfiguration aus einer JSON-Datei und Flags.
//
// Reihenfolge (spätere gewinnt): Standardwerte → Datei (-config) → Umgebung → Flags.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Config ist die Agent-Konfiguration.
type Config struct {
	ServerURL          string `json:"server_url"` // z. B. ws://server:8080/ws/agent
	Token              string `json:"token"`
	AgentID            string `json:"agent_id"` // Standard: Hostname
	IntervalSec        int    `json:"interval_sec"`
	ProcessIntervalSec int    `json:"process_interval_sec"`
	LogLevel           string `json:"log_level"` // debug, info, warn, error
}

// Defaults liefert die Standardkonfiguration.
func Defaults() Config {
	return Config{IntervalSec: 10, ProcessIntervalSec: 30, LogLevel: "info"}
}

// Load liest Datei, Umgebung (MAT_AGENT_TOKEN) und Flags ein.
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
	if err := fs.Parse(args); err != nil {
		return cfg, err
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
	return nil
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
