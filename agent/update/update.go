// Package update führt das Self-Update des Agenten aus (Job agent.update).
//
// Ablauf: Manifest und Signatur vom Server holen, Signatur mit dem eingebauten
// Schlüssel prüfen (dem Server wird nicht vertraut), passendes Binary laden,
// Größe und SHA-256 prüfen, das neue Binary probehalber mit "version"
// aufrufen, dann austauschen. Das alte Binary bleibt als *.old liegen. Der
// Agent beendet sich danach; Dienst-Manager bzw. systemd starten ihn neu.
//
// Startet die neue Version mehrmals, ohne stabil mit dem Server verbunden zu
// sein, setzt Startup das alte Binary zurück.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
)

const (
	// maxAttempts ist die Zahl der Starts, die eine neue Version bekommt, bevor
	// sie zurückgenommen wird.
	maxAttempts   = 3
	probeTimeout  = 15 * time.Second
	downloadPath  = "/agent/update/"
	agentWSSuffix = "/ws/agent"
)

// Updater lädt und installiert neue Agent-Versionen.
type Updater struct {
	Exe        string       // Pfad des laufenden Binaries
	Version    string       // eigene Version
	BaseURL    string       // Server-Basis, z. B. https://192.168.100.50 (siehe BaseURL)
	Token      string       // Agent-Token
	HTTPClient *http.Client // mit gepinntem Zertifikat; nil = Standard
	Log        *slog.Logger

	// Für Tests austauschbar.
	goos, goarch string
	verify       func(manifest, sig []byte) (release.Manifest, error)
	probe        func(ctx context.Context, path string) (string, error)
}

// BaseURL leitet aus der WebSocket-URL des Servers die HTTP-Basis ab:
// wss://host/pfad/ws/agent → https://host/pfad.
func BaseURL(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("server-url: %w", err)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("server-url: unbekanntes schema %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), agentWSSuffix)
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// Apply führt agent.update aus. params enthält genau "version". Bei Erfolg ist
// das neue Binary installiert; der Aufrufer muss den Agenten neu starten.
func (u *Updater) Apply(ctx context.Context, params map[string]string) (string, error) {
	target, ok := params["version"]
	if !ok || len(params) != 1 {
		return "", errors.New("parameter: genau \"version\" erwartet")
	}
	want, err := release.ParseVersion(target)
	if err != nil {
		return "", err
	}
	if !release.Newer(target, u.Version) {
		return "", fmt.Errorf("version %s ist nicht neuer als %s", target, u.Version)
	}

	manifest, err := u.get(ctx, target, release.ManifestName, release.MaxManifestBytes)
	if err != nil {
		return "", err
	}
	sig, err := u.get(ctx, target, release.SignatureName, release.MaxSignatureBytes)
	if err != nil {
		return "", err
	}
	verify := u.verify
	if verify == nil {
		verify = release.Verify
	}
	m, err := verify(manifest, sig)
	if err != nil {
		return "", fmt.Errorf("release prüfen: %w", err)
	}
	if got, _ := release.ParseVersion(m.Version); got != want {
		return "", fmt.Errorf("manifest gehört zu %s, nicht zu %s", m.Version, target)
	}
	goos, goarch := u.platform()
	f, ok := m.For(goos, goarch)
	if !ok {
		return "", fmt.Errorf("release %s enthält kein binary für %s/%s", target, goos, goarch)
	}

	newPath := sibling(u.Exe, "new")
	if err := u.download(ctx, target, f, newPath); err != nil {
		os.Remove(newPath)
		return "", err
	}
	if err := u.checkProbe(ctx, newPath, want); err != nil {
		os.Remove(newPath)
		return "", err
	}
	if err := u.swap(newPath, target); err != nil {
		os.Remove(newPath)
		return "", err
	}
	u.Log.Info("update installiert", "from", u.Version, "to", target)
	return fmt.Sprintf("%s installiert, agent startet neu", target), nil
}

func (u *Updater) platform() (string, string) {
	if u.goos != "" {
		return u.goos, u.goarch
	}
	return runtime.GOOS, runtime.GOARCH
}

func (u *Updater) client() *http.Client {
	if u.HTTPClient != nil {
		return u.HTTPClient
	}
	return http.DefaultClient
}

// open fragt eine Release-Datei beim Server an.
func (u *Updater) open(ctx context.Context, version, name string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		u.BaseURL+downloadPath+url.PathEscape(version)+"/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, fmt.Errorf("anfrage %s: %w", name, err)
	}
	req.Header.Set("Authorization", "Bearer "+u.Token)
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s laden: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s laden: http %d", name, resp.StatusCode)
	}
	return resp, nil
}

// get lädt eine kleine Datei vollständig (höchstens limit Bytes).
func (u *Updater) get(ctx context.Context, version, name string, limit int64) ([]byte, error) {
	resp, err := u.open(ctx, version, name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s lesen: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s zu groß", name)
	}
	return data, nil
}

