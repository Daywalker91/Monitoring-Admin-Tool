package proto

import (
	"strconv"
	"strings"
	"time"
)

// ActionLogRead liest die letzten Zeilen einer Log-Quelle (Fähigkeit CapLogs).
// Parameter: source (eine der im hello gemeldeten Quellen), lines (1–1000,
// Standard 200).
const ActionLogRead = "log.read"

const (
	DefaultLogLines = 200
	MaxLogLines     = 1000
	maxLogSources   = 32
)

func init() {
	ActionCapability[ActionLogRead] = CapLogs
	actionParams[ActionLogRead] = map[string]Param{
		"source": {Valid: ValidLogSource},
		"lines":  {Valid: validLogLines, Optional: true},
	}
	actionTimeout[ActionLogRead] = time.Minute
	readOnly[ActionLogRead] = true
}

// ValidLogSource prüft eine Log-Quelle:
//
//	journal            gesamtes systemd-Journal
//	journal:kernel     Kernel-Meldungen (journalctl -k)
//	journal:<unit>     eine Unit, z. B. journal:sshd
//	eventlog:<Name>    Windows-Ereignisprotokoll, z. B. eventlog:System
func ValidLogSource(s string) bool {
	if s == "journal" {
		return true
	}
	kind, name, ok := strings.Cut(s, ":")
	if !ok || name == "" || len(name) > 128 || name[0] == '-' {
		return false
	}
	switch kind {
	case "journal":
		return ValidServiceName(name) && !strings.Contains(name, " ")
	case "eventlog":
		for _, r := range name {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '-', r == '_', r == '.', r == ' ', r == '/':
			default:
				return false
			}
		}
		return true
	}
	return false
}

// LogLines liest den Parameter lines (leer = DefaultLogLines).
func LogLines(v string) int {
	if v == "" {
		return DefaultLogLines
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxLogLines {
		return DefaultLogLines
	}
	return n
}

func validLogLines(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n >= 1 && n <= MaxLogLines
}

// ValidLogSources prüft die Quellen-Liste eines hello bzw. der Konfiguration.
func ValidLogSources(list []string) bool {
	if len(list) > maxLogSources {
		return false
	}
	for i, s := range list {
		if !ValidLogSource(s) {
			return false
		}
		for _, t := range list[:i] {
			if t == s {
				return false
			}
		}
	}
	return true
}
