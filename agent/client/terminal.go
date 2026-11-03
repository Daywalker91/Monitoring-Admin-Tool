package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// openTerminal baut für eine Sitzung eine eigene, ausgehende Verbindung zum
// Server auf (<server_url>/terminal/<id>) und übergibt sie dem Terminal. Ist das
// Terminal hier nicht freigeschaltet, meldet der Agent das über dieselbe
// Verbindung, damit der Browser den Grund erfährt.
func (c *Client) openTerminal(ctx context.Context, req proto.TerminalOpen) {
	log := c.log.With("session_id", req.SessionID)
	u, err := terminalURL(c.cfg.ServerURL, req.SessionID)
	if err != nil {
		log.Warn("terminal-adresse", "err", err)
		return
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.Token)
	conn, _, err := websocket.Dial(dialCtx, u, &websocket.DialOptions{HTTPHeader: header, HTTPClient: c.httpClient})
	if err != nil {
		log.Warn("terminal-verbindung", "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(proto.TerminalMaxFrame + 1024)

	if c.terminal == nil || !slices.Contains(c.hello.Capabilities, proto.CapTerminal) {
		log.Warn("terminal abgelehnt: nicht freigeschaltet", "gui_user", req.User)
		refuseTerminal(conn, "terminal ist auf diesem host nicht freigeschaltet")
		return
	}
	c.terminal(ctx, req, conn)
}

// terminalURL hängt /terminal/<id> an die Server-URL (/ws/agent) an.
func terminalURL(serverURL, sessionID string) (string, error) {
	if !proto.ValidTerminalID(sessionID) {
		return "", fmt.Errorf("ungültige sitzungs-id")
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/terminal/" + sessionID
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func refuseTerminal(conn *websocket.Conn, reason string) {
	data, _ := json.Marshal(proto.TerminalControl{Type: proto.TerminalExit, Reason: reason})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
	conn.Close(websocket.StatusNormalClosure, "")
}
