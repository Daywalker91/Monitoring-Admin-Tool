package proto

import "fmt"

// Jobs sind Aufträge des Servers an einen Agenten.
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
	CapUpdate   Capability = "update"   // Self-Update (M5), nur als Dienst bzw. unter systemd
	CapPackages Capability = "packages" // Pakete auflisten/aktualisieren (später)
	CapUsers    Capability = "users"    // Benutzer verwalten (später)
	CapReboot   Capability = "reboot"   // Neustart auslösen (später)
	CapServices Capability = "services" // Dienste starten/stoppen (später)
)

// Nachrichtentypen für Jobs.
const (
	TypeJobRequest = "job_request" // Server → Agent
	TypeJobResult  = "job_result"  // Agent → Server
)

// Aktionen (Whitelist). Jede Aktion gehört zu genau einer Fähigkeit.
const (
	// ActionAgentUpdate aktualisiert den Agenten. Parameter: version (z. B. "v0.5.1").
	ActionAgentUpdate = "agent.update"
)

// ActionCapability ordnet jeder bekannten Aktion ihre Fähigkeit zu.
var ActionCapability = map[string]Capability{
	ActionAgentUpdate: CapUpdate,
}

// Obergrenzen für Jobs.
const (
	maxParams       = 8
	maxParamLen     = 256
	MaxJobOutputLen = 4096
	maxJobErrorLen  = 1024
)

// JobRequest beauftragt den Agenten mit einer Aktion aus der Whitelist.
// Params enthält nur einfache Schlüssel/Wert-Paare, die je Aktion streng geprüft werden.
type JobRequest struct {
	JobID  string            `json:"job_id"`
	Action string            `json:"action"` // z. B. "agent.update"
	Params map[string]string `json:"params,omitempty"`
}

// Validate prüft Form und Größe; die Parameter selbst prüft die jeweilige Aktion.
func (j *JobRequest) Validate() error {
	if !validID(j.JobID) {
		return fmt.Errorf("ungültige job_id")
	}
	if _, ok := ActionCapability[j.Action]; !ok {
		return fmt.Errorf("unbekannte aktion %q", j.Action)
	}
	if len(j.Params) > maxParams {
		return fmt.Errorf("zu viele parameter")
	}
	for k, v := range j.Params {
		if !validID(k) || len(v) > maxParamLen {
			return fmt.Errorf("ungültiger parameter %q", k)
		}
	}
	return nil
}

// JobResult meldet das Ergebnis eines Jobs.
type JobResult struct {
	JobID    string `json:"job_id"`
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"` // gekürzt auf MaxJobOutputLen
	Error    string `json:"error,omitempty"`
}

// Validate prüft Form und Größe.
func (r *JobResult) Validate() error {
	if !validID(r.JobID) {
		return fmt.Errorf("ungültige job_id")
	}
	if len(r.Output) > MaxJobOutputLen || len(r.Error) > maxJobErrorLen {
		return fmt.Errorf("job-ergebnis zu lang")
	}
	return nil
}

// Truncate kürzt s auf höchstens n Bytes, ohne ein UTF-8-Zeichen zu zerteilen.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
