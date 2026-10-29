package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
)

func TestPrepareConfig(t *testing.T) {
	var restricted []string
	old := restrict
	restrict = func(p string) error { restricted = append(restricted, p); return nil }
	t.Cleanup(func() { restrict = old })

	tmp := t.TempDir()
	ca := filepath.Join(tmp, "upload.crt")
	os.WriteFile(ca, []byte("PEM"), 0o644)
	dataDir := filepath.Join(tmp, "data")
	cfgPath := filepath.Join(dataDir, "agent.json")
	cfg := config.Config{ServerURL: "wss://s/ws/agent", Token: "geheim", IntervalSec: 10, ProcessIntervalSec: 30, CAFile: ca}

	if err := prepareConfig(&cfg, dataDir, cfgPath); err != nil {
		t.Fatal(err)
	}
	if len(restricted) != 2 || restricted[0] != dataDir || restricted[1] != cfgPath {
		t.Fatalf("geschützt: %v", restricted)
	}
	var got config.Config
	raw, _ := os.ReadFile(cfgPath)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	wantCA := filepath.Join(dataDir, "server.crt")
	if got.Token != "geheim" || got.CAFile != wantCA {
		t.Fatalf("konfiguration: %+v", got)
	}
	if b, _ := os.ReadFile(wantCA); string(b) != "PEM" {
		t.Fatal("ca-datei nicht kopiert")
	}

	// Erneutes Einrichten mit der bereits kopierten CA-Datei.
	if err := prepareConfig(&got, dataDir, cfgPath); err != nil {
		t.Fatal(err)
	}
}

func TestCutListFlag(t *testing.T) {
	rest, v, set := cutListFlag([]string{"-server", "wss://x/ws", "-jobs", "services, packages", "--log-sources=eventlog:System", "-id", "pc"}, "jobs")
	if !set || len(v) != 2 || v[0] != "services" || v[1] != "packages" {
		t.Fatalf("jobs: %v %v", v, set)
	}
	if strings.Join(rest, " ") != "-server wss://x/ws --log-sources=eventlog:System -id pc" {
		t.Fatalf("rest: %v", rest)
	}
	rest, v, set = cutListFlag(rest, "log-sources")
	if !set || len(v) != 1 || v[0] != "eventlog:System" || len(rest) != 4 {
		t.Fatalf("log-sources: %v %v %v", v, set, rest)
	}
	// Leer: Freischaltung entfernen.
	if _, v, set = cutListFlag([]string{"-jobs="}, "jobs"); !set || v != nil {
		t.Fatalf("leer: %v %v", v, set)
	}
	if _, _, set = cutListFlag([]string{"-id", "pc"}, "jobs"); set {
		t.Fatal("ohne flag gesetzt")
	}
}
