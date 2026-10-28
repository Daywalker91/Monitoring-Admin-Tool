package proto

// Dieser Teil ist für spätere Meilensteine VORBEREITET und wird in M1 nicht verwendet.
//
// Grundsätze (siehe CLAUDE.md, Sicherheitsregeln):
//   - Es gibt keine freie Shell. Jede Admin-Aktion ist ein Job mit einem festen
//     Aktionsnamen aus einer Whitelist und typisierten, validierten Parametern.
//   - Der Agent führt nur Aktionen aus, deren Fähigkeit er selbst gemeldet hat,
//     und validiert die Parameter selbst noch einmal.

// Capability ist eine Fähigkeit, die ein Agent meldet. Die GUI zeigt nur Aktionen
// an, deren Fähigkeit der jeweilige Agent unterstützt.
type Capability string

// Bekannte Fähigkeiten.
const (
	CapMetrics  Capability = "metrics"  // Metriken erfassen (M1)
	CapPackages Capability = "packages" // Pakete auflisten/aktualisieren (später)
	CapUsers    Capability = "users"    // Benutzer verwalten (später)
	CapReboot   Capability = "reboot"   // Neustart auslösen (später)
	CapServices Capability = "services" // Dienste starten/stoppen (später)
)

// Geplante Nachrichtentypen für Jobs (noch nicht implementiert).
const (
	TypeJobRequest = "job_request" // Server → Agent
	TypeJobResult  = "job_result"  // Agent → Server
)

// JobRequest beauftragt den Agenten mit einer Aktion aus der Whitelist.
// Params enthält nur einfache Schlüssel/Wert-Paare, die je Aktion streng geprüft werden.
type JobRequest struct {
	JobID  string            `json:"job_id"`
	Action string            `json:"action"` // z. B. "packages.upgrade", "system.reboot"
	Params map[string]string `json:"params,omitempty"`
}

// JobResult meldet das Ergebnis eines Jobs.
type JobResult struct {
	JobID    string `json:"job_id"`
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"` // gekürzt auf eine feste Maximallänge
	Error    string `json:"error,omitempty"`
}
