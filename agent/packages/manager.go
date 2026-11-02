package packages

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// manager beschreibt eine Paketverwaltung. Fehlende Felder bedeuten, dass es die
// Aktion dort nicht gibt.
type manager struct {
	name    string // für proto.Updates; leer = keine Update-Liste
	label   string // für Meldungen
	path    string // fester Pfad, nie über PATH suchen
	env     []string
	refresh []string
	upgrade []string
	install func(name string) []string
	// checkName ergänzt proto.ValidPackageName um die Regeln dieser Verwaltung.
	checkName func(name string) error
	// before läuft vor der Installation, z. B. um den Namen exakt nachzuschlagen.
	before func(ctx context.Context, c *Checker, m *manager, name string) error
	list   func(ctx context.Context, c *Checker, m *manager) (pkgs []proto.Package, total, security int, err error)
	// reboot liefert den Neustart-Bedarf; nil = Datei bzw. Registry (Checker.rebootRequired).
	reboot func(ctx context.Context, c *Checker, m *manager) (required, known bool)
	okExit []int // Exit-Codes außer 0, die Erfolg bedeuten
}

// detect sucht die Paketverwaltung an festen Pfaden.
func detect(exists func(string) bool, run runFunc) *manager {
	for _, m := range []*manager{aptManager(), dnfManager(), zypperManager(), apkManager()} {
		if exists(m.path) {
			return m
		}
	}
	return platformManager(run)
}

// --- Namensregeln -------------------------------------------------------------

var errPackageName = errors.New("ungültiger paketname für diese paketverwaltung")

// lowerOnly: apt und apk kennen nur kleingeschriebene Namen.
func lowerOnly(name string) bool { return name == strings.ToLower(name) }

// noColon: dnf liest ":" als Epoche, zypper "patch:foo" als Patch, apk "cmd:foo" als Anbieter.
func noColon(name string) error {
	if strings.Contains(name, ":") {
		return errPackageName
	}
	return nil
}

// --- apt ----------------------------------------------------------------------

const aptCachePath = "/usr/bin/apt-cache"

// aptArch: optionaler Architektur-Zusatz, z. B. "libc6:arm64".
var aptArch = regexp.MustCompile(`^[a-z0-9]+$`)

func aptManager() *manager {
	return &manager{
		name: "apt", label: "apt-get", path: "/usr/bin/apt-get",
		// needrestart soll nur auflisten, nicht selbst Dienste neu starten.
		env:     []string{"LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", "APT_LISTCHANGES_FRONTEND=none"},
		refresh: []string{"-q", "update"},
		upgrade: []string{"-y", "-q", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "upgrade"},
		// --no-remove: lieber abbrechen, als wegen eines Konflikts Pakete zu entfernen.
		install: func(name string) []string {
			return []string{"-y", "-q", "--no-remove", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "install", "--", name}
		},
		checkName: func(name string) error {
			base, arch, hasArch := strings.Cut(name, ":")
			if !lowerOnly(name) || base == "" || (hasArch && !aptArch.MatchString(arch)) {
				return errPackageName
			}
			return nil
		},
		before: aptExact,
		list:   aptList,
	}
}

// aptExact verlangt, dass es den Namen exakt gibt. Sonst liest apt-get ihn als
// Regex ("zlib1.-dev" wählt zlib1g-dev) oder als Befehl ("nano+" installiert nano).
func aptExact(ctx context.Context, c *Checker, m *manager, name string) error {
	base, _, _ := strings.Cut(name, ":")
	out, err := c.capture(ctx, m, aptCachePath, "pkgnames", base)
	if err != nil {
		return fmt.Errorf("apt-cache pkgnames: %w", err)
	}
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		if sc.Text() == base {
			return nil
		}
	}
	return fmt.Errorf("paket %q nicht gefunden (paketlisten aktuell?)", base)
}

func aptList(ctx context.Context, c *Checker, m *manager) ([]proto.Package, int, int, error) {
	out, err := c.capture(ctx, m, m.path, "-s", "-o", "Debug::NoLocking=1", "upgrade")
	if err != nil {
		return nil, 0, 0, err
	}
	pkgs, total, sec := parseApt(out)
	return pkgs, total, sec, nil
}

// parseApt wertet die "Inst"-Zeilen von "apt-get -s upgrade" aus, z. B.
//
//	Inst libssl3 [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian-Security:12/stable-security [arm64])
//	Inst linux-image-current-rockchip64 (24.8.1 Armbian:24.8.1/bookworm [arm64])
func parseApt(out []byte) (pkgs []proto.Package, total, security int) {
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		line, ok := strings.CutPrefix(sc.Text(), "Inst ")
		if !ok {
			continue
		}
		name, rest, _ := strings.Cut(line, " ")
		p := proto.Package{Name: name}
		if strings.HasPrefix(rest, "[") {
			if end := strings.IndexByte(rest, ']'); end > 0 {
				p.From = rest[1:end]
				rest = strings.TrimSpace(rest[end+1:])
			}
		}
		if strings.HasPrefix(rest, "(") {
			to, origin, _ := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(rest, "("), ")"), " ")
			p.To = to
			p.Security = strings.Contains(strings.ToLower(origin), "-security")
		}
		total++
		if p.Security {
			security++
		}
		if len(pkgs) < proto.MaxPackages {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, total, security
}

