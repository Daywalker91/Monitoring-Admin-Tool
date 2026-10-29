// Package k8s liest den Zustand des Kubernetes-Clusters (Nodes, Pods,
// Workloads, ArgoCD-Applications) über die REST-API, ohne client-go. Der
// Server braucht dafür nur Leserechte (ClusterRole in deploy/k8s).
package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Pfade des ServiceAccounts im Pod.
const (
	saDir        = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenFile    = saDir + "/token"
	caFile       = saDir + "/ca.crt"
	tokenReload  = time.Minute // projizierte Tokens werden rotiert
	requestLimit = 32 << 20    // Obergrenze für eine Antwort (Pod-Liste)
	logLimit     = 1 << 20     // Obergrenze für Pod-Logs
)

// ErrNotFound meldet eine unbekannte Ressource (z. B. ArgoCD nicht installiert).
var ErrNotFound = errors.New("nicht gefunden")

// Client spricht mit der Kubernetes-API.
type Client struct {
	base      string
	http      *http.Client
	tokenPath string // leer = ohne Token (z. B. kubectl proxy)

	mu        sync.Mutex
	token     string
	tokenRead time.Time
}

// FromEnv baut den Client: MAT_K8S=off schaltet ab, MAT_K8S_API (z. B.
// http://127.0.0.1:8001 für `kubectl proxy`) gilt für Tests außerhalb des
// Clusters, sonst die Umgebung des Pods. nil, nil = kein Cluster.
func FromEnv() (*Client, error) {
	if os.Getenv("MAT_K8S") == "off" {
		return nil, nil
	}
	if api := os.Getenv("MAT_K8S_API"); api != "" {
		u, err := url.Parse(api)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("MAT_K8S_API ungültig")
		}
		if u.Scheme == "http" && !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("MAT_K8S_API: http nur zu localhost (kubectl proxy)")
		}
		return &Client{base: strings.TrimRight(api, "/"), http: &http.Client{Timeout: 15 * time.Second}}, nil
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, nil // nicht im Cluster
	}
	if _, err := os.Stat(tokenFile); err != nil {
		return nil, fmt.Errorf("im cluster, aber kein serviceaccount-token (automountServiceAccountToken?): %w", err)
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("cluster-ca lesen: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("cluster-ca ungültig")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &Client{base: "https://" + net.JoinHostPort(host, port), tokenPath: tokenFile,
		http: &http.Client{Timeout: 15 * time.Second, Transport: transport}}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) bearer() (string, error) {
	if c.tokenPath == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || time.Since(c.tokenRead) > tokenReload {
		b, err := os.ReadFile(c.tokenPath)
		if err != nil {
			return "", fmt.Errorf("serviceaccount-token lesen: %w", err)
		}
		c.token, c.tokenRead = strings.TrimSpace(string(b)), time.Now()
	}
	return c.token, nil
}

// get ruft path (mit Query) ab und liefert den Inhalt bis limit Bytes.
func (c *Client) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: antwort größer als %d bytes", path, limit)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%s: keine berechtigung (clusterrole prüfen)", path)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	return body, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	body, err := c.get(ctx, path, requestLimit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: antwort lesen: %w", path, err)
	}
	return nil
}

// Logs liefert die letzten lines Zeilen eines Containers (mit Zeitstempel).
// Namen müssen vorher geprüft sein (ValidName).
func (c *Client) Logs(ctx context.Context, ns, pod, container string, lines int, previous bool) (string, error) {
	q := url.Values{}
	q.Set("tailLines", fmt.Sprint(lines))
	q.Set("timestamps", "true")
	q.Set("limitBytes", fmt.Sprint(logLimit))
	if container != "" {
		q.Set("container", container)
	}
	if previous {
		q.Set("previous", "true")
	}
	body, err := c.get(ctx, "/api/v1/namespaces/"+ns+"/pods/"+pod+"/log?"+q.Encode(), logLimit+64<<10)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ValidName prüft einen Kubernetes-Namen (DNS-1123, höchstens 253 Zeichen),
// bevor er in einen API-Pfad eingesetzt wird.
func ValidName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '.') && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}