// download speichert das Binary unter path und prüft Größe und Prüfsumme.
func (u *Updater) download(ctx context.Context, version string, f release.File, path string) error {
	resp, err := u.open(ctx, version, f.Name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("binary anlegen: %w", err)
	}
	_, err = io.Copy(out, io.LimitReader(resp.Body, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("binary laden: %w", err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("binary ausführbar machen: %w", err)
	}
	return release.CheckFile(path, f)
}

// checkProbe ruft das neue Binary mit "version" auf: so fällt ein Binary, das
// auf diesem System nicht startet, vor dem Austausch auf.
func (u *Updater) checkProbe(ctx context.Context, path string, want release.Version) error {
	probe := u.probe
	if probe == nil {
		probe = runVersion
	}
	out, err := probe(ctx, path)
	if err != nil {
		return fmt.Errorf("neues binary startet nicht: %w", err)
	}
	if got, err := release.ParseVersion(strings.TrimSpace(out)); err != nil || got != want {
		return fmt.Errorf("neues binary meldet version %q", strings.TrimSpace(out))
	}
	return nil
}

// runVersion startet path mit fester Argumentliste (keine Shell).
func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	return string(out), err
}

// state merkt sich ein laufendes Update über den Neustart hinweg.
type state struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Attempts   int    `json:"attempts"`
	RolledBack bool   `json:"rolled_back,omitempty"`
}

// swap ersetzt das laufende Binary. Unter Windows lässt sich ein laufendes
// Programm umbenennen, aber nicht überschreiben; deshalb erst umbenennen.
func (u *Updater) swap(newPath, target string) error {
	statePath := stateFile(u.Exe)
	if err := writeState(statePath, state{From: u.Version, To: target}); err != nil {
		return err
	}
	oldPath := sibling(u.Exe, "old")
	if err := os.Remove(oldPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		os.Remove(statePath)
		return fmt.Errorf("altes backup entfernen: %w", err)
	}
	if err := os.Rename(u.Exe, oldPath); err != nil {
		os.Remove(statePath)
		return fmt.Errorf("binary sichern: %w", err)
	}
	if err := os.Rename(newPath, u.Exe); err != nil {
		if rerr := os.Rename(oldPath, u.Exe); rerr != nil {
			u.Log.Error("altes binary zurücklegen fehlgeschlagen", "err", rerr)
		}
		os.Remove(statePath)
		return fmt.Errorf("binary ersetzen: %w", err)
	}
	return nil
}

// Startup wird beim Start des Agenten aufgerufen. note beschreibt den Ausgang
// eines vorherigen Updates (für hello.update_note); restart bedeutet, dass das
// alte Binary zurückgelegt wurde und der Agent sich sofort neu starten muss.
func Startup(exe, version string, log *slog.Logger) (note string, restart bool) {
	path := stateFile(exe)
	st, err := readState(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("update-status unlesbar, wird verworfen", "err", err)
			os.Remove(path)
		}
		return "", false
	}
	switch version {
	case st.To:
		st.Attempts++
		if st.Attempts <= maxAttempts {
			if err := writeState(path, st); err != nil {
				log.Warn("update-status speichern", "err", err)
			}
			log.Info("neue version gestartet, warte auf stabile verbindung", "from", st.From, "attempt", st.Attempts)
			return "", false
		}
		log.Error("neue version läuft nicht stabil – altes binary wird zurückgelegt", "from", st.From, "to", st.To)
		if err := rollback(exe); err != nil {
			log.Error("zurücklegen fehlgeschlagen", "err", err)
			os.Remove(path)
			return fmt.Sprintf("update auf %s läuft nicht stabil, zurücklegen fehlgeschlagen: %v", st.To, err), false
		}
		st.RolledBack = true
		if err := writeState(path, st); err != nil {
			log.Warn("update-status speichern", "err", err)
		}
		return "", true
	case st.From:
		os.Remove(path)
		if st.RolledBack {
			return fmt.Sprintf("update auf %s zurückgenommen (startete %d-mal ohne stabile verbindung)", st.To, maxAttempts), false
		}
		return fmt.Sprintf("update auf %s nicht übernommen", st.To), false
	}
	os.Remove(path)
	return "", false
}

// Confirm meldet, dass die laufende Version stabil mit dem Server verbunden
// ist. Ein laufendes Update gilt damit als gelungen.
func Confirm(exe, version string, log *slog.Logger) {
	path := stateFile(exe)
	st, err := readState(path)
	if err != nil || st.To != version {
		return
	}
	if err := os.Remove(path); err != nil {
		log.Warn("update-status entfernen", "err", err)
		return
	}
	log.Info("update abgeschlossen", "from", st.From, "to", st.To)
}

// rollback legt das alte Binary zurück; das fehlerhafte bleibt als *.failed liegen.
func rollback(exe string) error {
	oldPath := sibling(exe, "old")
	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("kein altes binary: %w", err)
	}
	failed := sibling(exe, "failed")
	os.Remove(failed)
	if err := os.Rename(exe, failed); err != nil {
		return fmt.Errorf("fehlerhaftes binary beiseitelegen: %w", err)
	}
	if err := os.Rename(oldPath, exe); err != nil {
		os.Rename(failed, exe)
		return fmt.Errorf("altes binary zurücklegen: %w", err)
	}
	return nil
}

// sibling liefert den Pfad neben exe mit Zusatz, unter Windows vor der Endung:
// mat-agent.exe → mat-agent.old.exe, mat-agent → mat-agent.old.
func sibling(exe, tag string) string {
	if ext := filepath.Ext(exe); strings.EqualFold(ext, ".exe") {
		return strings.TrimSuffix(exe, ext) + "." + tag + ext
	}
	return exe + "." + tag
}

func stateFile(exe string) string {
	if ext := filepath.Ext(exe); strings.EqualFold(ext, ".exe") {
		exe = strings.TrimSuffix(exe, ext)
	}
	return exe + ".update.json"
}

func readState(path string) (state, error) {
	var st state
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("update-status lesen: %w", err)
	}
	return st, nil
}

func writeState(path string, st state) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("update-status serialisieren: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("update-status schreiben: %w", err)
	}
	return nil
}
