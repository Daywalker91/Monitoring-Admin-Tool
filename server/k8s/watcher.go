package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Interval ist der Abstand zwischen zwei Abfragen des Clusters.
const Interval = 30 * time.Second

// Watcher fragt den Cluster regelmäßig ab und hält den letzten Stand.
type Watcher struct {
	c      *Client
	log    *slog.Logger
	onSnap func(context.Context, *Snapshot) // nach jedem erfolgreichen Abruf

	mu      sync.Mutex
	snap    *Snapshot
	lastErr string
}

// NewWatcher erzeugt einen Watcher; onSnap darf nil sein.
func NewWatcher(c *Client, log *slog.Logger, onSnap func(context.Context, *Snapshot)) *Watcher {
	return &Watcher{c: c, log: log, onSnap: onSnap}
}

// Run fragt sofort und danach alle Interval ab, bis ctx endet.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		w.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Watcher) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	snap, err := w.c.Fetch(ctx)
	w.mu.Lock()
	if err != nil {
		if w.lastErr != err.Error() {
			w.log.Warn("cluster abfragen", "err", err)
		}
		w.lastErr = err.Error()
		w.mu.Unlock()
		return
	}
	w.snap, w.lastErr = snap, ""
	w.mu.Unlock()
	if w.onSnap != nil {
		w.onSnap(ctx, snap)
	}
}

// Current liefert den letzten Stand (nil = noch keiner) und den letzten Fehler.
func (w *Watcher) Current() (*Snapshot, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap, w.lastErr
}

// Logs liefert Pod-Logs (Namen werden hier geprüft).
func (w *Watcher) Logs(ctx context.Context, ns, pod, container string, lines int, previous bool) (string, error) {
	if !ValidName(ns) || !ValidName(pod) || (container != "" && !ValidName(container)) {
		return "", fmt.Errorf("ungültiger name")
	}
	return w.c.Logs(ctx, ns, pod, container, lines, previous)
}

// Cond ist der Zustand eines überwachten Ziels.
type Cond struct {
	Bad    bool
	Detail string
}

// problemReasons sind Wartegründe von Containern, die einen Alarm wert sind.
var problemReasons = map[string]bool{
	"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true,
	"CreateContainerConfigError": true, "InvalidImageName": true, "OOMKilled": true,
}

// NodeConds liefert je Node, ob er nicht bereit ist; ok = false, wenn die
// Nodes nicht vollständig geladen wurden.
func (s *Snapshot) NodeConds() (map[string]Cond, bool) {
	out := map[string]Cond{}
	for _, n := range s.Nodes {
		out[n.Name] = Cond{Bad: !n.Ready, Detail: n.Reason}
	}
	return out, s.nodesOK
}

// WorkloadConds liefert Workloads mit zu wenigen bereiten Pods und Pods in
// einem Fehlerzustand (z. B. CrashLoopBackOff). Ziele: "Deployment ns/name"
// bzw. "Pod ns/name".
func (s *Snapshot) WorkloadConds() (map[string]Cond, bool) {
	out := map[string]Cond{}
	for _, w := range s.Workloads {
		out[w.Kind+" "+w.Namespace+"/"+w.Name] = Cond{Bad: w.Desired > 0 && w.Ready < w.Desired,
			Detail: fmt.Sprintf("%d/%d bereit", w.Ready, w.Desired)}
	}
	for _, p := range s.Pods {
		if problemReasons[p.Reason] {
			out["Pod "+p.Namespace+"/"+p.Name] = Cond{Bad: true, Detail: fmt.Sprintf("%s, %d Neustarts", p.Reason, p.Restarts)}
		}
	}
	return out, s.workloadsOK && s.podsOK
}

// AppConds liefert ArgoCD-Apps, die Degraded/Missing oder OutOfSync sind.
func (s *Snapshot) AppConds() (map[string]Cond, bool) {
	out := map[string]Cond{}
	for _, a := range s.Apps {
		bad := a.Health == "Degraded" || a.Health == "Missing" || a.Sync == "OutOfSync"
		out[a.Name] = Cond{Bad: bad, Detail: "Health " + a.Health + ", Sync " + a.Sync}
	}
	return out, s.appsOK
}
