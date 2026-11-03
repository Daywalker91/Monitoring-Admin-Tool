package terminal

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func TestUnitName(t *testing.T) {
	got, err := unitName(proto.TerminalIDPrefix + strings.Repeat("0a", 16))
	if err != nil || got != "mat-term-"+strings.Repeat("0a", 16)+".service" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", "term-x", "term-../../etc", "term-" + strings.Repeat("0a", 16) + ";ls"} {
		if _, err := unitName(bad); err == nil {
			t.Errorf("%q angenommen", bad)
		}
	}
}

// TestArgs: feste Argumentliste, Shell als eigenes Argument nach "--", kein -c.
func TestArgs(t *testing.T) {
	s := &systemdSpawner{user: "hans", shell: "/bin/bash", maxSec: 3600}
	args := s.args("mat-term-x.service")
	if !slices.Contains(args, "--uid=hans") || !slices.Contains(args, "--pty") ||
		!slices.Contains(args, "--property=RuntimeMaxSec=3660") {
		t.Fatalf("%v", args)
	}
	if i := slices.Index(args, "--"); i < 0 || !slices.Equal(args[i+1:], []string{"/bin/bash", "-l"}) {
		t.Fatalf("shell-aufruf: %v", args)
	}
	if slices.Contains(args, "-c") {
		t.Fatal("-c in der argumentliste")
	}
}

func TestNewSystemdSpawner(t *testing.T) {
	dir := t.TempDir()
	// Attrappen statt systemd-run/systemctl (werden nur auf Vorhandensein geprüft)
	fakeRun, fakeCtl := filepath.Join(dir, "systemd-run"), filepath.Join(dir, "systemctl")
	os.WriteFile(fakeRun, nil, 0o755)
	os.WriteFile(fakeCtl, nil, 0o755)
	oldRun, oldCtl := systemdRunPaths, systemctlPaths
	systemdRunPaths, systemctlPaths = []string{filepath.Join(dir, "fehlt"), fakeRun}, []string{fakeCtl}
	t.Cleanup(func() { systemdRunPaths, systemctlPaths = oldRun, oldCtl })
	shells := filepath.Join(dir, "shells")
	os.WriteFile(shells, []byte("# kommentar\n/bin/sh\n"), 0o644)
	sysdir := filepath.Join(dir, "systemd")
	os.Mkdir(sysdir, 0o755)
	other := "nobody"
	if _, err := user.Lookup(other); err != nil {
		t.Skip("kein benutzer nobody")
	}
	cfg := func(u, sh string) *config.Terminal {
		return &config.Terminal{User: u, Shell: sh, MaxSessions: 1, IdleTimeoutSec: 60, MaxDurationSec: 60}
	}
	sp, err := newSystemdSpawner(cfg(other, "/bin/sh"), shells, sysdir)
	if err != nil {
		t.Fatal(err)
	}
	if sp.run != fakeRun || sp.ctl != fakeCtl {
		t.Fatalf("pfade %s %s", sp.run, sp.ctl)
	}
	for name, c := range map[string]struct {
		cfg    *config.Terminal
		sysdir string
	}{
		"root":           {cfg("root", "/bin/sh"), sysdir},
		"unbekannt":      {cfg("gibt-es-nicht-xyz", "/bin/sh"), sysdir},
		"shell fehlt":    {cfg(other, "/bin/bash-gibt-es-nicht"), sysdir},
		"nicht gelistet": {cfg(other, "/bin/true"), sysdir},
		"systemd fehlt":  {cfg(other, "/bin/sh"), filepath.Join(dir, "fehlt")},
	} {
		if _, err := newSystemdSpawner(c.cfg, shells, c.sysdir); err == nil {
			t.Errorf("%s: kein fehler", name)
		}
	}
}

// TestProcPTY prüft Lesen, Schreiben, Größe und Beenden am echten PTY, mit
// /bin/cat statt systemd-run (systemctl wird durch /bin/true ersetzt).
func TestProcPTY(t *testing.T) {
	cat, ok := firstExisting([]string{"/bin/cat", "/usr/bin/cat"}), firstExisting([]string{"/bin/true", "/usr/bin/true"})
	if cat == "" || ok == "" {
		t.Skip("cat/true fehlen")
	}
	cmd := exec.Command(cat)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		t.Skipf("kein pty: %v", err)
	}
	p := &systemdProc{f: f, cmd: cmd, ctl: ok, unit: "mat-term-test.service"}
	if _, err := p.Write([]byte("hallo\n")); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	buf := make([]byte, 256)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "hallo") && time.Now().Before(deadline) {
		n, err := p.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got.String(), "hallo") {
		t.Fatalf("ausgabe %q", got.String())
	}
	if err := p.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if rows, cols, err := pty.Getsize(f); err != nil || rows != 40 || cols != 120 {
		t.Fatalf("größe %dx%d %v", cols, rows, err)
	}
	p.Close()
	p.Close() // zweimal erlaubt
	if cmd.ProcessState == nil {
		t.Fatal("prozess nicht beendet")
	}
}

func TestListedShell(t *testing.T) {
	f := filepath.Join(t.TempDir(), "shells")
	os.WriteFile(f, []byte("/bin/sh\n  /bin/bash  \n/usr/bin/zsh-x\n"), 0o644)
	for sh, want := range map[string]bool{"/bin/sh": true, "/bin/bash": true, "/usr/bin/zsh": false, "/bin": false} {
		if got, err := listedShell(f, sh); err != nil || got != want {
			t.Errorf("%s: %v %v", sh, got, err)
		}
	}
}
