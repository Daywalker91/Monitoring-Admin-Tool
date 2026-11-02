package users

import (
	"context"
	"strings"
	"testing"
)

func allTools() map[string]bool { return map[string]bool{netExe(): true} }

func TestCreateWindows(t *testing.T) {
	f := &fakeSystem{files: allTools()}
	m := &Manager{sys: f.sys()}
	if !m.Supported() {
		t.Fatal("net.exe nicht erkannt")
	}
	out, _, err := m.Job(context.Background(), map[string]string{"name": "notfall"})
	if err != nil || len(f.calls) != 1 || f.calls[0] != netExe()+" user notfall /add /active:no" || !strings.Contains(out, "deaktiviert") {
		t.Fatalf("err=%v aufrufe=%v out=%q", err, f.calls, out)
	}
	for _, p := range []map[string]string{
		{"name": "notfall", "sudo": "true"},
		{"name": strings.Repeat("a", maxWindowsName+1)},
	} {
		f := &fakeSystem{files: allTools()}
		if _, _, err := (&Manager{sys: f.sys()}).Job(context.Background(), p); err == nil || len(f.calls) != 0 {
			t.Errorf("%v: err=%v aufrufe=%v", p, err, f.calls)
		}
	}
}
