// Package winsvc lässt Agent und Server als Windows-Dienst laufen und
// verwaltet den Dienst (installieren, starten, stoppen, entfernen).
//
// Auf anderen Systemen liefern die Verwaltungsfunktionen ErrUnsupported; dort
// übernimmt systemd (Agent) bzw. Kubernetes/Docker (Server) diese Aufgabe.
// Das Paket hat außer golang.org/x/sys keine Abhängigkeiten.
package winsvc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsupported meldet, dass Dienste auf diesem System nicht unterstützt werden.
var ErrUnsupported = errors.New("windows-dienste werden nur unter windows unterstützt")

// ErrNotInstalled meldet, dass der Dienst nicht existiert.
var ErrNotInstalled = errors.New("dienst ist nicht installiert")

// Spec beschreibt einen Dienst.
type Spec struct {
	Name        string // interner Name, z. B. "mat-agent"
	DisplayName string // Anzeigename in services.msc
	Description string
	ExePath     string   // absoluter Pfad zum Programm
	Args        []string // feste Argumente, z. B. -config <pfad>
}

// DataDir liefert das Datenverzeichnis unter Windows, z. B. C:\ProgramData\mat-agent.
func DataDir(name string) string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, name)
}

// ProgramDir liefert das Programmverzeichnis, z. B. C:\Program Files\mat-agent.
func ProgramDir(name string) string {
	base := os.Getenv("ProgramFiles")
	if base == "" {
		base = `C:\Program Files`
	}
	return filepath.Join(base, name)
}

// InstallBinary kopiert das laufende Programm nach dir\<name>.exe und liefert
// den Zielpfad. Läuft das Programm bereits von dort, wird nichts kopiert.
func InstallBinary(dir, name string) (string, error) {
	src, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("eigenen pfad ermitteln: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("programmverzeichnis anlegen: %w", err)
	}
	target := filepath.Join(dir, name+".exe")
	if !SamePath(src, target) {
		if err := CopyFile(src, target); err != nil {
			return "", fmt.Errorf("programm kopieren: %w", err)
		}
	}
	return target, nil
}

// SamePath vergleicht zwei Pfade ohne Rücksicht auf Groß-/Kleinschreibung (Windows).
func SamePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// CopyFile kopiert über eine temporäre Datei, damit kein halbes Ziel entsteht.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
