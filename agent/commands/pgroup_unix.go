//go:build !windows

package commands

import (
	"os/exec"
	"syscall"
)

// setProcessGroup startet den Befehl in einer eigenen Prozessgruppe und beendet
// beim Abbruch die ganze Gruppe, damit keine Kindprozesse eines Skripts übrig
// bleiben.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
