package k8s

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot ist der verdichtete Zustand des Clusters für GUI und Alarme.
type Snapshot struct {
	FetchedAt int64      `json:"fetched_at"`
	Errors    []string   `json:"errors,omitempty"` // Teilfehler (z. B. metrics-server fehlt)
	Nodes     []Node     `json:"nodes"`
	Workloads []Workload `json:"workloads"`
	Pods      []Pod      `json:"pods"`
	Apps      []App      `json:"apps"`
	HasArgo   bool       `json:"has_argocd"`

	// Welche Teile vollständig geladen wurden; nur diese gehen in die Alarme.
	nodesOK, workloadsOK, podsOK, appsOK bool
}

// Node ist ein Knoten.
type Node struct {
	Name     string   `json:"name"`
	Ready    bool     `json:"ready"`
	Reason   string   `json:"reason,omitempty"` // bei NotReady: Meldung der Ready-Condition
	Roles    []string `json:"roles"`
	Version  string   `json:"version"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	IP       string   `json:"ip"`
	Created  int64    `json:"created"`
	CPUCores float64  `json:"cpu_cores"`
	MemBytes int64    `json:"mem_bytes"`
	CPUPct   *float64 `json:"cpu_pct"` // nil = kein metrics-server
	MemPct   *float64 `json:"mem_pct"`
	Pods     int      `json:"pods"`
}

// Workload ist ein Deployment, StatefulSet oder DaemonSet.
type Workload struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"` // "Deployment", "StatefulSet", "DaemonSet"
	Name      string `json:"name"`
	Desired   int    `json:"desired"`
	Ready     int    `json:"ready"`
	Created   int64  `json:"created"`
}

// Pod ist ein Pod mit seinen Containern.
type Pod struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Phase      string   `json:"phase"`
	Reason     string   `json:"reason,omitempty"` // z. B. CrashLoopBackOff, ImagePullBackOff
	Ready      int      `json:"ready"`
	Total      int      `json:"total"`
	Restarts   int      `json:"restarts"`
	Node       string   `json:"node"`
	Created    int64    `json:"created"`
	Owner      string   `json:"owner,omitempty"` // z. B. "ReplicaSet"
	Containers []string `json:"containers"`
}

// App ist eine ArgoCD-Application.
type App struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Project   string `json:"project"`
	Sync      string `json:"sync"`   // Synced, OutOfSync, Unknown
	Health    string `json:"health"` // Healthy, Progressing, Degraded, Missing, …
	Revision  string `json:"revision,omitempty"`
	Path      string `json:"path,omitempty"`
	DestNS    string `json:"dest_namespace,omitempty"`
}

// --- Rohformate der API (nur benötigte Felder) ------------------------------

type meta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	OwnerReferences   []struct {
		Kind string `json:"kind"`
	} `json:"ownerReferences"`
}

type nodeList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
		Status   struct {
			Capacity   map[string]string `json:"capacity"`
			Conditions []struct {
				Type, Status, Message string
			} `json:"conditions"`
			Addresses []struct {
				Type, Address string
			} `json:"addresses"`
			NodeInfo struct {
				KubeletVersion  string `json:"kubeletVersion"`
				OperatingSystem string `json:"operatingSystem"`
				Architecture    string `json:"architecture"`
				OSImage         string `json:"osImage"`
			} `json:"nodeInfo"`
		} `json:"status"`
	} `json:"items"`
}

type nodeMetricsList struct {
	Items []struct {
		Metadata meta              `json:"metadata"`
		Usage    map[string]string `json:"usage"`
	} `json:"items"`
}

type workloadList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas          int `json:"readyReplicas"`
			DesiredNumberScheduled int `json:"desiredNumberScheduled"` // DaemonSet
			NumberReady            int `json:"numberReady"`            // DaemonSet
		} `json:"status"`
	} `json:"items"`
}

type containerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
		Terminated *struct {
			Reason string `json:"reason"`
		} `json:"terminated"`
	} `json:"state"`
}