// --- dnf ----------------------------------------------------------------------

const rpmPath = "/usr/bin/rpm"

func dnfManager() *manager {
	return &manager{
		name: "dnf", label: "dnf", path: "/usr/bin/dnf",
		env:     []string{"LC_ALL=C"},
		refresh: []string{"-y", "makecache"},
		upgrade: []string{"-y", "upgrade"},
		// Ohne --allowerasing entfernt dnf nichts.
		install:   func(name string) []string { return []string{"-y", "install", "--", name} },
		checkName: noColon,
		list:      dnfList,
		reboot:    dnfReboot,
	}
}

func dnfList(ctx context.Context, c *Checker, m *manager) ([]proto.Package, int, int, error) {
	out, err := c.capture(ctx, m, m.path, "-q", "check-update")
	if err != nil && exitCode(err) != 100 { // 100 = es gibt Updates
		return nil, 0, 0, err
	}
	// Beides sind Zusatzangaben: Fehlen sie, bleibt die Liste trotzdem richtig.
	installed := map[string]string{}
	if o, err := c.capture(ctx, m, rpmPath, "-qa", "--qf", "%{NAME}.%{ARCH} %{VERSION}-%{RELEASE}\n"); err == nil {
		installed = parseRpmInstalled(o)
	}
	secure := map[string]bool{}
	if o, err := c.capture(ctx, m, m.path, "-q", "updateinfo", "list", "--security", "--updates"); err == nil {
		secure = parseDnfSecurity(o)
	}
	pkgs, total, sec := parseDnfUpdates(out, installed, secure)
	return pkgs, total, sec, nil
}

