//go:build !linux

package terminal

import (
	"log/slog"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
)

// New liefert außerhalb von Linux immer einen Fehler, sofern ein Terminal
// konfiguriert ist (unter Windows bräuchte die Shell eines anderen Benutzers
// dessen Passwort oder ein S4U-Token; beides passt nicht zu MAT).
func New(cfg *config.Terminal, log *slog.Logger) (*Manager, error) {
	if cfg == nil {
		return nil, nil
	}
	return nil, errUnsupported
}

// Cleanup hat außerhalb von Linux nichts zu tun.
func Cleanup(log *slog.Logger) {}
