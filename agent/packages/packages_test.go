package packages

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func TestParseApt(t *testing.T) {
	out := []byte(`NOTE: This is only a simulation!
Reading package lists...
Calculating upgrade...
The following packages will be upgraded:
  bash libssl3 linux-image-current-rockchip64
3 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.
Inst bash [5.2.15-2+b2] (5.2.15-2+b7 Debian:12.8/stable [arm64])
Inst libssl3 [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian-Security:12/stable-security [arm64])
Inst libc6 [2.39-0ubuntu8.3] (2.39-0ubuntu8.4 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Inst linux-image-current-rockchip64 (24.8.1 Armbian:24.8.1/bookworm [arm64])
Conf bash (5.2.15-2+b7 Debian:12.8/stable [arm64])
`)
	pkgs, total, sec := parseApt(out)
	want := []proto.Package{
		{Name: "bash", From: "5.2.15-2+b2", To: "5.2.15-2+b7"},
		{Name: "libssl3", From: "3.0.11-1~deb12u1", To: "3.0.11-1~deb12u2", Security: true},
		{Name: "libc6", From: "2.39-0ubuntu8.3", To: "2.39-0ubuntu8.4", Security: true},
		{Name: "linux-image-current-rockchip64", To: "24.8.1"},
	}
	if total != 4 || sec != 2 || len(pkgs) != len(want) {
		t.Fatalf("total=%d sec=%d %+v", total, sec, pkgs)
	}
	for i := range want {
		if pkgs[i] != want[i] {
			t.Errorf("%d: %+v, erwartet %+v", i, pkgs[i], want[i])
		}
	}

	// Über MaxPackages hinaus wird nur gezählt.
	var b strings.Builder
	for i := range proto.MaxPackages + 10 {
		fmt.Fprintf(&b, "Inst p%d [1] (2 Debian:12/stable [amd64])\n", i)
	}
	if pkgs, total, _ := parseApt([]byte(b.String())); len(pkgs) != proto.MaxPackages || total != proto.MaxPackages+10 {
		t.Errorf("begrenzung: %d / %d", len(pkgs), total)
	}
}

func TestTail(t *testing.T) {
	if got := tail("kurz", 10); got != "kurz" {
		t.Errorf("kurz: %q", got)
	}
	got := tail(strings.Repeat("ä", 100), 51)
	if len(got) > 51 || !strings.HasPrefix(got, "…\n") || !utf8.ValidString(got) {
		t.Errorf("gekürzt: %q (%d)", got, len(got))
	}
}

func TestWithoutApt(t *testing.T) {
	c := &Checker{rebootRequired: func() bool { return true }}
	if c.Supported() {
		t.Fatal("ohne apt unterstützt")
	}
	if _, _, err := c.Upgrade(context.Background(), nil); err == nil {
		t.Fatal("upgrade ohne apt")
	}
	u := c.Status(context.Background())
	if u.Manager != "" || !u.RebootRequired || u.Time.IsZero() {
		t.Fatalf("status: %+v", u)
	}
}
