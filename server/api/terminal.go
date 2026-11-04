package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
//  4. Bricht die Browser-Verbindung ab, bleibt die Sitzung terminalResumeWait
//     bestehen. Der Browser nimmt sie mit /ws/terminal?session=…&resume=<token>
//     wieder auf (Token aus der letzten resume-Nachricht, einmal gültig, an
//     Benutzer und GUI-Session gebunden; kein erneutes Passwort). "close" vom
//     Browser beendet die Sitzung sofort.

// Variablen für Tests: terminalAgentWait – so lange darf sich der Agent mit dem
// Verbinden Zeit lassen; terminalResumeWait – so lange bleibt eine Sitzung nach
// dem Abbruch der Browser-Verbindung bestehen.
var (
	terminalAgentWait  = 15 * time.Second
	terminalResumeWait = 5 * time.Minute
)

const (
	terminalResumeBuf    = 256 << 10 // Ausgabe, die während einer Trennung gepuffert wird
	terminalWriteTimeout = 10 * time.Second
	terminalTicketTTL    = 30 * time.Second
	terminalPerHost      = 3 // Obergrenze im Server (der Agent hat max_sessions)
	terminalTotal        = 10
	terminalReadLimit    = proto.TerminalMaxFrame + 1024
	terminalSessionsUI   = 20
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
	id, host      string
	user, sessKey string               // nur dieser Benutzer in dieser GUI-Session darf wieder aufnehmen
	agent         chan *websocket.Conn // der Agent hat sich verbunden
	attach        chan termAttach      // ein Browser nimmt die Sitzung wieder auf
	done          chan struct{}        // Sitzung beendet (die Handler warten darauf)

	mu     sync.Mutex
	resume string // aktuelles Token zum Wiederaufnehmen, gilt einmal
}

// termAttach ist eine Browser-Verbindung einer Sitzung.
type termAttach struct {
	conn     *websocket.Conn
	remote   string
	released chan struct{} // geschlossen, sobald die Sitzung die Verbindung nicht mehr nutzt
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
	if r.URL.Query().Has("resume") {
		s.resumeTerminal(w, r, user, auth.SessionKey(c.Value))
		return
	}
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
	sess := &termSession{id: proto.TerminalIDPrefix + id, host: t.host, user: user, sessKey: t.sessKey,
		agent: make(chan *websocket.Conn, 1), attach: make(chan termAttach), done: make(chan struct{})}
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
	// Die Sitzung kann den ersten Browser überleben, deshalb nicht am Kontext
	// seiner Anfrage hängen.
	first := termAttach{conn: browser, remote: r.RemoteAddr, released: make(chan struct{})}
	reason := s.runTerminal(context.WithoutCancel(r.Context()), sess, first, t, &in, &out, log)
	if err := s.store.FinishTerminalSession(context.Background(), sess.id, s.nowFor(), reason, in.Load(), out.Load()); err != nil {
		log.Error("terminal-sitzung beenden", "err", err)
	}
	log.Info("terminal beendet", "reason", reason, "duration", s.nowFor().Sub(started).Round(time.Second),
		"bytes_in", in.Load(), "bytes_out", out.Load())
}

// runTerminal fordert den Agenten an, wartet auf ihn und vermittelt dann. Liefert
// den Grund des Endes.
func (s *Server) runTerminal(ctx context.Context, sess *termSession, first termAttach, t termTicket,
	in, out *atomic.Int64, log *slog.Logger) string {
	browser := first.conn
	open := proto.TerminalOpen{SessionID: sess.id, Cols: t.cols, Rows: t.rows, User: sess.user}
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
		return "server beendet"
	}
	r := &termRelay{s: s, sess: sess, agent: agent, in: in, out: out, log: log, detached: make(chan struct{}, 1)}
	return r.run(ctx, first)
}

// termRelay vermittelt zwischen Agent und dem jeweils verbundenen Browser. Bricht
// die Browser-Verbindung ab (Handy: App gewechselt, Funkloch), bleibt die
// Sitzung terminalResumeWait lang bestehen: Die Shell läuft weiter, ihre Ausgabe
// sammelt sich im Speicher (höchstens terminalResumeBuf, nie auf der Platte),
// und der Browser kann sie mit dem Token aus der letzten resume-Nachricht wieder
// aufnehmen. Leerlauf- und Gesamtzeitlimit des Agenten gelten unverändert.
type termRelay struct {
	s       *Server
	sess    *termSession
	agent   *websocket.Conn
	in, out *atomic.Int64
	log     *slog.Logger
	// detached meldet der Hauptschleife einen Abbruch (Frist starten).
	detached chan struct{}

	mu      sync.Mutex
	browser *termAttach        // nil = getrennt
	bcancel context.CancelFunc // beendet Lesen und Pings des Browsers
	buf     []byte             // Ausgabe während der Trennung
	dropped bool               // buf ist übergelaufen
	pending string             // Grund für den Browser beim Ende (exit des Agenten)
}

