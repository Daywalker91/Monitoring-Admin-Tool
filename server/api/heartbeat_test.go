package api

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestHeartbeatStatus(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := st.UpsertHost(ctx, proto.Hello{AgentID: id, Hostname: "geheimer-name-" + id}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	s := New(st, auth.New(st), "secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithVersion("1.5.0")
	got, err := s.HeartbeatStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := "MAT 1.5.0 · 0/2 Hosts online"; got != want {
		t.Fatalf("status %q, erwartet %q", got, want)
	}

	// Ohne Datenbank kein Lebenszeichen.
	st.Close()
	if _, err := s.HeartbeatStatus(ctx); err == nil {
		t.Fatal("geschlossene datenbank ohne fehler")
	}
}
