package services

import (
	"bufio"
	"bytes"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Auswertung der systemctl-Ausgaben; ohne Build-Tag, damit die Tests überall laufen.

// parseSystemd wertet die Ausgaben von list-units (UNIT LOAD ACTIVE SUB
// BESCHREIBUNG) und list-unit-files (UNIT ZUSTAND [VORGABE]) aus.
func parseSystemd(units, files []byte) []proto.Service {
	startType := map[string]string{}
	for sc := bufio.NewScanner(bytes.NewReader(files)); sc.Scan(); {
		if f := strings.Fields(sc.Text()); len(f) >= 2 {
			startType[f[0]] = f[1]
		}
	}
	var out []proto.Service
	for sc := bufio.NewScanner(bytes.NewReader(units)); sc.Scan(); {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[1] == "not-found" {
			continue
		}
		unit, active, sub := f[0], f[2], f[3]
		name, ok := strings.CutSuffix(unit, ".service")
		if !ok {
			continue
		}
		st := startType[unit]
		if st == "" {
			// Instanz einer Vorlage, z. B. getty@tty1 → getty@.service
			if at := strings.IndexByte(unit, '@'); at >= 0 {
				st = startType[unit[:at+1]+".service"]
			}
		}
		out = append(out, proto.Service{Name: name, Description: strings.Join(f[4:], " "),
			State: systemdState(active), Detail: active + "/" + sub, StartType: st})
	}
	return out
}

func systemdState(active string) string {
	switch active {
	case "active", "reloading", "refreshing":
		return proto.SvcRunning
	case "activating":
		return proto.SvcStarting
	case "deactivating":
		return proto.SvcStopping
	case "failed":
		return proto.SvcFailed
	}
	return proto.SvcStopped
}
