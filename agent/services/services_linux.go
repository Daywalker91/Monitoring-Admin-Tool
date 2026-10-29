package services

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

type systemd struct{}

func newPlatform() platform {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	return systemd{}
}

func (systemd) list(ctx context.Context) ([]proto.Service, error) {
	units, err := exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--all",
		"--no-legend", "--no-pager", "--plain").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units: %w", err)
	}
	files, err := exec.CommandContext(ctx, "systemctl", "list-unit-files", "--type=service",
		"--no-legend", "--no-pager").Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl list-unit-files: %w", err)
	}
	return parseSystemd(units, files), nil
}

// control ruft systemctl mit fester Argumentliste auf. Die Endung ".service"
// sorgt dafür, dass nur Dienste gemeint sein können (nicht z. B. reboot.target).
func (systemd) control(ctx context.Context, verb, name string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "--no-ask-password", verb, "--", name+".service").CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s %s: %w", verb, name, err)
	}
	return string(out), nil
}
