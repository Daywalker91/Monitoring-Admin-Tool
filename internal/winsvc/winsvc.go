// Package winsvc lässt Agent und Server als Windows-Dienst laufen und
// verwaltet den Dienst (installieren, starten, stoppen, entfernen).
//
// Auf anderen Systemen liefern die Verwaltungsfunktionen ErrUnsupported; dort
// übernimmt systemd (Agent) bzw. Kubernetes/Docker (Server) diese Aufgabe.
// Das Paket hat außer golang.org/x/sys keine Abhängigkeiten.
package winsvc

import (
	"errors"
	"os"
	"path/filepath"
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
