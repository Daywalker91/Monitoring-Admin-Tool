package collector

import "github.com/Daywalker91/Monitoring-Admin-Tool/proto"

// cpuTemps liest die Temperatursensoren aus /sys.
func cpuTemps() (*float64, []proto.Temp) { return readTemps("/sys/class") }
