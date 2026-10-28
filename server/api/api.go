// Package api stellt den WebSocket-Endpunkt für Agenten, die JSON-API und die
// eingebettete Web-GUI bereit.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const (
	helloTimeout = 10 * time.Second
	pingInterval = 30 * time.Second
	pingTimeout  = 10 * time.Second
)

// Server bündelt die HTTP-Handler.
type Server struct {
	store *store.Store
	token []byte
	gui   fs.FS
	log   *slog.Logger

	mu     sync.Mutex
	conns  map[string]*websocket.Conn // aktuell verbundene Agenten je ID
	nowFor func() time.Time
}

// New erzeugt einen Server. token ist das gemeinsame Agent-Token, gui die
// Frontend-Dateien.
func New(st *store.Store, token string, gui fs.FS, log *slog.Logger) *Server {
	return &Server{store: st, token: []byte(token), gui: gui, log: log,
		conns: make(map[string]*websocket.Conn), nowFor: time.Now}
}

// Handler liefert den HTTP-Router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/agent", s.handleAgent)
	mux.HandleFunc("GET /api/hosts", s.handleHosts)
	mux.HandleFunc("GET /api/hosts/{id}/history", s.handleHistory)
	mux.HandleFunc("DELETE /api/hosts/{id}", s.handleDeleteHost)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /", http.FileServerFS(s.gui))
	return securityHeaders(mux)
}

// securityHeaders setzt restriktive Header für GUI und API.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// authorized prüft das Bearer-Token in konstanter Zeit.
func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), s.token) == 1
}

// handleAgent nimmt die WebSocket-Verbindung eines Agenten an.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.log.Warn("agent mit ungültigem token abgewiesen", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Standard-Origin-Prüfung bleibt aktiv: Browser anderer Seiten können sich nicht verbinden.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(proto.MaxMessageBytes)

	ctx := r.Context()
	hello, err := s.readHello(ctx, conn)
	if err != nil {
		s.log.Warn("agent-anmeldung fehlgeschlagen", "remote", r.RemoteAddr, "err", err)
		conn.Close(websocket.StatusPolicyViolation, "ungültige anmeldung")
		return
	}
	if err := s.store.UpsertHost(ctx, hello, r.RemoteAddr, s.nowFor()); err != nil {
		s.log.Error("host speichern", "err", err)
		conn.Close(websocket.StatusInternalError, "interner fehler")
		return
	}

	s.register(hello.AgentID, conn)
	defer s.unregister(hello.AgentID, conn)
	log := s.log.With("agent_id", hello.AgentID, "remote", r.RemoteAddr)
	log.Info("agent verbunden", "hostname", hello.Hostname, "version", hello.AgentVersion,
		"os", hello.OS, "arch", hello.Arch)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.pingLoop(ctx, conn, cancel)

	err = s.readLoop(ctx, conn, hello.AgentID, log)
	if status := websocket.CloseStatus(err); status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
		log.Info("agent getrennt")
	} else {
		log.Warn("agent-verbindung beendet", "err", err)
	}
}

func (s *Server) readHello(ctx context.Context, conn *websocket.Conn) (proto.Hello, error) {
	ctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	var hello proto.Hello
	env, err := readEnvelope(ctx, conn)
	if err != nil {
		return hello, err
	}
	if env.Type != proto.TypeHello {
		return hello, errors.New("erste nachricht ist kein hello")
	}
	if err := json.Unmarshal(env.Data, &hello); err != nil {
		return hello, err
	}
	return hello, hello.Validate()
}

func (s *Server) readLoop(ctx context.Context, conn *websocket.Conn, id string, log *slog.Logger) error {
	for {
		env, err := readEnvelope(ctx, conn)
		if err != nil {
			return err
		}
		now := s.nowFor()
		switch env.Type {
		case proto.TypeMetrics:
			var m proto.Metrics
			if err := json.Unmarshal(env.Data, &m); err != nil {
				log.Warn("ungültige metrics-nachricht", "err", err)
				continue
			}
			if err := m.Validate(); err != nil {
				log.Warn("metrics abgelehnt", "err", err)
				continue
			}
			if err := s.store.SaveMetrics(ctx, id, m, env.Data, now); err != nil {
				log.Error("metrics speichern", "err", err)
			}
		case proto.TypeProcesses:
			var p proto.Processes
			if err := json.Unmarshal(env.Data, &p); err != nil {
				log.Warn("ungültige processes-nachricht", "err", err)
				continue
			}
			if err := p.Validate(); err != nil {
				log.Warn("processes abgelehnt", "err", err)
				continue
			}
			if err := s.store.SaveProcesses(ctx, id, env.Data, now); err != nil {
				log.Error("processes speichern", "err", err)
			}
		default:
			log.Debug("unbekannter nachrichtentyp ignoriert", "type", env.Type)
		}
	}
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (proto.Envelope, error) {
	var env proto.Envelope
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return env, err
	}
	if typ != websocket.MessageText {
		return env, errors.New("nur textnachrichten erlaubt")
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, err
	}
	return env, nil
}

// pingLoop erkennt tote Verbindungen und beendet sie.
func (s *Server) pingLoop(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

// register merkt sich die Verbindung. Meldet sich dieselbe Agent-ID erneut an,
// wird die ältere Verbindung geschlossen.
func (s *Server) register(id string, conn *websocket.Conn) {
	s.mu.Lock()
	old := s.conns[id]
	s.conns[id] = conn
	s.mu.Unlock()
	if old != nil {
		old.Close(websocket.StatusPolicyViolation, "durch neue verbindung ersetzt")
	}
}

func (s *Server) unregister(id string, conn *websocket.Conn) {
	s.mu.Lock()
	if s.conns[id] == conn {
		delete(s.conns, id)
	}
	s.mu.Unlock()
}

func (s *Server) online(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id] != nil
}

// hostView ergänzt den gespeicherten Host um den Live-Status.
type hostView struct {
	store.Host
	Online bool `json:"online"`
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.log.Error("hosts lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	out := make([]hostView, len(hosts))
	for i, h := range hosts {
		out[i] = hostView{Host: h, Online: s.online(h.ID)}
	}
	writeJSON(w, out)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	hours := 1
	if v := r.URL.Query().Get("hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 24*7 {
			http.Error(w, "hours muss zwischen 1 und 168 liegen", http.StatusBadRequest)
			return
		}
		hours = n
	}
	since := s.nowFor().Add(-time.Duration(hours) * time.Hour)
	points, err := s.store.History(r.Context(), r.PathValue("id"), since)
	if err != nil {
		s.log.Error("historie lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, points)
}

// handleDeleteHost entfernt einen Host aus der Liste. Verbundene Hosts können
// nicht gelöscht werden, sie würden sich sofort neu anlegen.
func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if s.online(id) {
		http.Error(w, "host ist online", http.StatusConflict)
		return
	}
	found, err := s.store.DeleteHost(r.Context(), id)
	if err != nil {
		s.log.Error("host löschen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return
	}
	s.log.Info("host gelöscht", "agent_id", id, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

// sameOrigin lehnt ändernde Browser-Anfragen von fremden Seiten ab. Anfragen
// ohne Origin-Header (z. B. curl) sind erlaubt.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// RunRetention löscht stündlich Historienpunkte, die älter als retention sind.
func RunRetention(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	prune := func() {
		n, err := st.Prune(ctx, time.Now().Add(-retention))
		if err != nil {
			log.Error("retention", "err", err)
		} else if n > 0 {
			log.Info("alte historie gelöscht", "rows", n)
		}
	}
	prune()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}
