package proto

import (
	"time"
	"unicode"
)

// ActionCommandRun führt einen Befehl aus der commands-Whitelist des Hosts
// (agent.json) aus. Parameter: name. Die Befehlszeile steht fest am Host; der
// Server kann nur den Namen wählen.
const ActionCommandRun = "command.run"

const (
	// MaxCommands begrenzt die Befehle je Host.
	MaxCommands = 32
	// MaxCommandTimeout ist die längste erlaubte Laufzeit eines Befehls.
	MaxCommandTimeout = time.Hour
	maxCommandName    = 32
	maxCommandDesc    = 128
)

func init() {
	ActionCapability[ActionCommandRun] = CapCommands
	actionParams[ActionCommandRun] = map[string]Param{"name": {Valid: ValidCommandName}}
	actionTimeout[ActionCommandRun] = MaxCommandTimeout // je Befehl kürzer einstellbar (agent.json)
}

// CommandInfo beschreibt einen freigegebenen Befehl für die GUI (ohne Befehlszeile).
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ValidCommandName erlaubt Kleinbuchstaben, Ziffern, '-' und '_' (1–32 Zeichen,
// beginnend mit Buchstabe oder Ziffer).
func ValidCommandName(s string) bool {
	if len(s) == 0 || len(s) > maxCommandName {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// ValidCommandDescription erlaubt bis zu 128 Bytes ohne Steuerzeichen.
func ValidCommandDescription(s string) bool {
	if len(s) > maxCommandDesc {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidCommands prüft die Befehlsliste einer Hello-Nachricht.
func ValidCommands(list []CommandInfo) bool {
	if len(list) > MaxCommands {
		return false
	}
	seen := make(map[string]bool, len(list))
	for _, c := range list {
		if !ValidCommandName(c.Name) || !ValidCommandDescription(c.Description) || seen[c.Name] {
			return false
		}
		seen[c.Name] = true
	}
	return true
}
