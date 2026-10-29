package proto

import (
	"encoding/json"
	"strings"
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

func TestJobValidate(t *testing.T) {
	ok := JobRequest{JobID: "j1", Action: ActionAgentUpdate, Params: map[string]string{"version": "v0.5.1"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []JobRequest{
		{JobID: "j1", Action: "shell.exec"},
		{JobID: "", Action: ActionAgentUpdate},
		{JobID: "j1", Action: ActionAgentUpdate, Params: map[string]string{"a b": "x"}},
		{JobID: "j1", Action: ActionAgentUpdate, Params: map[string]string{"version": strings.Repeat("x", 300)}},
		{JobID: "j1", Action: ActionAgentUpdate},                                                       // fehlt
		{JobID: "j1", Action: ActionAgentUpdate, Params: map[string]string{"version": "v1; rm -rf /"}}, // wert
		{JobID: "j1", Action: ActionAgentUpdate, Params: map[string]string{"version": "v1", "x": "y"}}, // zusätzlich
	}
	for i, j := range bad {
		if err := j.Validate(); err == nil {
			t.Errorf("%d: ungültiger job akzeptiert", i)
		}
	}
	r := JobResult{JobID: "j1", Output: strings.Repeat("x", MaxJobOutputLen+1)}
	if err := r.Validate(); err == nil {
		t.Error("zu lange ausgabe akzeptiert")
	}
	if JobTimeout("unbekannt") != defaultJobTimeout {
		t.Error("standard-zeitlimit fehlt")
	}
	if got := CleanOutput("a\x00b\xf6c", 10); got != "ab?c" {
		t.Errorf("CleanOutput = %q", got)
	}
	if got := Truncate("aä", 2); got != "a" {
		t.Errorf("Truncate = %q", got)
	}
}

func TestMetricsValidateTemps(t *testing.T) {
	temp := func(v float64) *float64 { return &v }
	ok := Metrics{CPUTemp: temp(48.5), Temps: []Temp{{Name: "cpu-thermal", Celsius: 48.5}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Metrics{
		{CPUTemp: temp(1e9)},
		{Temps: []Temp{{Name: "x", Celsius: -500}}},
		{Temps: []Temp{{Name: strings.Repeat("x", 600), Celsius: 40}}},
		{Temps: make([]Temp, 100)},
	}
	for i, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("%d: ungültige temperatur akzeptiert", i)
		}
	}
}

func TestLogSources(t *testing.T) {
	for _, s := range []string{"journal", "journal:kernel", "journal:sshd", "journal:getty@tty1", "eventlog:System",
		"eventlog:Microsoft-Windows-PowerShell/Operational"} {
		if !ValidLogSource(s) {
			t.Errorf("%q abgelehnt", s)
		}
	}
	for _, s := range []string{"", "journal:", "journal:-f", "journal:a b", "file:/etc/shadow", "eventlog:Sys;tem",
		"eventlog:-x", "journal:" + strings.Repeat("x", 200)} {
		if ValidLogSource(s) {
			t.Errorf("%q akzeptiert", s)
		}
	}
	if ValidLogSources([]string{"journal", "journal"}) || !ValidLogSources(nil) {
		t.Error("liste falsch geprüft")
	}
	j := JobRequest{JobID: "j1", Action: ActionLogRead, Params: map[string]string{"source": "journal:sshd", "lines": "500"}}
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, lines := range []string{"0", "1001", "abc", "-5"} {
		j.Params["lines"] = lines
		if j.Validate() == nil {
			t.Errorf("lines=%s akzeptiert", lines)
		}
	}
	if LogLines("") != DefaultLogLines || LogLines("50") != 50 || !ReadOnly(ActionLogRead) || ReadOnly(ActionReboot) {
		t.Error("LogLines/ReadOnly")
	}
}
