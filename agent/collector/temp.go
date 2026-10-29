package collector

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// maxTemps begrenzt die Zahl der gemeldeten Sensoren.
const maxTemps = 32

// hwmonCPU sind hwmon-Treiber, die CPU-Temperaturen liefern. Andere Chips
// (NVMe, Grafik, Mainboard) bleiben außen vor; thermal_zone deckt SoCs ab.
var hwmonCPU = map[string]bool{"coretemp": true, "k10temp": true, "zenpower": true}

// readTemps liest die Temperatursensoren unter root (normalerweise /sys/class):
// thermal/thermal_zone* (Raspberry Pi, RK35xx, x86_pkg_temp) und hwmon/hwmon*
// der CPU-Treiber. Die CPU-Temperatur ist der höchste CPU-Sensor; ohne einen
// solchen (z. B. in einer VM) ist sie nil. Liefert coretemp & Co. Werte, fällt
// die Zone x86_pkg_temp weg – sie ist derselbe Sensor wie "Package id 0".
func readTemps(root string) (*float64, []proto.Temp) {
	var temps []proto.Temp
	cpu := math.Inf(-1)
	pkgZone := -1 // Index von x86_pkg_temp in temps

	zones, _ := filepath.Glob(filepath.Join(root, "thermal", "thermal_zone*"))
	for _, z := range zones {
		name := readLine(filepath.Join(z, "type"))
		c, ok := readMilliCelsius(filepath.Join(z, "temp"))
		if name == "" || !ok {
			continue
		}
		if name == "x86_pkg_temp" {
			pkgZone = len(temps)
		}
		temps = append(temps, proto.Temp{Name: name, Celsius: c})
		if cpuZone(name) {
			cpu = max(cpu, c)
		}
	}
	hwmonCPUFound := false

	mons, _ := filepath.Glob(filepath.Join(root, "hwmon", "hwmon*"))
	for _, h := range mons {
		chip := readLine(filepath.Join(h, "name"))
		if !hwmonCPU[chip] {
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(h, "temp*_input"))
		for _, in := range inputs {
			c, ok := readMilliCelsius(in)
			if !ok {
				continue
			}
			name := chip
			if label := readLine(strings.TrimSuffix(in, "_input") + "_label"); label != "" {
				name += " " + label
			}
			temps = append(temps, proto.Temp{Name: name, Celsius: c})
			cpu = max(cpu, c)
			hwmonCPUFound = true
		}
	}
	if hwmonCPUFound && pkgZone >= 0 {
		temps = append(temps[:pkgZone], temps[pkgZone+1:]...)
	}

	sort.Slice(temps, func(i, j int) bool { return temps[i].Name < temps[j].Name })
	if len(temps) > maxTemps {
		temps = temps[:maxTemps]
	}
	if math.IsInf(cpu, -1) {
		return nil, temps
	}
	return &cpu, temps
}

// cpuZone erkennt thermal_zones der CPU, z. B. "cpu-thermal" (Raspberry Pi),
// "soc-thermal"/"bigcore0-thermal" (Rockchip) oder "x86_pkg_temp" (Intel).
func cpuZone(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"cpu", "soc", "core", "x86_pkg"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// readMilliCelsius liest einen Sensorwert in tausendstel Grad Celsius, wie der
// Kernel ihn liefert. Unplausible Werte (nicht angeschlossene Sensoren melden
// z. B. -273 oder 0) fallen heraus.
func readMilliCelsius(path string) (float64, bool) {
	v, err := strconv.ParseInt(readLine(path), 10, 64)
	if err != nil {
		return 0, false
	}
	c := float64(v) / 1000
	if c <= -40 || c == 0 || c >= 200 {
		return 0, false
	}
	return math.Round(c*10) / 10, true
}

// readLine liest die erste Zeile einer kleinen Datei (leer bei Fehlern).
func readLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 256 {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}
