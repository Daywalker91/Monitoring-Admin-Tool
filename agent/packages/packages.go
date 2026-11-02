// Package packages ermittelt offene Paket-Updates und ob ein Neustart nötig ist
// und führt die Jobs packages.refresh, packages.upgrade und package.install aus.
// Unterstützt werden apt, dnf, zypper und apk (manager.go); unter Windows nur die
// Installation per winget (winget.go).
//
// Sicherheit: Programme liegen an festen Pfaden und laufen mit fester
// Argumentliste, nicht interaktiv. Den einzigen Parameter (Paketname) prüfen
// proto.ValidPackageName und die jeweilige Paketverwaltung. Die Ermittlung
// offener Updates ändert nichts.
package packages

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/progress"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// statusTTL: so lange gilt die letzte Ermittlung; der Neustart-Hinweis aus Datei
// bzw. Registry wird bei jeder Abfrage frisch gelesen.
const statusTTL = 30 * time.Minute

// runFunc führt ein Programm aus und schreibt seine Ausgabe nach w.
type runFunc func(ctx context.Context, env []string, w io.Writer, path string, args ...string) error

func execRun(ctx context.Context, env []string, w io.Writer, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// Checker liefert den Update-Stand und hält die letzte Ermittlung vor.
type Checker struct {
	mgr            *manager    // nil = keine Paketverwaltung
	rebootRequired func() bool // je Plattform
	run            runFunc

	mu     sync.Mutex
	cached proto.Updates
	at     time.Time
}

// New erkennt die Paketverwaltung des Hosts. rebootRequired meldet, ob ein
// Neustart nötig ist (siehe Paket power).
func New(rebootRequired func() bool) *Checker {
	return &Checker{mgr: detect(fileExists, execRun), rebootRequired: rebootRequired, run: execRun}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// Supported meldet, ob Paket-Jobs möglich sind.
func (c *Checker) Supported() bool { return c.mgr != nil }

// Invalidate verwirft die letzte Ermittlung (z. B. nach einem Upgrade).
func (c *Checker) Invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// Status liefert den aktuellen Stand; die Ermittlung läuft höchstens alle statusTTL.
func (c *Checker) Status(ctx context.Context) proto.Updates {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.mgr
	if m != nil && m.list != nil && time.Since(c.at) > statusTTL {
		u := proto.Updates{Manager: m.name}
		var err error
		if u.Packages, u.Upgradable, u.Security, err = m.list(ctx, c, m); err != nil {
			u.Error = proto.Truncate(m.label+": "+err.Error(), 256)
		}
		if m.reboot != nil {
			required, known := m.reboot(ctx, c, m)
			u.RebootRequired, u.RebootUnknown = required, !known
		}
		c.cached, c.at = u, time.Now()
	}
	u := c.cached
	u.Packages = slices.Clone(u.Packages)
	u.Time = time.Now()
	if m == nil || m.reboot == nil {
		u.RebootRequired = c.rebootRequired()
	}
	return u
}

// capture führt ein Programm der Paketverwaltung aus und liefert die Ausgabe.
func (c *Checker) capture(ctx context.Context, m *manager, path string, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	err := c.run(ctx, m.env, &buf, path, args...)
	return buf.Bytes(), err
}

// Refresh ist der Job packages.refresh.
func (c *Checker) Refresh(ctx context.Context, _ map[string]string) (string, bool, error) {
	if c.mgr == nil || c.mgr.refresh == nil {
		return "", false, errors.New("paketlisten holen ist auf diesem host nicht möglich")
	}
	return c.job(ctx, c.mgr.refresh)
}

// Upgrade ist der Job packages.upgrade. Geänderte Konfigurationsdateien bleiben
// wie sie sind (bei apt per confold, dnf und zypper tun das von sich aus).
func (c *Checker) Upgrade(ctx context.Context, _ map[string]string) (string, bool, error) {
	if c.mgr == nil || c.mgr.upgrade == nil {
		return "", false, errors.New("updates installieren ist auf diesem host nicht möglich")
	}
	return c.job(ctx, c.mgr.upgrade)
}

// Install ist der Job package.install: genau ein Paket, nichts wird entfernt.
// Der Agent prüft den Namen selbst, statt dem Server zu vertrauen.
func (c *Checker) Install(ctx context.Context, p map[string]string) (string, bool, error) {
	m := c.mgr
	if m == nil || m.install == nil {
		return "", false, errors.New("pakete installieren ist auf diesem host nicht möglich")
	}
	name := p["name"]
	if !proto.ValidPackageName(name) {
		return "", false, errors.New("ungültiger paketname")
	}
	if err := m.checkName(name); err != nil {
		return "", false, err
	}
	if m.before != nil {
		if err := m.before(ctx, c, m, name); err != nil {
			return "", false, err
		}
	}
	return c.job(ctx, m.install(name))
}

func (c *Checker) job(ctx context.Context, args []string) (string, bool, error) {
	m := c.mgr
	note := ""
	if m.name == "apt" && !canSetUID() {
		// Ohne CAP_SETUID scheitert apt beim Wechsel zum Benutzer _apt
		// ("seteuid 105 failed"); dann ohne diese Sandbox laden. Die
		// Signaturprüfung der Paketquellen bleibt davon unberührt.
		args = append([]string{"-o", "APT::Sandbox::User=root"}, args...)
		note = "Hinweis: Dem Agenten fehlt CAP_SETUID, apt lädt ohne _apt-Sandbox als root.\n"
		io.WriteString(progress.Writer(ctx), note)
	}
	var buf bytes.Buffer
	// Bei Jobs geht die Ausgabe zusätzlich laufend als Zwischenstand raus.
	err := c.run(ctx, m.env, io.MultiWriter(&buf, progress.Writer(ctx)), m.path, args...)
	c.Invalidate()
	text := tail(note+buf.String(), proto.MaxJobOutputLen)
	if err != nil && !slices.Contains(m.okExit, exitCode(err)) {
		return text, false, fmt.Errorf("%s: %w", m.label, err)
	}
	return text, false, nil
}

// exitCode liefert den Exit-Code eines beendeten Programms, sonst -1.
func exitCode(err error) int {
	var e interface{ ExitCode() int }
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

// capSetUID ist das Bit von CAP_SETUID in den Capability-Masken.
const capSetUID = 7

// canSetUID meldet, ob der Agent die Benutzerkennung wechseln darf. Auf
// manchen Hosts fehlt systemd-Diensten CAP_SETUID; apt-get erbt das.
var canSetUID = func() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return true // kein Linux-procfs: nichts ändern
	}
	return hasCap(data, capSetUID)
}

// hasCap prüft, ob die Zeile CapEff aus /proc/<pid>/status das Bit cap
// enthält. Ohne lesbare Zeile gilt das Recht als vorhanden.
func hasCap(status []byte, cap uint) bool {
	for sc := bufio.NewScanner(bytes.NewReader(status)); sc.Scan(); {
		v, ok := strings.CutPrefix(sc.Text(), "CapEff:")
		if !ok {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			return true
		}
		return mask&(1<<cap) != 0
	}
	return true
}

// tail kürzt s auf die letzten n Bytes (das Ende einer apt-Ausgabe ist das
// Interessante), ohne ein UTF-8-Zeichen zu zerteilen.
func tail(s string, n int) string {
	const mark = "…\n"
	if len(s) <= n {
		return s
	}
	i := len(s) - (n - len(mark))
	for i < len(s) && s[i]&0xC0 == 0x80 {
		i++
	}
	return mark + s[i:]
}
