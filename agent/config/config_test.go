package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileAndFlags(t *testing.T) {
	t.Setenv("MAT_AGENT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(`{"server_url":"ws://a:8080/ws/agent","token":"t1","interval_sec":15}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load([]string{"-config", path, "-server", "wss://b/ws/agent"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != "wss://b/ws/agent" {
		t.Errorf("Flag hat Datei nicht überschrieben: %s", cfg.ServerURL)
	}
	if cfg.Token != "t1" || cfg.IntervalSec != 15 || cfg.ProcessIntervalSec != 30 {
		t.Errorf("unerwartete Werte: %+v", cfg)
	}
}

func TestValidate(t *testing.T) {
	t.Setenv("MAT_AGENT_TOKEN", "")
	bad := [][]string{
		{"-token", "x"}, // keine URL
		{"-server", "http://a/ws", "-token", "x"}, // falsches Schema
		{"-server", "ws://a/ws"},                  // kein Token
		{"-server", "ws://a/ws", "-token", "x", "-interval", "1"},
	}
	for _, args := range bad {
		if _, err := Load(args); err == nil {
			t.Errorf("ungültige Konfiguration akzeptiert: %v", args)
		}
	}
}

func TestInsecure(t *testing.T) {
	cases := map[string]bool{
		"ws://127.0.0.1:8080/ws/agent": false,
		"ws://localhost/ws/agent":      false,
		"wss://server.lan/ws/agent":    false,
		"ws://192.168.1.10/ws/agent":   true,
	}
	for u, want := range cases {
		c := Config{ServerURL: u}
		if got := c.Insecure(); got != want {
			t.Errorf("Insecure(%s) = %v, erwartet %v", u, got, want)
		}
	}
}

func TestSanitizeID(t *testing.T) {
	if got := SanitizeID("My PC_01.lan"); got != "my-pc_01.lan" {
		t.Errorf("SanitizeID = %q", got)
	}
	if got := SanitizeID(""); got != "unknown" {
		t.Errorf("SanitizeID leer = %q", got)
	}
}
