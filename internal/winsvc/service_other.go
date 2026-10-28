//go:build !windows

package winsvc

import (
	"context"
	"os"
)

// IsService ist außerhalb von Windows immer false.
func IsService() bool { return false }

// Run wird außerhalb von Windows nicht unterstützt.
func Run(string, func(context.Context)) error { return ErrUnsupported }

// Install wird außerhalb von Windows nicht unterstützt.
func Install(Spec) error { return ErrUnsupported }

// Uninstall wird außerhalb von Windows nicht unterstützt.
func Uninstall(string) error { return ErrUnsupported }

// Start wird außerhalb von Windows nicht unterstützt.
func Start(string) error { return ErrUnsupported }

// Stop wird außerhalb von Windows nicht unterstützt.
func Stop(string) error { return ErrUnsupported }

// Status wird außerhalb von Windows nicht unterstützt.
func Status(string) (string, error) { return "", ErrUnsupported }

// IsAdmin meldet, ob der Prozess als root läuft.
func IsAdmin() bool { return os.Geteuid() == 0 }

// RestrictToAdmins setzt die Rechte auf 0700 (Verzeichnis) bzw. 0600 (Datei).
func RestrictToAdmins(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}
