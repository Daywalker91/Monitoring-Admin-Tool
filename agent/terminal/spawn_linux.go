package terminal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Programme mit festem Pfad (nicht über PATH gesucht). Der erste vorhandene gilt.
var (
	systemdRunPaths = []string{"/usr/bin/systemd-run", "/bin/systemd-run"}
	systemctlPaths  = []string{"/usr/bin/systemctl", "/bin/systemctl"}
)

const (
	unitPrefix  = "mat-term-"
	stopTimeout = 15 * time.Second
)

// New prüft die Konfiguration gegen das System und liefert den Manager. nil
// ohne Fehler bedeutet: Terminal nicht konfiguriert.
func New(cfg *config.Terminal, log *slog.Logger) (*Manager, error) {
	if cfg == nil {
		return nil, nil
	}
	sp, err := newSystemdSpawner(cfg, "/etc/shells", "/run/systemd/system")
	if err != nil {
		return nil, err
	}
	return newManager(cfg, sp, log), nil
}

// newSystemdSpawner prüft Benutzer, Shell und systemd.
func newSystemdSpawner(cfg *config.Terminal, shellsFile, systemdDir string) (*systemdSpawner, error) {
	if fi, err := os.Stat(systemdDir); err != nil || !fi.IsDir() {
		return nil, errors.New("systemd läuft nicht (kein " + systemdDir + ")")
	}
	run, ctl := firstExisting(systemdRunPaths), firstExisting(systemctlPaths)
	if run == "" || ctl == "" {
		return nil, errors.New("systemd-run oder systemctl nicht gefunden")
	}
	u, err := user.Lookup(cfg.User)
	if err != nil {
		return nil, fmt.Errorf("benutzer %q: %w", cfg.User, err)
	}
	if u.Uid == "0" {
		return nil, fmt.Errorf("benutzer %q hat uid 0 – das terminal läuft nie als root", cfg.User)
	}
	ok, err := listedShell(shellsFile, cfg.Shell)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("shell %s steht nicht in %s", cfg.Shell, shellsFile)
	}
	if fi, err := os.Stat(cfg.Shell); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("shell %s ist nicht ausführbar", cfg.Shell)
	}
	return &systemdSpawner{run: run, ctl: ctl, user: cfg.User, shell: cfg.Shell, maxSec: cfg.MaxDurationSec}, nil
}

func firstExisting(paths []string) string {
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// listedShell meldet, ob shell als eigene Zeile in /etc/shells steht.
func listedShell(file, shell string) (bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return false, fmt.Errorf("%s lesen: %w", file, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == shell {
			return true, nil
		}
	}
	return false, sc.Err()
}

// systemdSpawner startet die Shell als kurzlebige Unit mat-term-<id>.service.
type systemdSpawner struct {
	run, ctl    string
	user, shell string
	maxSec      int
}

// unitName leitet den Unit-Namen aus der geprüften Sitzungs-ID ab (nur Hex).
func unitName(sessionID string) (string, error) {
	if !proto.ValidTerminalID(sessionID) {
		return "", errors.New("ungültige sitzungs-id")
	}
	return unitPrefix + strings.TrimPrefix(sessionID, proto.TerminalIDPrefix) + ".service", nil
}

// args liefert die feste Argumentliste für systemd-run. --uid lässt systemd den
// Benutzer samt Gruppen setzen; RuntimeMaxSec begrenzt die Dauer zusätzlich zum
// Agenten, TimeoutStopSec hält das Beenden kurz.
func (s *systemdSpawner) args(unit string) []string {
	return []string{
		"--pty", "--quiet", "--collect",
		"--unit=" + unit,
		"--uid=" + s.user,
		"--property=WorkingDirectory=~",
		"--property=RuntimeMaxSec=" + strconv.Itoa(s.maxSec+60),
		"--property=TimeoutStopSec=5",
		"--setenv=TERM=xterm-256color",
		"--", s.shell, "-l",
	}
}

func (s *systemdSpawner) Start(sessionID string, cols, rows int) (Proc, error) {
	unit, err := unitName(sessionID)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(s.run, s.args(unit)...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8"}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("systemd-run starten: %w", err)
	}
	return &systemdProc{f: f, cmd: cmd, ctl: s.ctl, unit: unit}, nil
}

type systemdProc struct {
	f    *os.File
	cmd  *exec.Cmd
	ctl  string
	unit string
	once sync.Once
}

func (p *systemdProc) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *systemdProc) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *systemdProc) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close stoppt die Unit (beendet die Shell samt Kindprozessen), schließt das PTY
// und räumt systemd-run ab.
func (p *systemdProc) Close() error {
	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		// Fehler sind normal, wenn die Unit schon weg ist (Shell beendet, --collect).
		_ = exec.CommandContext(ctx, p.ctl, "--no-ask-password", "stop", "--", p.unit).Run()
		p.f.Close()
		done := make(chan struct{})
		go func() { _ = p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
			<-done
		}
	})
	return nil
}

// Cleanup stoppt übrig gebliebene Terminal-Units, z. B. nach einem Absturz des
// Agenten. systemctl wertet das Muster selbst aus (keine Shell).
func Cleanup(log *slog.Logger) {
	ctl := firstExisting(systemctlPaths)
	if ctl == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, ctl, "--no-ask-password", "stop", "--", unitPrefix+"*.service").CombinedOutput(); err != nil {
		log.Debug("alte terminal-units stoppen", "err", err, "output", strings.TrimSpace(string(out)))
	}
}
