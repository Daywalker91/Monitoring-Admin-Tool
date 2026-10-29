// Package proto enthält die gemeinsamen Nachrichtentypen zwischen Agent und Server.
//
// Jede WebSocket-Nachricht ist ein JSON-Objekt vom Typ Envelope. Das Feld Type
// bestimmt, welcher Typ in Data steckt.
package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version des Nachrichtenprotokolls. Wird bei inkompatiblen Änderungen erhöht.
const Version = 1

// MaxMessageBytes begrenzt die Größe einer einzelnen Nachricht (Schutz vor Speicherüberlauf).
const MaxMessageBytes = 1 << 20

// CloseReplaced ist der WebSocket-Close-Code, mit dem der Server eine Verbindung
// beendet, weil sich ein anderer Agent mit derselben ID angemeldet hat.
const CloseReplaced = 4001

// Nachrichtentypen.
const (
	TypeHello     = "hello"     // Agent → Server: Anmeldung mit Host-Infos und Fähigkeiten
	TypeMetrics   = "metrics"   // Agent → Server: Systemmetriken
	TypeProcesses = "processes" // Agent → Server: Top-Prozesse
)

// Envelope ist die Hülle jeder Nachricht.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Encode verpackt eine Nutzlast in eine Envelope und serialisiert sie.
func Encode(typ string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("nutzlast %q serialisieren: %w", typ, err)
	}
	return json.Marshal(Envelope{Type: typ, Data: data})
}

// Hello ist die erste Nachricht nach dem Verbindungsaufbau.
type Hello struct {
	ProtoVersion int    `json:"proto_version"`
	AgentID      string `json:"agent_id"`
	AgentVersion string `json:"agent_version"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`               // z. B. "linux", "windows"
	Platform     string `json:"platform"`         // z. B. "debian", "Microsoft Windows 11 Pro"
	PlatformVer  string `json:"platform_version"` // z. B. "12.5"
	KernelVer    string `json:"kernel_version"`
	Arch         string `json:"arch"` // GOARCH
	CPUCores     int    `json:"cpu_cores"`
	BootTime     int64  `json:"boot_time"` // Unix-Sekunden
	// IntervalSec ist das Messintervall; der Server leitet daraus den Online-Status ab.
	IntervalSec  int          `json:"interval_sec"`
	Capabilities []Capability `json:"capabilities"`
	// UpdateNote meldet den Ausgang des letzten Self-Updates, z. B. eine
	// automatische Rücknahme. Leer = nichts zu melden.
	UpdateNote string `json:"update_note,omitempty"`
	// LogSources sind die Log-Quellen, die der Agent lesen darf (agent.json).
	LogSources []string `json:"log_sources,omitempty"`
}

// Validate prüft die Pflichtfelder einer Hello-Nachricht.
func (h *Hello) Validate() error {
	if !validID(h.AgentID) {
		return fmt.Errorf("ungültige agent_id")
	}
	if len(h.Hostname) == 0 || len(h.Hostname) > 255 {
		return fmt.Errorf("ungültiger hostname")
	}
	if h.IntervalSec < 1 || h.IntervalSec > 3600 {
		return fmt.Errorf("ungültiges interval_sec")
	}
	if len(h.AgentVersion) > 64 || len(h.UpdateNote) > maxNameLen {
		return fmt.Errorf("agent_version oder update_note zu lang")
	}
	if !ValidLogSources(h.LogSources) {
		return fmt.Errorf("ungültige log_sources")
	}
	return nil
}

// validID erlaubt nur unkritische Zeichen (Buchstaben, Ziffern, '-', '_', '.').
func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// Metrics ist ein Messpunkt des Agenten.
type Metrics struct {
	Time  time.Time   `json:"time"`
	CPU   CPU         `json:"cpu"`
	Mem   Memory      `json:"mem"`
	Swap  Memory      `json:"swap"`
	Net   []NetIface  `json:"net"`
	Disks []DiskUsage `json:"disks"`
	// CPUTemp ist die CPU-Temperatur in °C; nil = kein Sensor (z. B. Windows, VM).
	CPUTemp *float64 `json:"cpu_temp,omitempty"`
	// Temps sind alle gefundenen Temperatursensoren (nur zur Anzeige).
	Temps []Temp `json:"temps,omitempty"`
}

