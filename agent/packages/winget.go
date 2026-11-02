package packages

import (
	"context"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// wingetGlob: winget.exe liegt im Paketordner von App Installer mit der Version
// im Namen. Der Ordner WindowsApps ist für normale Benutzer nicht beschreibbar,
// die Herausgeber-Kennung am Ende steht fest. Der Eintrag im Benutzerprofil, den
// eine PowerShell findet, existiert für SYSTEM nicht.
const wingetGlob = `WindowsApps\Microsoft.DesktopAppInstaller_*_x64__8wekyb3d8bbwe\winget.exe`

// wingetManager kann nur installieren: Eine Update-Liste gehört nicht zu M10, und
// "--scope machine" verhindert, dass ein Paket im Profil von SYSTEM landet. Pakete
// ohne systemweite Installation schlagen deshalb fehl.
func wingetManager(path string) *manager {
	return &manager{
		label: "winget", path: path,
		install: func(name string) []string {
			return []string{"install", "--id", name, "--exact", "--source", "winget", "--scope", "machine", "--silent",
				"--accept-source-agreements", "--accept-package-agreements", "--disable-interactivity"}
		},
		checkName: noColon,
	}
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
