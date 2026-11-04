//go:build !linux && !windows

package collector

import "github.com/Daywalker91/Monitoring-Admin-Tool/proto"

// cpuTemps: auf anderen Plattformen gibt es keine Temperatur.
func cpuTemps() (*float64, []proto.Temp) { return nil, nil }
