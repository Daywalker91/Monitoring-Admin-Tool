package services

import (
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func TestParseSystemd(t *testing.T) {
	units := []byte(`ssh.service                 loaded    active   running OpenBSD Secure Shell server
k3s-agent.service           loaded    activating start   Lightweight Kubernetes
backup.service              loaded    failed   failed  Nächtliches Backup
getty@tty1.service          loaded    active   running Getty on tty1
apt-daily.service           loaded    inactive dead    Daily apt download activities
ghost.service               not-found inactive dead    ghost.service
systemd-fsck@dev-disk-by\x2duuid-1.service loaded active exited File System Check
`)
	files := []byte(`ssh.service             enabled  enabled
k3s-agent.service       enabled  enabled
backup.service          static   -
getty@.service          enabled  enabled
apt-daily.service       static   -
`)
	got := parseSystemd(units, files)
	want := []proto.Service{
		{Name: "ssh", Description: "OpenBSD Secure Shell server", State: proto.SvcRunning, Detail: "active/running", StartType: "enabled"},
		{Name: "k3s-agent", Description: "Lightweight Kubernetes", State: proto.SvcStarting, Detail: "activating/start", StartType: "enabled"},
		{Name: "backup", Description: "Nächtliches Backup", State: proto.SvcFailed, Detail: "failed/failed", StartType: "static"},
		{Name: "getty@tty1", Description: "Getty on tty1", State: proto.SvcRunning, Detail: "active/running", StartType: "enabled"},
		{Name: "apt-daily", Description: "Daily apt download activities", State: proto.SvcStopped, Detail: "inactive/dead", StartType: "static"},
		{Name: `systemd-fsck@dev-disk-by\x2duuid-1`, Description: "File System Check", State: proto.SvcRunning, Detail: "active/exited"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d dienste, erwartet %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v\nerwartet %+v", i, got[i], want[i])
		}
	}
	// Namen mit Escapes filtert erst Manager.List (ValidServiceName) heraus.
	if proto.ValidServiceName(want[5].Name) {
		t.Error("escape im namen akzeptiert")
	}
}
