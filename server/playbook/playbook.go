// Package playbook liest und prüft Playbooks (M12a): eine feste Reihenfolge
// bereits vorhandener Aktionen über mehrere Hosts. Ein Playbook bringt keine
// neuen Ausführungsmöglichkeiten; jeder Schritt ist ein Job aus der Whitelist
// in proto, den der Agent wie gewohnt selbst noch einmal prüft.
//
// Parsen (Parse), Prüfen (Validate) und Schreiben (Marshal) sind getrennt,
// damit später ein Editor in der GUI dasselbe Modell nutzen kann.
package playbook

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// ActionDrainReboot ist der Neustart mit drain (Ablauf im Server, kein Agent-Job).
const ActionDrainReboot = "k3s.drain_reboot"

// Obergrenzen.
const (
	MaxSteps       = 50
	MaxTargets     = 50
	MaxParallel    = 10
	MaxWait        = time.Hour
	MaxFileSize    = 64 << 10
	MaxFiles       = 64
	maxNameLen     = 64
	maxDescLen     = 500
	maxExitCodes   = 16
	maxTargetLen   = 128
	validateJobID  = "playbook-check"
	defaultOnError = OnErrorStop
)

// Gates (M12b): feste Prüfungen, auf die ein Schritt wartet. Ausdrücke gibt es nicht.
const (
	GateHostHealthy    = "host_healthy"    // Host verbunden, überwachte Dienste laufen, keine Units failed, kein SMART-/Platten-Alarm
	GateClusterHealthy = "cluster_healthy" // alle Nodes bereit, Workloads verfügbar (Ausnahmen wie bei den Alarmen)
	GateCheckOK        = "check_ok"        // externer Check (Name in check) meldet ok
)

// Fristen für Gates und Freigaben.
const (
	DefaultGateTimeout    = 10 * time.Minute
	MaxGateTimeout        = time.Hour
	DefaultApproveTimeout = 24 * time.Hour
	MaxApproveTimeout     = 7 * 24 * time.Hour
	maxCheckNameLen       = 100
	maxApproveLen         = 200
)

var gates = map[string]bool{GateHostHealthy: true, GateClusterHealthy: true, GateCheckOK: true}

// Ausführungsart.
const (
	ModeSerial   = "serial"
	ModeParallel = "parallel"
)

// Fehlerverhalten.
const (
	OnErrorStop     = "stop"
	OnErrorContinue = "continue"
)

// noContinue sind Aktionen, nach deren Fehlschlag ein Playbook nie einfach
// weiterlaufen darf (z. B. kein Neustart nach einem kaputten Upgrade).
var noContinue = map[string]bool{
	ActionDrainReboot:           true,
	proto.ActionReboot:          true,
	proto.ActionPackagesUpgrade: true,
}

// excluded sind Aktionen mit eigenem Ablauf, die kein Playbook auslöst.
var excluded = map[string]bool{proto.ActionAgentUpdate: true}

// Playbook ist ein geparstes Playbook.
type Playbook struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Targets     []string `yaml:"targets" json:"targets"` // Host-ID, Anzeigename oder Gruppe
	Mode        string   `yaml:"mode,omitempty" json:"mode,omitempty"`
	MaxParallel int      `yaml:"max_parallel,omitempty" json:"max_parallel,omitempty"`
	OnError     string   `yaml:"on_error,omitempty" json:"on_error,omitempty"`
	Schedule    string   `yaml:"schedule,omitempty" json:"schedule,omitempty"` // Zeitplan, siehe ParseSchedule
	Steps       []Step   `yaml:"steps" json:"steps"`
}

// Step ist genau eines von: Aktion (Action, Params), Wartezeit (Wait), Gate
// (Gate, Check, Timeout) oder Freigabe (Approve, Once, Timeout).
type Step struct {
	ID          string            `yaml:"id" json:"id"`
	Action      string            `yaml:"action,omitempty" json:"action,omitempty"`
	Params      map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
	OKExitCodes []int             `yaml:"ok_exit_codes,flow,omitempty" json:"ok_exit_codes,omitempty"`
	Wait        Duration          `yaml:"wait,omitempty" json:"wait,omitempty"`
	Gate        string            `yaml:"gate,omitempty" json:"gate,omitempty"`
	Check       string            `yaml:"check,omitempty" json:"check,omitempty"` // nur bei gate: check_ok
	Timeout     Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Approve     string            `yaml:"approve,omitempty" json:"approve,omitempty"` // Frage an den Menschen
	Once        bool              `yaml:"once,omitempty" json:"once,omitempty"`       // eine Freigabe gilt für alle Hosts des Laufs
	When        *When             `yaml:"when,omitempty" json:"when,omitempty"`
	OnError     string            `yaml:"on_error,omitempty" json:"on_error,omitempty"`
}

