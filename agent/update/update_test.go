package update

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fixture baut ein signiertes Release v0.5.1 und einen Server, der es ausliefert.
type fixture struct {
	dir     string // Release-Dateien
	exe     string // "laufendes" Binary
	srv     *httptest.Server
	u       *Updater
	newBody string
	auth    []string // gesehene Authorization-Header
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), newBody: "neues binary"}
	privText, pubText, err := release.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := release.ParsePrivateKey(privText)
	pub, _ := release.ParsePublicKey(pubText)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mat-agent-linux-amd64", f.newBody)
	write("mat-agent-linux-arm64", "anderes binary")
	manifest, err := release.BuildManifest("v0.5.1", f.dir)
	if err != nil {
		t.Fatal(err)
	}
	write(release.ManifestName, string(manifest))
	write(release.SignatureName, string(release.Sign(priv, manifest)))

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		name, ok := strings.CutPrefix(r.URL.Path, "/agent/update/v0.5.1/")
		if !ok || strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(f.dir, name))
	}))
	t.Cleanup(f.srv.Close)

	f.exe = filepath.Join(t.TempDir(), "mat-agent")
	if err := os.WriteFile(f.exe, []byte("altes binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.u = &Updater{Exe: f.exe, Version: "v0.5.0", BaseURL: f.srv.URL, Token: "tok", Log: quiet,
		goos: "linux", goarch: "amd64",
		verify: func(m, s []byte) (release.Manifest, error) { return release.VerifyWith(pub, m, s) },
		probe:  func(context.Context, string) (string, error) { return "v0.5.1\n", nil },
	}
	return f
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplyAndConfirm(t *testing.T) {
	f := newFixture(t)
	msg, err := f.u.Apply(context.Background(), map[string]string{"version": "v0.5.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "v0.5.1") {
		t.Errorf("meldung = %q", msg)
	}
	if got := readFile(t, f.exe); got != f.newBody {
		t.Fatalf("binary = %q", got)
	}
	if got := readFile(t, f.exe+".old"); got != "altes binary" {
		t.Fatalf("backup = %q", got)
	}
	for _, a := range f.auth {
		if a != "Bearer tok" {
			t.Fatalf("authorization = %q", a)
		}
	}

	// Neue Version startet und verbindet sich stabil.
	if note, restart := Startup(f.exe, "v0.5.1", quiet); note != "" || restart {
		t.Fatalf("Startup = %q, %v", note, restart)
	}
	Confirm(f.exe, "v0.5.1", quiet)
	if _, err := os.Stat(stateFile(f.exe)); !os.IsNotExist(err) {
		t.Fatal("update-status nach Confirm noch vorhanden")
	}
}

func TestRollbackAfterUnstableStarts(t *testing.T) {
	f := newFixture(t)
	if _, err := f.u.Apply(context.Background(), map[string]string{"version": "v0.5.1"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxAttempts; i++ {
		if _, restart := Startup(f.exe, "v0.5.1", quiet); restart {
			t.Fatalf("start %d: zu früh zurückgelegt", i+1)
		}
	}
	if _, restart := Startup(f.exe, "v0.5.1", quiet); !restart {
		t.Fatal("nach zu vielen starts nicht zurückgelegt")
	}
	if got := readFile(t, f.exe); got != "altes binary" {
		t.Fatalf("binary nach rollback = %q", got)
	}
	if got := readFile(t, f.exe+".failed"); got != f.newBody {
		t.Fatalf("failed = %q", got)
	}
	// Das alte Binary startet und meldet die Rücknahme einmal.
	note, restart := Startup(f.exe, "v0.5.0", quiet)
	if restart || !strings.Contains(note, "zurückgenommen") {
		t.Fatalf("Startup alt = %q, %v", note, restart)
	}
	if note, _ := Startup(f.exe, "v0.5.0", quiet); note != "" {
		t.Fatalf("meldung doppelt: %q", note)
	}
}

func TestApplyRejects(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
		change func(f *fixture)
		want   string
	}{
		{"downgrade", map[string]string{"version": "v0.5.0"}, nil, "nicht neuer"},
		{"zusatzparameter", map[string]string{"version": "v0.5.1", "url": "http://evil"}, nil, "parameter"},
		{"ungültige version", map[string]string{"version": "../../x"}, nil, "version"},
		{"falsche signatur", map[string]string{"version": "v0.5.1"}, func(f *fixture) {
			_, other, _ := release.GenerateKey()
			pub, _ := release.ParsePublicKey(other)
			f.u.verify = func(m, s []byte) (release.Manifest, error) { return release.VerifyWith(pub, m, s) }
		}, "signatur"},
		{"manipuliertes binary", map[string]string{"version": "v0.5.1"}, func(f *fixture) {
			os.WriteFile(filepath.Join(f.dir, "mat-agent-linux-amd64"), []byte("böse binary"), 0o644)
		}, "prüfsumme"},
		{"unbekannte plattform", map[string]string{"version": "v0.5.1"}, func(f *fixture) { f.u.goos = "plan9" }, "kein binary"},
		{"probe meldet andere version", map[string]string{"version": "v0.5.1"}, func(f *fixture) {
			f.u.probe = func(context.Context, string) (string, error) { return "v0.4.0", nil }
		}, "meldet version"},
		{"ohne eingebauten schlüssel", map[string]string{"version": "v0.5.1"}, func(f *fixture) { f.u.verify = nil }, "signaturschlüssel"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			if c.change != nil {
				c.change(f)
			}
			_, err := f.u.Apply(context.Background(), c.params)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, erwartet %q", err, c.want)
			}
			if got := readFile(t, f.exe); got != "altes binary" {
				t.Fatalf("binary verändert: %q", got)
			}
			if _, err := os.Stat(f.exe + ".new"); !os.IsNotExist(err) {
				t.Fatal("*.new bleibt liegen")
			}
		})
	}
}

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://10.0.0.108/ws/agent":        "https://10.0.0.108",
		"ws://127.0.0.1:8080/ws/agent":     "http://127.0.0.1:8080",
		"wss://example.org/mat/ws/agent/":  "https://example.org/mat",
		"wss://example.org/ws/agent?x=1#y": "https://example.org",
	} {
		got, err := BaseURL(in)
		if err != nil || got != want {
			t.Errorf("BaseURL(%q) = %q, %v; will %q", in, got, err, want)
		}
	}
	if _, err := BaseURL("http://x/ws/agent"); err == nil {
		t.Error("http-schema akzeptiert")
	}
}

func TestSibling(t *testing.T) {
	if got := sibling(`C:\Program Files\mat-agent\mat-agent.exe`, "old"); got != `C:\Program Files\mat-agent\mat-agent.old.exe` {
		t.Errorf("sibling = %s", got)
	}
	if got := stateFile("/usr/local/bin/mat-agent"); got != "/usr/local/bin/mat-agent.update.json" {
		t.Errorf("stateFile = %s", got)
	}
}
