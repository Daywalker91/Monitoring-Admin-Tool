package commands

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/progress"
)

func TestCheck(t *testing.T) {
	ok := func(argv ...string) Spec { return Spec{Argv: argv} }
	cases := []struct {
		name, goos string
		spec       Spec
		valid      bool
	}{
		{"backup", "linux", ok("/opt/backup.sh", "--all"), true},
		{"amp-restart", "linux", ok("/usr/bin/systemctl", "restart", "ampinstmgr"), true},
		{"rel", "linux", ok("backup.sh"), false},
		{"shell", "linux", ok("/bin/sh", "-c", "rm -rf /"), false},
		{"bash-skript", "linux", ok("/usr/bin/bash", "/opt/x.sh"), false},
		{"busybox", "linux", ok("/bin/busybox", "sh"), false},
		{"Gross", "linux", ok("/bin/true"), false},
		{"-start", "linux", ok("/bin/true"), false},
		{"leer", "linux", ok(), false},
		{"nul", "linux", ok("/bin/echo", "a\x00b"), false},
		{"zeit", "linux", Spec{Argv: []string{"/bin/true"}, TimeoutSec: 7200}, false},
		{"text", "linux", Spec{Argv: []string{"/bin/true"}, Description: "zeile\nzwei"}, false},

		{"win-exe", "windows", ok(`C:\Tools\backup.exe`, "/all"), true},
		{"win-bat", "windows", ok(`D:\skripte\aufraeumen.bat`), true},
		{"win-unc", "windows", ok(`\\nas\tools\x.exe`), true},
		{"win-rel", "windows", ok(`tools\x.exe`), false},
		{"win-cmd", "windows", ok(`C:\Windows\System32\cmd.exe`, "/c", "dir"), false},
		{"ps-file", "windows", ok(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
			"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", `C:\skripte\x.ps1`, "-Ziel", "nas"), true},
		{"pwsh-file", "windows", ok(`C:\Program Files\PowerShell\7\pwsh.exe`, "-File", `C:\x.ps1`), true},
		{"ps-command", "windows", ok(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "-Command", "Get-Process"), false},
		{"ps-encoded", "windows", ok(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "-enc", "AAAA"), false},
		{"ps-policy", "windows", ok(`C:\Windows\System32\WindowsPowerShell\v1.0\PowerShell.EXE`, "-ExecutionPolicy", "Unrestricted", "-File", `C:\x.ps1`), false},
		{"ps-rel-file", "windows", ok(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "-File", `x.ps1`), false},
		{"ps-ohne-file", "windows", ok(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "-NoProfile"), false},
		{"ps-linux", "linux", ok("/usr/bin/pwsh", "-File", "/opt/x.ps1"), false},
	}
	for _, c := range cases {
		if err := Check(c.name, c.spec, c.goos); (err == nil) != c.valid {
			t.Errorf("%s (%s): err = %v, gültig erwartet: %v", c.name, c.goos, err, c.valid)
		}
	}
}

// testProgram liefert einen Befehl, der "hallo" ausgibt, und einen, der lange läuft.
func testPrograms(t *testing.T) (hello, slow []string) {
	if runtime.GOOS == "windows" {
		sys := filepath.Join(os.Getenv("SystemRoot"), "System32")
		return []string{filepath.Join(sys, "where.exe"), "where.exe"},
			[]string{filepath.Join(sys, "PING.EXE"), "-n", "30", "127.0.0.1"}
	}
	return []string{"/bin/echo", "hallo"}, []string{"/bin/sleep", "30"}
}

func TestRun(t *testing.T) {
	hello, slow := testPrograms(t)
	r := New(map[string]Spec{
		"hallo":   {Argv: hello, Description: "Test"},
		"langsam": {Argv: slow, TimeoutSec: 1},
	})
	if infos := r.Infos(); len(infos) != 2 || infos[0].Name != "hallo" || infos[0].Description != "Test" {
		t.Fatalf("infos: %+v", infos)
	}

	buf := progress.NewBuffer(1 << 10)
	out, _, err := r.Run(progress.With(context.Background(), buf), map[string]string{"name": "hallo"})
	if err != nil || !strings.Contains(strings.ToLower(out), strings.ToLower(hello[1])) {
		t.Fatalf("hallo: %q %v", out, err)
	}
	if live, _ := buf.Peek(1 << 10); string(live) != out {
		t.Errorf("zwischenstand %q ≠ ausgabe %q", live, out)
	}

	if _, _, err := r.Run(context.Background(), map[string]string{"name": "fehlt"}); err == nil {
		t.Error("unbekannter befehl ausgeführt")
	}
	if _, _, err := r.Run(context.Background(), map[string]string{"name": "langsam"}); err == nil || !strings.Contains(err.Error(), "zeitlimit") {
		t.Errorf("zeitlimit: %v", err)
	}
}
