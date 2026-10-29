// Package packages ermittelt offene Paket-Updates (apt) und ob ein Neustart
// nötig ist, und führt die Jobs packages.refresh und packages.upgrade aus.
//
// Sicherheit: Es gibt keine Parameter; apt-get wird nur mit fester
// Argumentliste und nicht interaktiv aufgerufen. Die Ermittlung nutzt
// "apt-get -s" (Simulation, ändert nichts).
package packages

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// aptTTL: so lange gilt eine Simulation; der Neustart-Hinweis wird bei jeder
// Abfrage frisch gelesen (nur eine Datei bzw. ein Registry-Schlüssel).
const aptTTL = 30 * time.Minute

// Checker liefert den Update-Stand und hält die letzte apt-Simulation vor.
type Checker struct {
	apt            string      // Pfad zu apt-get, leer = keine Paketverwaltung
	rebootRequired func() bool // je Plattform

	mu     sync.Mutex
	cached proto.Updates
	at     time.Time
}

// New erkennt die Paketverwaltung des Hosts. rebootRequired meldet, ob ein
// Neustart nötig ist (siehe Paket power).
func New(rebootRequired func() bool) *Checker {
	c := &Checker{rebootRequired: rebootRequired}
	if p, err := exec.LookPath("apt-get"); err == nil {
		c.apt = p
	}
	return c
}

// Supported meldet, ob Paket-Jobs möglich sind (apt vorhanden).
func (c *Checker) Supported() bool { return c.apt != "" }

// Invalidate verwirft die letzte Simulation (z. B. nach einem Upgrade).
func (c *Checker) Invalidate() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// Status liefert den aktuellen Stand; die apt-Simulation läuft höchstens alle aptTTL.
func (c *Checker) Status(ctx context.Context) proto.Updates {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apt != "" && time.Since(c.at) > aptTTL {
		u := proto.Updates{Manager: "apt"}
		out, err := c.run(ctx, "-s", "-o", "Debug::NoLocking=1", "upgrade")
		if err != nil {
			u.Error = proto.Truncate("apt-get -s upgrade: "+err.Error(), 256)
		} else {
			u.Packages, u.Upgradable, u.Security = parseApt(out)
		}
		c.cached, c.at = u, time.Now()
	}
	u := c.cached
	u.Packages = slices.Clone(u.Packages)
	u.Time = time.Now()
	u.RebootRequired = c.rebootRequired()
	return u
}

// aptEnv sorgt für stabile, nicht interaktive Ausgaben. needrestart soll
// nur auflisten, nicht selbst Dienste neu starten.
var aptEnv = []string{"LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", "APT_LISTCHANGES_FRONTEND=none"}

func (c *Checker) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.apt, args...)
	cmd.Env = append(os.Environ(), aptEnv...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// Refresh ist der Job packages.refresh (apt-get update).
func (c *Checker) Refresh(ctx context.Context, _ map[string]string) (string, bool, error) {
	return c.job(ctx, "update", "-q")
}

// Upgrade ist der Job packages.upgrade. Geänderte Konfigurationsdateien
// bleiben wie sie sind (confold); neue Vorgaben nimmt dpkg nur, wo nichts
// geändert wurde (confdef).
func (c *Checker) Upgrade(ctx context.Context, _ map[string]string) (string, bool, error) {
	return c.job(ctx, "-y", "-q", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "upgrade")
}

func (c *Checker) job(ctx context.Context, args ...string) (string, bool, error) {
	if c.apt == "" {
		return "", false, errors.New("keine unterstützte paketverwaltung (apt)")
	}
	out, err := c.run(ctx, args...)
	c.Invalidate()
	text := tail(string(out), proto.MaxJobOutputLen)
	if err != nil {
		return text, false, fmt.Errorf("apt-get %s: %w", args[len(args)-1], err)
	}
	return text, false, nil
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

// parseApt wertet die "Inst"-Zeilen von "apt-get -s upgrade" aus, z. B.
//
//	Inst libssl3 [3.0.11-1~deb12u1] (3.0.11-1~deb12u2 Debian-Security:12/stable-security [arm64])
//	Inst linux-image-current-rockchip64 (24.8.1 Armbian:24.8.1/bookworm [arm64])
func parseApt(out []byte) (pkgs []proto.Package, total, security int) {
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		line, ok := strings.CutPrefix(sc.Text(), "Inst ")
		if !ok {
			continue
		}
		name, rest, _ := strings.Cut(line, " ")
		p := proto.Package{Name: name}
		if strings.HasPrefix(rest, "[") {
			if end := strings.IndexByte(rest, ']'); end > 0 {
				p.From = rest[1:end]
				rest = strings.TrimSpace(rest[end+1:])
			}
		}
		if strings.HasPrefix(rest, "(") {
			to, origin, _ := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(rest, "("), ")"), " ")
			p.To = to
			p.Security = strings.Contains(strings.ToLower(origin), "-security")
		}
		total++
		if p.Security {
			security++
		}
		if len(pkgs) < proto.MaxPackages {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, total, security
}
