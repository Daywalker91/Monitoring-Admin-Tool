package proto

import (
	"fmt"
	"strings"
)

// Web-Terminal (M11). Das Terminal ist die einzige Funktion mit freier Shell und
// deshalb eine bewusste, eng begrenzte Ausnahme (siehe CLAUDE.md):
//   - Freischaltung nur am Host (Block "terminal" in agent.json), nie per Server.
//   - Die Shell läuft als der dort eingetragene, unprivilegierte Benutzer.
//   - Der Server vermittelt nur Bytes zwischen Browser und Agent; er erzeugt keine
//     Eingaben und speichert weder Eingabe noch Ausgabe.
//
// Ablauf: Der Server schickt über die Hauptverbindung TerminalOpen. Der Agent baut
// dafür eine eigene, ausgehende WebSocket-Verbindung zu <server_url>/terminal/<id>
// auf. Auf dieser Verbindung (und genauso zwischen Browser und Server) gilt:
//   - Binärnachrichten sind Terminal-Daten (Eingabe bzw. Ausgabe), höchstens
//     TerminalMaxFrame Bytes.
//   - Textnachrichten sind TerminalControl (JSON): "resize" vom Browser, "exit"
//     (Grund des Endes) vom Agenten. Nur zwischen Browser und Server: "close"
//     (Browser beendet die Sitzung bewusst) und "resume" (Server → Browser:
//     Token, um die Sitzung nach einem Verbindungsabbruch wieder aufzunehmen).

// CapTerminal meldet ein freigeschaltetes Web-Terminal.
const CapTerminal Capability = "terminal"

// TypeTerminalOpen fordert eine Terminal-Sitzung an (Server → Agent).
const TypeTerminalOpen = "terminal_open"

// Typen von TerminalControl.
const (
	TerminalResize = "resize" // Browser → Agent: neue Größe
	TerminalExit   = "exit"   // Agent → Browser: Sitzung endet, Reason nennt den Grund
	TerminalClose  = "close"  // Browser → Server: Sitzung sofort beenden (geht nicht an den Agenten)
	TerminalResume = "resume" // Server → Browser: Session und Token zum Wiederaufnehmen
)

// Grenzen für Terminal-Sitzungen.
const (
	TerminalMaxFrame  = 32 << 10 // Daten je Nachricht
	TerminalMaxCols   = 500
	TerminalMaxRows   = 200
	maxTerminalReason = 200
	// TerminalIDPrefix kennzeichnet Sitzungs-IDs (Rest: 32 Hex-Zeichen, zufällig).
	TerminalIDPrefix = "term-"
)

// TerminalOpen fordert eine Sitzung an. User ist der GUI-Benutzer (nur fürs Log
// des Agenten); als welcher Benutzer die Shell läuft, bestimmt allein agent.json.
type TerminalOpen struct {
	SessionID string `json:"session_id"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	User      string `json:"user"`
}

// Validate prüft Form und Größe.
func (t *TerminalOpen) Validate() error {
	if !ValidTerminalID(t.SessionID) {
		return fmt.Errorf("ungültige session_id")
	}
	if !validTerminalSize(t.Cols, t.Rows) {
		return fmt.Errorf("ungültige terminalgröße")
	}
	if !validID(t.User) {
		return fmt.Errorf("ungültiger benutzer")
	}
	return nil
}

// ValidTerminalID prüft eine Sitzungs-ID: "term-" und 32 Hex-Zeichen (128 Bit).
func ValidTerminalID(s string) bool {
	hex, ok := strings.CutPrefix(s, TerminalIDPrefix)
	if !ok || len(hex) != 32 {
		return false
	}
	for _, r := range hex {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func validTerminalSize(cols, rows int) bool {
	return cols >= 1 && cols <= TerminalMaxCols && rows >= 1 && rows <= TerminalMaxRows
}

// TerminalControl ist eine Steuernachricht auf der Terminal-Verbindung.
type TerminalControl struct {
	Type   string `json:"type"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Nur bei resume (vom Server erzeugt, wird nicht geprüft).
	Session string `json:"session,omitempty"`
	Token   string `json:"token,omitempty"`
}

// Validate prüft eine Steuernachricht; fromAgent wählt die erlaubte Richtung
// (Agent: nur exit, Browser: resize und close).
func (t *TerminalControl) Validate(fromAgent bool) error {
	switch {
	case fromAgent && t.Type == TerminalExit:
		if len(t.Reason) > maxTerminalReason {
			return fmt.Errorf("grund zu lang")
		}
	case !fromAgent && t.Type == TerminalClose:
	case !fromAgent && t.Type == TerminalResize:
		if !validTerminalSize(t.Cols, t.Rows) {
			return fmt.Errorf("ungültige terminalgröße")
		}
	default:
		return fmt.Errorf("unerwartete steuernachricht %q", t.Type)
	}
	return nil
}
