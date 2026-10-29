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
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/checks"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
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
	auth  *auth.Manager
	token []byte
	gui   fs.FS
	log   *slog.Logger

	metricsToken   []byte         // leer = /metrics deaktiviert
	trustedProxies []netip.Prefix // Proxies, deren X-Forwarded-*-Header gelten

	alerts   *alert.Evaluator   // nil = keine Alarmauswertung
	notifier *notify.Dispatcher // nil = keine externen Kanäle
	upd      *updates           // nil = kein Self-Update
	checks   *checks.Runner     // nil = keine externen Checks
	cluster  *k8s.Watcher       // nil = keine k3s-Ansicht
	version  string             // Server-Version (Anzeige)

	certNotAfter    time.Time // Ablauf des TLS-Zertifikats, null = kein TLS
	certFingerprint string    // SHA-256 des TLS-Zertifikats (hex)

	mu     sync.Mutex
	conns  map[string]agentConn // aktuell verbundene Agenten je ID
	names  map[string]string    // Anzeigenamen verbundener Agenten (für Alarme)
	nowFor func() time.Time
}

// New erzeugt einen Server. token ist das gemeinsame Agent-Token, gui die
// Frontend-Dateien.
func New(st *store.Store, am *auth.Manager, token string, gui fs.FS, log *slog.Logger) *Server {
	return &Server{store: st, auth: am, token: []byte(token), gui: gui, log: log,
		conns: make(map[string]agentConn), names: make(map[string]string), nowFor: time.Now}
}

// WithVersion setzt die angezeigte Server-Version.
func (s *Server) WithVersion(v string) *Server {
	s.version = v
	return s
}

// Dateien, die ohne Anmeldung erreichbar sein müssen (Login-Seite).
var publicFiles = []string{"/login.html", "/login.js", "/style.css"}

// Handler liefert den HTTP-Router. Alles außer Login, Agent-WebSocket und
// Health-Check erfordert eine gültige Session.
func (s *Server) Handler() http.Handler {
	static := http.FileServerFS(s.gui)

	private := http.NewServeMux()
	private.HandleFunc("GET /api/me", s.handleMe)
	private.HandleFunc("GET /api/hosts", s.handleHosts)
	private.HandleFunc("GET /api/hosts/{id}/history", s.handleHistory)
	private.HandleFunc("DELETE /api/hosts/{id}", s.handleDeleteHost)
	private.HandleFunc("POST /api/hosts/{id}/meta", s.handleHostMeta)
	private.HandleFunc("GET /api/alerts", s.handleAlerts)
	private.HandleFunc("POST /api/alerts/test", s.handleTestAlert)
	private.HandleFunc("GET /api/alert-rules", s.handleGetAlertRules)
	private.HandleFunc("POST /api/alert-rules", s.handleSetAlertRules)
	private.HandleFunc("GET /api/hosts/{id}/alert-rules", s.handleGetHostRules)
	private.HandleFunc("POST /api/hosts/{id}/alert-rules", s.handleSetHostRules)
	private.HandleFunc("POST /api/hosts/{id}/mute", s.handleMute)
	private.HandleFunc("DELETE /api/hosts/{id}/mute", s.handleMute)
	private.HandleFunc("GET /api/update", s.handleUpdateStatus)
	private.HandleFunc("POST /api/update/check", s.handleUpdateCheck)
	private.HandleFunc("POST /api/update/all", s.handleUpdateAll)
	private.HandleFunc("POST /api/hosts/{id}/update", s.handleHostUpdate)
	if s.checks != nil {
		private.HandleFunc("GET /api/checks", s.handleChecks)
		private.HandleFunc("POST /api/checks", s.handleCreateCheck)
		private.HandleFunc("PUT /api/checks/{id}", s.handleEditCheck)
		private.HandleFunc("DELETE /api/checks/{id}", s.handleDeleteCheck)
		private.HandleFunc("GET /api/checks/{id}/history", s.handleCheckHistory)
	}
	if s.cluster != nil {
		private.HandleFunc("GET /api/cluster", s.handleCluster)
		private.HandleFunc("GET /api/cluster/logs", s.handleClusterLogs)
		private.HandleFunc("GET /api/cluster/ignore", s.handleGetClusterIgnore)
		private.HandleFunc("POST /api/cluster/ignore", s.handleSetClusterIgnore)
	}
	private.Handle("GET /", static)

	public := http.NewServeMux()
	public.HandleFunc("GET /ws/agent", s.handleAgent)
	public.HandleFunc("GET /agent/update/{version}/{file}", s.handleAgentDownload) // Agent-Token
	public.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	public.HandleFunc("GET /metrics", s.handleMetrics) // eigenes Token statt Session
	public.HandleFunc("POST /api/login", s.handleLogin)
	public.HandleFunc("POST /api/logout", s.handleLogout)
	for _, f := range publicFiles {
		public.Handle("GET "+f, static)
	}
	public.Handle("/", s.requireSession(private))
	return s.realIP(securityHeaders(public))
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

	log := s.log.With("agent_id", hello.AgentID, "remote", r.RemoteAddr)
	if name, err := s.store.HostDisplayName(ctx, hello.AgentID); err != nil {
		log.Error("anzeigename lesen", "err", err)
	} else {
		s.setName(hello.AgentID, name)
	}
	s.register(hello.AgentID, agentConn{conn, r.RemoteAddr}, log)
	defer s.unregister(hello.AgentID, conn)
	log.Info("agent verbunden", "hostname", hello.Hostname, "version", hello.AgentVersion,
		"os", hello.OS, "arch", hello.Arch)
	s.agentHello(hello)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.pingLoop(ctx, conn, cancel)

	err = s.readLoop(ctx, conn, hello, log)
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

func (s *Server) readLoop(ctx context.Context, conn *websocket.Conn, hello proto.Hello, log *slog.Logger) error {
	id := hello.AgentID
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
			if s.alerts != nil {
				s.alerts.Observe(ctx, id, s.hostName(id, hello.Hostname), m, now)
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
		case proto.TypeJobResult:
			var res proto.JobResult
			if err := json.Unmarshal(env.Data, &res); err != nil {
				log.Warn("ungültiges job-ergebnis", "err", err)
				continue
			}
			if err := res.Validate(); err != nil {
				log.Warn("job-ergebnis abgelehnt", "err", err)
				continue
			}
			s.jobResult(id, res)
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

// agentConn ist eine aktive Agent-Verbindung.
type agentConn struct {
	conn   *websocket.Conn
	remote string
}

// register merkt sich die Verbindung. Meldet sich dieselbe Agent-ID erneut an,
// wird die ältere Verbindung mit proto.CloseReplaced geschlossen. Kommt die neue
// Verbindung von einer anderen Adresse, sind vermutlich zwei Agenten mit
// derselben ID konfiguriert.
func (s *Server) register(id string, ac agentConn, log *slog.Logger) {
	s.mu.Lock()
	old, ok := s.conns[id]
	s.conns[id] = ac
	s.mu.Unlock()
	if !ok {
		return
	}
	if hostOf(old.remote) != hostOf(ac.remote) {
		log.Warn("agent-id von zwei adressen verwendet – ältere verbindung getrennt",
			"old_remote", old.remote)
	}
	// Close wartet auf den Handshake der Gegenseite; das soll die neue Anmeldung nicht aufhalten.
	go old.conn.Close(proto.CloseReplaced, "agent-id wird bereits verwendet")
}

// hostOf liefert die IP ohne Port.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func (s *Server) unregister(id string, conn *websocket.Conn) {
	s.mu.Lock()
	if s.conns[id].conn == conn {
		delete(s.conns, id)
	}
	s.mu.Unlock()
}

// setName merkt sich den Anzeigenamen eines Hosts (leer = Hostname).
func (s *Server) setName(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		delete(s.names, id)
	} else {
		s.names[id] = name
	}
}

// hostName liefert den Anzeigenamen, ersatzweise hostname.
func (s *Server) hostName(id, hostname string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name := s.names[id]; name != "" {
		return name
	}
	return hostname
}

func (s *Server) online(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.conns[id]
	return ok
}

// hostView ergänzt den gespeicherten Host um den Live-Status.
type hostView struct {
	store.Host
	Online     bool   `json:"online"`
	MutedUntil *int64 `json:"muted_until"` // nil = nicht stumm
	// Self-Update: verfügbare neuere Version, laufendes/letztes Update, Hinweis des Agenten.
	UpdateAvailable string      `json:"update_available,omitempty"`
	Update          *hostUpdate `json:"update,omitempty"`
	UpdateNote      string      `json:"update_note,omitempty"`
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
		out[i] = hostView{Host: h, Online: s.online(h.ID), UpdateAvailable: s.updateAvailable(h),
			Update: s.hostUpdateState(h.ID), UpdateNote: s.updateNote(h.ID)}
		if s.alerts != nil {
			if until, muted := s.alerts.MutedUntil(h.ID, s.nowFor()); muted {
				ts := until.Unix()
				out[i].MutedUntil = &ts
			}
		}
	}
	writeJSON(w, out)
}

