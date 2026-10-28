package update

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

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
)

// fakeGitHub liefert ein signiertes Release wie die GitHub-API.
type fakeGitHub struct {
	srv       *httptest.Server
	dir       string
	tag       string
	downloads map[string]int
}

func newFakeGitHub(t *testing.T, version string) (*fakeGitHub, func([]byte, []byte) (release.Manifest, error)) {
	t.Helper()
	g := &fakeGitHub{dir: t.TempDir(), tag: version, downloads: map[string]int{}}
	privText, pubText, _ := release.GenerateKey()
	priv, _ := release.ParsePrivateKey(privText)
	pub, _ := release.ParsePublicKey(pubText)
	os.WriteFile(filepath.Join(g.dir, "mat-agent-linux-arm64"), []byte("arm64-binary"), 0o644)
	os.WriteFile(filepath.Join(g.dir, "mat-agent-windows-amd64.exe"), []byte("windows-binary"), 0o644)
	manifest, err := release.BuildManifest(version, g.dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(g.dir, release.ManifestName), manifest, 0o644)
	os.WriteFile(filepath.Join(g.dir, release.SignatureName), release.Sign(priv, manifest), 0o644)

	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r/releases/latest" {
			type asset struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			}
			var assets []asset
			entries, _ := os.ReadDir(g.dir)
			for _, e := range entries {
				assets = append(assets, asset{e.Name(), g.srv.URL + "/dl/" + e.Name()})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": g.tag, "assets": assets})
			return
		}
		if name, ok := strings.CutPrefix(r.URL.Path, "/dl/"); ok {
			g.downloads[name]++
			http.ServeFile(w, r, filepath.Join(g.dir, name))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g, func(m, s []byte) (release.Manifest, error) { return release.VerifyWith(pub, m, s) }
}

func newChecker(t *testing.T, g *fakeGitHub, verify func([]byte, []byte) (release.Manifest, error)) *Checker {
	c := New("o/r", g.srv.URL, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.keyed, c.verify = true, verify
	return c
}

func TestCheckPrepareServe(t *testing.T) {
	g, verify := newFakeGitHub(t, "v0.5.1")
	c := newChecker(t, g, verify)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Latest != "v0.5.1" || st.Error != "" || !st.Enabled {
		t.Fatalf("status = %+v", st)
	}
	// Manifest und Signatur liegen bereit, Binaries noch nicht.
	if _, ok := c.Path("v0.5.1", release.ManifestName); !ok {
		t.Fatal("manifest nicht im zwischenspeicher")
	}
	if _, ok := c.Path("v0.5.1", "mat-agent-linux-arm64"); ok {
		t.Fatal("binary vor Prepare verfügbar")
	}
	for i := 0; i < 2; i++ {
		v, err := c.Prepare(context.Background(), "linux", "arm64")
		if err != nil || v != "v0.5.1" {
			t.Fatalf("Prepare = %q, %v", v, err)
		}
	}
	if g.downloads["mat-agent-linux-arm64"] != 1 {
		t.Fatalf("binary %d-mal geladen, erwartet 1", g.downloads["mat-agent-linux-arm64"])
	}
	path, ok := c.Path("v0.5.1", "mat-agent-linux-arm64")
	if !ok {
		t.Fatal("binary nach Prepare nicht verfügbar")
	}
	if data, _ := os.ReadFile(path); string(data) != "arm64-binary" {
		t.Fatalf("binary = %q", data)
	}
	// Nur Dateien aus dem Manifest der aktuellen Version.
	for _, p := range [][2]string{{"v0.5.0", release.ManifestName}, {"v0.5.1", "../mat.db"}, {"v0.5.1", "install-agent.sh"}} {
		if _, ok := c.Path(p[0], p[1]); ok {
			t.Errorf("Path(%q, %q) erlaubt", p[0], p[1])
		}
	}
	if _, err := c.Prepare(context.Background(), "linux", "riscv64"); err == nil {
		t.Error("unbekannte plattform vorbereitet")
	}
	if err := c.CheckNow(context.Background()); err != ErrTooSoon {
		t.Errorf("CheckNow direkt danach = %v", err)
	}
}

func TestCheckRejectsBadRelease(t *testing.T) {
	g, verify := newFakeGitHub(t, "v0.5.1")

	// Manipuliertes Binary: Prepare schlägt fehl, nichts landet im Zwischenspeicher.
	c := newChecker(t, g, verify)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(g.dir, "mat-agent-linux-arm64"), []byte("böse"), 0o644)
	if _, err := c.Prepare(context.Background(), "linux", "arm64"); err == nil {
		t.Fatal("manipuliertes binary akzeptiert")
	}
	if _, ok := c.Path("v0.5.1", "mat-agent-linux-arm64"); ok {
		t.Fatal("manipuliertes binary im zwischenspeicher")
	}

	// Fremde Signatur.
	_, other := newFakeGitHub(t, "v0.5.1")
	if err := newChecker(t, g, other).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "signatur") {
		t.Fatalf("fremde signatur: %v", err)
	}

	// Tag passt nicht zum Manifest (z. B. altes Manifest an neuem Release).
	g.tag = "v0.6.0"
	if err := newChecker(t, g, verify).Check(context.Background()); err == nil {
		t.Fatal("tag/manifest-abweichung akzeptiert")
	}
}

func TestDisabled(t *testing.T) {
	c := New("off", "", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.keyed = true
	if c.Enabled() || c.Check(context.Background()) == nil {
		t.Fatal("abgeschalteter checker prüft")
	}
	c = New("", "", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.keyed = false
	if st := c.Status(); c.Enabled() || !strings.Contains(st.Message, "signaturschlüssel") {
		t.Fatalf("ohne schlüssel: %+v", st)
	}
}
