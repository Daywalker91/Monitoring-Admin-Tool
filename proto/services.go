package proto

import (
	"fmt"
	"time"
)

// TypeServices (Agent → Server) meldet die vollständige Dienstliste: sofort nach
// dem Verbinden, bei Änderungen und sonst in größeren Abständen.
const TypeServices = "services"

// Normalisierte Zustände eines Dienstes (systemd bzw. Windows-Dienst-Manager).
const (
	SvcRunning  = "running"
	SvcStopped  = "stopped"
	SvcFailed   = "failed" // nur systemd: Unit ist fehlgeschlagen
	SvcStarting = "starting"
	SvcStopping = "stopping"
)

// Aktionen für Dienste (Fähigkeit CapServices). Parameter: name.
const (
	ActionServiceStart   = "service.start"
	ActionServiceStop    = "service.stop"
	ActionServiceRestart = "service.restart"
)

func init() {
	for _, a := range []string{ActionServiceStart, ActionServiceStop, ActionServiceRestart} {
		ActionCapability[a] = CapServices
		actionParams[a] = map[string]Param{"name": {Valid: ValidServiceName}}
		actionTimeout[a] = 2 * time.Minute
	}
}

// Services ist die Dienstliste eines Hosts.
type Services struct {
	Time time.Time `json:"time"`
	List []Service `json:"list"`
}

// Service beschreibt einen Dienst.
type Service struct {
	// Name ist die Kennung für Jobs: systemd-Unit ohne ".service" bzw. der
	// Windows-Dienstname (nicht der Anzeigename).
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	State       string `json:"state"`      // siehe Svc*
	Detail      string `json:"detail"`     // Rohzustand, z. B. "active/running", "failed/failed"
	StartType   string `json:"start_type"` // z. B. enabled, disabled, static, auto, manual
}

const (
	maxServices       = 2000
	maxServiceNameLen = 256
)

// ValidServiceName erlaubt nur Zeichen, die in systemd-Units und Windows-
// Dienstnamen vorkommen. Ein führendes '-' ist verboten (keine Option für
// systemctl), ebenso Pfadtrenner.
func ValidServiceName(s string) bool {
	if len(s) == 0 || len(s) > maxServiceNameLen || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '@', r == ':', r == ' ':
		default:
			return false
		}
	}
	return true
}

// Validate prüft eine Services-Nachricht auf plausible Größen.
func (s *Services) Validate() error {
	if len(s.List) > maxServices {
		return fmt.Errorf("zu viele dienste")
	}
	for _, sv := range s.List {
		if !ValidServiceName(sv.Name) {
			return fmt.Errorf("ungültiger dienstname")
		}
		if len(sv.Description) > maxNameLen || len(sv.State) > 32 || len(sv.Detail) > 64 || len(sv.StartType) > 32 {
			return fmt.Errorf("dienst %q: feld zu lang", sv.Name)
		}
	}
	return nil
}
