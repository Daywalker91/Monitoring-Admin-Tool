package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
)

const sessionCookie = "mat_session"

type ctxKey struct{}

// userFrom liefert den angemeldeten Benutzer aus dem Request-Kontext.
func userFrom(ctx context.Context) string {
	u, _ := ctx.Value(ctxKey{}).(string)
	return u
}

// requireSession lässt nur Anfragen mit gültiger Session durch. API-Aufrufe
// erhalten 401, Seitenaufrufe werden zur Login-Seite umgeleitet.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		if c, err := r.Cookie(sessionCookie); err == nil {
			token = c.Value
		}
		user, err := s.auth.User(r.Context(), token)
		if err != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "nicht angemeldet", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login.html", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}

	token, expires, err := s.auth.Login(r.Context(), clientIP(r), req.Username, req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalid):
		s.log.Warn("anmeldung fehlgeschlagen", "user", req.Username, "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	case errors.Is(err, auth.ErrLocked):
		s.log.Warn("anmeldung gesperrt", "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case errors.Is(err, auth.ErrNoUsers):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		s.log.Error("anmeldung", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	s.log.Info("angemeldet", "user", req.Username, "remote", r.RemoteAddr)
	writeJSON(w, map[string]string{"username": req.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.auth.Logout(r.Context(), c.Value); err != nil {
			s.log.Error("abmelden", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"username": userFrom(r.Context())})
}

// clientIP liefert die IP des Absenders (ohne Port) für die Fehlversuchssperre.
// Hinter einem Reverse-Proxy wäre das die Proxy-IP; X-Forwarded-For wird bewusst
// nicht ausgewertet, weil es gefälscht werden kann.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
