package packages

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Testdaten: echte Ausgaben aus Containern (Fedora 44, Alpine 3.20, openSUSE Leap 16).

func TestParseDnf(t *testing.T) {
	secure := parseDnfSecurity(testdata(t, "dnf-updateinfo.txt"))
	if !secure["tar"] || !secure["ca-certificates"] || !secure["libstdc++"] || !secure["curl"] || secure["glib2"] {
		t.Fatalf("sicherheit: %v", secure)
	}
	if got := parseDnfSecurity([]byte("FEDORA-1 Moderate/Sec. tar-2:1.35-9.fc44.x86_64\n")); !got["tar"] {
		t.Fatalf("dnf4: %v", got)
	}
	installed := parseRpmInstalled([]byte("curl.x86_64 8.0-1.fc44\ntar.x86_64 1.34-1.fc44\n"))
	pkgs, total, sec := parseDnfUpdates(testdata(t, "dnf-check-update.txt"), installed, secure)
	if total != 41 || len(pkgs) != 41 || sec == 0 {
		t.Fatalf("total=%d sec=%d", total, sec)
	}
	i := slices.IndexFunc(pkgs, func(p proto.Package) bool { return p.Name == "curl" })
	if i < 0 || pkgs[i].From != "8.0-1.fc44" || pkgs[i].To != "8.18.0-14.fc44" || !pkgs[i].Security {
		t.Fatalf("curl: %+v", pkgs)
	}
	if _, total, _ := parseDnfUpdates([]byte("Upgrades\ncurl.x86_64 2 updates\nObsoleting\nalt.x86_64 1 updates\n"), nil, nil); total != 1 {
		t.Fatalf("obsoleting mitgezählt: %d", total)
	}
}

func TestParseZypper(t *testing.T) {
	pkgs, total := parseZypperUpdates(testdata(t, "zypper-list-updates.xml"))
	if total != 2 || pkgs[0].Name != "patterns-base-fips" || pkgs[0].From != "20241218-lp160.5.1" || pkgs[0].To != "20241218+leap-lp160.1.1" {
		t.Fatalf("%d %+v", total, pkgs)
	}
	// Leap 16 liefert trotz --category security auch "recommended".
	if n := countZypperSecurity(testdata(t, "zypper-list-patches.xml")); n != 0 {
		t.Fatalf("sicherheit: %d", n)
	}
	x := `<update kind="patch" name="a" category="security" status="needed"><update kind="patch" name="b" category="security" status="applied">`
	if n := countZypperSecurity([]byte(x)); n != 1 {
		t.Fatalf("nötige patches: %d", n)
	}
}

func TestParseApk(t *testing.T) {
	pkgs, total := parseApk(testdata(t, "apk-upgrade-s.txt"))
	if total != 10 || pkgs[0] != (proto.Package{Name: "musl", From: "1.2.5-r0", To: "1.2.5-r4"}) {
		t.Fatalf("%d %+v", total, pkgs)
	}
	if p, _ := parseApk([]byte("(2/3) Installing nano (8.0-r0)\n")); p[0] != (proto.Package{Name: "nano", To: "8.0-r0"}) {
		t.Fatalf("neu: %+v", p)
	}
}

func TestDetect(t *testing.T) {
	none := func(context.Context, []string, io.Writer, string, ...string) error { return errors.New("kein winget") }
	for path, want := range map[string]string{
		"/usr/bin/apt-get": "apt", "/usr/bin/dnf": "dnf", "/usr/bin/zypper": "zypper", "/sbin/apk": "apk",
	} {
		m := detect(func(p string) bool { return p == path }, none)
		if m == nil || m.name != want || m.path != path {
			t.Errorf("%s: %+v", path, m)
		}
	}
}

// fakeExit ist ein beendetes Programm mit Exit-Code.
type fakeExit int

func (e fakeExit) Error() string { return "exit" }
func (e fakeExit) ExitCode() int { return int(e) }

// fakeHost ersetzt die Programmausführung und merkt sich alle Aufrufe.
type fakeHost struct {
	calls []string
	reply func(call string) (string, error)
}

func (f *fakeHost) run(_ context.Context, _ []string, w io.Writer, path string, args ...string) error {
	call := path + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.reply == nil {
		return nil
	}
	out, err := f.reply(call)
	io.WriteString(w, out)
	return err
}

// setCanSetUID legt fest, ob der Agent im Test CAP_SETUID hat. Das Ergebnis darf
// nicht davon abhängen, unter welchem Benutzer die Tests laufen.
func setCanSetUID(t *testing.T, v bool) {
	t.Helper()
	old := canSetUID
	canSetUID = func() bool { return v }
	t.Cleanup(func() { canSetUID = old })
}

