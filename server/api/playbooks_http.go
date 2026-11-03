package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/playbook"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
)

const playbookRunsList = 50

// playbookList ist die Antwort von GET /api/playbooks.
type playbookList struct {
	Enabled  bool           `json:"enabled"`
	LoadedAt int64          `json:"loaded_at,omitempty"`
	Error    string         `json:"error,omitempty"` // Ordner nicht lesbar
	Files    []playbookItem `json:"files"`
}

type playbookItem struct {
	playbook.File
	Running string `json:"running,omitempty"` // ID des aktiven Laufs
}

func (s *Server) handlePlaybooks(w http.ResponseWriter, r *http.Request) {
	if s.pb == nil {
		writeJSON(w, playbookList{Files: []playbookItem{}})
		return
	}
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.log.Error("hosts lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.pb.mu.Lock()
	out := playbookList{Enabled: true, LoadedAt: s.pb.loadedAt.Unix(), Error: s.pb.loadErr, Files: []playbookItem{}}
	for _, f := range s.pb.files {
		// Ziele mit dem aktuellen Host-Stand prüfen: Hosts kommen und gehen.
		if f.OK() {
			if _, err := f.Playbook.Resolve(pbHosts(hosts)); err != nil {
				f.Err = err.Error()
			}
		}
		it := playbookItem{File: f}
		for id, r := range s.pb.runs {
			if f.Playbook != nil && r.pb.Name == f.Playbook.Name {
				it.Running = id
			}
		}
		out.Files = append(out.Files, it)
	}
	s.pb.mu.Unlock()
	writeJSON(w, out)
}

// requirePlaybooks antwortet 404, wenn Playbooks abgeschaltet sind.
func (s *Server) requirePlaybooks(w http.ResponseWriter) bool {
	if s.pb == nil {
		http.Error(w, "playbooks sind abgeschaltet (MAT_PLAYBOOK_DIR)", http.StatusNotFound)
		return false
	}
	return true
}

func (s *Server) handleReloadPlaybooks(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if !s.requirePlaybooks(w) {
		return
	}
	if err := s.reloadPlaybooks(); err != nil {
		s.log.Warn("playbooks laden", "err", err)
	}
	s.log.Info("playbooks neu geladen", "user", userFrom(r.Context()), "remote", r.RemoteAddr)
	s.handlePlaybooks(w, r)
}

// previewHost beschreibt einen Zielhost im Bestätigungsdialog.
type previewHost struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Online   bool     `json:"online"`
	Warnings []string `json:"warnings,omitempty"` // z. B. fehlende Fähigkeit: Schritt wird fehlschlagen
}

// handlePlaybookPreview zeigt, auf welche Hosts ein Start jetzt zielen würde.
func (s *Server) handlePlaybookPreview(w http.ResponseWriter, r *http.Request) {
	if !s.requirePlaybooks(w) {
		return
	}
	p, err := s.playbookFile(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "playbook nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	ids, err := s.resolvePlaybook(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.log.Error("hosts lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	out := []previewHost{}
	for _, id := range ids {
		i := slices.IndexFunc(hosts, func(h store.Host) bool { return h.ID == id })
		h := hosts[i]
		ph := previewHost{ID: h.ID, Name: h.Name(), Online: s.online(h.ID), Warnings: stepWarnings(p, h, s.canDrain())}
		if !ph.Online {
			ph.Warnings = append(ph.Warnings, "host ist offline")
		}
		out = append(out, ph)
	}
	writeJSON(w, map[string]any{"playbook": p, "hosts": out})
}

// stepWarnings nennt Schritte, die auf h sicher fehlschlagen würden.
func stepWarnings(p *playbook.Playbook, h store.Host, canDrain bool) []string {
	var out []string
	for _, st := range p.Steps {
		switch {
		case st.IsWait():
		case st.Action == playbook.ActionDrainReboot:
			if !canDrain || !slices.Contains(h.Capabilities, proto.CapReboot) {
				out = append(out, st.ID+": neustart mit drain nicht möglich")
			}
		case !slices.Contains(h.Capabilities, proto.ActionCapability[st.Action]):
			out = append(out, st.ID+": fähigkeit "+string(proto.ActionCapability[st.Action])+" fehlt")
		case st.Action == proto.ActionCommandRun &&
			!slices.ContainsFunc(h.Commands, func(c proto.CommandInfo) bool { return c.Name == st.Params["name"] }):
			out = append(out, st.ID+": befehl "+st.Params["name"]+" nicht freigegeben")
		case st.Action == proto.ActionLogRead && !slices.Contains(h.LogSources, st.Params["source"]):
			out = append(out, st.ID+": log-quelle "+st.Params["source"]+" nicht freigegeben")
		}
	}
	return out
}

func (s *Server) handleRunPlaybook(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if !s.requirePlaybooks(w) {
		return
	}
	run, err := s.startPlaybook(r.Context(), r.PathValue("name"), userFrom(r.Context()), r.RemoteAddr)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "playbook nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, run)
}

func (s *Server) handlePlaybookRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.PlaybookRuns(r.Context(), playbookRunsList)
	if err != nil {
		s.log.Error("playbook-läufe lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	writeJSON(w, runs)
}

// runView ist ein Lauf samt Kopie des Playbooks (für Schrittnamen und Aktionen).
type runView struct {
	store.PlaybookRun
	Playbook  *playbook.Playbook `json:"playbook,omitempty"`
	HostNames map[string]string  `json:"host_names"`
	Now       int64              `json:"now"`
}

func (s *Server) handlePlaybookRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.PlaybookRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "lauf nicht gefunden", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("playbook-lauf lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	v := runView{PlaybookRun: run, HostNames: map[string]string{}, Now: s.nowFor().Unix()}
	if p, err := playbook.Parse([]byte(run.Source)); err == nil {
		v.Playbook = p
	}
	if hosts, err := s.store.ListHosts(r.Context()); err == nil {
		for _, h := range hosts {
			if slices.Contains(run.Hosts, h.ID) {
				v.HostNames[h.ID] = h.Name()
			}
		}
	}
	writeJSON(w, v)
}

func (s *Server) handleCancelPlaybookRun(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	if !s.requirePlaybooks(w) {
		return
	}
	id := r.PathValue("id")
	found, err := s.cancelPlaybook(r.Context(), id)
	if err != nil {
		s.log.Error("playbook-lauf abbrechen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "kein laufender lauf mit dieser id", http.StatusNotFound)
		return
	}
	s.log.Info("playbook-lauf abgebrochen", "run_id", id, "user", userFrom(r.Context()), "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}
