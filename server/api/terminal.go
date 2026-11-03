package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

// Web-Terminal (M11): Der Server vermittelt nur zwischen Browser und Agent. Er
// erzeugt keine Eingaben, speichert weder Eingabe noch Ausgabe und kann das
// Terminal nicht einschalten – das entscheidet allein agent.json am Host.
//
// Ablauf:
//  1. POST /api/hosts/{id}/terminal mit erneuter Passworteingabe → einmaliges
//     Ticket (terminalTicketTTL), gebunden an Benutzer, Host und GUI-Session.
//  2. Browser öffnet /ws/terminal?ticket=… (Session-Cookie und Origin geprüft).
//  3. Server schickt terminal_open an den Agenten; der verbindet sich mit
//     /ws/agent/terminal/{id} (Agent-Token). Danach werden Nachrichten durchgereicht.

// terminalAgentWait: so lange darf sich der Agent mit dem Verbinden Zeit lassen
// (Variable für Tests).
var terminalAgentWait = 15 * time.Second

const (
	terminalTicketTTL  = 30 * time.Second
	terminalPerHost    = 3 // Obergrenze im Server (der Agent hat max_sessions)
	terminalTotal      = 10
	terminalReadLimit  = proto.TerminalMaxFrame + 1024
	terminalSessionsUI = 20
)

// terminals hält Tickets und laufende Sitzungen (nur im Speicher).
type terminals struct {
	mu       sync.Mutex
	tickets  map[string]termTicket
	sessions map[string]*termSession // auch solche, die noch auf den Agenten warten
}

func newTerminals() *terminals {
	return &terminals{tickets: map[string]termTicket{}, sessions: map[string]*termSession{}}
}

type termTicket struct {
	user, host, sessKey string
	cols, rows          int
	expires             time.Time
}

type termSession struct {
	id, host string
	agent    chan *websocket.Conn // der Agent hat sich verbunden
	done     chan struct{}        // Sitzung beendet (der Agent-Handler wartet darauf)
}

// terminalBusy meldet, warum auf dem Host gerade kein Terminal geöffnet werden darf.
func (s *Server) terminalBusy(ctx context.Context, h store.Host) error {
	switch {
	case !slices.Contains(h.Capabilities, proto.CapTerminal):
		return errors.New("terminal ist auf diesem host nicht freigeschaltet (block \"terminal\" in agent.json)")
	case !s.online(h.ID):
		return errors.New("host ist offline")
	case s.draining(h.ID, "") != "":
		return errors.New("auf dem host läuft ein neustart mit drain")
	case s.playbookOn(h.ID) != "":
		return fmt.Errorf("host gehört zu einem laufenden playbook (%s)", s.playbookOn(h.ID))
	}
	open, err := s.store.OpenJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range open {
		if j.HostID == h.ID && !proto.ReadOnly(j.Action) {
			return fmt.Errorf("auf dem host läuft noch ein job (%s)", j.Action)
		}
	}
	s.term.mu.Lock()
	defer s.term.mu.Unlock()
	n := 0
	for _, t := range s.term.sessions {
		if t.host == h.ID {
			n++
		}
	}
	switch {
	case n >= terminalPerHost:
		return fmt.Errorf("höchstens %d terminals je host", terminalPerHost)
	case len(s.term.sessions) >= terminalTotal:
		return fmt.Errorf("höchstens %d terminals gleichzeitig", terminalTotal)
	}
	return nil
}

// playbookOn liefert den Namen des laufenden Playbooks, zu dem der Host gehört.
func (s *Server) playbookOn(hostID string) string {
	if s.pb == nil {
		return ""
	}
	s.pb.mu.Lock()
	defer s.pb.mu.Unlock()
	for _, r := range s.pb.runs {
		if slices.Contains(r.hosts, hostID) {
			return r.pb.Name
		}
	}
	return ""
}

