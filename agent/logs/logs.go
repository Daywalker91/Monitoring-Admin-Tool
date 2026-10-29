// Package logs liest freigegebene Log-Quellen (systemd-Journal bzw.
// Windows-Ereignisprotokoll) für den Job log.read.
//
// Sicherheit: Nur Quellen aus "log_sources" in agent.json sind lesbar; der
// Agent prüft die Quelle selbst, auch wenn der Server sie schon geprüft hat.
// journalctl bzw. wevtutil laufen mit fester Argumentliste.
package logs

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Reader liest die freigegebenen Quellen.
type Reader struct {
	sources []string
}

// New übernimmt die Quellen aus der Konfiguration, die auf dieser Plattform
// lesbar sind (journal:* unter Linux, eventlog:* unter Windows).
func New(sources []string) *Reader {
	r := &Reader{}
	if !supported() {
		return r
	}
	for _, s := range sources {
		if proto.ValidLogSource(s) && platformKind(s) {
			r.sources = append(r.sources, s)
		}
	}
	return r
}

// Sources liefert die lesbaren Quellen (für das hello).
func (r *Reader) Sources() []string { return slices.Clone(r.sources) }

// Job ist die Aktion log.read.
func (r *Reader) Job(ctx context.Context, p map[string]string) (string, bool, error) {
	source := p["source"]
	if !proto.ValidLogSource(source) || !slices.Contains(r.sources, source) {
		return "", false, fmt.Errorf("log-quelle %q ist nicht freigegeben", source)
	}
	out, err := read(ctx, source, proto.LogLines(p["lines"]))
	if err != nil {
		return out, false, err
	}
	if strings.TrimSpace(out) == "" {
		out = "(keine Einträge)\n"
	}
	return out, false, nil
}

// journalArgs baut die Argumente für journalctl (ohne Build-Tag, damit der Test überall läuft).
func journalArgs(source string, lines int) []string {
	args := []string{"--no-pager", "--quiet", "-o", "short-iso", "-n", strconv.Itoa(lines)}
	switch unit, _ := strings.CutPrefix(source, "journal:"); {
	case source == "journal":
	case unit == "kernel":
		args = append(args, "-k")
	default:
		args = append(args, "-u", unit)
	}
	return args
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
