//go:build !windows

package users

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	useraddPath = "/usr/sbin/useradd"
	// Unter Alpine ist adduser ein BusyBox-Befehl, useradd fehlt.
	adduserPath = "/usr/sbin/adduser"
	sudoPath    = "/usr/bin/sudo"
	bashPath    = "/bin/bash"
	shPath      = "/bin/sh"
)

func supported(s system) bool {
	return s.exists(useraddPath) || s.exists(adduserPath)
}

func create(ctx context.Context, s system, w io.Writer, name string, sudo bool) error {
	group := ""
	if sudo {
		var err error
		if group, err = sudoGroup(s); err != nil {
			return err // vor jeder Änderung
		}
	}
	shell := shPath
	if s.exists(bashPath) {
		shell = bashPath
	}
	switch {
	case s.exists(useraddPath):
		args := []string{"--create-home", "--shell", shell}
		if group != "" {
			args = append(args, "--groups", group)
		}
		if err := s.run(ctx, w, useraddPath, append(args, "--", name)...); err != nil {
			return fmt.Errorf("useradd: %w", err)
		}
	case s.exists(adduserPath):
		// -D: kein Passwort, das Konto ist gesperrt.
		if err := s.run(ctx, w, adduserPath, "-D", "-s", shell, name); err != nil {
			return fmt.Errorf("adduser: %w", err)
		}
		if group != "" {
			if err := s.run(ctx, w, adduserPath, name, group); err != nil {
				return fmt.Errorf("benutzer %q angelegt, aber nicht in gruppe %s: %w", name, group, err)
			}
		}
	default:
		return errors.New("weder useradd noch adduser gefunden")
	}
	fmt.Fprintf(w, "Benutzer %s angelegt (Passwort gesperrt, Login per SSH-Key oder nach \"passwd %s\" als root).\n", name, name)
	if group != "" {
		fmt.Fprintf(w, "Gruppe %s: sudo fragt nach dem Passwort des Benutzers, geht also erst, wenn eines gesetzt ist.\n", group)
	}
	return nil
}

// sudoGroup erkennt die Gruppe für sudo. Nie in /etc/sudoers schreiben: Ob die
// Gruppe dort wirklich berechtigt ist, entscheidet die Konfiguration des Hosts.
func sudoGroup(s system) (string, error) {
	if !s.exists(sudoPath) {
		return "", errors.New("sudo ist nicht installiert, nichts geändert")
	}
	data, err := s.readFile("/etc/group")
	if err != nil {
		return "", fmt.Errorf("gruppen lesen: %w", err)
	}
	groups := map[string]bool{}
	for sc := bufio.NewScanner(bytes.NewReader(data)); sc.Scan(); {
		name, _, _ := strings.Cut(sc.Text(), ":")
		groups[name] = true
	}
	for _, g := range []string{"sudo", "wheel"} {
		if groups[g] {
			return g, nil
		}
	}
	return "", errors.New("weder gruppe sudo noch wheel vorhanden, nichts geändert")
}