// When ist die einzige Form von Bedingung: Exit-Code oder Ergebnis eines
// früheren Schritts auf demselben Host. Genau eines von ExitCode und OK ist gesetzt.
type When struct {
	Step     string `yaml:"step" json:"step"`
	ExitCode *int   `yaml:"exit_code,omitempty" json:"exit_code,omitempty"`
	OK       *bool  `yaml:"ok,omitempty" json:"ok,omitempty"`
}

// Duration ist eine Dauer, die in YAML als Text steht ("60s", "5m").
type Duration time.Duration

// UnmarshalYAML liest "60s" usw.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("wait: dauer erwartet, z. B. 60s")
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("wait: %q ist keine dauer (z. B. 60s, 5m)", n.Value)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML schreibt die Dauer als Text.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// MarshalJSON schreibt die Dauer als Text (Anzeige in der GUI).
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

// IsWait meldet, ob der Schritt nur eine feste Zeit wartet.
func (s Step) IsWait() bool { return s.Action == "" && s.Gate == "" && s.Approve == "" }

// IsGate meldet, ob der Schritt auf ein Gate wartet.
func (s Step) IsGate() bool { return s.Action == "" && s.Gate != "" }

// IsApprove meldet, ob der Schritt auf eine Freigabe in der GUI wartet.
func (s Step) IsApprove() bool { return s.Action == "" && s.Gate == "" && s.Approve != "" }

// IsAction meldet, ob der Schritt einen Job auslöst.
func (s Step) IsAction() bool { return s.Action != "" }

// StepTimeout liefert die Frist eines Gates bzw. einer Freigabe.
func (s Step) StepTimeout() time.Duration {
	switch {
	case s.Timeout > 0:
		return time.Duration(s.Timeout)
	case s.IsApprove():
		return DefaultApproveTimeout
	}
	return DefaultGateTimeout
}

// EffectiveOnError liefert das Fehlerverhalten des Schritts: am Schritt,
// sonst am Playbook, sonst stop.
func (p *Playbook) EffectiveOnError(s Step) string {
	switch {
	case s.OnError != "":
		return s.OnError
	case p.OnError != "":
		return p.OnError
	}
	return defaultOnError
}

// Parallel liefert, wie viele Hosts gleichzeitig laufen dürfen.
func (p *Playbook) Parallel(hosts int) int {
	if p.Mode != ModeParallel {
		return 1
	}
	n := p.MaxParallel
	if n <= 0 {
		n = MaxParallel
	}
	return max(1, min(n, hosts))
}

// Parse liest ein Playbook aus YAML. Unbekannte Felder und mehrere Dokumente
// sind Fehler. Die inhaltliche Prüfung macht Validate.
func Parse(data []byte) (*Playbook, error) {
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("datei größer als %d KiB", MaxFileSize>>10)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Playbook
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("yaml: nur ein dokument je datei erlaubt")
	}
	return &p, nil
}

// Marshal schreibt ein Playbook als YAML (z. B. als Kopie in einem Lauf).
func Marshal(p *Playbook) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("yaml schreiben: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yaml schreiben: %w", err)
	}
	return buf.Bytes(), nil
}

