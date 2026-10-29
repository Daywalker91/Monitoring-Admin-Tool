//go:build !linux

package collector

import "github.com/Daywalker91/Monitoring-Admin-Tool/proto"

// cpuTemps: außerhalb von Linux gibt es keine Temperatur. Unter Windows ginge
// das nur über WMI, oft ungenau, und würde den Agenten vergrößern.
func cpuTemps() (*float64, []proto.Temp) { return nil, nil }
