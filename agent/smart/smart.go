// Package smart liest die SMART-Werte der Datenträger mit smartctl
// (smartmontools), nur lesend.
//
// Geräte kommen ausschließlich aus "smartctl --scan-open" und werden vor dem
// Aufruf geprüft; smartctl läuft mit fester Argumentliste. Mit "-n standby"
// werden schlafende Platten nicht geweckt, dann gilt der letzte Stand.
package smart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// TTL: so lange gilt ein Lauf; ein Neuverbinden fragt die Platten nicht erneut ab.
const TTL = 30 * time.Minute

const maxOutput = 1 << 20

// ErrMissing meldet, dass smartmontools nicht installiert ist.
var ErrMissing = errors.New("smartctl nicht gefunden (smartmontools installieren)")

// Reader fragt die Datenträger ab und merkt sich den letzten Stand.
type Reader struct {
	path string // smartctl, leer = nicht installiert
	run  func(ctx context.Context, args ...string) ([]byte, error)

	mu     sync.Mutex
	cached proto.Smart
	at     time.Time
	last   map[string]proto.SmartDisk // letzter Stand je Gerät (für Standby)
}

// New sucht smartctl im PATH, unter Windows auch im Standard-Installationsordner.
func New() *Reader {
	r := &Reader{last: map[string]proto.SmartDisk{}}
	if p, err := exec.LookPath("smartctl"); err == nil {
		r.path = p
	} else if runtime.GOOS == "windows" {
		p := filepath.Join(os.Getenv("ProgramFiles"), "smartmontools", "bin", "smartctl.exe")
		if _, err := os.Stat(p); err == nil {
			r.path = p
		}
	}
	r.run = r.exec
	return r
}

func (r *Reader) exec(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, r.path, args...).Output()
	if len(out) > maxOutput {
		return nil, errors.New("smartctl-ausgabe zu groß")
	}
	// smartctl meldet Warnungen über Bits im Exit-Code; das JSON ist trotzdem gültig.
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(out) > 0 {
		err = nil
	}
	return out, err
}

// Read liefert den SMART-Stand; smartctl läuft höchstens alle TTL.
func (r *Reader) Read(ctx context.Context) proto.Smart {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && time.Since(r.at) < TTL {
		s := r.cached
		s.Disks = slices.Clone(s.Disks)
		return s
	}
	s := proto.Smart{Time: time.Now()}
	if r.path == "" {
		s.Error = ErrMissing.Error()
	} else if disks, err := r.scan(ctx); err != nil {
		s.Error = proto.Truncate(err.Error(), 256)
	} else {
		s.Disks = disks
	}
	r.cached, r.at = s, time.Now()
	return s
}

func (r *Reader) scan(ctx context.Context) ([]proto.SmartDisk, error) {
	out, err := r.run(ctx, "--scan-open", "--json")
	if err != nil {
		return nil, fmt.Errorf("smartctl --scan-open: %w", err)
	}
	var scan struct {
		Devices []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &scan); err != nil {
		return nil, fmt.Errorf("scan lesen: %w", err)
	}
	var disks []proto.SmartDisk
	for _, d := range scan.Devices {
		if !proto.ValidSmartDevice(d.Name, d.Type) || len(disks) == 64 {
			continue
		}
		out, err := r.run(ctx, "-a", "--json", "-n", "standby", "-d", d.Type, d.Name)
		if err != nil {
			continue
		}
		disk, standby, err := parseDevice(out)
		if err != nil {
			continue
		}
		disk.Device, disk.Type = d.Name, d.Type
		if standby {
			prev, ok := r.last[d.Name]
			if !ok {
				disks = append(disks, proto.SmartDisk{Device: d.Name, Type: d.Type, Standby: true})
				continue
			}
			prev.Standby = true
			disk = prev
		} else {
			r.last[d.Name] = disk
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

// device ist der benötigte Ausschnitt aus "smartctl -a --json".
type device struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`
	Firmware     string `json:"firmware_version"`
	UserCapacity struct {
		Bytes uint64 `json:"bytes"`
	} `json:"user_capacity"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current float64 `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours uint64 `json:"hours"`
	} `json:"power_on_time"`
	PowerCycleCount *uint64 `json:"power_cycle_count"`
	ATA             *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value uint64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMe *struct {
		CriticalWarning uint64 `json:"critical_warning"`
		AvailableSpare  uint64 `json:"available_spare"`
		SpareThreshold  uint64 `json:"available_spare_threshold"`
		PercentageUsed  uint64 `json:"percentage_used"`
		MediaErrors     uint64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
}

// parseDevice wertet die JSON-Ausgabe eines Geräts aus. standby meldet eine
// schlafende Platte (keine Werte).
func parseDevice(out []byte) (d proto.SmartDisk, standby bool, err error) {
	var v device
	if err := json.Unmarshal(out, &v); err != nil {
		return d, false, fmt.Errorf("smartctl-json: %w", err)
	}
	for _, m := range v.Smartctl.Messages {
		if strings.Contains(strings.ToUpper(m.String), "STANDBY") {
			return d, true, nil
		}
	}
	d = proto.SmartDisk{Model: proto.Truncate(v.ModelName, 128), Serial: proto.Truncate(v.SerialNumber, 64),
		Firmware: proto.Truncate(v.Firmware, 64), Bytes: v.UserCapacity.Bytes}
	if v.SmartStatus != nil {
		d.Passed = &v.SmartStatus.Passed
	}
	if v.Temperature != nil && v.Temperature.Current > 0 && v.Temperature.Current < 150 {
		d.TempC = &v.Temperature.Current
	}
	if v.PowerOnTime != nil {
		d.PowerOnHrs = &v.PowerOnTime.Hours
	}
	d.PowerCycles = v.PowerCycleCount
	if v.ATA != nil {
		for _, a := range v.ATA.Table {
			raw := a.Raw.Value
			switch a.ID {
			case 5:
				d.Reallocated = &raw
			case 197:
				d.Pending = &raw
			case 198:
				d.Uncorrectable = &raw
			case 199:
				d.CRCErrors = &raw
			}
		}
	}
	if n := v.NVMe; n != nil {
		d.CriticalWarning, d.PercentUsed, d.MediaErrors = &n.CriticalWarning, &n.PercentageUsed, &n.MediaErrors
		d.AvailableSpare, d.SpareThreshold = &n.AvailableSpare, &n.SpareThreshold
	}
	if d.Model == "" && d.Passed == nil && v.Smartctl.ExitStatus&3 != 0 {
		return d, false, errors.New("gerät nicht lesbar")
	}
	return d, false, nil
}
