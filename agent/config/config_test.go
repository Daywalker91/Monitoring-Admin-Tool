package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
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

func TestJobs(t *testing.T) {
	t.Setenv("MAT_AGENT_TOKEN", "")
	dir := t.TempDir()
	load := func(jobs string) (Config, error) {
		path := filepath.Join(dir, "agent.json")
		os.WriteFile(path, []byte(`{"server_url":"ws://a/ws","token":"t","jobs":`+jobs+`}`), 0o600)
		return Load([]string{"-config", path})
	}
	cfg, err := load(`["services","reboot"]`)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Allows(proto.CapServices) || !cfg.Allows(proto.CapReboot) || cfg.Allows(proto.CapPackages) {
		t.Errorf("freischaltung falsch: %v", cfg.Jobs)
	}
	for _, bad := range []string{`["shell"]`, `["update"]`, `["reboot","reboot"]`} {
		if _, err := load(bad); err == nil {
			t.Errorf("jobs %s akzeptiert", bad)
		}
	}
	if cfg, _ := load(`null`); cfg.Allows(proto.CapReboot) {
		t.Error("ohne eintrag freigeschaltet")
	}
	path := filepath.Join(dir, "agent.json")
	for body, ok := range map[string]bool{
		`{"server_url":"ws://a/ws","token":"t","log_sources":["journal:sshd","eventlog:System"]}`: true,
		`{"server_url":"ws://a/ws","token":"t","log_sources":["file:/etc/shadow"]}`:               false,
		`{"server_url":"ws://a/ws","token":"t","log_sources":["journal","journal"]}`:              false,
	} {
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load([]string{"-config", path}); (err == nil) != ok {
			t.Errorf("%s: err=%v", body, err)
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

func TestDefaultPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DefaultPath ist unter Linux fest /etc/mat-agent")
	}
	t.Setenv("ProgramData", t.TempDir())
	t.Setenv("MAT_AGENT_TOKEN", "")
	if _, err := Load(nil); err == nil {
		t.Fatal("ohne datei und flags akzeptiert")
	}
	os.MkdirAll(filepath.Dir(DefaultPath()), 0o700)
	os.WriteFile(DefaultPath(), []byte(`{"server_url":"ws://127.0.0.1:1/ws/agent","token":"t"}`), 0o600)
	cfg, err := Load([]string{"-interval", "15"})
	if err != nil || cfg.Token != "t" || cfg.IntervalSec != 15 {
		t.Fatalf("standarddatei: %+v %v", cfg, err)
	}
	// Mit -server wird die Standarddatei nicht gelesen.
	if _, err := Load([]string{"-server", "ws://127.0.0.1:1/ws/agent"}); err == nil {
		t.Fatal("token aus standarddatei trotz -server übernommen")
	}
}
