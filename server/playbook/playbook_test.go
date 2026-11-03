package playbook

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const ampUpdate = `name: amp-update
description: AMP aktualisieren, wenn es Updates gibt
targets: [amp-server]
mode: serial
max_parallel: 1
on_error: stop
steps:
  - id: pruefen
    action: command.run
    params: {name: amp-update-check}
    ok_exit_codes: [0, 10]
  - id: update
    action: command.run
    params: {name: amp-update}
    when: {step: pruefen, exit_code: 0}
  - id: pause
    wait: 60s
    on_error: continue
  - id: neu-starten
    action: service.restart
    params: {name: ampinstmgr}
    when: {step: update, ok: true}
    on_error: continue
`

func TestParseExample(t *testing.T) {
	p, err := Parse([]byte(ampUpdate))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 4 || time.Duration(p.Steps[2].Wait) != time.Minute || !p.Steps[2].IsWait() ||
		*p.Steps[1].When.ExitCode != 0 || !*p.Steps[3].When.OK || p.Steps[0].Params["name"] != "amp-update-check" {
		t.Fatalf("%+v", p)
	}
	if p.EffectiveOnError(p.Steps[0]) != OnErrorStop || p.EffectiveOnError(p.Steps[3]) != OnErrorContinue {
		t.Error("on_error")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	p, err := Parse([]byte(ampUpdate))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	q, err := Parse(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if !reflect.DeepEqual(p, q) {
		t.Fatalf("unterschied nach marshal:\n%s", data)
	}
}

func TestParseRejects(t *testing.T) {
	for name, src := range map[string]string{
		"unbekanntes feld":   "name: x\ntargets: [a]\nsteps: [{id: a, wait: 1s}]\nbecome: true\n",
		"feld im schritt":    "name: x\ntargets: [a]\nsteps: [{id: a, wait: 1s, shell: ls}]\n",
		"zwei dokumente":     "name: x\ntargets: [a]\nsteps: [{id: a, wait: 1s}]\n---\nname: y\n",
		"keine dauer":        "name: x\ntargets: [a]\nsteps: [{id: a, wait: bald}]\n",
		"exit_code als text": "name: x\ntargets: [a]\nsteps: [{id: a, action: command.run, params: {name: a}, ok_exit_codes: [null, x]}]\n",
		"zu groß":            "name: x\n#" + strings.Repeat("x", MaxFileSize),
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: kein fehler", name)
		}
	}
}

// TestParamsAsText: Werte wie true oder 42 kommen als Text an, damit dieselbe
// Prüfung wie bei Jobs aus der GUI greift.
func TestParamsAsText(t *testing.T) {
	p, err := Parse([]byte("name: x\ntargets: [a]\nsteps: [{id: a, action: user.create, params: {name: hans, sudo: true}}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Params["sudo"] != "true" {
		t.Fatalf("%q", p.Steps[0].Params["sudo"])
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	base := func(steps string) string { return "name: test\ntargets: [a]\nsteps:\n" + steps }
	for _, c := range []struct {
		name, src, want string // want leer = gültig
	}{
		{"wait", base("  - {id: a, wait: 1h}\n"), ""},
		{"drain", base("  - {id: a, action: k3s.drain_reboot}\n"), ""},
		{"parallel", "name: test\ntargets: [a, b]\nmode: parallel\nmax_parallel: 3\nsteps: [{id: a, action: packages.refresh}]\n", ""},
		{"reboot stop", base("  - {id: a, action: system.reboot, on_error: stop}\n"), ""},
		{"name groß", "name: Test\ntargets: [a]\nsteps: [{id: a, wait: 1s}]\n", "name:"},
		{"ohne ziele", "name: test\nsteps: [{id: a, wait: 1s}]\n", "targets"},
		{"ohne schritte", "name: test\ntargets: [a]\n", "steps"},
		{"mode", "name: test\ntargets: [a]\nmode: random\nsteps: [{id: a, wait: 1s}]\n", "mode"},
		{"max_parallel serial", "name: test\ntargets: [a]\nmax_parallel: 2\nsteps: [{id: a, wait: 1s}]\n", "max_parallel"},
		{"max_parallel zu groß", "name: test\ntargets: [a]\nmode: parallel\nmax_parallel: 11\nsteps: [{id: a, wait: 1s}]\n", "max_parallel"},
		{"on_error", "name: test\ntargets: [a]\non_error: ignore\nsteps: [{id: a, wait: 1s}]\n", "on_error"},
		{"wait zu lang", base("  - {id: a, wait: 2h}\n"), "wait"},
		{"leer", base("  - {id: a}\n"), "action, wait, gate oder approve"},
		{"action und wait", base("  - {id: a, action: packages.refresh, wait: 1s}\n"), "schließen sich aus"},
		{"gate", base("  - {id: a, gate: cluster_healthy, timeout: 30m}\n  - {id: b, gate: host_healthy}\n"), ""},
		{"gate check", base("  - {id: a, gate: check_ok, check: Nextcloud Web}\n"), ""},
		{"gate unbekannt", base("  - {id: a, gate: alles_gut}\n"), "gate:"},
		{"gate check fehlt", base("  - {id: a, gate: check_ok}\n"), "check:"},
		{"gate check falsch", base("  - {id: a, gate: host_healthy, check: x}\n"), "check nur bei"},
		{"gate timeout", base("  - {id: a, gate: host_healthy, timeout: 2h}\n"), "timeout"},
		{"gate und action", base("  - {id: a, gate: host_healthy, action: packages.refresh}\n"), "schließen sich aus"},
		{"timeout ohne gate", base("  - {id: a, wait: 1s, timeout: 1m}\n"), "nur bei gate"},
		{"gate params", base("  - {id: a, gate: host_healthy, params: {x: y}}\n"), "keine params"},
		{"when auf gate", base("  - {id: a, gate: host_healthy}\n  - {id: b, wait: 1s, when: {step: a, ok: false}}\n"), ""},
		{"approve", base("  - {id: a, approve: \"node1 ist durch – weiter?\", once: true, timeout: 48h}\n"), ""},
		{"approve zu lang", base("  - {id: a, approve: " + strings.Repeat("x", 201) + "}\n"), "approve:"},
		{"approve timeout", base("  - {id: a, approve: weiter, timeout: 200h}\n"), "timeout"},
		{"approve und wait", base("  - {id: a, approve: weiter, wait: 1s}\n"), "schließen sich aus"},
		{"once ohne approve", base("  - {id: a, wait: 1s, once: true}\n"), "once nur bei approve"},
		{"approve check", base("  - {id: a, approve: weiter, check: x}\n"), "check nur bei gate"},
		{"unbekannte aktion", base("  - {id: a, action: shell.run}\n"), "unbekannte aktion"},
		{"agent-update", base("  - {id: a, action: agent.update, params: {version: v1.0.0}}\n"), "nicht erlaubt"},
		{"param fehlt", base("  - {id: a, action: command.run}\n"), "fehlt"},
		{"param unbekannt", base("  - {id: a, action: packages.refresh, params: {x: y}}\n"), "ungültiger parameter"},
		{"paketname", base("  - {id: a, action: package.install, params: {name: foo-}}\n"), "ungültiger wert"},
		{"drain mit params", base("  - {id: a, action: k3s.drain_reboot, params: {node: x}}\n"), "keine params"},
		{"continue reboot", base("  - {id: a, action: system.reboot, on_error: continue}\n"), "nicht erlaubt"},
		{"continue geerbt", "name: test\ntargets: [a]\non_error: continue\nsteps: [{id: a, action: packages.upgrade}]\n", "nicht erlaubt"},
		{"continue drain", base("  - {id: a, action: k3s.drain_reboot, on_error: continue}\n"), "nicht erlaubt"},
		{"exit codes falsche aktion", base("  - {id: a, action: packages.refresh, ok_exit_codes: [0]}\n"), "ok_exit_codes nur"},
		{"exit code negativ", base("  - {id: a, action: command.run, params: {name: a}, ok_exit_codes: [-1]}\n"), "negativ"},
		{"id doppelt", base("  - {id: a, wait: 1s}\n  - {id: a, wait: 1s}\n"), "doppelt"},
		{"id fehlt", base("  - {wait: 1s}\n"), "id:"},
		{"when später", base("  - {id: a, wait: 1s, when: {step: b, ok: true}}\n  - {id: b, wait: 1s}\n"), "kein früherer"},
		{"when selbst", base("  - {id: a, wait: 1s, when: {step: a, ok: true}}\n"), "sich selbst"},
		{"when beides", base("  - {id: a, action: command.run, params: {name: a}}\n  - {id: b, wait: 1s, when: {step: a, ok: true, exit_code: 0}}\n"), "genau eines"},
		{"when nichts", base("  - {id: a, wait: 1s}\n  - {id: b, wait: 1s, when: {step: a}}\n"), "genau eines"},
		{"when exit_code ohne command", base("  - {id: a, action: packages.refresh}\n  - {id: b, wait: 1s, when: {step: a, exit_code: 0}}\n"), "nur für"},
	} {
		p, err := Parse([]byte(c.src))
		if err != nil {
			t.Errorf("%s: parse: %v", c.name, err)
			continue
		}
		err = p.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unerwarteter fehler: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: fehler %q erwartet, bekommen: %v", c.name, c.want, err)
		}
	}
}

func TestTooManySteps(t *testing.T) {
	var b strings.Builder
	b.WriteString("name: test\ntargets: [a]\nsteps:\n")
	for i := range MaxSteps + 1 {
		b.WriteString("  - {id: s" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ", wait: 1s}\n")
	}
	p, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "höchstens") {
		t.Fatalf("%v", err)
	}
}

func TestWhenAndSucceeded(t *testing.T) {
	zero, ten, yes, no := 0, 10, true, false
	for _, c := range []struct {
		w    When
		r    Result
		want bool
	}{
		{When{ExitCode: &zero}, Result{ExitCode: 0, OK: true}, true},
		{When{ExitCode: &zero}, Result{ExitCode: 10, OK: true}, false},
		{When{ExitCode: &ten}, Result{ExitCode: 10}, true},
		{When{ExitCode: &zero}, Result{Skipped: true}, false},
		{When{ExitCode: &zero}, Result{ExitCode: -1}, false},
		{When{OK: &yes}, Result{OK: true}, true},
		{When{OK: &yes}, Result{OK: false}, false},
		{When{OK: &no}, Result{OK: false}, true},
		{When{OK: &no}, Result{Skipped: true}, false}, // übersprungen ist kein Fehler
	} {
		if got := c.w.Match(c.r); got != c.want {
			t.Errorf("%+v bei %+v: %v", c.w, c.r, got)
		}
	}

	plain, codes := Step{}, Step{OKExitCodes: []int{0, 10}}
	for _, c := range []struct {
		s    Step
		ok   bool
		exit int
		want bool
	}{
		{plain, true, 0, true},
		{plain, false, 1, false},
		{codes, false, 10, true},
		{codes, true, 0, true},
		{codes, false, 1, false},
		{codes, false, -1, false},
	} {
		if got := c.s.Succeeded(c.ok, c.exit); got != c.want {
			t.Errorf("%v ok=%v exit=%d: %v", c.s.OKExitCodes, c.ok, c.exit, got)
		}
	}
}

func TestParallel(t *testing.T) {
	for _, c := range []struct {
		p     Playbook
		hosts int
		want  int
	}{
		{Playbook{}, 5, 1},
		{Playbook{Mode: ModeSerial}, 5, 1},
		{Playbook{Mode: ModeParallel}, 5, 5},
		{Playbook{Mode: ModeParallel}, 20, MaxParallel},
		{Playbook{Mode: ModeParallel, MaxParallel: 2}, 5, 2},
		{Playbook{Mode: ModeParallel, MaxParallel: 8}, 3, 3},
	} {
		if got := c.p.Parallel(c.hosts); got != c.want {
			t.Errorf("%+v bei %d hosts: %d", c.p, c.hosts, got)
		}
	}
}

func TestResolve(t *testing.T) {
	hosts := []Host{
		{ID: "node1", Group: "k3s"},
		{ID: "node2", Group: "K3S"},
		{ID: "gs3", DisplayName: "Gameserver", Group: "spiele"},
	}
	p := &Playbook{Targets: []string{"gameserver", "k3s", "node1"}}
	got, err := p.Resolve(hosts)
	if err != nil || !reflect.DeepEqual(got, []string{"gs3", "node1", "node2"}) {
		t.Fatalf("%v %v", got, err)
	}
	p.Targets = []string{"node1", "fehlt", "auch-nicht"}
	if _, err := p.Resolve(hosts); err == nil || !strings.Contains(err.Error(), "fehlt, auch-nicht") {
		t.Fatalf("%v", err)
	}
	p.Targets = []string{"NODE1"} // Host-IDs genau
	if _, err := p.Resolve(hosts); err == nil {
		t.Fatal("id ohne groß-/kleinschreibung aufgelöst")
	}
}

// TestExamples: Die Beispiele in deploy/playbooks müssen gültig bleiben.
func TestExamples(t *testing.T) {
	files, err := LoadDir(filepath.Join("..", "..", "deploy", "playbooks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 {
		t.Fatalf("%d beispiele", len(files))
	}
	for _, f := range files {
		if !f.OK() {
			t.Errorf("%s: %s", f.File, f.Err)
		}
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("amp.yaml", strings.Replace(ampUpdate, "amp-server", "gs3", 1))
	write("kaputt.yml", "name: [\n")
	write("ziel.yaml", "name: ziel\ntargets: [weg]\nsteps: [{id: a, wait: 1s}]\n")
	write("dop1.yaml", "name: doppelt\ntargets: [gs3]\nsteps: [{id: a, wait: 1s}]\n")
	write("dop2.yaml", "name: doppelt\ntargets: [gs3]\nsteps: [{id: a, wait: 1s}]\n")
	write("notiz.txt", "kein playbook")
	write(".versteckt.yaml", "name: [\n")

	files, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]File{}
	for _, f := range files {
		got[f.File] = f
	}
	if len(files) != 5 {
		t.Fatalf("%d dateien: %+v", len(files), files)
	}
	if !got["amp.yaml"].OK() {
		t.Errorf("amp: %s", got["amp.yaml"].Err)
	}
	if got["kaputt.yml"].OK() || got["kaputt.yml"].Playbook != nil {
		t.Error("kaputt.yml startbar")
	}
	if f := got["ziel.yaml"]; !f.OK() { // Ziele prüft erst Resolve beim Anzeigen und Starten
		t.Errorf("ziel: %+v", f)
	}
	if got["dop1.yaml"].OK() || got["dop2.yaml"].OK() || !strings.Contains(got["dop1.yaml"].Err, "dop2.yaml") {
		t.Errorf("doppelte namen: %q %q", got["dop1.yaml"].Err, got["dop2.yaml"].Err)
	}
	if _, err := LoadDir(filepath.Join(dir, "fehlt")); err == nil {
		t.Error("fehlender ordner ohne fehler")
	}
}
