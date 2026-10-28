// Package update holt signierte Agent-Releases von GitHub und hält sie für die
// Agenten bereit.
//
// Der Server prüft die Signatur selbst (bietet also nur echte Releases an),
// die Agenten prüfen sie beim Update aber noch einmal: der Server ist nur
// Verteiler. Binaries werden erst geladen, wenn ein Host sie braucht, und neben
// der Datenbank zwischengespeichert (<db-ordner>/updates/<version>/).
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
)

const (
	// DefaultRepo ist das GitHub-Repository mit den Releases.
	DefaultRepo = "Daywalker91/Monitoring-Admin-Tool"
	// DefaultAPI ist die GitHub-API.
	DefaultAPI = "https://api.github.com"

	checkInterval   = 6 * time.Hour
	minCheckGap     = time.Minute // manuelles Prüfen höchstens einmal pro Minute
	requestTimeout  = 30 * time.Second
	downloadTimeout = 5 * time.Minute
	maxAPIBytes     = 1 << 20
)

// Status beschreibt den letzten Stand der Release-Prüfung.
type Status struct {
	Enabled   bool   `json:"enabled"`           // Signaturschlüssel eingebaut und Quelle gesetzt
	Repo      string `json:"repo,omitempty"`    // z. B. Daywalker91/Monitoring-Admin-Tool
	Latest    string `json:"latest,omitempty"`  // neueste geprüfte Version, leer = keine
	CheckedAt int64  `json:"checked_at"`        // Unix-Sekunden, 0 = noch nie
	Error     string `json:"error,omitempty"`   // Fehler der letzten Prüfung
	Message   string `json:"message,omitempty"` // Hinweis, z. B. warum abgeschaltet
}

// Checker fragt GitHub nach dem neuesten Release.
type Checker struct {
	repo   string
	api    string
	dir    string
	client *http.Client
	log    *slog.Logger
	keyed  bool // Signaturschlüssel eingebaut
	verify func(manifest, sig []byte) (release.Manifest, error)
	now    func() time.Time

	mu        sync.Mutex
	manifest  *release.Manifest // neuestes geprüftes Release
	assets    map[string]string // Dateiname → Download-URL
	checkedAt time.Time
	lastErr   error

	fetchMu sync.Mutex // es lädt immer nur ein Binary gleichzeitig
}

// New erzeugt einen Checker. repo "off" schaltet die Prüfung ab; dir ist der
// Zwischenspeicher für Releases.
func New(repo, api, dir string, log *slog.Logger) *Checker {
	if repo == "" {
		repo = DefaultRepo
	}
	if api == "" {
		api = DefaultAPI
	}
	return &Checker{repo: repo, api: api, dir: dir, log: log, keyed: release.Enabled(), verify: release.Verify, now: time.Now,
		client: &http.Client{Timeout: downloadTimeout}}
}

// WithVerifier prüft Releases mit verify statt mit dem eingebauten Schlüssel
// und schaltet die Prüfung damit ein. Nur für Tests.
func (c *Checker) WithVerifier(verify func(manifest, sig []byte) (release.Manifest, error)) *Checker {
	c.keyed, c.verify = true, verify
	return c
}

// Enabled meldet, ob Updates möglich sind.
func (c *Checker) Enabled() bool { return c.repo != "off" && c.keyed }

