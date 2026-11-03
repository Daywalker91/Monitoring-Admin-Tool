package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestPlaybookRuns(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	run := PlaybookRun{ID: "pb-1", Name: "patchen", Source: "name: patchen\n", User: "admin", Hosts: []string{"a", "b"},
		Status: RunRunning, CreatedAt: now.Unix()}
	var steps []PlaybookStep
	for _, h := range run.Hosts {
		for i, id := range []string{"refresh", "upgrade"} {
			steps = append(steps, PlaybookStep{HostID: h, Index: i, StepID: id, Status: StepPending})
		}
	}
	if err := st.CreatePlaybookRun(ctx, run, steps); err != nil {
		t.Fatal(err)
	}
	active, err := st.ActivePlaybookRuns(ctx)
	if err != nil || len(active) != 1 || len(active[0].Steps) != 4 || active[0].Source != run.Source ||
		!reflect.DeepEqual(active[0].Hosts, run.Hosts) {
		t.Fatalf("%+v %v", active, err)
	}

	s := active[0].Steps[0]
	s.Status, s.JobID, s.StartedAt, s.Until, s.BootTime, s.ExitCode = StepRunning, "job-1", now.Unix(), now.Unix()+60, 42, 10
	if err := st.SavePlaybookStep(ctx, s); err != nil {
		t.Fatal(err)
	}
	if found, err := st.CancelPlaybookRun(ctx, "pb-1"); !found || err != nil {
		t.Fatalf("abbrechen: %v %v", found, err)
	}
	got, err := st.PlaybookRun(ctx, "pb-1")
	if err != nil || !got.Cancel || !reflect.DeepEqual(got.Steps[0], s) {
		t.Fatalf("%+v %v", got, err)
	}

	if err := st.FinishPlaybookRun(ctx, "pb-1", RunCancelled, "abgebrochen", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.PlaybookRun(ctx, "pb-1")
	if got.Status != RunCancelled || got.FinishedAt == nil || got.Steps[0].Status != StepRunning ||
		got.Steps[1].Status != StepSkipped || got.Steps[1].Message != "nicht ausgeführt" {
		t.Fatalf("%+v", got)
	}
	if found, _ := st.CancelPlaybookRun(ctx, "pb-1"); found {
		t.Error("beendeter lauf abgebrochen")
	}
	if active, _ := st.ActivePlaybookRuns(ctx); len(active) != 0 {
		t.Errorf("noch aktiv: %+v", active)
	}
	if list, err := st.PlaybookRuns(ctx, 10); err != nil || len(list) != 1 || list[0].Steps != nil {
		t.Fatalf("%+v %v", list, err)
	}
	if _, err := st.PlaybookRun(ctx, "fehlt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}

	// Bereinigen: nur beendete Läufe, Schritte gehen mit.
	run2 := run
	run2.ID, run2.CreatedAt = "pb-2", now.Unix()-10
	if err := st.CreatePlaybookRun(ctx, run2, steps[:1]); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PrunePlaybookRuns(ctx, now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("bereinigt %d %v", n, err)
	}
	var left int
	st.db.QueryRow(`SELECT COUNT(*) FROM playbook_steps`).Scan(&left)
	if left != 1 {
		t.Errorf("%d schritte übrig", left)
	}
}

func TestOpenJobs(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"j1", "j2"} {
		if err := st.CreateJob(ctx, Job{ID: id, HostID: "a", Action: "service.restart", CreatedAt: 1, Deadline: 100}); err != nil {
			t.Fatal(err)
		}
	}
	st.FinishJob(ctx, "j1", "a", time.Unix(2, 0), true, 0, "", "")
	jobs, err := st.OpenJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != "j2" {
		t.Fatalf("%+v %v", jobs, err)
	}
}
