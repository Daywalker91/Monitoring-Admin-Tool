package logs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func supported() bool {
	_, err := exec.LookPath("journalctl")
	return err == nil
}

func platformKind(source string) bool {
	return source == "journal" || strings.HasPrefix(source, "journal:")
}

func read(ctx context.Context, source string, lines int) (string, error) {
	cmd := exec.CommandContext(ctx, "journalctl", journalArgs(source, lines)...)
	cmd.Env = []string{"LC_ALL=C.UTF-8", "SYSTEMD_COLORS=0"}
	out, err := cmd.CombinedOutput()
	text := tail(string(out), proto.MaxJobOutputLen)
	if err != nil {
		return text, fmt.Errorf("journalctl: %w", err)
	}
	return text, nil
}
