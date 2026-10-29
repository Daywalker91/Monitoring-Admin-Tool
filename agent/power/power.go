// Package power meldet, ob ein Neustart nötig ist, und führt den Job
// system.reboot aus.
package power

import (
	"context"
	"log/slog"
	"time"
)

// rebootDelay: so lange wartet der Agent, damit das Job-Ergebnis noch beim
// Server ankommt, bevor das System herunterfährt.
const rebootDelay = 5 * time.Second

// RebootRequired meldet, ob das System einen Neustart verlangt (z. B. nach
// einem Kernel-Update).
func RebootRequired() bool { return rebootRequired() }

// Supported meldet, ob der Neustart auf dieser Plattform möglich ist.
func Supported() bool { return supported() }

// Job liefert die Aktion system.reboot: Sie antwortet sofort und startet das
// System kurz danach neu.
func Job(log *slog.Logger) func(ctx context.Context, _ map[string]string) (string, bool, error) {
	return func(context.Context, map[string]string) (string, bool, error) {
		if err := precheck(); err != nil {
			return "", false, err
		}
		time.AfterFunc(rebootDelay, func() {
			log.Warn("system wird auf anforderung des servers neu gestartet")
			if out, err := reboot(); err != nil {
				log.Error("neustart fehlgeschlagen", "err", err, "output", out)
			}
		})
		return "Neustart in " + rebootDelay.String() + " ausgelöst.\n", false, nil
	}
}
