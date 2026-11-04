package collector

import (
	"math"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/pawnio"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// cpuTemps liest die CPU-Temperatur über den Treiber PawnIO, falls er am Host
// installiert ist (siehe Paket pawnio). Die ACPI-Thermalzonen von Windows
// taugen dafür nicht: Auf Desktop-Boards fehlen sie oder zeigen einen festen Wert.
func cpuTemps() (*float64, []proto.Temp) {
	c, name, err := pawnio.CPUTemp()
	if err != nil {
		return nil, nil
	}
	c = math.Round(c*10) / 10
	return &c, []proto.Temp{{Name: name, Celsius: c}}
}