type podList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name string `json:"name"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase             string            `json:"phase"`
			Reason            string            `json:"reason"`
			ContainerStatuses []containerStatus `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type appList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			Project string `json:"project"`
			Source  struct {
				Path string `json:"path"`
			} `json:"source"`
			Destination struct {
				Namespace string `json:"namespace"`
			} `json:"destination"`
		} `json:"spec"`
		Status struct {
			Sync struct {
				Status   string `json:"status"`
				Revision string `json:"revision"`
			} `json:"sync"`
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
		} `json:"status"`
	} `json:"items"`
}

// Fetch lädt den aktuellen Zustand. Teilfehler landen in Snapshot.Errors;
// nur wenn gar nichts geht, kommt ein Fehler zurück.
func (c *Client) Fetch(ctx context.Context) (*Snapshot, error) {
	s := &Snapshot{FetchedAt: time.Now().Unix(), Nodes: []Node{}, Workloads: []Workload{}, Pods: []Pod{}, Apps: []App{}}
	fail := func(what string, err error) { s.Errors = append(s.Errors, what+": "+err.Error()) }

	var nodes nodeList
	if err := c.getJSON(ctx, "/api/v1/nodes", &nodes); err != nil {
		return nil, fmt.Errorf("nodes lesen: %w", err)
	}
	s.nodesOK = true
	byName := map[string]int{}
	for _, n := range nodes.Items {
		node := Node{Name: n.Metadata.Name, Version: n.Status.NodeInfo.KubeletVersion, OS: n.Status.NodeInfo.OSImage,
			Arch: n.Status.NodeInfo.Architecture, Created: n.Metadata.CreationTimestamp.Unix(), Roles: []string{}}
		for _, cnd := range n.Status.Conditions {
			if cnd.Type == "Ready" {
				node.Ready = cnd.Status == "True"
				if !node.Ready {
					node.Reason = cnd.Message
				}
			}
		}
		for _, a := range n.Status.Addresses {
			if a.Type == "InternalIP" && node.IP == "" {
				node.IP = a.Address
			}
		}
		for l := range n.Metadata.Labels {
			if role, ok := strings.CutPrefix(l, "node-role.kubernetes.io/"); ok && role != "" {
				node.Roles = append(node.Roles, role)
			}
		}
		sort.Strings(node.Roles)
		node.CPUCores = parseCPU(n.Status.Capacity["cpu"])
		node.MemBytes = parseMem(n.Status.Capacity["memory"])
		byName[node.Name] = len(s.Nodes)
		s.Nodes = append(s.Nodes, node)
	}

	var nm nodeMetricsList
	if err := c.getJSON(ctx, "/apis/metrics.k8s.io/v1beta1/nodes", &nm); err != nil {
		fail("metrics-server", err)
	} else {
		for _, m := range nm.Items {
			i, ok := byName[m.Metadata.Name]
			if !ok {
				continue
			}
			n := &s.Nodes[i]
			if n.CPUCores > 0 {
				v := round1(parseCPU(m.Usage["cpu"]) / n.CPUCores * 100)
				n.CPUPct = &v
			}
			if n.MemBytes > 0 {
				v := round1(float64(parseMem(m.Usage["memory"])) / float64(n.MemBytes) * 100)
				n.MemPct = &v
			}
		}
	}

	s.workloadsOK = true
	for _, kind := range []struct{ path, name string }{
		{"/apis/apps/v1/deployments", "Deployment"},
		{"/apis/apps/v1/statefulsets", "StatefulSet"},
		{"/apis/apps/v1/daemonsets", "DaemonSet"},
	} {
		var wl workloadList
		if err := c.getJSON(ctx, kind.path, &wl); err != nil {
			fail(kind.name, err)
			s.workloadsOK = false
			continue
		}
		for _, w := range wl.Items {
			item := Workload{Namespace: w.Metadata.Namespace, Kind: kind.name, Name: w.Metadata.Name,
				Created: w.Metadata.CreationTimestamp.Unix(), Ready: w.Status.ReadyReplicas, Desired: 1}
			if w.Spec.Replicas != nil {
				item.Desired = *w.Spec.Replicas
			}
			if kind.name == "DaemonSet" {
				item.Desired, item.Ready = w.Status.DesiredNumberScheduled, w.Status.NumberReady
			}
			s.Workloads = append(s.Workloads, item)
		}
	}
	sort.Slice(s.Workloads, func(i, j int) bool {
		a, b := s.Workloads[i], s.Workloads[j]
		return a.Namespace+"/"+a.Kind+"/"+a.Name < b.Namespace+"/"+b.Kind+"/"+b.Name
	})

	var pods podList
	if err := c.getJSON(ctx, "/api/v1/pods", &pods); err != nil {
		fail("pods", err)
	} else {
		s.podsOK = true
		for _, p := range pods.Items {
			pod := Pod{Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, Phase: p.Status.Phase,
				Reason: p.Status.Reason, Node: p.Spec.NodeName, Created: p.Metadata.CreationTimestamp.Unix(),
				Total: len(p.Spec.Containers), Containers: []string{}}
			if len(p.Metadata.OwnerReferences) > 0 {
				pod.Owner = p.Metadata.OwnerReferences[0].Kind
			}
			for _, ct := range p.Spec.Containers {
				pod.Containers = append(pod.Containers, ct.Name)
			}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Ready {
					pod.Ready++
				}
				pod.Restarts += cs.RestartCount
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "ContainerCreating" {
					pod.Reason = cs.State.Waiting.Reason
				} else if cs.State.Terminated != nil && pod.Reason == "" && p.Status.Phase != "Succeeded" {
					pod.Reason = cs.State.Terminated.Reason
				}
			}
			if i, ok := byName[pod.Node]; ok && p.Status.Phase == "Running" {
				s.Nodes[i].Pods++
			}
			s.Pods = append(s.Pods, pod)
		}
		sort.Slice(s.Pods, func(i, j int) bool {
			return s.Pods[i].Namespace+"/"+s.Pods[i].Name < s.Pods[j].Namespace+"/"+s.Pods[j].Name
		})
	}

	var apps appList
	switch err := c.getJSON(ctx, "/apis/argoproj.io/v1alpha1/applications", &apps); {
	case errors.Is(err, ErrNotFound):
		s.appsOK = true // ArgoCD nicht installiert
	case err != nil:
		fail("ArgoCD", err)
	default:
		s.appsOK, s.HasArgo = true, true
		for _, a := range apps.Items {
			s.Apps = append(s.Apps, App{Namespace: a.Metadata.Namespace, Name: a.Metadata.Name, Project: a.Spec.Project,
				Sync: a.Status.Sync.Status, Health: a.Status.Health.Status, Revision: short(a.Status.Sync.Revision, 8),
				Path: a.Spec.Source.Path, DestNS: a.Spec.Destination.Namespace})
		}
		sort.Slice(s.Apps, func(i, j int) bool { return s.Apps[i].Name < s.Apps[j].Name })
	}
	return s, nil
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// parseCPU liest Kubernetes-CPU-Mengen: "4", "250m", "1234567n".
func parseCPU(s string) float64 {
	switch {
	case strings.HasSuffix(s, "n"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "n"), 64)
		return v / 1e9
	case strings.HasSuffix(s, "u"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "u"), 64)
		return v / 1e6
	case strings.HasSuffix(s, "m"):
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		return v / 1e3
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// parseMem liest Speichermengen: "8037304Ki", "2Gi", "512M", "1000000".
func parseMem(s string) int64 {
	units := []struct {
		suffix string
		mult   float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, _ := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			return int64(v * u.mult)
		}
	}
	v, _ := strconv.ParseFloat(s, 64)
	return int64(v)
}
