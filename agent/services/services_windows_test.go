package services

import (
	"context"
	"slices"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Liest die echten Dienste dieses Rechners (nur lesend, ohne Administrator).
func TestWindowsList(t *testing.T) {
	m := New("mat-agent")
	list, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list, func(s proto.Service) bool { return s.Name == "EventLog" })
	if i < 0 {
		t.Fatalf("EventLog fehlt (%d dienste)", len(list))
	}
	if s := list[i]; s.State != proto.SvcRunning || s.StartType != "auto" || s.Description == "" {
		t.Errorf("EventLog: %+v", s)
	}
}
