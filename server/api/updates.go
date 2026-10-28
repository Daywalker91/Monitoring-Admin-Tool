package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/update"
)

const (
	// updateStale: so lange darf ein Update ohne Rückmeldung laufen.
	updateStale    = 10 * time.Minute
	prepareTimeout = 6 * time.Minute
	jobSendTimeout = 10 * time.Second
)

// Zustände eines Host-Updates.
const (
	updPrepare    = "prepare"    // Server lädt das Binary
	updRunning    = "running"    // Agent lädt und prüft
	updRestarting = "restarting" // Agent hat installiert und startet neu
	updDone       = "done"
	updFailed     = "failed"
)

// hostUpdate ist der Stand eines Updates (nur im Speicher).
type hostUpdate struct {
	JobID   string `json:"-"`
	Target  string `json:"target"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Started int64  `json:"started"`
	Changed int64  `json:"changed"`
}

// updates hält Update-Zustände und Hinweise der Agenten.
type updates struct {
	checker *update.Checker
	mu      sync.Mutex
	hosts   map[string]*hostUpdate
	notes   map[string]string // update_note aus dem letzten hello
}

// WithUpdates aktiviert das Self-Update der Agenten.
func (s *Server) WithUpdates(c *update.Checker) *Server {
	s.upd = &updates{checker: c, hosts: map[string]*hostUpdate{}, notes: map[string]string{}}
	return s
}

// handleAgentDownload liefert Manifest, Signatur und Binaries an Agenten
// (Agent-Token). Nur Dateien der neuesten geprüften Version.
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.upd == nil {
		http.NotFound(w, r)
		return
	}
	path, ok := s.upd.checker.Path(r.PathValue("version"), r.PathValue("file"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// updateStatusView ist die Antwort von /api/update.
type updateStatusView struct {
	update.Status
	Server string `json:"server_version"`
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.updateStatus())
}

func (s *Server) updateStatus() updateStatusView {
	v := updateStatusView{Server: s.version}
	if s.upd == nil {
		v.Message = "nicht eingerichtet"
		return v
	}
	v.Status = s.upd.checker.Status()
	return v
}

// handleUpdateCheck prüft sofort auf ein neues Release.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if s.upd == nil || !s.upd.checker.Enabled() {
		http.Error(w, "self-update ist abgeschaltet", http.StatusConflict)
		return
	}
	err := s.upd.checker.CheckNow(r.Context())
	if errors.Is(err, update.ErrTooSoon) {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	// Ein Fehler der Prüfung steht im Status.
	writeJSON(w, s.updateStatus())
}

// handleHostUpdate startet das Update eines Hosts.
func (s *Server) handleHostUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.log.Error("hosts lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	i := slices.IndexFunc(hosts, func(h store.Host) bool { return h.ID == id })
	if i < 0 {
		http.Error(w, "host nicht gefunden", http.StatusNotFound)
		return
	}
	if err := s.startUpdate(hosts[i], r.RemoteAddr); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleUpdateAll startet das Update aller Hosts, für die eines verfügbar ist.
func (s *Server) handleUpdateAll(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.log.Error("hosts lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	started := 0
	for _, h := range hosts {
		if s.updateAvailable(h) != "" && s.startUpdate(h, r.RemoteAddr) == nil {
			started++
		}
	}
	writeJSON(w, map[string]int{"started": started})
}

// updateAvailable liefert die neuere Version, die h installieren kann ("" = keine).
func (s *Server) updateAvailable(h store.Host) string {
	if s.upd == nil || !slices.Contains(h.Capabilities, proto.CapUpdate) || !s.online(h.ID) {
		return ""
	}
	m, ok := s.upd.checker.Latest()
	if !ok || !release.Newer(m.Version, h.AgentVersion) {
		return ""
	}
	if _, ok := m.For(h.OS, h.Arch); !ok {
		return ""
	}
	if u := s.hostUpdateState(h.ID); u != nil && active(u.State) {
		return ""
	}
	return m.Version
}

func active(state string) bool {
	return state == updPrepare || state == updRunning || state == updRestarting
}

// startUpdate prüft die Voraussetzungen und startet das Update im Hintergrund:
// Binary laden, dann den Job an den Agenten schicken.
func (s *Server) startUpdate(h store.Host, remote string) error {
	if s.upd == nil || !s.upd.checker.Enabled() {
		return errors.New("self-update ist abgeschaltet")
	}
	if !s.online(h.ID) {
		return errors.New("host ist offline")
	}
	if !slices.Contains(h.Capabilities, proto.CapUpdate) {
		return errors.New("agent unterstützt kein self-update (läuft er als dienst bzw. unter systemd?)")
	}
	m, ok := s.upd.checker.Latest()
	if !ok {
		return errors.New("kein release bekannt")
	}
	if !release.Newer(m.Version, h.AgentVersion) {
		return errors.New("agent ist bereits aktuell")
	}
	now := s.nowFor().Unix()
	u := &hostUpdate{JobID: newJobID(), Target: m.Version, State: updPrepare, Started: now, Changed: now}
	s.upd.mu.Lock()
	if cur := s.upd.hosts[h.ID]; cur != nil && active(cur.State) && now-cur.Changed < int64(updateStale.Seconds()) {
		s.upd.mu.Unlock()
		return errors.New("update läuft bereits")
	}
	s.upd.hosts[h.ID] = u
	s.upd.mu.Unlock()
	s.log.Info("agent-update gestartet", "agent_id", h.ID, "from", h.AgentVersion, "to", m.Version, "remote", remote)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), prepareTimeout)
		defer cancel()
		version, err := s.upd.checker.Prepare(ctx, h.OS, h.Arch)
		if err == nil && version != m.Version {
			err = errors.New("release hat sich während der vorbereitung geändert")
		}
		if err == nil {
			// Erst den Zustand setzen: die Antwort des Agenten kann sofort kommen.
			s.setUpdate(h.ID, u.JobID, updRunning, "")
			err = s.sendJob(ctx, h.ID, proto.JobRequest{JobID: u.JobID, Action: proto.ActionAgentUpdate,
				Params: map[string]string{"version": m.Version}})
		}
		if err != nil {
			s.log.Warn("agent-update fehlgeschlagen", "agent_id", h.ID, "err", err)
			s.setUpdate(h.ID, u.JobID, updFailed, err.Error())
		}
	}()
	return nil
}

// sendJob schickt einen Job an einen verbundenen Agenten.
func (s *Server) sendJob(ctx context.Context, id string, job proto.JobRequest) error {
	s.mu.Lock()
	ac, ok := s.conns[id]
	s.mu.Unlock()
	if !ok {
		return errors.New("host ist offline")
	}
	data, err := proto.Encode(proto.TypeJobRequest, job)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, jobSendTimeout)
	defer cancel()
	if err := ac.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return errors.New("job senden fehlgeschlagen")
	}
	return nil
}

// setUpdate ändert den Zustand, sofern der Job noch der aktuelle ist.
func (s *Server) setUpdate(id, jobID, state, msg string) {
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	u := s.upd.hosts[id]
	if u == nil || u.JobID != jobID {
		return
	}
	u.State, u.Error, u.Changed = state, msg, s.nowFor().Unix()
}

// jobResult verarbeitet die Rückmeldung eines Agenten.
func (s *Server) jobResult(id string, res proto.JobResult) {
	if s.upd == nil {
		return
	}
	s.upd.mu.Lock()
	u := s.upd.hosts[id]
	s.upd.mu.Unlock()
	if u == nil || u.JobID != res.JobID {
		s.log.Warn("job-ergebnis ohne passenden job", "agent_id", id, "job_id", res.JobID)
		return
	}
	if res.OK {
		s.log.Info("agent-update installiert, agent startet neu", "agent_id", id, "to", u.Target)
		s.setUpdate(id, res.JobID, updRestarting, "")
		return
	}
	s.log.Warn("agent-update vom agenten abgelehnt", "agent_id", id, "err", res.Error)
	s.setUpdate(id, res.JobID, updFailed, res.Error)
}

// agentHello gleicht ein neues hello mit einem laufenden Update ab.
func (s *Server) agentHello(hello proto.Hello) {
	if s.upd == nil {
		return
	}
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	id := hello.AgentID
	if hello.UpdateNote != "" {
		s.upd.notes[id] = hello.UpdateNote
		s.log.Warn("agent meldet problem mit update", "agent_id", id, "note", hello.UpdateNote)
	}
	u := s.upd.hosts[id]
	if u == nil {
		return
	}
	now := s.nowFor().Unix()
	target, _ := release.ParseVersion(u.Target)
	got, err := release.ParseVersion(hello.AgentVersion)
	switch {
	case hello.UpdateNote != "":
		// Rücknahme kann auch nach "done" kommen (neue Version lief nicht stabil).
		u.State, u.Error, u.Changed = updFailed, hello.UpdateNote, now
	case err == nil && got == target:
		if u.State != updDone {
			s.log.Info("agent-update abgeschlossen", "agent_id", id, "version", hello.AgentVersion)
		}
		u.State, u.Error, u.Changed = updDone, "", now
		delete(s.upd.notes, id)
	case active(u.State):
		u.State, u.Changed = updFailed, now
		u.Error = "agent hat sich während des updates neu verbunden, version " + hello.AgentVersion
	}
}

// hostUpdateState liefert eine Kopie des Zustands; veraltete laufende Updates gelten als fehlgeschlagen.
func (s *Server) hostUpdateState(id string) *hostUpdate {
	if s.upd == nil {
		return nil
	}
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	u := s.upd.hosts[id]
	if u == nil {
		return nil
	}
	if active(u.State) && s.nowFor().Unix()-u.Changed > int64(updateStale.Seconds()) {
		u.State, u.Error = updFailed, "keine rückmeldung vom agenten"
	}
	c := *u
	return &c
}

func (s *Server) updateNote(id string) string {
	if s.upd == nil {
		return ""
	}
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	return s.upd.notes[id]
}

func (s *Server) forgetUpdate(id string) {
	if s.upd == nil {
		return
	}
	s.upd.mu.Lock()
	delete(s.upd.hosts, id)
	delete(s.upd.notes, id)
	s.upd.mu.Unlock()
}

func newJobID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "upd-" + hex.EncodeToString(b)
}
