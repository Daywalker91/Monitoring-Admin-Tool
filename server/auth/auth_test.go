package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st), st
}

func TestLoginFlow(t *testing.T) {
	ctx := context.Background()
	m, st := newManager(t)

	if _, _, err := m.Login(ctx, "ip", "admin", "irgendwas123"); !errors.Is(err, ErrNoUsers) {
		t.Fatalf("ohne benutzer: %v", err)
	}

	hash, err := HashPassword("geheim-passwort")
	if err != nil {
		t.Fatal(err)
	}
	st.SetUser(ctx, "admin", hash, time.Now())

	if _, _, err := m.Login(ctx, "ip", "admin", "falsch-passwort"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("falsches passwort: %v", err)
	}
	if _, _, err := m.Login(ctx, "ip", "niemand", "geheim-passwort"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbekannter benutzer: %v", err)
	}

	token, _, err := m.Login(ctx, "ip", "admin", "geheim-passwort")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := m.User(ctx, token); err != nil || u != "admin" {
		t.Fatalf("User = %q, %v", u, err)
	}
	if err := m.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := m.User(ctx, token); err == nil {
		t.Fatal("session gilt nach logout noch")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	for i := 0; i < maxFailures-1; i++ {
		l.fail("a", now)
	}
	if l.locked("a", now) {
		t.Fatal("zu früh gesperrt")
	}
	l.fail("a", now)
	if !l.locked("a", now) || l.locked("b", now) {
		t.Fatal("sperre greift nicht oder trifft den falschen absender")
	}
	if l.locked("a", now.Add(lockDuration+time.Second)) {
		t.Fatal("sperre läuft nicht ab")
	}
}

func TestValidation(t *testing.T) {
	if ValidatePassword("kurz") == nil {
		t.Error("kurzes passwort akzeptiert")
	}
	if ValidateUsername("admin; drop") == nil {
		t.Error("ungültiger benutzername akzeptiert")
	}
	if ValidateUsername("day_w.admin") != nil {
		t.Error("gültiger benutzername abgelehnt")
	}
}
