package client

import (
	"strings"
	"testing"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func TestTerminalURL(t *testing.T) {
	id := proto.TerminalIDPrefix + strings.Repeat("cd", 16)
	for in, want := range map[string]string{
		"wss://mat.lan/ws/agent":        "wss://mat.lan/ws/agent/terminal/" + id,
		"ws://127.0.0.1:8080/ws/agent/": "ws://127.0.0.1:8080/ws/agent/terminal/" + id,
		"wss://mat.lan/ws/agent?x=1":    "wss://mat.lan/ws/agent/terminal/" + id,
	} {
		got, err := terminalURL(in, id)
		if err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if _, err := terminalURL("wss://mat.lan/ws/agent", "term-../x"); err == nil {
		t.Error("ungültige id angenommen")
	}
}
