package commands

import "os/exec"

// setProcessGroup: Unter Windows beendet der Abbruch nur den Prozess selbst.
func setProcessGroup(*exec.Cmd) {}
