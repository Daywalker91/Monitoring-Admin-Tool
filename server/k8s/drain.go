package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CordonAnnotation markiert einen Node, den MAT abgesperrt hat. Wert ist die
// Job-ID; freigegeben wird nur mit derselben ID, also nie ein Node, den jemand
// anders (z. B. der system-upgrade-controller) abgesperrt hat.
const CordonAnnotation = "mat/cordoned-by"

// ErrBlocked meldet, dass ein PodDisruptionBudget die Eviction gerade nicht
// zulässt (HTTP 429). Später erneut versuchen.
var ErrBlocked = errors.New("durch PodDisruptionBudget blockiert")

// NodeState ist der für den drain nötige Zustand eines Nodes.
type NodeState struct {
	Name            string
	Ready           bool
	Unschedulable   bool
	CordonedBy      string // Wert von CordonAnnotation
	ResourceVersion string
}

// EvictPod ist ein Pod, der vor dem Neustart vom Node muss.
type EvictPod struct {
	Namespace   string
	Name        string
	Terminating bool // Löschung läuft bereits
}

// Node liest den Zustand eines Nodes.
func (c *Client) Node(ctx context.Context, name string) (NodeState, error) {
	if !ValidName(name) {
		return NodeState{}, errors.New("ungültiger node-name")
	}
	var n struct {
		Metadata struct {
			Name            string            `json:"name"`
			Annotations     map[string]string `json:"annotations"`
			ResourceVersion string            `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			Conditions []struct{ Type, Status string } `json:"conditions"`
		} `json:"status"`
	}
	if err := c.getJSON(ctx, "/api/v1/nodes/"+name, &n); err != nil {
		return NodeState{}, err
	}
	s := NodeState{Name: n.Metadata.Name, Unschedulable: n.Spec.Unschedulable,
		CordonedBy: n.Metadata.Annotations[CordonAnnotation], ResourceVersion: n.Metadata.ResourceVersion}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			s.Ready = c.Status == "True"
		}
	}
	return s, nil
}

// Cordon sperrt den Node für neue Pods und merkt sich die Job-ID. Mit
// resourceVersion schlägt der Patch fehl (409), falls sich der Node seit dem
// Lesen geändert hat.
func (c *Client) Cordon(ctx context.Context, n NodeState, jobID string) error {
	return c.patchNode(ctx, n, map[string]any{
		"metadata": map[string]any{"resourceVersion": n.ResourceVersion,
			"annotations": map[string]any{CordonAnnotation: jobID}},
		"spec": map[string]any{"unschedulable": true},
	})
}

// Uncordon gibt den Node wieder frei – nur, wenn MAT ihn mit dieser Job-ID
// abgesperrt hat. Sonst passiert nichts (false).
func (c *Client) Uncordon(ctx context.Context, name, jobID string) (bool, error) {
	n, err := c.Node(ctx, name)
	if err != nil {
		return false, err
	}
	if n.CordonedBy != jobID {
		return false, nil
	}
	return true, c.patchNode(ctx, n, map[string]any{
		"metadata": map[string]any{"resourceVersion": n.ResourceVersion,
			"annotations": map[string]any{CordonAnnotation: nil}},
		"spec": map[string]any{"unschedulable": false},
	})
}

func (c *Client) patchNode(ctx context.Context, n NodeState, patch map[string]any) error {
	if !ValidName(n.Name) {
		return errors.New("ungültiger node-name")
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	path := "/api/v1/nodes/" + n.Name
	status, _, err := c.do(ctx, http.MethodPatch, path, "application/merge-patch+json", body, requestLimit)
	if err != nil {
		return err
	}
	return writeStatus(path, status)
}

// PodsToEvict liefert die Pods auf dem Node, die weichen müssen: ohne
// DaemonSet-Pods (die laufen auf jedem Node), statische Pods (gehören dem
// Kubelet) und beendete Pods.
func (c *Client) PodsToEvict(ctx context.Context, node string) ([]EvictPod, error) {
	if !ValidName(node) {
		return nil, errors.New("ungültiger node-name")
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Namespace         string            `json:"namespace"`
				Annotations       map[string]string `json:"annotations"`
				DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
				OwnerReferences   []struct {
					Kind string `json:"kind"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	q := url.Values{"fieldSelector": {"spec.nodeName=" + node}}
	if err := c.getJSON(ctx, "/api/v1/pods?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	pods := []EvictPod{}
	for _, p := range list.Items {
		m := p.Metadata
		if _, mirror := m.Annotations["kubernetes.io/config.mirror"]; mirror {
			continue
		}
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		daemon := false
		for _, o := range m.OwnerReferences {
			daemon = daemon || o.Kind == "DaemonSet"
		}
		if daemon {
			continue
		}
		pods = append(pods, EvictPod{Namespace: m.Namespace, Name: m.Name, Terminating: m.DeletionTimestamp != nil})
	}
	return pods, nil
}

// Evict verdrängt einen Pod über die Eviction-API; PodDisruptionBudgets
// gelten (ErrBlocked). Ein schon verschwundener Pod ist kein Fehler.
func (c *Client) Evict(ctx context.Context, p EvictPod) error {
	if !ValidName(p.Namespace) || !ValidName(p.Name) {
		return errors.New("ungültiger pod-name")
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "policy/v1", "kind": "Eviction",
		"metadata": map[string]string{"name": p.Name, "namespace": p.Namespace},
	})
	if err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + p.Namespace + "/pods/" + p.Name + "/eviction"
	status, _, err := c.do(ctx, http.MethodPost, path, "application/json", body, requestLimit)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNotFound:
		return nil
	case http.StatusTooManyRequests:
		return ErrBlocked
	}
	return writeStatus(path, status)
}

// CanDrain prüft per SelfSubjectAccessReview, ob der ServiceAccount Nodes
// patchen und Pods verdrängen darf. Nur dann bietet MAT den drain an.
func (c *Client) CanDrain(ctx context.Context) (bool, error) {
	for _, attr := range []map[string]string{
		{"verb": "patch", "resource": "nodes"},
		{"verb": "create", "resource": "pods", "subresource": "eviction"},
	} {
		ok, err := c.allowed(ctx, attr)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func (c *Client) allowed(ctx context.Context, attr map[string]string) (bool, error) {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": attr},
	})
	if err != nil {
		return false, err
	}
	path := "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	status, out, err := c.do(ctx, http.MethodPost, path, "application/json", body, requestLimit)
	if err != nil {
		return false, err
	}
	if err := writeStatus(path, status); err != nil {
		return false, err
	}
	var res struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return false, fmt.Errorf("%s: antwort lesen: %w", path, err)
	}
	return res.Status.Allowed, nil
}

// writeStatus wertet den Status einer schreibenden Anfrage aus.
func writeStatus(path string, status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusForbidden:
		return fmt.Errorf("%s: keine berechtigung (clusterrole mat-server-drain prüfen)", path)
	case status == http.StatusConflict:
		return fmt.Errorf("%s: zwischenzeitlich geändert, bitte erneut versuchen", path)
	}
	return fmt.Errorf("%s: status %d", path, status)
}
