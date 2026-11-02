package users

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeSystem führt nichts aus und merkt sich alle Aufrufe.
type fakeSystem struct {
	files map[string]bool
	known map[string]bool
	calls []string
	fail  bool
}

func (f *fakeSystem) sys() system {
	return system{
		exists:    func(p string) bool { return f.files[p] },
		userKnown: func(n string) bool { return f.known[n] },
		readFile:  func(string) ([]byte, error) { return []byte("root:x:0:\nwheel:x:10:\nstaff:x:50:\n"), nil },
		run: func(_ context.Context, _ io.Writer, path string, args ...string) error {
			f.calls = append(f.calls, path+" "+strings.Join(args, " "))
			if f.fail {
				return errors.New("kaputt")
			}
			return nil
		},
	}
}

func TestJobRejects(t *testing.T) {
	for _, p := range []map[string]string{
		{"name": ""}, {"name": "root"}, {"name": "-x"}, {"name": "a b"}, {"name": "Hans"}, {"name": `a\b`},
		{"name": "hans", "sudo": "ja"}, {"name": "vorhanden"},
	} {
		f := &fakeSystem{known: map[string]bool{"vorhanden": true}}
		if _, _, err := (&Manager{sys: f.sys()}).Job(context.Background(), p); err == nil || len(f.calls) != 0 {
			t.Errorf("%v: err=%v aufrufe=%v", p, err, f.calls)
		}
	}
}

func TestJobFailure(t *testing.T) {
	f := &fakeSystem{files: allTools(), fail: true}
	if _, _, err := (&Manager{sys: f.sys()}).Job(context.Background(), map[string]string{"name": "hans"}); err == nil {
		t.Fatal("fehler verschluckt")
	}
}