func (r *termRelay) run(ctx context.Context, first termAttach) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	var reason string
	end := func(why string) {
		once.Do(func() { reason = why })
		cancel()
	}
	// Pings erkennen tote Verbindungen und halten Proxys im Leerlauf offen.
	go r.s.pingLoop(ctx, r.agent, func() { end("agent antwortet nicht") })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.fromAgent(ctx, end)
	}()
	r.attach(ctx, first, end)

	var grace *time.Timer
	var graceC <-chan time.Time
	stopGrace := func() {
		if grace != nil {
			grace.Stop()
			grace, graceC = nil, nil
		}
	}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-r.detached:
			if r.isDetached() && grace == nil {
				r.log.Info("terminal getrennt, warte auf wiederaufnahme", "wait", terminalResumeWait)
				grace = time.NewTimer(terminalResumeWait)
				graceC = grace.C
			}
		case a := <-r.sess.attach:
			stopGrace()
			r.log.Info("terminal wieder aufgenommen", "remote", a.remote)
			r.attach(ctx, a, end)
		case <-graceC:
			end("browser nicht zurückgekehrt")
		}
	}
	stopGrace()
	end("server beendet")
	r.mu.Lock()
	old, bcancel := r.dropLocked()
	r.mu.Unlock()
	if old != nil {
		sendTerminalExit(old.conn, reason)
		releaseBrowser(old, bcancel)
	}
	r.agent.Close(websocket.StatusNormalClosure, "")
	wg.Wait()
	return reason
}

// fromAgent reicht die Ausgabe an den Browser weiter oder puffert sie, solange
// keiner verbunden ist. Vom Agenten zählt als Steuernachricht nur exit.
func (r *termRelay) fromAgent(ctx context.Context, end func(string)) {
	for {
		typ, data, err := r.agent.Read(ctx)
		if err != nil {
			end("agent getrennt")
			return
		}
		if typ == websocket.MessageText {
			var ctl proto.TerminalControl
			if json.Unmarshal(data, &ctl) != nil || ctl.Validate(true) != nil {
				continue
			}
			end(ctl.Reason) // exit: der Agent schließt gleich selbst
			continue
		}
		r.out.Add(int64(len(data)))
		r.mu.Lock()
		if r.browser != nil {
			c := r.browser.conn
			wctx, cancel := context.WithTimeout(context.Background(), terminalWriteTimeout)
			err := c.Write(wctx, websocket.MessageBinary, data)
			cancel()
			if err == nil {
				r.mu.Unlock()
				continue
			}
			r.bufferLocked(data)
			old, bcancel := r.dropLocked()
			r.mu.Unlock()
			releaseBrowser(old, bcancel)
			r.notifyDetached()
			continue
		}
		r.bufferLocked(data)
		r.mu.Unlock()
	}
}

// fromBrowser reicht Eingaben an den Agenten weiter. Vom Browser zählen als
// Steuernachrichten resize (geht weiter) und close (beendet die Sitzung).
func (r *termRelay) fromBrowser(ctx, bctx context.Context, c *websocket.Conn, end func(string)) {
	for {
		typ, data, err := c.Read(bctx)
		if err != nil {
			r.lost(c)
			return
		}
		if typ == websocket.MessageText {
			var ctl proto.TerminalControl
			if json.Unmarshal(data, &ctl) != nil || ctl.Validate(false) != nil {
				continue
			}
			if ctl.Type == proto.TerminalClose {
				end("im browser geschlossen")
				return
			}
		} else {
			r.in.Add(int64(len(data)))
		}
		// Schreiben unter ctx der Sitzung: Endete der Kontext des Browsers
		// mitten im Schreiben, schlösse coder/websocket die Agent-Verbindung.
		if err := r.agent.Write(ctx, typ, data); err != nil {
			end("agent getrennt")
			return
		}
	}
}

// attach macht a zum verbundenen Browser: neues Token, dann die gepufferte
// Ausgabe. Ein noch verbundener Browser wird abgelöst.
func (r *termRelay) attach(ctx context.Context, a termAttach, end func(string)) {
	token, err := r.sess.newResume()
	r.mu.Lock()
	old, oldCancel := r.dropLocked()
	// Eigener Kontext, nicht von ctx abgeleitet: Endet die Sitzung, muss die
	// exit-Nachricht noch hinaus, bevor die Verbindung schließt (coder/websocket
	// schließt sie, sobald der Kontext eines Reads endet).
	bctx, bcancel := context.WithCancel(context.Background())
	r.browser, r.bcancel = &a, bcancel
	ok := err == nil && r.writeLocked(websocket.MessageText, resumeMessage(r.sess.id, token))
	if ok && r.dropped {
		ok = r.writeLocked(websocket.MessageBinary, []byte("\r\n[… ältere Ausgabe verworfen]\r\n"))
	}
	for ok && len(r.buf) > 0 {
		n := min(len(r.buf), proto.TerminalMaxFrame)
		if ok = r.writeLocked(websocket.MessageBinary, r.buf[:n]); ok {
			r.buf = r.buf[n:]
		}
	}
	if ok {
		r.buf, r.dropped = nil, false
	}
	r.mu.Unlock()
	if old != nil {
		sendTerminalExit(old.conn, "in einem anderen fenster wieder aufgenommen")
		releaseBrowser(old, oldCancel)
	}
	go r.fromBrowser(ctx, bctx, a.conn, end)
	go r.s.pingLoop(bctx, a.conn, func() { r.lost(a.conn) })
	if !ok {
		r.lost(a.conn)
	}
}

