package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const approveBook = `name: freigabe
targets: [n1, n2]
steps:
  - {id: vorher, action: packages.refresh}
  - {id: ok, approve: "weiter mit dem nächsten Host", once: %s, timeout: %s}
  - {id: danach, action: packages.refresh}
`

func approveBookWith(once, timeout string) string {
	return strings.Replace(strings.Replace(approveBook, "%s", once, 1), "%s", timeout, 1)
}

// waitApproval wartet, bis der Freigabe-Schritt (Index 1) auf host läuft.
func waitApproval(t *testing.T, s *Server, id, host string) *pbRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.pb.mu.Lock()
		run := s.pb.runs[id]
		s.pb.mu.Unlock()
		if run != nil && run.step(host, 1).Status == store.StepRunning {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("freigabe auf %s wartet nicht", host)
	return nil
}

func TestPlaybookApproveOnce(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(),
		map[string]string{"a.yaml": approveBookWith("true", "1h")})
	run, err := s.startPlaybook(context.Background(), "freigabe", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	r := waitApproval(t, s, run.ID, "n1")
	if f.started() != "n1/vorher" || r.step("n1", 1).Message != msgAwaitApproval {
		t.Fatalf("vor der freigabe: %s / %q", f.started(), r.step("n1", 1).Message)
	}
	if !r.decide("n1", 1, true, approval{true, "anna"}) || r.decide("n2", 1, true, approval{false, "bob"}) {
		t.Fatal("once: erste entscheidung muss gelten")
	}
	done := waitRun(t, s, run.ID)
	if done.Status != store.RunOK || f.started() != "n1/vorher n1/danach n2/vorher n2/danach" {
		t.Fatalf("%s: %s / %s", done.Status, stepStates(done), f.started())
	}
	if done.Steps[1].Message != "freigegeben von anna (für alle hosts)" || done.Steps[4].Message != done.Steps[1].Message {
		t.Errorf("meldungen: %q %q", done.Steps[1].Message, done.Steps[4].Message)
	}
}

func TestPlaybookApproveReject(t *testing.T) {
	s, f, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(),
		map[string]string{"a.yaml": approveBookWith("false", "1h")})
	run, _ := s.startPlaybook(context.Background(), "freigabe", "admin", "")
	waitApproval(t, s, run.ID, "n1").decide("n1", 1, false, approval{false, "anna"})
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunStopped || r.Steps[1].Message != "abgelehnt von anna" || f.started() != "n1/vorher" {
		t.Fatalf("%s: %s / %q", r.Status, stepStates(r), r.Steps[1].Message)
	}
}

func TestPlaybookApproveTimeout(t *testing.T) {
	s, _, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(),
		map[string]string{"a.yaml": approveBookWith("false", "1s")})
	run, _ := s.startPlaybook(context.Background(), "freigabe", "admin", "")
	r := waitRun(t, s, run.ID)
	if r.Status != store.RunStopped || r.Steps[1].Message != "keine freigabe nach 1s" {
		t.Fatalf("%s: %q", r.Status, r.Steps[1].Message)
	}
}

func TestPlaybookApproveHTTP(t *testing.T) {
	s, _, _ := newPlaybookServer(t, filepath.Join(t.TempDir(), "mat.db"), threeHosts(),
		map[string]string{"a.yaml": approveBookWith("false", "1h")})
	s.auth = auth.New(s.store)
	hash, _ := auth.HashPassword(testPassword)
	s.store.SetUser(context.Background(), "admin", hash, time.Now())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := login(t, ts)

	run, _ := s.startPlaybook(context.Background(), "freigabe", "admin", "")
	waitApproval(t, s, run.ID, "n1")
	url := ts.URL + "/api/playbook-runs/" + run.ID + "/approve"
	post := func(body, origin string) int {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Origin", origin)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, x := range []struct {
		body, origin string
		want         int
	}{
		{`{"host":"n1","step":1,"ok":true}`, "https://evil.example", http.StatusForbidden},
		{`{"host":"n1","step":0,"ok":true}`, ts.URL, http.StatusBadRequest}, // kein Freigabe-Schritt
		{`{"host":"n9","step":1,"ok":true}`, ts.URL, http.StatusBadRequest}, // nicht im Lauf
		{`{"host":"n2","step":1,"ok":true}`, ts.URL, http.StatusConflict},   // wartet dort noch nicht
		{`{"host":"n1","step":1,"ok":true}`, ts.URL, http.StatusNoContent},
	} {
		if got := post(x.body, x.origin); got != x.want {
			t.Errorf("%s: %d, erwartet %d", x.body, got, x.want)
		}
	}
	waitApproval(t, s, run.ID, "n2").decide("n2", 1, false, approval{true, "admin"})
	if r := waitRun(t, s, run.ID); r.Status != store.RunOK || r.Steps[1].Message != "freigegeben von admin" {
		t.Fatalf("%s: %q", r.Status, r.Steps[1].Message)
	}
	if got := post(`{"host":"n1","step":1,"ok":true}`, ts.URL); got != http.StatusNotFound {
		t.Errorf("beendeter lauf: %d", got)
	}
}

func TestRestoreApproval(t *testing.T) {
	step := playbook.Step{Approve: "weiter", Once: true}
	r := &pbRun{}
	r.restoreApproval(step, store.PlaybookStep{HostID: "n1", Index: 2, Status: store.StepOK, Message: "freigegeben von anna (für alle hosts)"})
	if a, ok := r.decision("n2", 2, true); !ok || !a.ok || a.user != "anna" {
		t.Fatalf("%+v %v", a, ok)
	}
	r.restoreApproval(step, store.PlaybookStep{HostID: "n1", Index: 3, Status: store.StepFailed, Message: "keine freigabe nach 24h0m0s"})
	if _, ok := r.decision("n2", 3, true); ok {
		t.Fatal("frist ist keine entscheidung")
	}
}