// Status liefert den letzten Stand.
func (c *Checker) Status() Status {
	st := Status{Enabled: c.Enabled(), Repo: c.repo}
	switch {
	case !c.keyed:
		st.Message = "kein signaturschlüssel eingebaut – self-update ist in diesem build abgeschaltet"
		st.Repo = ""
	case c.repo == "off":
		st.Message = "abgeschaltet (MAT_UPDATE_REPO=off)"
		st.Repo = ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.manifest != nil {
		st.Latest = c.manifest.Version
	}
	if !c.checkedAt.IsZero() {
		st.CheckedAt = c.checkedAt.Unix()
	}
	if c.lastErr != nil {
		st.Error = c.lastErr.Error()
	}
	return st
}

// Latest liefert das neueste geprüfte Manifest.
func (c *Checker) Latest() (release.Manifest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.manifest == nil {
		return release.Manifest{}, false
	}
	return *c.manifest, true
}

// Run prüft beim Start und danach alle 6 Stunden.
func (c *Checker) Run(ctx context.Context) {
	if !c.Enabled() {
		return
	}
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		if err := c.Check(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("release-prüfung fehlgeschlagen", "repo", c.repo, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ErrTooSoon bedeutet, dass die letzte Prüfung weniger als eine Minute her ist.
var ErrTooSoon = errors.New("zuletzt vor weniger als einer minute geprüft")

// CheckNow prüft sofort, aber höchstens einmal pro Minute.
func (c *Checker) CheckNow(ctx context.Context) error {
	c.mu.Lock()
	recent := !c.checkedAt.IsZero() && c.now().Sub(c.checkedAt) < minCheckGap
	c.mu.Unlock()
	if recent {
		return ErrTooSoon
	}
	return c.Check(ctx)
}

// ghRelease ist der benötigte Teil der GitHub-Antwort.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check fragt das neueste Release ab, lädt Manifest und Signatur und prüft sie.
func (c *Checker) Check(ctx context.Context) error {
	if !c.Enabled() {
		return errors.New("self-update ist abgeschaltet")
	}
	m, assets, err := c.fetchLatest(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkedAt = c.now()
	c.lastErr = err
	if err != nil {
		return err
	}
	if c.manifest == nil || c.manifest.Version != m.Version {
		c.log.Info("agent-release verfügbar", "version", m.Version, "binaries", len(m.Files))
	}
	c.manifest, c.assets = &m, assets
	return nil
}

func (c *Checker) fetchLatest(ctx context.Context) (release.Manifest, map[string]string, error) {
	var m release.Manifest
	u := c.api + "/repos/" + c.repo + "/releases/latest"
	body, err := c.get(ctx, u, maxAPIBytes, "application/vnd.github+json")
	if errors.Is(err, errNotFound) {
		return m, nil, errors.New("noch kein release veröffentlicht")
	}
	if err != nil {
		return m, nil, err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return m, nil, fmt.Errorf("github-antwort lesen: %w", err)
	}
	assets := make(map[string]string, len(rel.Assets))
	for _, a := range rel.Assets {
		assets[a.Name] = a.URL
	}
	if assets[release.ManifestName] == "" || assets[release.SignatureName] == "" {
		return m, nil, fmt.Errorf("release %s ist nicht signiert (release.json/.sig fehlen)", rel.TagName)
	}
	manifest, err := c.get(ctx, assets[release.ManifestName], release.MaxManifestBytes, "")
	if err != nil {
		return m, nil, err
	}
	sig, err := c.get(ctx, assets[release.SignatureName], release.MaxSignatureBytes, "")
	if err != nil {
		return m, nil, err
	}
	if m, err = c.verify(manifest, sig); err != nil {
		return m, nil, fmt.Errorf("release %s: %w", rel.TagName, err)
	}
	if tag, err := release.ParseVersion(rel.TagName); err != nil || tag != mustVersion(m.Version) {
		return m, nil, fmt.Errorf("release-tag %q passt nicht zum manifest %s", rel.TagName, m.Version)
	}
	// Geprüftes Manifest samt Signatur ablegen: die Agenten prüfen es selbst.
	vdir := filepath.Join(c.dir, m.Version)
	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return m, nil, fmt.Errorf("zwischenspeicher anlegen: %w", err)
	}
	for name, data := range map[string][]byte{release.ManifestName: manifest, release.SignatureName: sig} {
		if err := writeAtomic(filepath.Join(vdir, name), data); err != nil {
			return m, nil, err
		}
	}
	c.cleanup(m.Version)
	return m, assets, nil
}

func mustVersion(s string) release.Version {
	v, _ := release.ParseVersion(s)
	return v
}

// cleanup entfernt zwischengespeicherte ältere Versionen.
func (c *Checker) cleanup(keep string) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != keep {
			if err := os.RemoveAll(filepath.Join(c.dir, e.Name())); err != nil {
				c.log.Warn("alten release-zwischenspeicher entfernen", "dir", e.Name(), "err", err)
			}
		}
	}
}

// Prepare stellt sicher, dass das Binary für goos/goarch der neuesten Version
// geprüft im Zwischenspeicher liegt, und liefert diese Version.
func (c *Checker) Prepare(ctx context.Context, goos, goarch string) (string, error) {
	c.mu.Lock()
	m, assets := c.manifest, c.assets
	c.mu.Unlock()
	if m == nil {
		return "", errors.New("kein release bekannt")
	}
	f, ok := m.For(goos, goarch)
	if !ok {
		return "", fmt.Errorf("release %s enthält kein binary für %s/%s", m.Version, goos, goarch)
	}
	path := filepath.Join(c.dir, m.Version, f.Name)

	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	if release.CheckFile(path, f) == nil {
		return m.Version, nil
	}
	src := assets[f.Name]
	if src == "" {
		return "", fmt.Errorf("release %s: %s fehlt", m.Version, f.Name)
	}
	if err := c.download(ctx, src, path, f); err != nil {
		return "", err
	}
	c.log.Info("agent-binary geladen", "version", m.Version, "file", f.Name, "bytes", f.Size)
	return m.Version, nil
}

// Path liefert den Pfad einer zwischengespeicherten Datei der neuesten Version:
// Manifest, Signatur oder ein bereits geladenes Binary. Andere Namen gibt es nicht.
func (c *Checker) Path(version, name string) (string, bool) {
	c.mu.Lock()
	m := c.manifest
	c.mu.Unlock()
	if m == nil || m.Version != version {
		return "", false
	}
	if _, ok := m.File(name); !ok && name != release.ManifestName && name != release.SignatureName {
		return "", false
	}
	path := filepath.Join(c.dir, m.Version, name)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

var errNotFound = errors.New("nicht gefunden")

// get lädt eine Antwort vollständig (höchstens limit Bytes).
func (c *Checker) get(ctx context.Context, rawURL string, limit int64, accept string) ([]byte, error) {
	resp, err := c.open(ctx, rawURL, accept, requestTimeout)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("laden: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("antwort von %s zu groß", hostOf(rawURL))
	}
	return data, nil
}

func (c *Checker) open(ctx context.Context, rawURL, accept string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("anfrage: %w", err)
	}
	req.Header.Set("User-Agent", "mat-server")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s: %w", hostOf(rawURL), err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("%s: http %d", hostOf(rawURL), resp.StatusCode)
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// download lädt ein Binary nach path und prüft Größe und Prüfsumme, bevor es
// unter seinem Namen sichtbar wird.
func (c *Checker) download(ctx context.Context, src, path string, f release.File) error {
	resp, err := c.open(ctx, src, "", downloadTimeout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp := path + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("zwischenspeicher: %w", err)
	}
	_, err = io.Copy(out, io.LimitReader(resp.Body, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = release.CheckFile(tmp, f)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s laden: %w", f.Name, err)
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("zwischenspeicher schreiben: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("zwischenspeicher schreiben: %w", err)
	}
	return nil
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return "?"
}
