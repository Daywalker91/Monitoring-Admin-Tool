package power

import (
	"errors"
	"os"
	"os/exec"
)

func rebootRequired() bool {
	_, err := os.Stat("/var/run/reboot-required") // Debian/Ubuntu/Armbian
	return err == nil
}

func supported() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

func precheck() error {
	if !supported() {
		return errors.New("systemctl nicht gefunden")
	}
	return nil
}

func reboot() (string, error) {
	out, err := exec.Command("systemctl", "reboot").CombinedOutput()
	return string(out), err
}
