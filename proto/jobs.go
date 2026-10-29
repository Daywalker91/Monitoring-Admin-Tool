package proto

import (
	"fmt"
	"time"
)

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
	CapPackages Capability = "packages" // Paketlisten holen, Updates installieren (M7, apt)
	CapUsers    Capability = "users"    // Benutzer verwalten (später)
	CapReboot   Capability = "reboot"   // Neustart auslösen (M7)
	CapServices Capability = "services" // Dienste starten/stoppen/neu starten (M7)
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

// Param beschreibt einen erlaubten Parameter einer Aktion.
type Param struct {
	Valid    func(string) bool
	Optional bool
}

// actionParams legt je Aktion die erlaubten Parameter fest. Andere Parameter
// werden abgelehnt; Aktionen ohne Eintrag haben keine Parameter.
var actionParams = map[string]map[string]Param{
	// Die Version prüft der Agent beim Update zusätzlich gegen das signierte Manifest.
	ActionAgentUpdate: {"version": {Valid: validID}},
}

// actionTimeout ist die Laufzeitgrenze je Aktion; fehlt eine Aktion, gilt
// defaultJobTimeout. Agent (Abbruch) und Server (keine Rückmeldung) nutzen
// denselben Wert.
var actionTimeout = map[string]time.Duration{}

const defaultJobTimeout = 5 * time.Minute

// JobTimeout liefert die Laufzeitgrenze einer Aktion.
func JobTimeout(action string) time.Duration {
	if d, ok := actionTimeout[action]; ok {
		return d
	}
	return defaultJobTimeout
}

// Obergrenzen für Jobs.
const (
	maxParams       = 8
	maxParamLen     = 256
	MaxJobOutputLen = 64 << 10 // reicht für Logs und Paket-Ausgaben
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
	spec := actionParams[j.Action]
	for k, v := range j.Params {
		p, ok := spec[k]
		if !ok || len(v) > maxParamLen {
			return fmt.Errorf("ungültiger parameter %q", k)
		}
		if !p.Valid(v) {
			return fmt.Errorf("ungültiger wert für %q", k)
		}
	}
	for k, p := range spec {
		if _, ok := j.Params[k]; !ok && !p.Optional {
			return fmt.Errorf("parameter %q fehlt", k)
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
