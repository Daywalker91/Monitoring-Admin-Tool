// Package auth kümmert sich um die Anmeldung an der Web-GUI: Passwort-Hashes
// (bcrypt), Sessions (zufälliges Token im Cookie, Hash in der Datenbank) und
// eine Sperre nach wiederholten Fehlversuchen.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const (
	bcryptCost     = 12
	MinPasswordLen = 10
	SessionTTL     = 7 * 24 * time.Hour
)

// Fehler für den Aufrufer.
var (
	ErrInvalid = errors.New("benutzername oder passwort falsch")
	ErrLocked  = errors.New("zu viele fehlversuche, bitte später erneut versuchen")
	ErrNoUsers = errors.New("es ist noch kein benutzer angelegt")
)

// dummyHash wird bei unbekannten Benutzern geprüft, damit die Antwortzeit nicht
// verrät, ob ein Benutzername existiert.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-passwort-zum-zeitausgleich"), bcryptCost)

// HashPassword erzeugt einen bcrypt-Hash.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// ValidatePassword prüft die Mindestanforderungen an ein Passwort.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return fmt.Errorf("passwort muss mindestens %d zeichen lang sein", MinPasswordLen)
	}
	if len(password) > 72 {
		return errors.New("passwort darf höchstens 72 bytes lang sein (bcrypt-grenze)")
	}
	return nil
}

// ValidateUsername erlaubt nur unkritische Zeichen.
func ValidateUsername(name string) error {
	if len(name) < 1 || len(name) > 64 {
		return errors.New("benutzername muss 1–64 zeichen lang sein")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return errors.New("benutzername darf nur buchstaben, ziffern, '-', '_' und '.' enthalten")
		}
	}
	return nil
}

// Manager verwaltet Anmeldungen und Sessions.
type Manager struct {
	store   *store.Store
	limiter *limiter
	now     func() time.Time
}

// New erzeugt einen Manager.
func New(st *store.Store) *Manager {
	return &Manager{store: st, limiter: newLimiter(), now: time.Now}
}

// Login prüft die Zugangsdaten und legt bei Erfolg eine Session an. key
// identifiziert den Absender für die Fehlversuchssperre (z. B. die IP).
func (m *Manager) Login(ctx context.Context, key, username, password string) (token string, expires time.Time, err error) {
	now := m.now()
	if m.limiter.locked(key, now) {
		return "", time.Time{}, ErrLocked
	}

	hash, err := m.store.PasswordHash(ctx, username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		users, uerr := m.store.Users(ctx)
		if uerr == nil && len(users) == 0 {
			return "", time.Time{}, ErrNoUsers
		}
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		m.limiter.fail(key, now)
		return "", time.Time{}, ErrInvalid
	case err != nil:
		return "", time.Time{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		m.limiter.fail(key, now)
		return "", time.Time{}, ErrInvalid
	}
	m.limiter.reset(key)

	token, err = newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = now.Add(SessionTTL)
	if err := m.store.CreateSession(ctx, hashToken(token), username, now, expires); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// User liefert den Benutzer zu einem Session-Token oder store.ErrNotFound.
func (m *Manager) User(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", store.ErrNotFound
	}
	return m.store.SessionUser(ctx, hashToken(token), m.now())
}

// Logout beendet eine Session.
func (m *Manager) Logout(ctx context.Context, token string) error {
	return m.store.DeleteSession(ctx, hashToken(token))
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// limiter sperrt einen Absender nach maxFailures Fehlversuchen innerhalb von
// window für lockDuration.
type limiter struct {
	mu      sync.Mutex
	entries map[string]*limitEntry
}

type limitEntry struct {
	failures    int
	first       time.Time
	lockedUntil time.Time
}

const (
	maxFailures  = 5
	failWindow   = 15 * time.Minute
	lockDuration = 5 * time.Minute
)

func newLimiter() *limiter { return &limiter{entries: make(map[string]*limitEntry)} }

func (l *limiter) locked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	return e != nil && now.Before(e.lockedUntil)
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanup(now)
	e := l.entries[key]
	if e == nil || now.Sub(e.first) > failWindow {
		e = &limitEntry{first: now}
		l.entries[key] = e
	}
	e.failures++
	if e.failures >= maxFailures {
		e.lockedUntil = now.Add(lockDuration)
		e.failures = 0
		e.first = now
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// cleanup verhindert, dass die Map bei vielen Absendern unbegrenzt wächst.
func (l *limiter) cleanup(now time.Time) {
	if len(l.entries) < 1000 {
		return
	}
	for k, e := range l.entries {
		if now.Sub(e.first) > failWindow && now.After(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
}