// Validate prüft das Playbook inhaltlich, ohne Hosts aufzulösen (siehe Resolve).
func (p *Playbook) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !validName(p.Name) {
		add("name: a-z, 0-9 und -, höchstens %d zeichen", maxNameLen)
	}
	if len(p.Description) > maxDescLen || hasControl(p.Description) {
		add("description: höchstens %d zeichen, keine steuerzeichen", maxDescLen)
	}
	switch {
	case len(p.Targets) == 0:
		add("targets: mindestens ein ziel")
	case len(p.Targets) > MaxTargets:
		add("targets: höchstens %d ziele", MaxTargets)
	}
	for _, t := range p.Targets {
		if t == "" || len(t) > maxTargetLen || hasControl(t) {
			add("targets: ungültiges ziel %q", t)
		}
	}
	switch p.Mode {
	case "", ModeSerial, ModeParallel:
	default:
		add("mode: serial oder parallel")
	}
	if p.MaxParallel < 0 || p.MaxParallel > MaxParallel {
		add("max_parallel: 0 bis %d", MaxParallel)
	}
	if p.MaxParallel > 1 && p.Mode != ModeParallel {
		add("max_parallel > 1 nur mit mode: parallel")
	}
	if !validOnError(p.OnError) {
		add("on_error: stop oder continue")
	}
	if p.Schedule != "" {
		if _, err := ParseSchedule(p.Schedule); err != nil {
			add("%v", err)
		}
	}
	switch {
	case len(p.Steps) == 0:
		add("steps: mindestens ein schritt")
	case len(p.Steps) > MaxSteps:
		add("steps: höchstens %d schritte", MaxSteps)
	}

	seen := map[string]int{} // Schritt-ID → Position
	for i, s := range p.Steps {
		label := fmt.Sprintf("schritt %d", i+1)
		if s.ID != "" {
			label += " (" + s.ID + ")"
		}
		for _, err := range p.validateStep(s, seen) {
			add("%s: %v", label, err)
		}
		if validName(s.ID) {
			if _, dup := seen[s.ID]; dup {
				add("%s: id doppelt", label)
			}
			seen[s.ID] = i
		}
	}
	return errors.Join(errs...)
}

// validateStep prüft einen Schritt; seen enthält nur frühere Schritte.
func (p *Playbook) validateStep(s Step, seen map[string]int) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !validName(s.ID) {
		add("id: a-z, 0-9 und -, höchstens %d zeichen", maxNameLen)
	}
	if !validOnError(s.OnError) {
		add("on_error: stop oder continue")
	}
	kinds := 0
	for _, set := range []bool{s.Action != "", s.Wait != 0, s.Gate != "", s.Approve != ""} {
		if set {
			kinds++
		}
	}
	if kinds > 1 {
		add("action, wait, gate und approve schließen sich aus")
	}
	if !s.IsGate() && s.Check != "" {
		add("check nur bei gate")
	}
	if !s.IsGate() && !s.IsApprove() && s.Timeout != 0 {
		add("timeout nur bei gate und approve")
	}
	if !s.IsApprove() && s.Once {
		add("once nur bei approve")
	}
	switch {
	case s.IsWait():
		switch {
		case s.Wait <= 0:
			add("action, wait, gate oder approve fehlt")
		case time.Duration(s.Wait) > MaxWait:
			add("wait: höchstens %v", MaxWait)
		}
		if len(s.Params) > 0 || len(s.OKExitCodes) > 0 {
			add("wait-schritte haben keine params und keine ok_exit_codes")
		}
	case s.IsGate():
		if !gates[s.Gate] {
			add("gate: %s, %s oder %s", GateHostHealthy, GateClusterHealthy, GateCheckOK)
		}
		switch {
		case s.Gate == GateCheckOK && (s.Check == "" || len(s.Check) > maxCheckNameLen || hasControl(s.Check)):
			add("check: name des externen checks fehlt oder ist ungültig")
		case s.Gate != GateCheckOK && s.Check != "":
			add("check nur bei gate: %s", GateCheckOK)
		}
		if s.Timeout < 0 || time.Duration(s.Timeout) > MaxGateTimeout {
			add("timeout: höchstens %v", MaxGateTimeout)
		}
		if len(s.Params) > 0 || len(s.OKExitCodes) > 0 {
			add("gate-schritte haben keine params und keine ok_exit_codes")
		}
	case s.IsApprove():
		if len(s.Approve) > maxApproveLen || hasControl(s.Approve) {
			add("approve: höchstens %d zeichen, keine steuerzeichen", maxApproveLen)
		}
		if s.Timeout < 0 || time.Duration(s.Timeout) > MaxApproveTimeout {
			add("timeout: höchstens %v", MaxApproveTimeout)
		}
		if len(s.Params) > 0 || len(s.OKExitCodes) > 0 {
			add("approve-schritte haben keine params und keine ok_exit_codes")
		}
	default:
		switch {
		case excluded[s.Action]:
			add("aktion %s ist in playbooks nicht erlaubt", s.Action)
		case s.Action == ActionDrainReboot:
			if len(s.Params) > 0 {
				add("%s hat keine params", s.Action)
			}
		default:
			// Dieselbe Prüfung wie für Jobs aus der GUI und im Agenten.
			job := proto.JobRequest{JobID: validateJobID, Action: s.Action, Params: s.Params}
			if err := job.Validate(); err != nil {
				add("%v", err)
			}
		}
		if noContinue[s.Action] && p.EffectiveOnError(s) == OnErrorContinue {
			add("on_error: continue ist bei %s nicht erlaubt", s.Action)
		}
		if len(s.OKExitCodes) > 0 && s.Action != proto.ActionCommandRun {
			add("ok_exit_codes nur bei %s", proto.ActionCommandRun)
		}
		if len(s.OKExitCodes) > maxExitCodes {
			add("ok_exit_codes: höchstens %d werte", maxExitCodes)
		}
		for _, c := range s.OKExitCodes {
			if c < 0 { // Windows kennt große Codes (z. B. 3010), negative nicht
				add("ok_exit_codes: negativer wert %d", c)
			}
		}
	}
	if w := s.When; w != nil {
		i, ok := seen[w.Step]
		switch {
		case w.Step == s.ID:
			add("when: verweist auf sich selbst")
		case !ok:
			add("when: %q ist kein früherer schritt", w.Step)
		case (w.ExitCode == nil) == (w.OK == nil):
			add("when: genau eines von exit_code und ok angeben")
		case w.ExitCode != nil && p.Steps[i].Action != proto.ActionCommandRun:
			add("when: exit_code nur für %s-schritte", proto.ActionCommandRun)
		case w.ExitCode != nil && *w.ExitCode < 0:
			add("when: exit_code ist nicht negativ")
		}
	}
	return errs
}

