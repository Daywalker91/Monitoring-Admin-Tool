package logs

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJournalArgs(t *testing.T) {
	cases := map[string]string{
		"journal":        "--no-pager --quiet -o short-iso -n 50",
		"journal:kernel": "--no-pager --quiet -o short-iso -n 50 -k",
		"journal:sshd":   "--no-pager --quiet -o short-iso -n 50 -u sshd",
	}
	for src, want := range cases {
		if got := strings.Join(journalArgs(src, 50), " "); got != want {
			t.Errorf("%s: %q", src, got)
		}
	}
}

func TestReader(t *testing.T) {
	r := New([]string{"journal:sshd", "eventlog:System", "kaputt:x"})
	want := map[string][]string{"linux": {"journal:sshd"}, "windows": {"eventlog:System"}}[runtime.GOOS]
	if !slices.Equal(r.Sources(), want) {
		t.Fatalf("quellen: %v, erwartet %v", r.Sources(), want)
	}
	for _, src := range []string{"journal", "eventlog:Security", "journal:-f"} {
		if _, _, err := r.Job(context.Background(), map[string]string{"source": src}); err == nil {
			t.Errorf("%q gelesen, obwohl nicht freigegeben", src)
		}
	}
	if runtime.GOOS != "windows" {
		return
	}
	// Liest drei Ereignisse aus dem echten Systemprotokoll (nur lesend).
	out, _, err := r.Job(context.Background(), map[string]string{"source": "eventlog:System", "lines": "3"})
	if err != nil || !strings.Contains(out, "Event[0]") {
		t.Fatalf("eventlog: %v\n%.300s", err, out)
	}
	if !utf8.ValidString(out) || strings.ContainsAny(out, "\x00\r") {
		t.Errorf("ausgabe nicht bereinigt: %q", out[:min(len(out), 300)])
	}
}
