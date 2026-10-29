package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/checks"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func TestChecksAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hash, _ := auth.HashPassword(testPassword)
	st.SetUser(ctx, "admin", hash, time.Now())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ev, _ := alert.New(ctx, st, func(alert.Event) {}, log, time.Now())

	var srv *Server
	runner := checks.NewRunner(st, log, func(ctx context.Context, c store.Check, r store.CheckResult) {
		srv.CheckResult(ctx, c, r)
	})
	srv = New(st, auth.New(st), "secret", fstest.MapFS{}, log).WithAlerts(ev, nil).WithChecks(runner)
	go runner.Run(ctx)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()
	c := login(t, ts)

	send := func(method, path, origin string, body any) *http.Response {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(data))
		req.Header.Set("Origin", origin)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	in := checkInput{Name: "Web", Kind: "http", Target: target.URL, Enabled: true}
	if r := send("POST", "/api/checks", "http://evil.example", in); r.StatusCode != http.StatusForbidden {
		t.Errorf("fremde herkunft: %d", r.StatusCode)
	}
	if r := send("POST", "/api/checks", ts.URL, checkInput{Name: "x", Kind: "http", Target: "file:///etc/passwd"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("ungültig: %d", r.StatusCode)
	}
	r := send("POST", "/api/checks", ts.URL, in)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("anlegen: %d", r.StatusCode)
	}
	var created store.Check
	json.NewDecoder(r.Body).Decode(&created)
	if created.ID == 0 || created.IntervalSec != 60 {
		t.Fatalf("angelegt: %+v", created)
	}

	// Der Runner führt den neuen Check sofort aus.
	var list []checkView
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := c.Get(ts.URL + "/api/checks")
		list = nil
		json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if len(list) == 1 && list[0].Last != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(list) != 1 || list[0].Last == nil || !list[0].Last.OK || list[0].Last.Status != 200 {
		t.Fatalf("liste: %+v", list)
	}

	in.Enabled = false
	path := "/api/checks/" + strconv.FormatInt(created.ID, 10)
	if r := send("PUT", path, ts.URL, in); r.StatusCode != http.StatusOK {
		t.Fatalf("ändern: %d", r.StatusCode)
	}
	if got, _ := st.CheckByID(ctx, created.ID); got.Enabled {
		t.Fatal("nicht abgeschaltet")
	}
	resp, _ := c.Get(ts.URL + path + "/history?hours=1&points=60")
	var hist []store.CheckPoint
	json.NewDecoder(resp.Body).Decode(&hist)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(hist) != 1 || hist[0].OKPct != 100 {
		t.Fatalf("verlauf: %d %+v", resp.StatusCode, hist)
	}
	if r := send("DELETE", path, ts.URL, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("löschen: %d", r.StatusCode)
	}
	if r := send("DELETE", path, ts.URL, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("doppelt löschen: %d", r.StatusCode)
	}
	if r := send("PUT", "/api/checks/abc", ts.URL, in); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("ungültige id: %d", r.StatusCode)
	}
}
