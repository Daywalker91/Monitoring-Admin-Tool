// Package users legt Benutzer an (Job user.create).
//
// Sicherheit: Nie ein Passwort. Unter Linux bekommt das Konto ein gesperrtes
// Passwort und wird per SSH-Key oder lokal eingerichtet, unter Windows entsteht
// nur ein deaktiviertes Konto. Programme liegen an festen Pfaden, der Agent prüft
// den Namen selbst. Ein vorhandener Benutzer wird nicht verändert.
package users

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/progress"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// system bündelt die Zugriffe aufs Betriebssystem, damit Tests nichts ändern.
type system struct {
	exists    func(path string) bool
	userKnown func(name string) bool
	readFile  func(path string) ([]byte, error)
	run       func(ctx context.Context, w io.Writer, path string, args ...string) error
}

func realSystem() system {
	return system{
		exists: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && !fi.IsDir()
		},
		userKnown: func(name string) bool {
			_, err := user.Lookup(name)
			return err == nil
		},
		readFile: os.ReadFile,
		run: func(ctx context.Context, w io.Writer, path string, args ...string) error {
			cmd := exec.CommandContext(ctx, path, args...)
			cmd.Stdout, cmd.Stderr = w, w
			return cmd.Run()
		},
	}
}

// Manager legt Benutzer auf diesem Host an.
type Manager struct{ sys system }

// New erzeugt den Manager für dieses System.
func New() *Manager { return &Manager{sys: realSystem()} }

// Supported meldet, ob sich auf diesem Host Benutzer anlegen lassen.
func (m *Manager) Supported() bool { return supported(m.sys) }

// Job ist der Job user.create. Der Agent prüft die Parameter selbst, statt dem
// Server zu vertrauen.
func (m *Manager) Job(ctx context.Context, p map[string]string) (string, bool, error) {
	name := p["name"]
	if !proto.ValidUserName(name) {
		return "", false, errors.New("ungültiger benutzername")
	}
	if s := p["sudo"]; s != "" && s != "true" && s != "false" {
		return "", false, errors.New("ungültiger wert für sudo")
	}
	if m.sys.userKnown(name) {
		return "", false, fmt.Errorf("benutzer %q existiert bereits, nichts geändert", name)
	}
	var out strings.Builder
	w := io.MultiWriter(&out, progress.Writer(ctx))
	if err := create(ctx, m.sys, w, name, p["sudo"] == "true"); err != nil {
		return out.String(), false, err
	}
	return out.String(), false, nil
}
