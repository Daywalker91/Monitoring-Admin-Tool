package proto

import (
	"fmt"
	"time"
)

// TypeUpdates (Agent → Server) meldet offene Paket-Updates und ob ein Neustart
// nötig ist: nach dem Verbinden, regelmäßig und nach Paket-Jobs.
const TypeUpdates = "updates"

// Aktionen für Pakete (Fähigkeit CapPackages) und Neustart (CapReboot).
// Keine Parameter: es gibt nur "Paketlisten holen" und "alles aktualisieren".
const (
	ActionPackagesRefresh = "packages.refresh"
	ActionPackagesUpgrade = "packages.upgrade"
	ActionReboot          = "system.reboot"
)

func init() {
	ActionCapability[ActionPackagesRefresh] = CapPackages
	ActionCapability[ActionPackagesUpgrade] = CapPackages
	ActionCapability[ActionReboot] = CapReboot
	actionTimeout[ActionPackagesRefresh] = 10 * time.Minute
	actionTimeout[ActionPackagesUpgrade] = 30 * time.Minute
	actionTimeout[ActionReboot] = time.Minute
}

// Updates ist der Update-Stand eines Hosts.
type Updates struct {
	Time time.Time `json:"time"`
	// Manager ist die Paketverwaltung ("apt"); leer = keine (z. B. Windows,
	// dann ist nur RebootRequired belegt).
	Manager        string    `json:"manager,omitempty"`
	Packages       []Package `json:"packages,omitempty"` // höchstens maxPackages
	Upgradable     int       `json:"upgradable"`         // alle, auch über maxPackages hinaus
	Security       int       `json:"security"`
	RebootRequired bool      `json:"reboot_required"`
	Error          string    `json:"error,omitempty"` // Paketstand nicht ermittelbar
}

// Package ist ein Paket mit verfügbarem Update.
type Package struct {
	Name     string `json:"name"`
	From     string `json:"from,omitempty"` // leer = wird neu installiert
	To       string `json:"to"`
	Security bool   `json:"security,omitempty"`
}

// MaxPackages begrenzt die Paketliste einer Updates-Nachricht.
const MaxPackages = 500

// Validate prüft eine Updates-Nachricht auf plausible Größen.
func (u *Updates) Validate() error {
	if len(u.Packages) > MaxPackages || u.Upgradable < 0 || u.Security < 0 || u.Upgradable > 100000 {
		return fmt.Errorf("unplausible paketzahlen")
	}
	if len(u.Manager) > 16 || len(u.Error) > maxNameLen {
		return fmt.Errorf("manager oder error zu lang")
	}
	for _, p := range u.Packages {
		if p.Name == "" || len(p.Name) > 128 || len(p.From) > 128 || len(p.To) > 128 {
			return fmt.Errorf("paket ungültig")
		}
	}
	return nil
}
