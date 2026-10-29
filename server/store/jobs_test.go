package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestJobLifecycle(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	for i, id := range []string{"j1", "j2"} {
		j := Job{ID: id, HostID: "pi", Action: "service.restart", Params: map[string]string{"name": "ssh"},
			User: "admin", CreatedAt: now.Unix() + int64(i), Deadline: now.Unix() + 60}
		if err := st.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	// Ergebnis nur vom eigenen Host.
	if found, err := st.FinishJob(ctx, "j1", "anderer", now, true, 0, "x", ""); err != nil || found {
		t.Fatalf("fremder host: found=%v err=%v", found, err)
	}
	if found, _ := st.FinishJob(ctx, "j1", "pi", now.Add(time.Second), true, 0, "neu gestartet", ""); !found {
		t.Fatal("ergebnis nicht gespeichert")
	}
	if found, _ := st.FinishJob(ctx, "j1", "pi", now, false, 1, "", "doppelt"); found {
		t.Fatal("beendeter job überschrieben")
	}

	// j2 läuft ab; ein spätes Ergebnis gilt trotzdem.
	if n, _ := st.ExpireJobs(ctx, now.Add(2*time.Minute)); n != 1 {
		t.Fatalf("abgelaufen: %d", n)
	}
	j2, err := st.JobByID(ctx, "j2")
	if err != nil || j2.FinishedAt == nil || j2.ExitCode != ExitNoResponse {
		t.Fatalf("j2 nach ablauf: %+v %v", j2, err)
	}
	if found, _ := st.FinishJob(ctx, "j2", "pi", now.Add(3*time.Minute), true, 0, "spät", ""); !found {
		t.Fatal("spätes ergebnis verworfen")
	}

	jobs, err := st.Jobs(ctx, "pi", 10)
	if err != nil || len(jobs) != 2 || jobs[0].ID != "j2" || jobs[1].Output != "" {
		t.Fatalf("liste: %+v %v", jobs, err)
	}
	j1, _ := st.JobByID(ctx, "j1")
	if !j1.OK || j1.Output != "neu gestartet" || j1.Params["name"] != "ssh" || j1.User != "admin" {
		t.Fatalf("j1: %+v", j1)
	}
	if _, err := st.JobByID(ctx, "fehlt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fehlender job: %v", err)
	}
	if n, _ := st.PruneJobs(ctx, now.Add(time.Hour)); n != 2 {
		t.Fatalf("bereinigt: %d", n)
	}
}
