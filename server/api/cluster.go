package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
)

const (
	defaultLogLines = 500
	maxLogLines     = 5000
)

// WithCluster aktiviert die k3s-Ansicht.
func (s *Server) WithCluster(w *k8s.Watcher) *Server {
	s.cluster = w
	return s
}

// ClusterSnapshot leitet einen neuen Cluster-Stand an die Alarmauswertung weiter.
func (s *Server) ClusterSnapshot(ctx context.Context, snap *k8s.Snapshot) {
	if s.alerts == nil {
		return
	}
	now := s.nowFor()
	patterns := s.clusterIgnore(ctx)
	for _, part := range []struct {
		kind  string
		conds func() (map[string]k8s.Cond, bool)
		appNS string // Namensraum für Ausnahmen bei ArgoCD-Apps
	}{
		{alert.KindK8sNode, snap.NodeConds, ""},
		{alert.KindK8sWorkload, snap.WorkloadConds, ""},
		{alert.KindArgoCD, snap.AppConds, "argocd"},
	} {
		conds, ok := part.conds()
		if !ok {
			continue // unvollständig geladen: nichts auslösen, nichts beenden
		}
		if part.kind != alert.KindK8sNode {
			filterConds(conds, patterns, part.appNS)
		}
		set := make(map[string]alert.Condition, len(conds))
		for target, c := range conds {
			set[target] = alert.Condition{Bad: c.Bad, Detail: c.Detail}
		}
		s.alerts.ObserveSet(ctx, alert.ClusterID, "Cluster", part.kind, set, now)
	}
}

// clusterView ist die Antwort von /api/cluster.
type clusterView struct {
	*k8s.Snapshot
	Error string `json:"error,omitempty"` // letzter Abruffehler
}

func (s *Server) handleCluster(w http.ResponseWriter, _ *http.Request) {
	snap, errText := s.cluster.Current()
	if snap == nil {
		snap = &k8s.Snapshot{}
	}
	writeJSON(w, clusterView{Snapshot: snap, Error: errText})
}

// handleClusterLogs liefert die letzten Zeilen eines Containers als Text:
// ?ns=…&pod=…&container=…&lines=1..5000&previous=1.
func (s *Server) handleClusterLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lines, ok := intParam(q.Get("lines"), defaultLogLines, 1, maxLogLines)
	if !ok {
		http.Error(w, "lines muss zwischen 1 und 5000 liegen", http.StatusBadRequest)
		return
	}
	ns, pod, container := q.Get("ns"), q.Get("pod"), q.Get("container")
	if !k8s.ValidName(ns) || !k8s.ValidName(pod) || (container != "" && !k8s.ValidName(container)) {
		http.Error(w, "ungültiger name", http.StatusBadRequest)
		return
	}
	text, err := s.cluster.Logs(r.Context(), ns, pod, container, lines, q.Get("previous") == "1")
	switch {
	case errors.Is(err, k8s.ErrNotFound):
		http.Error(w, "pod oder container nicht gefunden (oder kein vorheriger container)", http.StatusNotFound)
		return
	case err != nil:
		s.log.Warn("pod-logs lesen", "ns", ns, "pod", pod, "err", err)
		http.Error(w, "logs nicht lesbar: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.log.Info("pod-logs gelesen", "ns", ns, "pod", pod, "container", container, "user", userFrom(r.Context()))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(text))
}
