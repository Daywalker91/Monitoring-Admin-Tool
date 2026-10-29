package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// sysFiles legt eine nachgebaute /sys/class-Struktur an.
func sysFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestReadTemps(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		cpu     float64 // 0 = keine CPU-Temperatur
		sensors int
	}{
		{"raspberry pi", map[string]string{
			"thermal/thermal_zone0/type": "cpu-thermal\n",
			"thermal/thermal_zone0/temp": "48312\n",
		}, 48.3, 1},
		{"rockchip", map[string]string{
			"thermal/thermal_zone0/type": "soc-thermal\n",
			"thermal/thermal_zone0/temp": "41500\n",
			"thermal/thermal_zone1/type": "bigcore0-thermal\n",
			"thermal/thermal_zone1/temp": "43750\n",
			"thermal/thermal_zone2/type": "gpu-thermal\n",
			"thermal/thermal_zone2/temp": "60000\n", // keine CPU, zählt nicht mit
		}, 43.8, 3},
		{"intel mit coretemp", map[string]string{
			"thermal/thermal_zone0/type":  "acpitz\n",
			"thermal/thermal_zone0/temp":  "27800\n",
			"hwmon/hwmon1/name":           "coretemp\n",
			"hwmon/hwmon1/temp1_input":    "55000\n",
			"hwmon/hwmon1/temp1_label":    "Package id 0\n",
			"hwmon/hwmon1/temp2_input":    "57000\n",
			"hwmon/hwmon1/temp2_label":    "Core 0\n",
			"hwmon/hwmon2/name":           "nvme\n", // kein CPU-Treiber
			"hwmon/hwmon2/temp1_input":    "70000\n",
			"thermal/thermal_zone1/type":  "x86_pkg_temp\n",
			"thermal/thermal_zone1/temp":  "-273000\n", // nicht angeschlossen
			"thermal/thermal_zone2/type":  "iwlwifi_1\n",
			"thermal/thermal_zone2/temp":  "0\n",
			"thermal/thermal_zone3/type":  "pch_cannonlake\n",
			"thermal/thermal_zone3/temp":  "kaputt\n",
			"hwmon/hwmon1/temp3_input":    "999000\n", // unplausibel
			"hwmon/hwmon1/temp3_label":    "Core 1\n",
			"thermal/thermal_zone4/type":  "",
			"thermal/thermal_zone4/temp":  "30000\n", // ohne Namen
			"thermal/thermal_zone9/other": "x",
		}, 57, 3},
		{"intel: x86_pkg_temp doppelt zu coretemp", map[string]string{
			"thermal/thermal_zone0/type": "x86_pkg_temp\n",
			"thermal/thermal_zone0/temp": "52000\n",
			"hwmon/hwmon3/name":          "coretemp\n",
			"hwmon/hwmon3/temp1_input":   "52000\n",
			"hwmon/hwmon3/temp1_label":   "Package id 0\n",
		}, 52, 1},
		{"intel nur x86_pkg_temp", map[string]string{
			"thermal/thermal_zone0/type": "x86_pkg_temp\n",
			"thermal/thermal_zone0/temp": "52000\n",
		}, 52, 1},
		{"vm ohne sensoren", map[string]string{}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cpu, temps := readTemps(sysFiles(t, c.files))
			if len(temps) != c.sensors {
				t.Errorf("sensoren = %v", temps)
			}
			switch {
			case c.cpu == 0 && cpu != nil:
				t.Errorf("cpu = %v, erwartet keine", *cpu)
			case c.cpu != 0 && (cpu == nil || *cpu != c.cpu):
				t.Errorf("cpu = %v, erwartet %v", cpu, c.cpu)
			}
		})
	}
}
