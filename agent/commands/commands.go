// Package commands führt die Befehle aus der commands-Whitelist in agent.json
// aus (Job command.run).
//
// Sicherheit: Jeder Befehl hat eine feste Argumentliste, die nur am Host steht;
// der Server wählt allein den Namen. Das Programm braucht einen absoluten Pfad.
// Shells sind als Programm verboten (keine freie Shell); einzige Ausnahme ist
// PowerShell mit -File <Skript> unter Windows, weil sich .ps1 nicht direkt
// starten lässt. Aufgerufen wird ohne Shell, mit fester Argumentliste.
package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/progress"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Spec ist ein Befehl aus agent.json.
type Spec struct {
	Argv        []string `json:"argv"`
	TimeoutSec  int      `json:"timeout_sec"` // 0 = defaultTimeout
	Description string   `json:"description"`
}

const (
	defaultTimeout = 5 * time.Minute
	maxArgs        = 64
	maxArgLen      = 1024
)

// shells sind Programme, die beliebigen Code aus ihren Argumenten ausführen
// (Vergleich ohne Groß-/Kleinschreibung und ohne .exe).
var shells = []string{"sh", "bash", "dash", "zsh", "ksh", "mksh", "ash", "fish", "csh", "tcsh", "busybox",
	"cmd", "powershell", "pwsh", "wscript", "cscript", "mshta"}

// powershellFlags dürfen vor -File stehen (Flag → erlaubte Werte; nil = ohne Wert).
var powershellFlags = map[string][]string{
	"-noprofile": nil, "-noninteractive": nil, "-nologo": nil,
	"-executionpolicy": {"bypass", "remotesigned", "allsigned"},
}

// Check prüft einen Befehl aus agent.json für das Betriebssystem goos.
func Check(name string, s Spec, goos string) error {
	if !proto.ValidCommandName(name) {
		return fmt.Errorf("name %q ungültig (a-z, 0-9, - und _, höchstens 32 zeichen)", name)
	}
	if !proto.ValidCommandDescription(s.Description) {
		return fmt.Errorf("%s: description zu lang oder mit steuerzeichen", name)
	}
	if s.TimeoutSec < 0 || time.Duration(s.TimeoutSec)*time.Second > proto.MaxCommandTimeout {
		return fmt.Errorf("%s: timeout_sec muss zwischen 0 und %d liegen", name, int(proto.MaxCommandTimeout.Seconds()))
	}
	if len(s.Argv) == 0 || len(s.Argv) > maxArgs {
		return fmt.Errorf("%s: argv braucht 1 bis %d einträge", name, maxArgs)
	}
	for _, a := range s.Argv {
		if len(a) > maxArgLen || strings.ContainsRune(a, 0) {
			return fmt.Errorf("%s: argument zu lang oder mit NUL-zeichen", name)
		}
	}
	if !isAbs(s.Argv[0], goos) {
		return fmt.Errorf("%s: programm braucht einen absoluten pfad: %q", name, s.Argv[0])
	}
	prog := progName(s.Argv[0], goos)
	if !slices.Contains(shells, prog) {
		return nil
	}
	if goos == "windows" && (prog == "powershell" || prog == "pwsh") && powershellFile(s.Argv[1:], goos) {
		return nil
	}
	return fmt.Errorf("%s: %s als programm ist nicht erlaubt (keine freie shell); skripte direkt aufrufen"+
		" (unter windows: powershell.exe -File <pfad>)", name, prog)
}

// powershellFile erlaubt genau: [erlaubte Flags …] -File <absoluter Pfad> [Argumente fürs Skript].
func powershellFile(args []string, goos string) bool {
	for i := 0; i < len(args); i++ {
		a := strings.ToLower(args[i])
		if a == "-file" {
			return i+1 < len(args) && isAbs(args[i+1], goos)
		}
		values, ok := powershellFlags[a]
		if !ok {
			return false
		}
		if values != nil {
			i++
			if i >= len(args) || !slices.Contains(values, strings.ToLower(args[i])) {
				return false
			}
		}
	}
	return false
}

// isAbs prüft einen absoluten Pfad für goos (Windows: Laufwerk oder UNC).
func isAbs(p, goos string) bool {
	if goos != "windows" {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, `\\`) {
		return len(p) > 2
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

// progName liefert den Programmnamen klein und ohne .exe.
func progName(p, goos string) string {
	base := p
	if i := strings.LastIndexAny(p, `/\`); i >= 0 && (goos == "windows" || p[i] == '/') {
		base = p[i+1:]
	}
	base = strings.ToLower(base)
	if goos == "windows" {
		base = strings.TrimSuffix(base, ".exe")
	}
	return base
}

// Runner führt die freigegebenen Befehle aus.
type Runner struct {
	specs map[string]Spec
}

// New erzeugt einen Runner; specs sind bereits mit Check geprüft.
func New(specs map[string]Spec) *Runner { return &Runner{specs: specs} }

// Infos liefert Namen und Beschreibungen für die hello-Nachricht, sortiert.
func (r *Runner) Infos() []proto.CommandInfo {
	out := make([]proto.CommandInfo, 0, len(r.specs))
	for name, s := range r.specs {
		out = append(out, proto.CommandInfo{Name: name, Description: s.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run ist der Job command.run. Der Name wird gegen die eigene Liste geprüft.
func (r *Runner) Run(ctx context.Context, params map[string]string) (string, bool, error) {
	name := params["name"]
	s, ok := r.specs[name]
	if !ok {
		return "", false, fmt.Errorf("befehl %q ist auf diesem host nicht freigegeben", name)
	}
	timeout := defaultTimeout
	if s.TimeoutSec > 0 {
		timeout = time.Duration(s.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
	cmd.Dir = filepath.Dir(s.Argv[0])
	var buf bytes.Buffer
	w := outputWriter(io.MultiWriter(&buf, progress.Writer(ctx)))
	cmd.Stdout, cmd.Stderr = w, w
	setProcessGroup(cmd)
	// Hält ein Kindprozess die Ausgabe offen, nicht ewig auf sie warten.
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	out := tail(buf.String(), proto.MaxJobOutputLen)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, false, fmt.Errorf("%s: zeitlimit von %s überschritten", name, timeout)
	}
	if err != nil {
		return out, false, fmt.Errorf("%s: %w", name, err)
	}
	return out, false, nil
}

// tail kürzt s auf die letzten n Bytes, ohne ein UTF-8-Zeichen zu zerteilen.
func tail(s string, n int) string {
	const mark = "…\n"
	if len(s) <= n {
		return s
	}
	i := len(s) - (n - len(mark))
	for i < len(s) && s[i]&0xC0 == 0x80 {
		i++
	}
	return mark + s[i:]
}
