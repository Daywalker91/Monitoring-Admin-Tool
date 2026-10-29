package api

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
)

const (
	settingClusterIgnore = "k8s_ignore"
	maxIgnorePatterns    = 50
	maxPatternLen        = 253
)

// defaultClusterIgnore: Ohne eigene Einstellung bleiben die Pods des k3s-ServiceLB
// außen vor; sie hängen auf Pending, sobald MetalLB die LoadBalancer bedient.
var defaultClusterIgnore = []string{"kube-system/svclb-*"}

// clusterIgnore liefert die Muster ("namespace/name", Platzhalter wie bei
// path.Match), deren Ziele keine Cluster-Alarme auslösen.
func (s *Server) clusterIgnore(ctx context.Context) []string {
	v, ok, err := s.store.AlertSetting(ctx, settingClusterIgnore)
	if err != nil {
		s.log.Error("cluster-ausnahmen lesen", "err", err)
		return defaultClusterIgnore
	}
	if !ok {
		return defaultClusterIgnore
	}
	return splitPatterns(v)
}

func splitPatterns(v string) []string {
	out := []string{}
	for _, line := range strings.Split(v, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ignored meldet, ob ns/name auf eines der Muster passt.
func ignored(patterns []string, nsName string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, nsName); ok {
			return true
		}
	}
	return false
}

// filterConds entfernt ausgenommene Ziele. Ziele haben die Form "Kind ns/name"
// (Workloads, Pods) bzw. "name" bei ArgoCD-Apps, die als "argocd/name" geprüft werden.
func filterConds(conds map[string]k8s.Cond, patterns []string, appNS string) {
	for target := range conds {
		nsName := target
		if _, rest, ok := strings.Cut(target, " "); ok {
			nsName = rest
		} else if appNS != "" {
			nsName = appNS + "/" + target
		}
		if ignored(patterns, nsName) {
			delete(conds, target)
		}
	}
}

type ignoreView struct {
	Patterns []string `json:"patterns"`
}

func (s *Server) handleGetClusterIgnore(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, ignoreView{Patterns: s.clusterIgnore(r.Context())})
}

func (s *Server) handleSetClusterIgnore(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "fremde herkunft", http.StatusForbidden)
		return
	}
	var in ignoreView
	if !decodeJSON(w, r, &in) || len(in.Patterns) > maxIgnorePatterns {
		http.Error(w, "ungültige anfrage (höchstens 50 muster)", http.StatusBadRequest)
		return
	}
	clean := []string{}
	for _, p := range in.Patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > maxPatternLen || strings.ContainsAny(p, "\n\r\t ") {
			http.Error(w, "muster zu lang oder mit leerzeichen: "+p, http.StatusBadRequest)
			return
		}
		if _, err := path.Match(p, ""); err != nil {
			http.Error(w, "ungültiges muster: "+p, http.StatusBadRequest)
			return
		}
		clean = append(clean, p)
	}
	if err := s.store.SetAlertSetting(r.Context(), settingClusterIgnore, strings.Join(clean, "\n")); err != nil {
		s.log.Error("cluster-ausnahmen speichern", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}
	s.log.Info("cluster-ausnahmen geändert", "patterns", len(clean), "user", userFrom(r.Context()))
	// Sofort anwenden, damit Alarme ausgenommener Ziele enden.
	if snap, _ := s.cluster.Current(); snap != nil {
		s.ClusterSnapshot(r.Context(), snap)
	}
	writeJSON(w, ignoreView{Patterns: clean})
}
