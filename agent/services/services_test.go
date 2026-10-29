package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// fake ist eine Plattform zum Testen der Prüfungen.
type fake struct{ calls []string }

func (f *fake) list(context.Context) ([]proto.Service, error) {
	return []proto.Service{
		{Name: "nginx", State: proto.SvcRunning, Detail: "active/running"},
		{Name: "mat-agent", State: proto.SvcRunning},
		{Name: `bad\x2dname`, State: proto.SvcRunning},
		{Name: "a", Description: strings.Repeat("x", 500)},
	}, nil
}

func (f *fake) control(_ context.Context, verb, name string) (string, error) {
	f.calls = append(f.calls, verb+" "+name)
	return "", nil
}

func TestManager(t *testing.T) {
	f := &fake{}
	m := &Manager{Self: "mat-agent", plat: f}
	ctx := context.Background()
	list, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Name != "a" || len(list[0].Description) != maxDescription {
		t.Fatalf("liste: %+v", list)
	}

	out, _, err := m.Job(proto.ActionServiceRestart)(ctx, map[string]string{"name": "nginx"})
	if err != nil || !strings.Contains(out, "active/running") {
		t.Fatalf("restart: %q %v", out, err)
	}
	for _, name := range []string{"mat-agent", "fehlt", "-x", "reboot.target", `bad\x2dname`} {
		if _, _, err := m.Job(proto.ActionServiceStop)(ctx, map[string]string{"name": name}); err == nil {
			t.Errorf("%q gesteuert", name)
		}
	}
	if len(f.calls) != 1 || f.calls[0] != "restart nginx" {
		t.Errorf("aufrufe: %v", f.calls)
	}
	if _, _, err := (&Manager{}).Job(proto.ActionServiceStart)(ctx, map[string]string{"name": "nginx"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ohne plattform: %v", err)
	}
}
