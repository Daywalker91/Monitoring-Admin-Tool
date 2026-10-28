package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const userUsage = `Benutzerverwaltung für die Web-GUI:
  mat-server user add <name>      Benutzer anlegen (Passwort wird abgefragt)
  mat-server user passwd <name>   Passwort ändern (beendet alle Sessions des Benutzers)
  mat-server user list            Benutzer auflisten
  mat-server user del <name>      Benutzer löschen

Ohne Terminal (z. B. Skript) wird das Passwort als eine Zeile von stdin gelesen.
Flags:
  -db <pfad>   SQLite-Datenbank (Standard: $MAT_DB oder data/mat.db)`

func runUser(args []string) error {
	fs := flag.NewFlagSet("user", flag.ContinueOnError)
	dbPath := fs.String("db", envOr("MAT_DB", "data/mat.db"), "Pfad zur SQLite-Datenbank")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, userUsage) }
	if len(args) == 0 {
		fs.Usage()
		return errors.New("unterbefehl fehlt")
	}
	cmd := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch cmd {
	case "list":
		users, err := st.Users(ctx)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Println("(keine benutzer)")
		}
		for _, u := range users {
			fmt.Println(u)
		}
		return nil

	case "add", "passwd":
		name, err := nameArg(fs)
		if err != nil {
			return err
		}
		if err := auth.ValidateUsername(name); err != nil {
			return err
		}
		_, lookupErr := st.PasswordHash(ctx, name)
		exists := lookupErr == nil
		if cmd == "add" && exists {
			return fmt.Errorf("benutzer %q existiert bereits (passwort ändern mit: user passwd %s)", name, name)
		}
		if cmd == "passwd" && !exists {
			return fmt.Errorf("benutzer %q existiert nicht", name)
		}
		password, err := readPassword()
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(password)
		if err != nil {
			return err
		}
		if err := st.SetUser(ctx, name, hash, time.Now()); err != nil {
			return err
		}
		if cmd == "add" {
			fmt.Printf("benutzer %q angelegt\n", name)
		} else {
			fmt.Printf("passwort für %q geändert, bestehende sessions beendet\n", name)
		}
		return nil

	case "del":
		name, err := nameArg(fs)
		if err != nil {
			return err
		}
		if err := st.DeleteUser(ctx, name); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("benutzer %q existiert nicht", name)
		} else if err != nil {
			return err
		}
		fmt.Printf("benutzer %q gelöscht\n", name)
		return nil
	}

	fs.Usage()
	return fmt.Errorf("unbekannter unterbefehl %q", cmd)
}

func nameArg(fs *flag.FlagSet) (string, error) {
	if fs.NArg() != 1 {
		return "", errors.New("genau ein benutzername erwartet")
	}
	return fs.Arg(0), nil
}

// readPassword fragt das Passwort verdeckt und doppelt ab. Ohne Terminal wird
// eine Zeile von stdin gelesen.
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("passwort von stdin lesen: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprintf(os.Stderr, "Passwort (mind. %d Zeichen): ", auth.MinPasswordLen)
	p1, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Passwort wiederholen: ")
	p2, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(p1) != string(p2) {
		return "", errors.New("passwörter stimmen nicht überein")
	}
	return string(p1), nil
}