// parseDnfUpdates wertet "dnf check-update" aus, z. B.
//
//	Upgrades
//	curl.x86_64    8.18.0-14.fc44   updates
func parseDnfUpdates(out []byte, installed map[string]string, secure map[string]bool) (pkgs []proto.Package, total, security int) {
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		f := strings.Fields(sc.Text())
		if len(f) > 0 && strings.HasPrefix(f[0], "Obsoleting") {
			break
		}
		if len(f) != 3 {
			continue
		}
		i := strings.LastIndexByte(f[0], '.')
		if i <= 0 {
			continue
		}
		p := proto.Package{Name: f[0][:i], From: installed[f[0]], To: f[1], Security: secure[f[0][:i]]}
		total++
		if p.Security {
			security++
		}
		if len(pkgs) < proto.MaxPackages {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, total, security
}

// parseRpmInstalled liest "name.arch version-release" je Zeile.
func parseRpmInstalled(out []byte) map[string]string {
	m := map[string]string{}
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

// parseDnfSecurity liest die Paketnamen aus "dnf updateinfo list --security", z. B.
//
//	FEDORA-2026-044e56d9ff security Moderate  tar-2:1.35-9.fc44.x86_64 2026-09-09 04:47:08   (dnf5)
//	FEDORA-2026-044e56d9ff Moderate/Sec.      tar-2:1.35-9.fc44.x86_64                       (dnf4)
func parseDnfSecurity(out []byte) map[string]bool {
	names := map[string]bool{}
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || (f[1] != "security" && !strings.HasSuffix(f[1], "/Sec.")) {
			continue
		}
		for _, nevra := range f[2:] {
			if !strings.Contains(nevra, ".") || !strings.Contains(nevra, "-") {
				continue
			}
			// Name-Version-Release: die letzten beiden Teile abschneiden.
			if i := strings.LastIndexByte(nevra, '-'); i > 0 {
				if j := strings.LastIndexByte(nevra[:i], '-'); j > 0 {
					names[nevra[:j]] = true
				}
			}
			break
		}
	}
	return names
}

// dnfReboot fragt das Plugin needs-restarting. Exit-Code 1 hieße sonst auch
// "Befehl unbekannt", deshalb zählt er nur zusammen mit einer Meldung zum Neustart.
func dnfReboot(ctx context.Context, c *Checker, m *manager) (required, known bool) {
	out, err := c.capture(ctx, m, m.path, "-q", "needs-restarting", "-r")
	if err == nil {
		return false, true
	}
	if exitCode(err) == 1 && strings.Contains(strings.ToLower(string(out)), "reboot") {
		return true, true
	}
	return false, false
}

// --- zypper -------------------------------------------------------------------

const zypperRebootNeeded = 102

func zypperManager() *manager {
	return &manager{
		name: "zypper", label: "zypper", path: "/usr/bin/zypper",
		env:     []string{"LC_ALL=C"},
		refresh: []string{"--non-interactive", "refresh"},
		upgrade: []string{"--non-interactive", "update"},
		// --no-force-resolution: lieber abbrechen, als wegen eines Konflikts etwas zu entfernen.
		install: func(name string) []string {
			return []string{"--non-interactive", "install", "--no-force-resolution", "--", name}
		},
		checkName: noColon,
		list:      zypperList,
		reboot:    zypperReboot,
		okExit:    []int{zypperRebootNeeded, 103}, // Erfolg, aber Neustart nötig
	}
}

func zypperList(ctx context.Context, c *Checker, m *manager) ([]proto.Package, int, int, error) {
	out, err := c.capture(ctx, m, m.path, "--non-interactive", "--xmlout", "list-updates")
	if err != nil {
		return nil, 0, 0, err
	}
	pkgs, total := parseZypperUpdates(out)
	sec := 0
	if o, err := c.capture(ctx, m, m.path, "--non-interactive", "--xmlout", "list-patches", "--category", "security"); err == nil {
		sec = countZypperSecurity(o)
	}
	return pkgs, total, sec, nil
}

var (
	zypperUpdate = regexp.MustCompile(`<update ([^>]*)>`)
	zypperAttr   = regexp.MustCompile(`([a-z-]+)="([^"]*)"`)
	xmlEntities  = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&")
)

// zypperUpdates liefert die Attribute der <update>-Elemente. Ein eigener kleiner
// Leser, weil encoding/xml den Agenten nur dafür vergrößern würde.
func zypperUpdates(out []byte) []map[string]string {
	var list []map[string]string
	for _, m := range zypperUpdate.FindAllSubmatch(out, -1) {
		attrs := map[string]string{}
		for _, a := range zypperAttr.FindAllSubmatch(m[1], -1) {
			attrs[string(a[1])] = xmlEntities.Replace(string(a[2]))
		}
		list = append(list, attrs)
	}
	return list
}

func parseZypperUpdates(out []byte) (pkgs []proto.Package, total int) {
	for _, u := range zypperUpdates(out) {
		if u["kind"] != "package" || u["name"] == "" {
			continue
		}
		total++
		if len(pkgs) < proto.MaxPackages {
			pkgs = append(pkgs, proto.Package{Name: u["name"], From: u["edition-old"], To: u["edition"]})
		}
	}
	return pkgs, total
}

// countZypperSecurity zählt nötige Sicherheits-Patches. Das Feld wird selbst
// geprüft: Leap 16 liefert mit --category security auch andere Kategorien.
// Patches lassen sich nicht einzelnen Paketen zuordnen, deshalb nur die Zahl.
func countZypperSecurity(out []byte) int {
	n := 0
	for _, u := range zypperUpdates(out) {
		if u["kind"] == "patch" && u["category"] == "security" && u["status"] == "needed" {
			n++
		}
	}
	return n
}

func zypperReboot(ctx context.Context, c *Checker, m *manager) (required, known bool) {
	_, err := c.capture(ctx, m, m.path, "--non-interactive", "needs-rebooting")
	switch {
	case err == nil:
		return false, true
	case exitCode(err) == zypperRebootNeeded:
		return true, true
	}
	return false, false
}

// --- apk ----------------------------------------------------------------------

func apkManager() *manager {
	return &manager{
		name: "apk", label: "apk", path: "/sbin/apk",
		env:     []string{"LC_ALL=C"},
		refresh: []string{"update"},
		upgrade: []string{"upgrade", "--no-interactive"},
		install: func(name string) []string { return []string{"add", "--no-interactive", "--", name} },
		checkName: func(name string) error {
			if !lowerOnly(name) {
				return errPackageName
			}
			return noColon(name)
		},
		list: apkList,
		// apk kennt keinen zuverlässigen Hinweis: lieber "unbekannt" als raten.
		reboot: func(context.Context, *Checker, *manager) (bool, bool) { return false, false },
	}
}

func apkList(ctx context.Context, c *Checker, m *manager) ([]proto.Package, int, int, error) {
	out, err := c.capture(ctx, m, m.path, "upgrade", "--simulate", "--no-interactive")
	if err != nil {
		return nil, 0, 0, err
	}
	pkgs, total := parseApk(out)
	return pkgs, total, 0, nil // apk kennt keine Sicherheits-Kennzeichnung
}

// apkLine trifft "(1/10) Upgrading musl (1.2.5-r0 -> 1.2.5-r4)" und
// "(2/3) Installing nano (8.0-r0)".
var apkLine = regexp.MustCompile(`^\(\d+/\d+\) (Upgrading|Installing|Replacing) (\S+) \((?:(\S+) -> )?(\S+)\)$`)

func parseApk(out []byte) (pkgs []proto.Package, total int) {
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		m := apkLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		total++
		if len(pkgs) < proto.MaxPackages {
			pkgs = append(pkgs, proto.Package{Name: m[2], From: m[3], To: m[4]})
		}
	}
	return pkgs, total
}
