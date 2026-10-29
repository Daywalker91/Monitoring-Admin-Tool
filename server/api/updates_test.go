package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/update"
)

// fakeReleases spielt die GitHub-API mit einem signierten Release v0.5.1.
func fakeReleases(t *testing.T) (*httptest.Server, func([]byte, []byte) (release.Manifest, error)) {
	t.Helper()
	dir := t.TempDir()
	privText, pubText, _ := release.GenerateKey()
	priv, _ := release.ParsePrivateKey(privText)
	pub, _ := release.ParsePublicKey(pubText)
	os.WriteFile(filepath.Join(dir, "mat-agent-linux-amd64"), []byte("neues binary"), 0o644)
	manifest, err := release.BuildManifest("v0.5.1", dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, release.ManifestName), manifest, 0o644)
	os.WriteFile(filepath.Join(dir, release.SignatureName), release.Sign(priv, manifest), 0o644)

	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/releases/latest" {
			var assets []map[string]string
			for _, n := range []string{"mat-agent-linux-amd64", release.ManifestName, release.SignatureName} {
				assets = append(assets, map[string]string{"name": n, "browser_download_url": gh.URL + "/dl/" + n})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.5.1", "assets": assets})
			return
		}
		if name, ok := strings.CutPrefix(r.URL.Path, "/dl/"); ok {
			http.ServeFile(w, r, filepath.Join(dir, name))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gh.Close)
	return gh, func(m, s []byte) (release.Manifest, error) { return release.VerifyWith(pub, m, s) }
}

func newUpdateTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword(testPassword)
	st.SetUser(context.Background(), "admin", hash, time.Now())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	gh, verify := fakeReleases(t)
	checker := update.New("o/r", gh.URL, t.TempDir(), log).WithVerifier(verify)
	if err := checker.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	gui := fstest.MapFS{"index.html": {Data: []byte("gui")}}
	ts := httptest.NewServer(New(st, auth.New(st), "secret", gui, log).WithUpdates(checker).WithVersion("v0.5.1").Handler())
	t.Cleanup(ts.Close)
	return ts
}

// fakeAgent verbindet sich mit Version und den Fähigkeiten metrics und update.
func fakeAgent(t *testing.T, ctx context.Context, ts *httptest.Server, version, note string) *websocket.Conn {
	t.Helper()
	return fakeAgentCaps(t, ctx, ts, version, note, proto.CapMetrics, proto.CapUpdate)
}

func fakeAgentCaps(t *testing.T, ctx context.Context, ts *httptest.Server, version, note string, caps ...proto.Capability) *websocket.Conn {
	t.Helper()
	h := http.Header{"Authorization": {"Bearer secret"}}
	conn, _, err := websocket.Dial(ctx, wsURL(ts), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	data, _ := proto.Encode(proto.TypeHello, proto.Hello{AgentID: "a1", Hostname: "a1", IntervalSec: 10,
		AgentVersion: version, OS: "linux", Arch: "amd64", UpdateNote: note,
		Capabilities: caps})
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	return conn
}

// waitHost fragt /api/hosts ab, bis cond für den einzigen Host gilt.
func waitHost(t *testing.T, c *http.Client, ts *httptest.Server, what string, cond func(hostView) bool) hostView {
	t.Helper()
	var last []hostView
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
		resp, err := c.Get(ts.URL + "/api/hosts")
		if err != nil {
			t.Fatal(err)
		}
		last = nil
		json.NewDecoder(resp.Body).Decode(&last)
		resp.Body.Close()
		if len(last) == 1 && cond(last[0]) {
			return last[0]
		}
	}
	t.Fatalf("%s: %+v", what, last)
	return hostView{}
}

