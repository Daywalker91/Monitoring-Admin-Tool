package packages

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// wingetGlob: winget.exe liegt im Paketordner von App Installer mit der Version
// im Namen. Der Ordner WindowsApps ist für normale Benutzer nicht beschreibbar,
// die Herausgeber-Kennung am Ende steht fest. Der Eintrag im Benutzerprofil, den
// eine PowerShell findet, existiert für SYSTEM nicht.
const wingetGlob = `WindowsApps\Microsoft.DesktopAppInstaller_*_x64__8wekyb3d8bbwe\winget.exe`

// Exit-Codes von winget (HRESULT, unter Windows als uint32 im int). Variablen
// statt Konstanten, weil die Werte auf 32-Bit-Plattformen nicht in int passen
// (die Datei wird überall übersetzt, winget gibt es nur unter Windows amd64).
var (
	wingetNoPackages    uint32 = 0x8A150014 // APPINSTALLER_CLI_ERROR_NO_APPLICATIONS_FOUND
	wingetNotApplicable uint32 = 0x8A15002B // APPINSTALLER_CLI_ERROR_UPDATE_NOT_APPLICABLE
)

// wingetUpgradeArgs: Liste und Upgrade nutzen dieselbe Auswahl, damit die Zahl
// in der GUI zu dem passt, was "Updates installieren" tut. Nur die Quelle
// winget (nicht der Store); mit "winget pin add" festgehaltene Pakete und solche
// mit unbekannter Version lässt winget bei --all aus.
var wingetUpgradeArgs = []string{"upgrade", "--source", "winget", "--accept-source-agreements", "--disable-interactivity"}

// wingetManager: "--scope machine" bei der Installation verhindert, dass ein
// Paket im Profil von SYSTEM landet. Pakete ohne systemweite Installation
// schlagen deshalb fehl. Updates sieht der Agent nur für Programme, die für alle
// Benutzer installiert sind.
func wingetManager(path string) *manager {
	return &manager{
		name: "winget", label: "winget", path: path,
		refresh: []string{"source", "update", "--name", "winget", "--disable-interactivity"},
		upgrade: append(slices.Clone(wingetUpgradeArgs), "--all", "--silent", "--accept-package-agreements"),
		install: func(name string) []string {
			return []string{"install", "--id", name, "--exact", "--source", "winget", "--scope", "machine", "--silent",
				"--accept-source-agreements", "--accept-package-agreements", "--disable-interactivity"}
		},
		checkName: noColon,
		list:      wingetList,
		// "nichts zu aktualisieren" ist kein Fehler.
		okExit: []int{int(wingetNoPackages), int(wingetNotApplicable)},
	}
}

func wingetList(ctx context.Context, c *Checker, m *manager) ([]proto.Package, int, int, error) {
	out, err := c.capture(ctx, m, m.path, wingetUpgradeArgs...)
	if err != nil && !slices.Contains(m.okExit, exitCode(err)) {
		return nil, 0, 0, err
	}
	pkgs, total := parseWingetUpgrade(out)
	return pkgs, total, 0, nil // winget kennt keine Sicherheits-Kennzeichnung
}

// ansiSeq: Steuerfolgen für Farbe und Cursor, falls winget sie trotz Umleitung schreibt.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// parseWingetUpgrade liest die Tabelle von "winget upgrade", z. B.
//
//	Name                Id              Version  Available Source
//	--------------------------------------------------------------
//	Git                 Git.Git         2.44.0   2.45.1    winget
//	7-Zip 23.01 (x64)   7zip.7zip       23.01    24.07     winget
//	2 upgrades available.
//
// Die Überschriften sind übersetzt ("Verfügbar", "Quelle"), deshalb zählen nur
// ihre Positionen: Jede Spalte beginnt, wo in der Zeile über der Strichlinie
// ein Wort beginnt. Gemessen wird in Zeichen, nicht in Bytes ("…" in gekürzten
// Namen). Gelesen wird nur die erste Tabelle; eine weitere ("explizites Ziel
// nötig") enthält festgehaltene Pakete, die --all nicht aktualisiert.
func parseWingetUpgrade(out []byte) (pkgs []proto.Package, total int) {
	var lines []string
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		line := ansiSeq.ReplaceAllString(sc.Text(), "")
		// Fortschrittsanzeigen überschreiben sich per "\r"; es zählt das Letzte.
		if i := strings.LastIndexByte(line, '\r'); i >= 0 {
			line = line[i+1:]
		}
		lines = append(lines, strings.TrimRight(line, " \t"))
	}
	var cols []int
	start := 0
	for i := 1; i < len(lines); i++ {
		if isDashLine(lines[i]) {
			cols, start = wingetColumns(lines[i-1]), i+1
			break
		}
	}
	if len(cols) < 4 {
		return nil, 0
	}
	for _, line := range lines[start:] {
		f := cutColumns(line, cols)
		id, from, to := f[1], f[2], f[3]
		// Ende der Tabelle: Leerzeile oder Zusammenfassung ("2 upgrades available.").
		if id == "" || from == "" || to == "" || strings.ContainsAny(id, " \t") {
			break
		}
		if len(f) > 4 && f[4] != "" && f[4] != "winget" {
			break
		}
		total++
		if len(pkgs) < proto.MaxPackages {
			// Die Id statt des (oft gekürzten) Namens: eindeutig und passt zu package.install.
			pkgs = append(pkgs, proto.Package{Name: proto.Truncate(id, 128), From: proto.Truncate(from, 64), To: proto.Truncate(to, 64)})
		}
	}
	return pkgs, total
}

func isDashLine(s string) bool {
	return len(s) >= 10 && strings.Trim(s, "-") == ""
}

// wingetColumns liefert die Startpositionen (in Zeichen) der Wörter der Kopfzeile.
func wingetColumns(header string) []int {
	var cols []int
	prev := ' '
	for i, r := range []rune(header) {
		if r != ' ' && prev == ' ' {
			cols = append(cols, i)
		}
		prev = r
	}
	return cols
}

// cutColumns zerlegt eine Zeile an den Spaltenpositionen; fehlende Spalten sind leer.
func cutColumns(line string, cols []int) []string {
	r := []rune(line)
	f := make([]string, len(cols))
	for i, c := range cols {
		if c >= len(r) {
			break
		}
		end := len(r)
		if i+1 < len(cols) && cols[i+1] < end {
			end = cols[i+1]
		}
		f[i] = strings.TrimSpace(string(r[c:end]))
	}
	return f
}

// newestWinget wählt aus den gefundenen Pfaden die höchste Version.
func newestWinget(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return slices.MaxFunc(paths, func(a, b string) int { return slices.Compare(wingetVersion(a), wingetVersion(b)) })
}

// wingetVersion liest die Version aus ...\Microsoft.DesktopAppInstaller_1.29.380.0_x64__...\winget.exe.
func wingetVersion(path string) []int {
	_, rest, ok := strings.Cut(path, "DesktopAppInstaller_")
	if !ok {
		return nil
	}
	ver, _, _ := strings.Cut(rest, "_")
	var v []int
	for _, s := range strings.Split(ver, ".") {
		n, _ := strconv.Atoi(s)
		v = append(v, n)
	}
	return v
}

// wingetWorks ist der Selbsttest: Läuft winget unter diesem Konto (SYSTEM)
// nicht, meldet der Agent die Fähigkeit nicht.
func wingetWorks(run runFunc, path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return run(ctx, nil, io.Discard, path, "--version") == nil
}
