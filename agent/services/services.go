// Package services liest die Dienste des Hosts (systemd bzw. Windows-Dienst-
// Manager) und startet, stoppt oder startet sie neu.
//
// Sicherheit: Dienstnamen kommen vom Server und werden hier noch einmal geprüft
// (proto.ValidServiceName); gesteuert werden nur Dienste, die in der aktuellen
// Liste stehen. Der Agent selbst ist ausgenommen, sonst bricht der Job ab.
// Programme werden nur mit fester Argumentliste aufgerufen.
package services

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// ErrUnsupported meldet eine Plattform ohne Dienstverwaltung.
var ErrUnsupported = errors.New("dienste werden auf dieser plattform nicht unterstützt")

const maxDescription = 200

// Manager liest und steuert Dienste. Self ist der eigene Dienstname.
type Manager struct {
	Self string
	plat platform
}

// New erzeugt einen Manager für die aktuelle Plattform.
func New(self string) *Manager { return &Manager{Self: self, plat: newPlatform()} }

// Supported meldet, ob es auf dieser Plattform Dienste gibt.
func (m *Manager) Supported() bool { return m.plat != nil }

// List liest alle Dienste. Dienste mit Namen, die die Prüfung nicht bestehen
// (z. B. systemd-Escapes wie "\x2d"), fehlen in der Liste.
func (m *Manager) List(ctx context.Context) ([]proto.Service, error) {
	if m.plat == nil {
		return nil, ErrUnsupported
	}
	list, err := m.plat.list(ctx)
	if err != nil {
		return nil, err
	}
	out := list[:0]
	for _, s := range list {
		if !proto.ValidServiceName(s.Name) {
			continue
		}
		s.Description = proto.Truncate(s.Description, maxDescription)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b proto.Service) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Job ist die Aktion für service.start/stop/restart.
func (m *Manager) Job(action string) func(ctx context.Context, p map[string]string) (string, bool, error) {
	return func(ctx context.Context, p map[string]string) (string, bool, error) {
		name := p["name"]
		if err := m.allowed(ctx, name); err != nil {
			return "", false, err
		}
		var verb string
		switch action {
		case proto.ActionServiceStart:
			verb = "start"
		case proto.ActionServiceStop:
			verb = "stop"
		case proto.ActionServiceRestart:
			verb = "restart"
		default:
			return "", false, fmt.Errorf("unbekannte aktion %q", action)
		}
		out, err := m.plat.control(ctx, verb, name)
		if err != nil {
			return out, false, err
		}
		state := m.state(ctx, name)
		return proto.Truncate(out+fmt.Sprintf("%s: %s → Zustand %s\n", name, verb, state), proto.MaxJobOutputLen), false, nil
	}
}

// allowed prüft den Namen und dass der Dienst existiert (frische Liste).
func (m *Manager) allowed(ctx context.Context, name string) error {
	if m.plat == nil {
		return ErrUnsupported
	}
	if !proto.ValidServiceName(name) {
		return errors.New("ungültiger dienstname")
	}
	if name == m.Self {
		return errors.New("der agent kann sich nicht selbst steuern")
	}
	list, err := m.List(ctx)
	if err != nil {
		return fmt.Errorf("dienste lesen: %w", err)
	}
	if !slices.ContainsFunc(list, func(s proto.Service) bool { return s.Name == name }) {
		return fmt.Errorf("dienst %q nicht gefunden", name)
	}
	return nil
}

func (m *Manager) state(ctx context.Context, name string) string {
	list, err := m.List(ctx)
	if err != nil {
		return "unbekannt"
	}
	if i := slices.IndexFunc(list, func(s proto.Service) bool { return s.Name == name }); i >= 0 {
		return list[i].Detail
	}
	return "unbekannt"
}

// platform ist die Umsetzung je Betriebssystem.
type platform interface {
	list(ctx context.Context) ([]proto.Service, error)
	control(ctx context.Context, verb, name string) (string, error)
}
