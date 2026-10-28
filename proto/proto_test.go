package proto

import (
	"encoding/json"
	"testing"
)

func TestEncodeRoundtrip(t *testing.T) {
	in := Hello{ProtoVersion: Version, AgentID: "pi-1", Hostname: "pi", IntervalSec: 10,
		Capabilities: []Capability{CapMetrics}}
	raw, err := Encode(TypeHello, in)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeHello {
		t.Fatalf("type = %q", env.Type)
	}
	var out Hello
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.AgentID != in.AgentID || len(out.Capabilities) != 1 {
		t.Fatalf("unerwartet: %+v", out)
	}
}

func TestHelloValidate(t *testing.T) {
	ok := Hello{AgentID: "srv-01.lan", Hostname: "srv", IntervalSec: 10}
	if err := ok.Validate(); err != nil {
		t.Fatalf("gültige Hello abgelehnt: %v", err)
	}
	bad := []Hello{
		{AgentID: "", Hostname: "x", IntervalSec: 10},
		{AgentID: "a b", Hostname: "x", IntervalSec: 10},
		{AgentID: "a;rm", Hostname: "x", IntervalSec: 10},
		{AgentID: "a", Hostname: "", IntervalSec: 10},
		{AgentID: "a", Hostname: "x", IntervalSec: 0},
	}
	for i, h := range bad {
		if err := h.Validate(); err == nil {
			t.Errorf("Fall %d: ungültige Hello akzeptiert: %+v", i, h)
		}
	}
}
