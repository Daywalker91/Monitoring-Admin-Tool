package proto

import (
	"fmt"
	"regexp"
	"time"
)

// TypeSmart (Agent → Server) meldet die SMART-Werte der Datenträger, etwa
// alle 30 Minuten (nur mit installiertem smartmontools).
const TypeSmart = "smart"

// Smart ist der SMART-Stand eines Hosts.
type Smart struct {
	Time  time.Time   `json:"time"`
	Disks []SmartDisk `json:"disks"`
	// Error: smartctl fehlt oder der Scan ist fehlgeschlagen.
	Error string `json:"error,omitempty"`
}

// SmartDisk sind die wichtigsten Werte eines Datenträgers. Zeiger sind nil,
// wenn der Datenträger den Wert nicht liefert (z. B. ATA-Attribute bei NVMe).
type SmartDisk struct {
	Device   string `json:"device"` // z. B. /dev/sda, /dev/nvme0
	Type     string `json:"type"`   // smartctl -d, z. B. sat, nvme
	Model    string `json:"model,omitempty"`
	Serial   string `json:"serial,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	Bytes    uint64 `json:"bytes,omitempty"`
	// Standby: Platte schlief, Werte stammen vom letzten Lauf mit Antwort.
	Standby bool  `json:"standby,omitempty"`
	Passed  *bool `json:"passed"` // Gesamtbewertung der Platte; nil = unbekannt

	TempC       *float64 `json:"temp_c,omitempty"`
	PowerOnHrs  *uint64  `json:"power_on_hours,omitempty"`
	PowerCycles *uint64  `json:"power_cycles,omitempty"`
	// ATA/SATA
	Reallocated   *uint64 `json:"reallocated,omitempty"`   // Attribut 5
	Pending       *uint64 `json:"pending,omitempty"`       // Attribut 197
	Uncorrectable *uint64 `json:"uncorrectable,omitempty"` // Attribut 198
	CRCErrors     *uint64 `json:"crc_errors,omitempty"`    // Attribut 199 (meist Kabel)
	// NVMe
	CriticalWarning *uint64 `json:"critical_warning,omitempty"`
	PercentUsed     *uint64 `json:"percent_used,omitempty"`
	MediaErrors     *uint64 `json:"media_errors,omitempty"`
	AvailableSpare  *uint64 `json:"available_spare,omitempty"`
	SpareThreshold  *uint64 `json:"spare_threshold,omitempty"`
}

const maxSmartDisks = 64

var (
	smartDevice = regexp.MustCompile(`^/dev/[A-Za-z0-9/_.-]{1,58}$`)
	smartType   = regexp.MustCompile(`^[a-z0-9,_+-]{1,32}$`)
)

// ValidSmartDevice prüft Gerätename und -typ aus dem smartctl-Scan.
func ValidSmartDevice(device, typ string) bool {
	return smartDevice.MatchString(device) && smartType.MatchString(typ)
}

// Problem nennt den Grund für einen SMART-Alarm ("" = in Ordnung). Dauerhafte
// Altlasten (wieder zugewiesene Sektoren, Medienfehler) lösen keinen Alarm
// aus, sie werden nur angezeigt.
func (d SmartDisk) Problem() string {
	switch {
	case d.Passed != nil && !*d.Passed:
		return "SMART-Bewertung: FAILED"
	case d.CriticalWarning != nil && *d.CriticalWarning != 0:
		return fmt.Sprintf("NVMe kritische Warnung 0x%02x", *d.CriticalWarning)
	case d.Pending != nil && *d.Pending > 0:
		return fmt.Sprintf("%d ausstehende Sektoren", *d.Pending)
	case d.Uncorrectable != nil && *d.Uncorrectable > 0:
		return fmt.Sprintf("%d nicht korrigierbare Sektoren", *d.Uncorrectable)
	}
	return ""
}

// Validate prüft eine Smart-Nachricht auf plausible Größen.
func (s *Smart) Validate() error {
	if len(s.Disks) > maxSmartDisks || len(s.Error) > maxNameLen {
		return fmt.Errorf("smart-nachricht zu groß")
	}
	for _, d := range s.Disks {
		if !ValidSmartDevice(d.Device, d.Type) {
			return fmt.Errorf("ungültiges gerät")
		}
		if len(d.Model) > 128 || len(d.Serial) > 64 || len(d.Firmware) > 64 {
			return fmt.Errorf("gerät %s: feld zu lang", d.Device)
		}
		if d.TempC != nil && !plausibleTemp(*d.TempC) {
			return fmt.Errorf("gerät %s: temperatur unplausibel", d.Device)
		}
	}
	return nil
}
