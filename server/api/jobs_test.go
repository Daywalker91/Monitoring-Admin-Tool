package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Testaktionen ohne Parameter, nur für diese Tests in der Whitelist.
const (
	testAction       = "test.ping"
	testActionReboot = "test.reboot"
)

func init() {
	proto.ActionCapability[testAction] = proto.CapServices
	proto.ActionCapability[testActionReboot] = proto.CapReboot
}

func getJSON(t *testing.T, c *http.Client, url string, v any) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(v)
	return resp.StatusCode
}

func TestJobsAPI(t *testing.T) {
	ts := newUpdateTestServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := fakeAgentCaps(t, ctx, ts, "v0.5.0", "", proto.CapMetrics, proto.CapUpdate, proto.CapServices)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })

	url := ts.URL + "/api/hosts/a1/jobs"
	for _, tc := range []struct {
		origin string
		body   jobRequest
		want   int
	}{
		{"http://evil.example", jobRequest{Action: testAction}, http.StatusForbidden},
		{ts.URL, jobRequest{Action: "shell.exec", Params: map[string]string{"cmd": "id"}}, http.StatusBadRequest},
		{ts.URL, jobRequest{Action: testAction, Params: map[string]string{"x": "y"}}, http.StatusBadRequest},
		{ts.URL, jobRequest{Action: proto.ActionAgentUpdate, Params: map[string]string{"version": "v0.5.1"}}, http.StatusBadRequest},
		{ts.URL, jobRequest{Action: testActionReboot}, http.StatusConflict}, // fähigkeit nicht gemeldet
	} {
		if code := postJSON(t, c, url, tc.origin, tc.body).StatusCode; code != tc.want {
			t.Errorf("%+v: %d, erwartet %d", tc.body, code, tc.want)
		}
	}
	if code := postJSON(t, c, ts.URL+"/api/hosts/fehlt/jobs", ts.URL, jobRequest{Action: testAction}).StatusCode; code != http.StatusNotFound {
		t.Errorf("unbekannter host: %d", code)
	}

	resp := postJSON(t, c, url, ts.URL, jobRequest{Action: testAction})
	var started store.Job
	json.NewDecoder(resp.Body).Decode(&started)
	if resp.StatusCode != http.StatusAccepted || started.ID == "" || started.User != "admin" {
		t.Fatalf("job starten: %d %+v", resp.StatusCode, started)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	var job proto.JobRequest
	json.Unmarshal(data, &env)
	json.Unmarshal(env.Data, &job)
	if env.Type != proto.TypeJobRequest || job.JobID != started.ID || job.Action != testAction {
		t.Fatalf("beim agenten: %s", data)
	}

	var list []store.Job
	if getJSON(t, c, url, &list); len(list) != 1 || list[0].FinishedAt != nil {
		t.Fatalf("laufender job: %+v", list)
	}

	// Zwischenstände: in Reihenfolge angehängt, Wiederholungen und fremde Jobs ignoriert.
	for _, p := range []proto.JobProgress{
		{JobID: job.JobID, Seq: 1, Data: "po"},
		{JobID: job.JobID, Seq: 1, Data: "XX"},
		{JobID: "job-fremd", Seq: 2, Data: "YY"},
		{JobID: job.JobID, Seq: 2, Data: "ng…"},
	} {
		msg, _ := proto.Encode(proto.TypeJobProgress, p)
		conn.Write(ctx, websocket.MessageText, msg)
	}
	var got store.Job
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
		if getJSON(t, c, ts.URL+"/api/jobs/"+job.JobID, &got); got.Output == "pong…" {
			break
		}
	}
	if got.FinishedAt != nil || got.Output != "pong…" {
		t.Fatalf("zwischenstand: %+v", got)
	}

	res, _ := proto.Encode(proto.TypeJobResult, proto.JobResult{JobID: job.JobID, OK: true, Output: "pong"})
	conn.Write(ctx, websocket.MessageText, res)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
		if getJSON(t, c, ts.URL+"/api/jobs/"+job.JobID, &got); got.FinishedAt != nil {
			break
		}
	}
	if got.FinishedAt == nil || !got.OK || got.Output != "pong" {
		t.Fatalf("ergebnis: %+v", got)
	}
	if code := getJSON(t, c, ts.URL+"/api/jobs/job-fehlt", &got); code != http.StatusNotFound {
		t.Errorf("fehlender job: %d", code)
	}

	// Das Self-Update landet ebenfalls im Audit-Log.
	if code := status(t, c, "POST", ts.URL+"/api/hosts/a1/update", ts.URL); code != http.StatusAccepted {
		t.Fatalf("update: %d", code)
	}
	getJSON(t, c, url, &list)
	if len(list) != 2 || list[0].Action != proto.ActionAgentUpdate || list[0].Params["version"] != "v0.5.1" {
		t.Fatalf("update im verlauf: %+v", list)
	}
}

func TestJobOffline(t *testing.T) {
	ts := newUpdateTestServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := fakeAgentCaps(t, ctx, ts, "v0.5.0", "", proto.CapServices)
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	conn.Close(websocket.StatusNormalClosure, "")
	waitHost(t, c, ts, "offline", func(h hostView) bool { return !h.Online })
	if code := postJSON(t, c, ts.URL+"/api/hosts/a1/jobs", ts.URL, jobRequest{Action: testAction}).StatusCode; code != http.StatusConflict {
		t.Fatalf("offline: %d, erwartet 409", code)
	}
}

func TestLogReadSources(t *testing.T) {
	ts := newUpdateTestServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello, _ := proto.Encode(proto.TypeHello, proto.Hello{AgentID: "a1", Hostname: "a1", IntervalSec: 10,
		Capabilities: []proto.Capability{proto.CapLogs}, LogSources: []string{"journal:sshd"}})
	conn.Write(ctx, websocket.MessageText, hello)
	h := waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	if len(h.LogSources) != 1 || h.LogSources[0] != "journal:sshd" {
		t.Fatalf("quellen: %v", h.LogSources)
	}

	url := ts.URL + "/api/hosts/a1/jobs"
	if code := postJSON(t, c, url, ts.URL, jobRequest{Action: proto.ActionLogRead,
		Params: map[string]string{"source": "journal:k3s"}}).StatusCode; code != http.StatusBadRequest {
		t.Errorf("nicht freigegebene quelle: %d", code)
	}
	if code := postJSON(t, c, url, ts.URL, jobRequest{Action: proto.ActionLogRead,
		Params: map[string]string{"source": "journal:sshd", "lines": "50"}}).StatusCode; code != http.StatusAccepted {
		t.Fatalf("freigegebene quelle: %d", code)
	}
	_, data, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"source":"journal:sshd"`) {
		t.Fatalf("beim agenten: %s %v", data, err)
	}
}
