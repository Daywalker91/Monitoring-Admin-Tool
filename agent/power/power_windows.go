package power

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// Schlüssel, die Windows Update bzw. die Komponentenwartung bei ausstehendem
// Neustart anlegt.
var rebootKeys = []string{
	`SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired`,
	`SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`,
}

func rebootRequired() bool {
	for _, p := range rebootKeys {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, p, registry.QUERY_VALUE)
		if err == nil {
			k.Close()
			return true
		}
	}
	return false
}

// shutdownExe liegt fest im System-Verzeichnis (nicht über PATH suchen).
func shutdownExe() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "shutdown.exe")
}

func supported() bool {
	_, err := os.Stat(shutdownExe())
	return err == nil
}

func precheck() error {
	if !supported() {
		return errors.New("shutdown.exe nicht gefunden")
	}
	return nil
}

func reboot() (string, error) {
	out, err := exec.Command(shutdownExe(), "/r", "/t", "30", "/d", "p:0:0",
		"/c", "Neustart durch Monitoring and Administration Tool (MAT)").CombinedOutput()
	return string(out), err
}