type terminalRequest struct {
	Password string `json:"password"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
}

// handleTerminalTicket prüft das Passwort erneut und gibt ein einmaliges Ticket aus.
func (s *Server) handleTerminalTicket(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var req terminalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "ungültige anfrage", http.StatusBadRequest)
		return
	}
	if req.Cols < 1 || req.Cols > proto.TerminalMaxCols || req.Rows < 1 || req.Rows > proto.TerminalMaxRows {
		http.Error(w, "ungültige terminalgröße", http.StatusBadRequest)
		return
	}
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	user := userFrom(r.Context())
	switch err := s.auth.Verify(r.Context(), clientIP(r), user, req.Password); {
	case errors.Is(err, auth.ErrInvalid):
		s.log.Warn("terminal: passwort falsch", "user", user, "agent_id", h.ID, "remote", r.RemoteAddr)
		http.Error(w, "passwort falsch", http.StatusUnauthorized)
		return
	case errors.Is(err, auth.ErrLocked):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case err != nil:
		s.log.Error("terminal: passwort prüfen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	if err := s.terminalBusy(r.Context(), h); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		http.Error(w, "nicht angemeldet", http.StatusUnauthorized)
		return
	}
	ticket, err := randomToken(32)
	if err != nil {
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	now := s.nowFor()
	s.term.mu.Lock()
	for k, t := range s.term.tickets { // abgelaufene Tickets wegräumen
		if now.After(t.expires) {
			delete(s.term.tickets, k)
		}
	}
	s.term.tickets[ticket] = termTicket{user: user, host: h.ID, sessKey: auth.SessionKey(c.Value),
		cols: req.Cols, rows: req.Rows, expires: now.Add(terminalTicketTTL)}
	s.term.mu.Unlock()
	writeJSON(w, map[string]string{"ticket": ticket})
}

// takeTicket löst ein Ticket genau einmal ein.
func (s *Server) takeTicket(ticket, user, sessKey string) (termTicket, bool) {
	s.term.mu.Lock()
	defer s.term.mu.Unlock()
	t, ok := s.term.tickets[ticket]
	delete(s.term.tickets, ticket)
	return t, ok && t.user == user && t.sessKey == sessKey && s.nowFor().Before(t.expires)
}

// handleTerminalBrowser nimmt die WebSocket-Verbindung des Browsers an.
func (s *Server) handleTerminalBrowser(w http.ResponseWriter, r *http.Request) {
	// Browser senden beim WebSocket-Upgrade immer einen Origin-Header; ohne ihn
	// (oder mit fremdem) gibt es kein Terminal.
	if r.Header.Get("Origin") == "" || !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		http.Error(w, "nicht angemeldet", http.StatusUnauthorized)
		return
	}
	user := userFrom(r.Context())
	t, ok := s.takeTicket(r.URL.Query().Get("ticket"), user, auth.SessionKey(c.Value))
	if !ok {
		http.Error(w, "ticket ungültig oder abgelaufen", http.StatusForbidden)
		return
	}
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	i := slices.IndexFunc(hosts, func(h store.Host) bool { return h.ID == t.host })
	if i < 0 {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return
	}
	busy := s.terminalBusy(r.Context(), hosts[i])

	browser, err := websocket.Accept(w, r, nil) // prüft Origin zusätzlich selbst
	if err != nil {
		return
	}
	defer browser.CloseNow()
	browser.SetReadLimit(terminalReadLimit)
	if busy != nil {
		sendTerminalExit(browser, busy.Error())
		browser.Close(websocket.StatusNormalClosure, "")
		return
	}

	id, err := randomHex(16)
	if err != nil {
		return
	}
	sess := &termSession{id: proto.TerminalIDPrefix + id, host: t.host,
		agent: make(chan *websocket.Conn, 1), done: make(chan struct{})}
	s.term.mu.Lock()
	s.term.sessions[sess.id] = sess
	s.term.mu.Unlock()
	defer func() {
		s.term.mu.Lock()
		delete(s.term.sessions, sess.id)
		s.term.mu.Unlock()
		close(sess.done)
	}()

	log := s.log.With("session_id", sess.id, "agent_id", t.host, "user", user, "remote", r.RemoteAddr)
	started := s.nowFor()
	rec := store.TerminalSession{ID: sess.id, HostID: t.host, User: user, Remote: r.RemoteAddr, StartedAt: started.Unix()}
	if err := s.store.CreateTerminalSession(r.Context(), rec); err != nil {
		log.Error("terminal-sitzung speichern", "err", err)
		sendTerminalExit(browser, "interner fehler")
		return
	}
	log.Info("terminal angefordert")

	var in, out atomic.Int64
	reason := s.runTerminal(r.Context(), sess, browser, t, user, &in, &out)
	if err := s.store.FinishTerminalSession(context.Background(), sess.id, s.nowFor(), reason, in.Load(), out.Load()); err != nil {
		log.Error("terminal-sitzung beenden", "err", err)
	}
	log.Info("terminal beendet", "reason", reason, "duration", s.nowFor().Sub(started).Round(time.Second),
		"bytes_in", in.Load(), "bytes_out", out.Load())
}

// runTerminal fordert den Agenten an, wartet auf ihn und reicht dann Nachrichten
// durch. Liefert den Grund des Endes.
func (s *Server) runTerminal(ctx context.Context, sess *termSession, browser *websocket.Conn, t termTicket, user string,
	in, out *atomic.Int64) string {
	open := proto.TerminalOpen{SessionID: sess.id, Cols: t.cols, Rows: t.rows, User: user}
	if err := s.sendToAgent(ctx, t.host, proto.TypeTerminalOpen, open); err != nil {
		reason := "agent nicht erreichbar: " + err.Error()
		sendTerminalExit(browser, reason)
		browser.Close(websocket.StatusNormalClosure, "")
		return reason
	}
	var agent *websocket.Conn
	select {
	case agent = <-sess.agent:
	case <-time.After(terminalAgentWait):
		const reason = "agent hat nicht geantwortet (version ab v1.7.0 nötig)"
		sendTerminalExit(browser, reason)
		browser.Close(websocket.StatusNormalClosure, "")
		return reason
	case <-ctx.Done():
		return "browser getrennt"
	}
	return s.relayTerminal(ctx, browser, agent, in, out)
}

// relayTerminal reicht Nachrichten in beide Richtungen durch, bis eine Seite
// endet. Steuernachrichten werden geprüft: vom Browser nur resize, vom Agenten
// nur exit. Lesen läuft unter ctx; endet eine Seite, werden beide geschlossen.
func (s *Server) relayTerminal(ctx context.Context, browser, agent *websocket.Conn, in, out *atomic.Int64) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	var reason string
	end := func(r string) {
		once.Do(func() { reason = r })
		cancel()
	}
	// Pings erkennen tote Verbindungen und halten Proxys im Leerlauf offen.
	go s.pingLoop(ctx, browser, func() { end("browser antwortet nicht") })
	go s.pingLoop(ctx, agent, func() { end("agent antwortet nicht") })
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // Agent → Browser
		defer wg.Done()
		for {
			typ, data, err := agent.Read(ctx)
			if err != nil {
				end("agent getrennt")
				return
			}
			if typ == websocket.MessageText {
				var ctl proto.TerminalControl
				if json.Unmarshal(data, &ctl) != nil || ctl.Validate(true) != nil {
					continue
				}
				if err := browser.Write(ctx, typ, data); err != nil {
					end("browser getrennt")
					return
				}
				end(ctl.Reason) // exit: der Agent schließt gleich selbst
				continue
			}
			out.Add(int64(len(data)))
			if err := browser.Write(ctx, typ, data); err != nil {
				end("browser getrennt")
				return
			}
		}
	}()
	go func() { // Browser → Agent
		defer wg.Done()
		for {
			typ, data, err := browser.Read(ctx)
			if err != nil {
				end("browser getrennt")
				return
			}
			if typ == websocket.MessageText {
				var ctl proto.TerminalControl
				if json.Unmarshal(data, &ctl) != nil || ctl.Validate(false) != nil {
					continue
				}
			} else {
				in.Add(int64(len(data)))
			}
			if err := agent.Write(ctx, typ, data); err != nil {
				end("agent getrennt")
				return
			}
		}
	}()
	<-ctx.Done()
	end("server beendet")
	browser.Close(websocket.StatusNormalClosure, "")
	agent.Close(websocket.StatusNormalClosure, "")
	wg.Wait()
	return reason
}

// handleTerminalAgent nimmt die Terminal-Verbindung eines Agenten an.
func (s *Server) handleTerminalAgent(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	s.term.mu.Lock()
	sess, ok := s.term.sessions[id]
	s.term.mu.Unlock()
	if !proto.ValidTerminalID(id) || !ok {
		http.Error(w, "unbekannte sitzung", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(terminalReadLimit)
	select {
	case sess.agent <- conn: // nur die erste Verbindung je Sitzung zählt
	default:
		conn.Close(websocket.StatusPolicyViolation, "sitzung bereits verbunden")
		return
	}
	<-sess.done // die Verbindung gehört jetzt der Sitzung
}

// handleTerminalSessions liefert die letzten Terminal-Sitzungen eines Hosts.
func (s *Server) handleTerminalSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.TerminalSessions(r.Context(), r.PathValue("id"), terminalSessionsUI)
	if err != nil {
		s.log.Error("terminal-sitzungen lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

// sendToAgent schickt eine Nachricht über die Hauptverbindung eines Agenten.
func (s *Server) sendToAgent(ctx context.Context, id, typ string, payload any) error {
	s.mu.Lock()
	ac, ok := s.conns[id]
	s.mu.Unlock()
	if !ok {
		return errors.New("host ist offline")
	}
	data, err := proto.Encode(typ, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, jobSendTimeout)
	defer cancel()
	return ac.conn.Write(ctx, websocket.MessageText, data)
}

func sendTerminalExit(conn *websocket.Conn, reason string) {
	data, err := json.Marshal(proto.TerminalControl{Type: proto.TerminalExit, Reason: proto.Truncate(reason, 200)})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