// lost meldet, dass die Verbindung c abgebrochen ist; nur der aktuelle Browser zählt.
func (r *termRelay) lost(c *websocket.Conn) {
	r.mu.Lock()
	if r.browser == nil || r.browser.conn != c {
		r.mu.Unlock()
		return
	}
	old, bcancel := r.dropLocked()
	r.mu.Unlock()
	releaseBrowser(old, bcancel)
	r.notifyDetached()
}

func (r *termRelay) notifyDetached() {
	select {
	case r.detached <- struct{}{}:
	default:
	}
}

func (r *termRelay) isDetached() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.browser == nil
}

// dropLocked löst den aktuellen Browser; releaseBrowser erledigt den Rest außerhalb der Sperre.
func (r *termRelay) dropLocked() (*termAttach, context.CancelFunc) {
	old, bcancel := r.browser, r.bcancel
	r.browser, r.bcancel = nil, nil
	return old, bcancel
}

// releaseBrowser beendet Lesen und Pings eines abgelösten Browsers und gibt dessen
// Handler frei.
func releaseBrowser(a *termAttach, bcancel context.CancelFunc) {
	if a == nil {
		return
	}
	if bcancel != nil {
		bcancel()
	}
	// CloseNow: Ein ordentliches Close wartete auf die Antwort des Browsers,
	// die ohne laufenden Leser nicht mehr gelesen wird.
	a.conn.CloseNow()
	close(a.released)
}

func (r *termRelay) writeLocked(typ websocket.MessageType, data []byte) bool {
	wctx, cancel := context.WithTimeout(context.Background(), terminalWriteTimeout)
	defer cancel()
	return r.browser.conn.Write(wctx, typ, data) == nil
}

// bufferLocked hängt Ausgabe an den Puffer; zu viel verwirft das Älteste.
func (r *termRelay) bufferLocked(data []byte) {
	r.buf = append(r.buf, data...)
	if over := len(r.buf) - terminalResumeBuf; over > 0 {
		r.buf = append([]byte(nil), r.buf[over:]...)
		r.dropped = true
	}
}

func resumeMessage(id, token string) []byte {
	data, _ := json.Marshal(proto.TerminalControl{Type: proto.TerminalResume, Session: id, Token: token})
	return data
}

// newResume erzeugt ein neues Token; das bisherige gilt danach nicht mehr.
func (t *termSession) newResume() (string, error) {
	token, err := randomToken(32)
	t.mu.Lock()
	t.resume = token // bei einem Fehler leer: Wiederaufnehmen geht dann nicht
	t.mu.Unlock()
	return token, err
}

// takeResume löst ein Token genau einmal ein, nur für denselben Benutzer in
// derselben GUI-Session.
func (t *termSession) takeResume(token, user, sessKey string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ok := t.resume != "" && subtle.ConstantTimeCompare([]byte(token), []byte(t.resume)) == 1 &&
		t.user == user && t.sessKey == sessKey
	if ok {
		t.resume = ""
	}
	return ok
}

// resumeTerminal nimmt eine getrennte (oder noch nicht als getrennt erkannte)
// Sitzung mit dem Token aus der letzten resume-Nachricht wieder auf.
func (s *Server) resumeTerminal(w http.ResponseWriter, r *http.Request, user, sessKey string) {
	q := r.URL.Query()
	s.term.mu.Lock()
	sess := s.term.sessions[q.Get("session")]
	s.term.mu.Unlock()
	if sess == nil || !sess.takeResume(q.Get("resume"), user, sessKey) {
		http.Error(w, "sitzung nicht mehr vorhanden oder token ungültig", http.StatusForbidden)
		return
	}
	browser, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer browser.CloseNow()
	browser.SetReadLimit(terminalReadLimit)
	a := termAttach{conn: browser, remote: r.RemoteAddr, released: make(chan struct{})}
	select {
	case sess.attach <- a:
	case <-sess.done:
		sendTerminalExit(browser, "sitzung beendet")
		return
	}
	select { // die Verbindung gehört jetzt der Sitzung
	case <-a.released:
	case <-sess.done:
	}
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