// Temp ist ein Temperatursensor in °C.
type Temp struct {
	Name    string  `json:"name"`
	Celsius float64 `json:"celsius"`
}

// CPU enthält Auslastung in Prozent (0–100) und Load-Werte.
type CPU struct {
	TotalPct float64   `json:"total_pct"`
	PerCore  []float64 `json:"per_core"`
	Load1    float64   `json:"load1"`
	Load5    float64   `json:"load5"`
	Load15   float64   `json:"load15"`
}

// Memory gilt für RAM und Swap. Angaben in Bytes.
type Memory struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	UsedPct float64 `json:"used_pct"`
}

// NetIface enthält Raten pro Sekunde für ein Netzwerkinterface.
type NetIface struct {
	Name     string  `json:"name"`
	RxBps    float64 `json:"rx_bps"` // Bytes/s empfangen
	TxBps    float64 `json:"tx_bps"` // Bytes/s gesendet
	RxPps    float64 `json:"rx_pps"` // Pakete/s empfangen
	TxPps    float64 `json:"tx_pps"` // Pakete/s gesendet
	RxErrors uint64  `json:"rx_errors"`
	TxErrors uint64  `json:"tx_errors"`
}

// DiskUsage beschreibt einen Mountpoint. Angaben in Bytes.
type DiskUsage struct {
	Mount   string  `json:"mount"`
	FSType  string  `json:"fstype"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	UsedPct float64 `json:"used_pct"`
}

// Processes enthält die Top-Prozesse nach CPU und nach RAM.
type Processes struct {
	Time   time.Time `json:"time"`
	TopCPU []Process `json:"top_cpu"`
	TopMem []Process `json:"top_mem"`
}

// Process beschreibt einen einzelnen Prozess.
type Process struct {
	PID  int32  `json:"pid"`
	Name string `json:"name"`
	User string `json:"user,omitempty"`
	// CPUPct ist relativ zu einem Kern (wie top): 200 = zwei Kerne voll ausgelastet.
	CPUPct float64 `json:"cpu_pct"`
	RSS    uint64  `json:"rss"` // Bytes
}

// Obergrenzen für Listen und Texte in eingehenden Nachrichten.
const (
	maxCores   = 1024
	maxIfaces  = 256
	maxDisks   = 256
	maxProcs   = 50
	maxTemps   = 64
	maxNameLen = 512
)

// Validate prüft eine Metrics-Nachricht auf plausible Größen.
func (m *Metrics) Validate() error {
	if len(m.CPU.PerCore) > maxCores || len(m.Net) > maxIfaces || len(m.Disks) > maxDisks {
		return fmt.Errorf("zu viele einträge in metrics")
	}
	for _, n := range m.Net {
		if len(n.Name) > maxNameLen {
			return fmt.Errorf("interface-name zu lang")
		}
	}
	for _, d := range m.Disks {
		if len(d.Mount) > maxNameLen || len(d.FSType) > maxNameLen {
			return fmt.Errorf("mountpoint zu lang")
		}
	}
	if len(m.Temps) > maxTemps {
		return fmt.Errorf("zu viele temperatursensoren")
	}
	if m.CPUTemp != nil && !plausibleTemp(*m.CPUTemp) {
		return fmt.Errorf("cpu_temp unplausibel")
	}
	for _, t := range m.Temps {
		if len(t.Name) > maxNameLen || !plausibleTemp(t.Celsius) {
			return fmt.Errorf("temperatursensor ungültig")
		}
	}
	return nil
}

// plausibleTemp grenzt Sensorwerte ein (auch NaN fällt heraus).
func plausibleTemp(c float64) bool { return c >= -100 && c <= 300 }

// Validate prüft eine Processes-Nachricht auf plausible Größen.
func (p *Processes) Validate() error {
	if len(p.TopCPU) > maxProcs || len(p.TopMem) > maxProcs {
		return fmt.Errorf("zu viele prozesse")
	}
	for _, list := range [][]Process{p.TopCPU, p.TopMem} {
		for _, pr := range list {
			if len(pr.Name) > maxNameLen || len(pr.User) > maxNameLen {
				return fmt.Errorf("prozessname zu lang")
			}
		}
	}
	return nil
}
