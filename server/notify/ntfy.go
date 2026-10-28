package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Ntfy sendet Push-Nachrichten an einen ntfy-Server (https://ntfy.sh oder selbst gehostet).
type Ntfy struct {
	base   string // URL des Servers, an die per JSON veröffentlicht wird
	topic  string
	token  string // optional: Access-Token (Bearer)
	client *http.Client
}

// NewNtfy erzeugt den Kanal aus der Topic-URL, z. B. https://ntfy.sh/mein-topic.
// insecure meldet, dass die URL unverschlüsselt (http) ist.
func NewNtfy(topicURL, token string) (n *Ntfy, insecure bool, err error) {
	u, err := url.Parse(topicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, false, fmt.Errorf("ntfy-url muss die form https://server/topic haben")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, false, fmt.Errorf("ntfy-url darf keine zugangsdaten, parameter oder fragmente enthalten")
	}
	path := strings.TrimSuffix(u.Path, "/")
	i := strings.LastIndex(path, "/")
	topic := path[i+1:]
	if i < 0 || !validTopic(topic) {
		return nil, false, fmt.Errorf("ntfy-url enthält kein gültiges topic")
	}
	u.Path = path[:i+1]
	return &Ntfy{base: u.String(), topic: topic, token: token, client: &http.Client{}}, u.Scheme == "http", nil
}

func validTopic(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// Name implementiert Channel.
func (n *Ntfy) Name() string { return "ntfy" }

// Send implementiert Channel.
func (n *Ntfy) Send(ctx context.Context, m Message) error {
	priority := 3
	if m.High {
		priority = 4
	}
	payload := map[string]any{"topic": n.topic, "title": m.Title, "message": m.Body, "priority": priority}
	if m.Tag != "" {
		payload["tags"] = []string{m.Tag}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("ntfy: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
