//go:build !windows

package users

import (
	"context"
	"strings"
	"testing"
)

func allTools() map[string]bool {
	return map[string]bool{useraddPath: true, sudoPath: true, bashPath: true}
}

func TestCreateUnix(t *testing.T) {
	busybox := map[string]bool{adduserPath: true, sudoPath: true}
	for _, c := range []struct {
		files  map[string]bool
		params map[string]string
		want   []string // leer = abgelehnt
	}{
		{allTools(), map[string]string{"name": "hans"}, []string{"/usr/sbin/useradd --create-home --shell /bin/bash -- hans"}},
		{allTools(), map[string]string{"name": "hans", "sudo": "false"}, []string{"/usr/sbin/useradd --create-home --shell /bin/bash -- hans"}},
		{allTools(), map[string]string{"name": "hans", "sudo": "true"}, []string{"/usr/sbin/useradd --create-home --shell /bin/bash --groups wheel -- hans"}},
		{busybox, map[string]string{"name": "hans"}, []string{"/usr/sbin/adduser -D -s /bin/sh hans"}},
		{busybox, map[string]string{"name": "hans", "sudo": "true"}, []string{"/usr/sbin/adduser -D -s /bin/sh hans", "/usr/sbin/adduser hans wheel"}},
		{map[string]bool{useraddPath: true}, map[string]string{"name": "hans", "sudo": "true"}, nil}, // sudo fehlt
	} {
		f := &fakeSystem{files: c.files}
		out, _, err := (&Manager{sys: f.sys()}).Job(context.Background(), c.params)
		if c.want == nil {
			if err == nil || len(f.calls) != 0 {
				t.Errorf("%v: err=%v aufrufe=%v", c.params, err, f.calls)
			}
			continue
		}
		if err != nil || strings.Join(f.calls, "|") != strings.Join(c.want, "|") || !strings.Contains(out, "gesperrt") {
			t.Errorf("%v: err=%v aufrufe=%v out=%q", c.params, err, f.calls, out)
		}
	}
}
