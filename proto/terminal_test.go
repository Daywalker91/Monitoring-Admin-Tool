package proto

import (
	"strings"
	"testing"
)

func TestValidTerminalID(t *testing.T) {
	ok := TerminalIDPrefix + strings.Repeat("a1", 16)
	if !ValidTerminalID(ok) {
		t.Fatal("gültige id abgelehnt")
	}
	for _, s := range []string{"", "term-", "job-" + strings.Repeat("a1", 16), ok + "0", ok[:len(ok)-1],
		TerminalIDPrefix + strings.Repeat("A1", 16), TerminalIDPrefix + strings.Repeat("g1", 16)} {
		if ValidTerminalID(s) {
			t.Errorf("%q angenommen", s)
		}
	}
}

func TestTerminalOpenValidate(t *testing.T) {
	id := TerminalIDPrefix + strings.Repeat("0f", 16)
	good := TerminalOpen{SessionID: id, Cols: 80, Rows: 24, User: "hans"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]TerminalOpen{
		"id":       {SessionID: "x", Cols: 80, Rows: 24, User: "hans"},
		"cols 0":   {SessionID: id, Cols: 0, Rows: 24, User: "hans"},
		"rows":     {SessionID: id, Cols: 80, Rows: TerminalMaxRows + 1, User: "hans"},
		"benutzer": {SessionID: id, Cols: 80, Rows: 24, User: "a b"},
	} {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: kein fehler", name)
		}
	}
}

func TestTerminalControlValidate(t *testing.T) {
	for _, c := range []struct {
		ctl       TerminalControl
		fromAgent bool
		ok        bool
	}{
		{TerminalControl{Type: TerminalResize, Cols: 120, Rows: 40}, false, true},
		{TerminalControl{Type: TerminalResize, Cols: 120, Rows: 40}, true, false}, // Agent ändert keine Größe
		{TerminalControl{Type: TerminalResize, Cols: 0, Rows: 40}, false, false},
		{TerminalControl{Type: TerminalExit, Reason: "leerlauf"}, true, true},
		{TerminalControl{Type: TerminalExit}, false, false}, // Browser beendet durch Trennen
		{TerminalControl{Type: TerminalExit, Reason: strings.Repeat("x", 201)}, true, false},
		{TerminalControl{Type: "input"}, false, false},
	} {
		if err := c.ctl.Validate(c.fromAgent); (err == nil) != c.ok {
			t.Errorf("%+v agent=%v: %v", c.ctl, c.fromAgent, err)
		}
	}
}

func TestHelloTerminalUser(t *testing.T) {
	h := Hello{AgentID: "a", Hostname: "a", IntervalSec: 10, TerminalUser: "hans"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	h.TerminalUser = "root"
	if err := h.Validate(); err == nil {
		t.Fatal("root als terminal_user angenommen")
	}
}