// handleHistory liefert die Historie. ?hours=1..168 wählt den Zeitraum,
// ?points=0..2000 begrenzt die Punktzahl durch Verdichtung (0 = Rohdaten).
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hours, ok := intParam(q.Get("hours"), 1, 1, 24*7)
	if !ok {
		http.Error(w, "hours muss zwischen 1 und 168 liegen", http.StatusBadRequest)
		return
	}
	maxPoints, ok := intParam(q.Get("points"), 0, 0, 2000)
	if !ok {
		http.Error(w, "points muss zwischen 0 und 2000 liegen", http.StatusBadRequest)
		return
	}
	span := time.Duration(hours) * time.Hour
	var bucket time.Duration
	if maxPoints > 0 {
		bucket = (span + time.Duration(maxPoints) - 1) / time.Duration(maxPoints)
	}
	since := s.nowFor().Add(-span)
	points, err := s.store.History(r.Context(), r.PathValue("id"), since, bucket)
	if err != nil {
		s.log.Error("historie lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, points)
}

// intParam liest eine ganze Zahl im Bereich [lo, hi]; leer ergibt def.
func intParam(v string, def, lo, hi int) (int, bool) {
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= lo && n <= hi
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
	if s.alerts != nil {
		s.alerts.Forget(id)
	}
	s.forgetUpdate(id)
	s.setName(id, "")
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

// RunRetention löscht stündlich Historienpunkte, die älter als retention sind,
// sowie abgelaufene Sessions.
func RunRetention(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	prune := func() {
		n, err := st.Prune(ctx, time.Now().Add(-retention))
		if err != nil {
			log.Error("retention", "err", err)
		} else if n > 0 {
			log.Info("alte historie gelöscht", "rows", n)
		}
		if _, err := st.PruneSessions(ctx, time.Now()); err != nil {
			log.Error("sessions bereinigen", "err", err)
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
