package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestApplyEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.env")
	content := "\ufeff# Kommentar\nMAT_TEST_A=eins\nexport MAT_TEST_B = \"zwei drei\"\n\nMAT_TEST_C='x=y'\nMAT_TEST_SET=aus-datei\n"
	os.WriteFile(path, []byte(content), 0o600)
	t.Setenv("MAT_TEST_SET", "aus-umgebung")
	for _, k := range []string{"MAT_TEST_A", "MAT_TEST_B", "MAT_TEST_C"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Cleanup(func() { envFilePath = "" })

	rest, err := applyEnvFile([]string{"user", "add", "-env-file", path, "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rest, []string{"user", "add", "admin"}) || envFilePath != path {
		t.Fatalf("rest=%v path=%q", rest, envFilePath)
	}
	want := map[string]string{"MAT_TEST_A": "eins", "MAT_TEST_B": "zwei drei", "MAT_TEST_C": "x=y", "MAT_TEST_SET": "aus-umgebung"}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s=%q, erwartet %q", k, got, v)
		}
	}

	envFilePath = ""
	if _, err := applyEnvFile([]string{"--env-file=" + path}); err != nil || envFilePath != path {
		t.Errorf("--env-file=: %v %q", err, envFilePath)
	}

	bad := filepath.Join(t.TempDir(), "bad.env")
	os.WriteFile(bad, []byte("MAT_OK=1\nkein gleichheitszeichen\n"), 0o600)
	if _, err := applyEnvFile([]string{"-env-file", bad}); err == nil {
		t.Error("ungültige zeile akzeptiert")
	}
	if _, err := applyEnvFile([]string{"-env-file"}); err == nil {
		t.Error("fehlender dateiname akzeptiert")
	}
}

func TestPrepareData(t *testing.T) {
	old := restrict
	var restricted []string
	restrict = func(p string) error { restricted = append(restricted, p); return nil }
	t.Cleanup(func() { restrict = old; envFilePath = "" })

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.env")
	os.WriteFile(src, []byte("MAT_AGENT_TOKEN=12345678\n"), 0o600)
	olddb := filepath.Join(tmp, "old.db")
	os.WriteFile(olddb, []byte("db"), 0o600)
	dataDir := filepath.Join(tmp, "data")
	envPath, dbPath := filepath.Join(dataDir, "server.env"), filepath.Join(dataDir, "mat.db")

	t.Setenv("MAT_AGENT_TOKEN", "")
	envFilePath = src
	if err := prepareData(dataDir, envPath, dbPath, ""); err == nil {
		t.Fatal("ohne token akzeptiert")
	}
	t.Setenv("MAT_AGENT_TOKEN", "12345678")
	t.Setenv("MAT_TLS_CERT", "data/server.crt")
	if err := prepareData(dataDir, envPath, dbPath, ""); err == nil {
		t.Fatal("relativer zertifikatspfad akzeptiert")
	}
	t.Setenv("MAT_TLS_CERT", "")
	if err := prepareData(dataDir, envPath, dbPath, olddb); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(envPath); string(b) != "MAT_AGENT_TOKEN=12345678\n" {
		t.Errorf("env-datei: %q", b)
	}
	if b, _ := os.ReadFile(dbPath); string(b) != "db" {
		t.Errorf("datenbank nicht übernommen")
	}
	if len(restricted) != 2 {
		t.Errorf("geschützt: %v", restricted)
	}
	if err := prepareData(dataDir, envPath, dbPath, olddb); err == nil {
		t.Error("vorhandene datenbank überschrieben")
	}
}
