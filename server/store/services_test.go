package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func TestServices(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	if err := st.UpsertHost(ctx, proto.Hello{AgentID: "pi", Hostname: "pi", IntervalSec: 10}, "", now); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := st.Services(ctx, "pi"); err != nil || raw != nil {
		t.Fatalf("ohne liste: %s %v", raw, err)
	}
	if _, _, err := st.Services(ctx, "fehlt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbekannter host: %v", err)
	}
	st.SaveServices(ctx, "pi", []byte(`{"list":[]}`), now)
	if raw, at, _ := st.Services(ctx, "pi"); string(raw) != `{"list":[]}` || at != now.Unix() {
		t.Fatalf("liste: %s %d", raw, at)
	}

	st.SetServiceWatch(ctx, "pi", "ssh", true)
	st.SetServiceWatch(ctx, "pi", "nginx", true)
	st.SetServiceWatch(ctx, "pi", "nginx", true) // doppelt egal
	st.SetServiceWatch(ctx, "pi", "ssh", false)
	if w, _ := st.ServiceWatches(ctx, "pi"); !slices.Equal(w, []string{"nginx"}) {
		t.Fatalf("überwacht: %v", w)
	}
	st.DeleteHost(ctx, "pi")
	if w, _ := st.ServiceWatches(ctx, "pi"); len(w) != 0 {
		t.Fatalf("nach löschen: %v", w)
	}
}
