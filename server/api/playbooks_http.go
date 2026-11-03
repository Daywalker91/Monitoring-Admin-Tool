package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
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
	Running  string        `json:"running,omitempty"`  // ID des aktiven Laufs
	Schedule *scheduleView `json:"schedule,omitempty"` // nur bei gültigem Zeitplan
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
	scheds := s.scheduleViews(r.Context())
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
		if f.Playbook != nil {
			if v, ok := scheds[f.Playbook.Name]; ok && f.OK() {
				it.Schedule = &v
			}
		}
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

// previewHost beschreibt einen Zielhost im Probelauf.
type previewHost struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Online bool          `json:"online"`
	Steps  []previewStep `json:"steps"` // je Schritt des Playbooks
}

// Einschätzung eines Schritts im Probelauf.
const (
	previewOK   = "ok"   // würde nach heutigem Stand laufen
	previewInfo = "info" // Hinweis (Wartezeit, ausstehende Updates)
	previewWarn = "warn" // jetzt nicht erfüllt (z. B. Host offline, Gate offen)
	previewFail = "fail" // schlägt sicher fehl (Fähigkeit fehlt)
)

type previewStep struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// handlePlaybookPreview ist der Probelauf: Zielhosts, Einschätzung jedes
// Schritts je Host und was einen Start gerade verhindert. Es wird nichts
// gesendet und nichts gespeichert; Gates zeigen den Stand von jetzt.
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
	open, err := s.store.OpenJobs(r.Context())
	if err != nil {
		s.log.Error("offene jobs lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	out := []previewHost{}
	for _, id := range ids {
		i := slices.IndexFunc(hosts, func(h store.Host) bool { return h.ID == id })
		h := hosts[i]
		ph := previewHost{ID: h.ID, Name: h.Name(), Online: s.online(h.ID)}
		for _, st := range p.Steps {
			ph.Steps = append(ph.Steps, s.previewStep(r.Context(), st, h, ph.Online))
		}
		out = append(out, ph)
	}
	s.pb.mu.Lock()
	blockers := s.playbookBlockers(p.Name, ids, open)
	s.pb.mu.Unlock()
	writeJSON(w, map[string]any{"playbook": p, "hosts": out, "blockers": blockers})
}

// previewStep schätzt einen Schritt auf h nach heutigem Stand ein.
func (s *Server) previewStep(ctx context.Context, st playbook.Step, h store.Host, online bool) previewStep {
	switch {
	case st.IsWait():
		return previewStep{previewInfo, "wartet " + time.Duration(st.Wait).String()}
	case st.IsGate():
		ok, detail, err := s.gateState(ctx, st, h.ID, time.Time{})
		var eg errGate
		switch {
		case errors.As(err, &eg):
			return previewStep{previewFail, eg.msg}
		case err != nil:
			return previewStep{previewWarn, "prüfung fehlgeschlagen"}
		case ok:
			return previewStep{previewOK, "jetzt erfüllt"}
		}
		return previewStep{previewWarn, "jetzt nicht erfüllt: " + detail}
	case st.Action == playbook.ActionDrainReboot:
		return s.previewDrain(ctx, h, online)
	}
	switch {
	case !slices.Contains(h.Capabilities, proto.ActionCapability[st.Action]):
		return previewStep{previewFail, "fähigkeit " + string(proto.ActionCapability[st.Action]) + " fehlt"}
	case st.Action == proto.ActionCommandRun &&
		!slices.ContainsFunc(h.Commands, func(c proto.CommandInfo) bool { return c.Name == st.Params["name"] }):
		return previewStep{previewFail, "befehl " + st.Params["name"] + " nicht freigegeben"}
	case st.Action == proto.ActionLogRead && !slices.Contains(h.LogSources, st.Params["source"]):
		return previewStep{previewFail, "log-quelle " + st.Params["source"] + " nicht freigegeben"}
	case !online:
		return previewStep{previewWarn, "host ist offline"}
	}
	u := h.Updates
	switch {
	case st.Action == proto.ActionPackagesUpgrade && u != nil && u.Error == "":
		if u.Upgradable == 0 {
			return previewStep{previewInfo, "keine updates ausstehend"}
		}
		return previewStep{previewInfo, fmt.Sprintf("%d updates ausstehend (%d sicherheit)", u.Upgradable, u.Security)}
	case st.Action == proto.ActionReboot && u != nil && !u.RebootUnknown:
		if u.RebootRequired {
			return previewStep{previewInfo, "neustart nötig"}
		}
		return previewStep{previewInfo, "neustart nicht nötig"}
	}
	return previewStep{previewOK, "bereit"}
}

// previewDrain prüft wie startDrain, ohne etwas zu reservieren.
func (s *Server) previewDrain(ctx context.Context, h store.Host, online bool) previewStep {
	switch {
	case !s.canDrain():
		return previewStep{previewFail, "drain nicht verfügbar (clusterrole mat-server-drain fehlt)"}
	case !slices.Contains(h.Capabilities, proto.CapReboot):
		return previewStep{previewFail, "fähigkeit reboot fehlt"}
	case !online:
		return previewStep{previewWarn, "host ist offline"}
	}
	n, err := s.nodeOfHost(ctx, h)
	switch {
	case errors.Is(err, k8s.ErrNotFound):
		return previewStep{previewFail, "kein k3s-node zu diesem host"}
	case err != nil:
		return previewStep{previewWarn, "node nicht lesbar"}
	case !n.Ready:
		return previewStep{previewWarn, "node " + n.Name + " ist nicht bereit"}
	case n.Unschedulable:
		return previewStep{previewWarn, "node " + n.Name + " ist abgesperrt"}
	}
	return previewStep{previewOK, "node " + n.Name}
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