// Result ist das Ergebnis eines Schritts auf einem Host, wie when es sieht.
type Result struct {
	Skipped  bool // übersprungen oder nicht ausgeführt
	OK       bool
	ExitCode int // nur bei command.run aussagekräftig; -1 = keine Rückmeldung
}

// Match meldet, ob die Bedingung für das Ergebnis r des referenzierten
// Schritts erfüllt ist. Ein übersprungener Schritt erfüllt keine Bedingung.
func (w *When) Match(r Result) bool {
	switch {
	case r.Skipped:
		return false
	case w.ExitCode != nil:
		return r.ExitCode >= 0 && r.ExitCode == *w.ExitCode
	case w.OK != nil:
		return r.OK == *w.OK
	}
	return false
}

// Succeeded bewertet das Job-Ergebnis eines Aktionsschritts. Mit
// ok_exit_codes zählt nur der Exit-Code (z. B. 10 = „nichts zu tun“).
func (s Step) Succeeded(jobOK bool, exitCode int) bool {
	if len(s.OKExitCodes) == 0 {
		return jobOK
	}
	return exitCode >= 0 && slices.Contains(s.OKExitCodes, exitCode)
}

// Host ist das, was Resolve über einen Host wissen muss.
type Host struct {
	ID          string
	DisplayName string
	Group       string
}

// Resolve löst die Ziele zu Host-IDs auf: Host-ID (genau), Anzeigename oder
// Gruppe (ohne Groß-/Kleinschreibung). Reihenfolge wie die Ziele, ohne
// Doppelte. Ein Ziel ohne Treffer ist ein Fehler.
func (p *Playbook) Resolve(hosts []Host) ([]string, error) {
	var out []string
	var missing []string
	for _, t := range p.Targets {
		found := false
		for _, h := range hosts {
			if h.ID == t || (h.DisplayName != "" && strings.EqualFold(h.DisplayName, t)) ||
				(h.Group != "" && strings.EqualFold(h.Group, t)) {
				found = true
				if !slices.Contains(out, h.ID) {
					out = append(out, h.ID)
				}
			}
		}
		if !found {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("unbekannte ziele: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func validName(s string) bool {
	if s == "" || len(s) > maxNameLen || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func validOnError(s string) bool {
	return s == "" || s == OnErrorStop || s == OnErrorContinue
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}