func download(t *testing.T, ts *httptest.Server, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestAgentUpdateFlow(t *testing.T) {
	ts := newUpdateTestServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := fakeAgent(t, ctx, ts, "v0.5.0", "")
	waitHost(t, c, ts, "update verfügbar", func(h hostView) bool { return h.UpdateAvailable == "v0.5.1" })

	if code := status(t, c, "POST", ts.URL+"/api/hosts/a1/update", "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("fremde herkunft: %d", code)
	}
	if code := status(t, c, "POST", ts.URL+"/api/hosts/a1/update", ts.URL); code != http.StatusAccepted {
		t.Fatalf("update starten: %d", code)
	}
	if code := status(t, c, "POST", ts.URL+"/api/hosts/a1/update", ts.URL); code != http.StatusConflict {
		t.Fatalf("zweites update: %d, erwartet 409", code)
	}

	// Der Agent bekommt den Job …
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env proto.Envelope
	var job proto.JobRequest
	if json.Unmarshal(data, &env) != nil || env.Type != proto.TypeJobRequest || json.Unmarshal(env.Data, &job) != nil {
		t.Fatalf("kein job: %s", data)
	}
	if job.Action != proto.ActionAgentUpdate || job.Params["version"] != "v0.5.1" || job.Validate() != nil {
		t.Fatalf("job = %+v", job)
	}

	// … und lädt das Binary mit dem Agent-Token.
	if code, body := download(t, ts, "/agent/update/v0.5.1/mat-agent-linux-amd64", "secret"); code != 200 || body != "neues binary" {
		t.Fatalf("download: %d %q", code, body)
	}
	if code, _ := download(t, ts, "/agent/update/v0.5.1/release.json.sig", "secret"); code != 200 {
		t.Fatalf("signatur: %d", code)
	}
	for _, p := range []struct{ path, token string }{
		{"/agent/update/v0.5.1/mat-agent-linux-amd64", ""},
		{"/agent/update/v0.5.1/mat-agent-linux-amd64", "falsch"},
	} {
		if code, _ := download(t, ts, p.path, p.token); code != http.StatusUnauthorized {
			t.Errorf("download ohne gültiges token: %d", code)
		}
	}
	for _, p := range []string{"/agent/update/v0.5.0/release.json", "/agent/update/v0.5.1/..%2F..%2Ft.db", "/agent/update/v0.5.1/x"} {
		if code, _ := download(t, ts, p, "secret"); code != http.StatusNotFound {
			t.Errorf("%s: %d, erwartet 404", p, code)
		}
	}

	res, _ := proto.Encode(proto.TypeJobResult, proto.JobResult{JobID: job.JobID, OK: true, Output: "installiert"})
	conn.Write(ctx, websocket.MessageText, res)
	waitHost(t, c, ts, "neustart", func(h hostView) bool { return h.Update != nil && h.Update.State == updRestarting })

	// Neustart mit neuer Version.
	conn.Close(websocket.StatusGoingAway, "agent startet neu")
	conn = fakeAgent(t, ctx, ts, "v0.5.1", "")
	h := waitHost(t, c, ts, "fertig", func(h hostView) bool { return h.Update != nil && h.Update.State == updDone })
	if h.UpdateAvailable != "" {
		t.Errorf("nach update noch verfügbar: %q", h.UpdateAvailable)
	}

	// Läuft die neue Version nicht stabil, meldet das alte Binary die Rücknahme.
	conn.Close(websocket.StatusGoingAway, "")
	fakeAgent(t, ctx, ts, "v0.5.0", "update auf v0.5.1 zurückgenommen")
	h = waitHost(t, c, ts, "rücknahme", func(h hostView) bool { return h.Update != nil && h.Update.State == updFailed })
	if !strings.Contains(h.UpdateNote, "zurückgenommen") {
		t.Errorf("hinweis = %q", h.UpdateNote)
	}
}

func TestAgentUpdateRejected(t *testing.T) {
	ts := newUpdateTestServer(t)
	c := login(t, ts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := fakeAgent(t, ctx, ts, "v0.5.0", "")
	waitHost(t, c, ts, "online", func(h hostView) bool { return h.Online })
	if code := status(t, c, "POST", ts.URL+"/api/hosts/a1/update", ts.URL); code != http.StatusAccepted {
		t.Fatalf("update starten: %d", code)
	}
	_, data, _ := conn.Read(ctx)
	var env proto.Envelope
	var job proto.JobRequest
	json.Unmarshal(data, &env)
	json.Unmarshal(env.Data, &job)
	res, _ := proto.Encode(proto.TypeJobResult, proto.JobResult{JobID: job.JobID, ExitCode: 1, Error: "signatur ungültig"})
	conn.Write(ctx, websocket.MessageText, res)
	h := waitHost(t, c, ts, "fehler", func(h hostView) bool { return h.Update != nil && h.Update.State == updFailed })
	if h.Update.Error != "signatur ungültig" {
		t.Errorf("fehler = %q", h.Update.Error)
	}

	// Ein bereits aktueller Agent bekommt kein Update.
	conn.Close(websocket.StatusNormalClosure, "")
	fakeAgent(t, ctx, ts, "v0.5.1", "")
	waitHost(t, c, ts, "aktuell", func(h hostView) bool { return h.AgentVersion == "v0.5.1" && h.Online })
	if code := status(t, c, "POST", ts.URL+"/api/hosts/a1/update", ts.URL); code != http.StatusConflict {
		t.Fatalf("update für aktuellen agenten: %d, erwartet 409", code)
	}
}
