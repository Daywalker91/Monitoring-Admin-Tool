package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const usersSchema = `
CREATE TABLE IF NOT EXISTS users (
	username      TEXT PRIMARY KEY,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);

-- Es wird nur der SHA-256-Hash des Session-Tokens gespeichert, nie das Token selbst.
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	username   TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
`

// ErrNotFound meldet, dass ein Eintrag nicht existiert.
var ErrNotFound = errors.New("nicht gefunden")

// SetUser legt einen Benutzer an oder ändert sein Passwort. Bei einer Änderung
// werden alle bestehenden Sessions des Benutzers beendet.
func (s *Store) SetUser(ctx context.Context, username, passwordHash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)
ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash`,
		username, passwordHash, now.Unix()); err != nil {
		return fmt.Errorf("benutzer speichern: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE username = ?`, username); err != nil {
		return fmt.Errorf("sessions beenden: %w", err)
	}
	return tx.Commit()
}

// PasswordHash liefert den Passwort-Hash eines Benutzers oder ErrNotFound.
func (s *Store) PasswordHash(ctx context.Context, username string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE username = ?`, username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return hash, err
}

// Users liefert alle Benutzernamen.
func (s *Store) Users(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT username FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// DeleteUser entfernt einen Benutzer samt Sessions.
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return fmt.Errorf("benutzer löschen: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateSession speichert eine neue Session.
func (s *Store) CreateSession(ctx context.Context, tokenHash, username string, now, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, username, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, username, now.Unix(), expires.Unix())
	if err != nil {
		return fmt.Errorf("session speichern: %w", err)
	}
	return nil
}

// SessionUser liefert den Benutzer einer gültigen Session oder ErrNotFound.
func (s *Store) SessionUser(ctx context.Context, tokenHash string, now time.Time) (string, error) {
	var user string
	err := s.db.QueryRowContext(ctx, `SELECT username FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, now.Unix()).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return user, err
}

// DeleteSession beendet eine Session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PruneSessions löscht abgelaufene Sessions.
func (s *Store) PruneSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("sessions bereinigen: %w", err)
	}
	return res.RowsAffected()
}