func TestAptWithoutSetUID(t *testing.T) {
	setCanSetUID(t, false)
	host := &fakeHost{}
	c := &Checker{mgr: aptManager(), run: host.run}
	if _, _, err := c.Refresh(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if want := "/usr/bin/apt-get -o APT::Sandbox::User=root -q update"; host.calls[0] != want {
		t.Fatalf("aufruf: %s", host.calls[0])
	}
}

func TestInstallCommands(t *testing.T) {
	setCanSetUID(t, true)
	pkgnames := func(call string) (string, error) {
		if strings.Contains(call, "pkgnames") {
			return "nano-tiny\nnano\ng++\ng++-14\nlibc6\n", nil
		}
		return "", nil
	}
	cases := []struct {
		m    *manager
		name string
		want string // letzter Aufruf; leer = wird abgelehnt, nichts wird installiert
	}{
		{aptManager(), "nano", "/usr/bin/apt-get -y -q --no-remove -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install -- nano"},
		{aptManager(), "g++", "/usr/bin/apt-get -y -q --no-remove -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install -- g++"},
		{aptManager(), "libc6:arm64", "/usr/bin/apt-get -y -q --no-remove -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold install -- libc6:arm64"},
		{aptManager(), "nano-", ""},      // würde entfernen
		{aptManager(), "nano+", ""},      // Befehl statt Name
		{aptManager(), "zlib1.-dev", ""}, // Regex
		{aptManager(), "NANO", ""},       // Großbuchstaben
		{aptManager(), "-oAPT::Update", ""},
		{aptManager(), "libc6:-x", ""},
		{dnfManager(), "NetworkManager-libnm", "/usr/bin/dnf -y install -- NetworkManager-libnm"},
		{dnfManager(), "foo:1", ""},
		{zypperManager(), "nano", "/usr/bin/zypper --non-interactive install --no-force-resolution -- nano"},
		{zypperManager(), "NetworkManager", "/usr/bin/zypper --non-interactive install --no-force-resolution -- NetworkManager"},
		{zypperManager(), "patch:foo", ""},
		{zypperManager(), "nano-", ""},
		{apkManager(), "nano", "/sbin/apk add --no-interactive -- nano"},
		{apkManager(), "Nano", ""},
		{apkManager(), "cmd:nano", ""},
		{wingetManager(`C:\w\winget.exe`), "Microsoft.PowerShell", `C:\w\winget.exe install --id Microsoft.PowerShell --exact --source winget --scope machine --silent --accept-source-agreements --accept-package-agreements --disable-interactivity`},
		{wingetManager(`C:\w\winget.exe`), "a:b", ""},
	}
	for _, tc := range cases {
		host := &fakeHost{reply: pkgnames}
		c := &Checker{mgr: tc.m, run: host.run}
		_, _, err := c.Install(context.Background(), map[string]string{"name": tc.name})
		installed := slices.ContainsFunc(host.calls, func(s string) bool {
			return strings.Contains(s, " install ") || strings.Contains(s, " add ")
		})
		if tc.want == "" {
			if err == nil || installed {
				t.Errorf("%s %q: err=%v aufrufe=%v", tc.m.label, tc.name, err, host.calls)
			}
			continue
		}
		if err != nil || host.calls[len(host.calls)-1] != tc.want {
			t.Errorf("%s %q: err=%v aufrufe=%v", tc.m.label, tc.name, err, host.calls)
		}
	}
}

func TestJobExitCodes(t *testing.T) {
	host := &fakeHost{reply: func(string) (string, error) { return "ok", fakeExit(zypperRebootNeeded) }}
	c := &Checker{mgr: zypperManager(), run: host.run}
	if _, _, err := c.Upgrade(context.Background(), nil); err != nil {
		t.Fatalf("zypper 102 ist erfolg: %v", err)
	}
	host.reply = func(string) (string, error) { return "kein paket", fakeExit(104) }
	if _, _, err := c.Install(context.Background(), map[string]string{"name": "gibtsnicht"}); err == nil {
		t.Fatal("zypper 104 ist fehler")
	}
}

func TestWingetInstallOnly(t *testing.T) {
	c := &Checker{mgr: wingetManager(`C:\w\winget.exe`), rebootRequired: func() bool { return true }, run: (&fakeHost{}).run}
	if !c.Supported() {
		t.Fatal("winget nicht unterstützt")
	}
	if _, _, err := c.Refresh(context.Background(), nil); err == nil {
		t.Fatal("refresh mit winget")
	}
	if _, _, err := c.Upgrade(context.Background(), nil); err == nil {
		t.Fatal("upgrade mit winget")
	}
	if u := c.Status(context.Background()); u.Manager != "" || !u.RebootRequired {
		t.Fatalf("status: %+v", u)
	}
}

func TestNewestWinget(t *testing.T) {
	a := `C:\Program Files\WindowsApps\Microsoft.DesktopAppInstaller_1.9.0.0_x64__8wekyb3d8bbwe\winget.exe`
	b := `C:\Program Files\WindowsApps\Microsoft.DesktopAppInstaller_1.29.380.0_x64__8wekyb3d8bbwe\winget.exe`
	if got := newestWinget([]string{a, b}); got != b {
		t.Fatalf("%s", got)
	}
	if newestWinget(nil) != "" {
		t.Fatal("ohne treffer")
	}
}

func TestRebootStatus(t *testing.T) {
	cases := []struct {
		m             *manager
		reply         func(string) (string, error)
		required, unk bool
	}{
		{zypperManager(), func(c string) (string, error) {
			if strings.Contains(c, "needs-rebooting") {
				return "", fakeExit(102)
			}
			return "", nil
		}, true, false},
		{zypperManager(), nil, false, false},
		{zypperManager(), func(c string) (string, error) {
			if strings.Contains(c, "needs-rebooting") {
				return "", fakeExit(7)
			}
			return "", nil
		}, false, true},
		{apkManager(), nil, false, true},
		{dnfManager(), func(c string) (string, error) {
			if strings.Contains(c, "needs-restarting") {
				return "Reboot is required to fully utilize these updates.", fakeExit(1)
			}
			return "", nil
		}, true, false},
		{dnfManager(), func(c string) (string, error) {
			if strings.Contains(c, "needs-restarting") {
				return "No such command: needs-restarting", fakeExit(1)
			}
			return "", nil
		}, false, true},
	}
	for i, tc := range cases {
		c := &Checker{mgr: tc.m, run: (&fakeHost{reply: tc.reply}).run, rebootRequired: func() bool { return true }}
		if u := c.Status(context.Background()); u.RebootRequired != tc.required || u.RebootUnknown != tc.unk {
			t.Errorf("%d %s: %+v", i, tc.m.label, u)
		}
	}
}
