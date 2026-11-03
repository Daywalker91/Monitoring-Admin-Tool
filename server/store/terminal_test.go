package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestTerminalSessions(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-100 * 24 * time.Hour)
	for _, s := range []TerminalSession{
		{ID: "term-a", HostID: "h1", User: "admin", Remote: "1.2.3.4:5", StartedAt: old.Unix()},
		{ID: "term-b", HostID: "h1", User: "admin", StartedAt: now.Unix()},
		{ID: "term-c", HostID: "h2", User: "admin", StartedAt: now.Unix()},
	} {
		if err := st.CreateTerminalSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FinishTerminalSession(ctx, "term-a", old.Add(time.Minute), "leerlauf", 10, 200); err != nil {
		t.Fatal(err)
	}
	// ein zweites Ende ändert nichts
	st.FinishTerminalSession(ctx, "term-a", now, "anders", 0, 0)
	if n, err := st.CloseOpenTerminalSessions(ctx, now); err != nil || n != 2 {
		t.Fatalf("offene beenden: %d %v", n, err)
	}
	list, err := st.TerminalSessions(ctx, "h1", 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("%+v %v", list, err)
	}
	if list[0].ID != "term-b" || list[0].Reason != "server neu gestartet" || list[1].Reason != "leerlauf" ||
		list[1].BytesIn != 10 || list[1].BytesOut != 200 || list[1].EndedAt == nil {
		t.Fatalf("%+v", list)
	}
	if n, err := st.PruneTerminalSessions(ctx, now.Add(-90*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("bereinigen: %d %v", n, err)
	}
}
