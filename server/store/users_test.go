package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUsersAndSessions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()

	if _, err := s.PasswordHash(ctx, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbekannter benutzer: %v", err)
	}
	if err := s.SetUser(ctx, "admin", "hash1", now); err != nil {
		t.Fatal(err)
	}
	if h, err := s.PasswordHash(ctx, "admin"); err != nil || h != "hash1" {
		t.Fatalf("hash = %q, %v", h, err)
	}

	if err := s.CreateSession(ctx, "tok", "admin", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if u, err := s.SessionUser(ctx, "tok", now); err != nil || u != "admin" {
		t.Fatalf("session: %q, %v", u, err)
	}
	if _, err := s.SessionUser(ctx, "tok", now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("abgelaufene session gilt noch")
	}

	// Passwortänderung beendet bestehende Sessions.
	if err := s.SetUser(ctx, "admin", "hash2", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, "tok", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("session überlebt passwortänderung")
	}

	// Löschen des Benutzers entfernt seine Sessions (ON DELETE CASCADE).
	if err := s.CreateSession(ctx, "tok2", "admin", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, "tok2", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("session überlebt das löschen des benutzers")
	}
	if err := s.DeleteUser(ctx, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("doppeltes löschen: %v", err)
	}
}

func TestPruneSessions(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	s.SetUser(ctx, "u", "h", now)
	s.CreateSession(ctx, "alt", "u", now, now.Add(-time.Minute))
	s.CreateSession(ctx, "neu", "u", now, now.Add(time.Hour))
	if n, err := s.PruneSessions(ctx, now); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
}
