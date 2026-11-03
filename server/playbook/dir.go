package playbook

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// File ist eine Playbook-Datei samt Prüfergebnis. Playbook ist auch bei
// Fehlern gesetzt, sofern die Datei sich parsen ließ (für die Anzeige).
type File struct {
	File     string    `json:"file"`
	Playbook *Playbook `json:"playbook,omitempty"`
	Err      string    `json:"error,omitempty"` // leer = startbar
}

// OK meldet, ob das Playbook gestartet werden darf.
func (f File) OK() bool { return f.Err == "" && f.Playbook != nil }

// LoadDir liest alle *.yaml/*.yml in dir (nicht rekursiv; Links werden
// verfolgt, weil k8s-ConfigMaps Dateien als Links einhängen) und prüft sie.
// Die Ziele prüft Resolve erst beim Anzeigen und Starten, weil sich Hosts
// ändern. Fehlerhafte Dateien stehen mit Meldung in der Liste; der Fehler von
// LoadDir betrifft nur den Ordner selbst.
func LoadDir(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("playbook-ordner lesen: %w", err)
	}
	var out []File
	for _, e := range entries {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if strings.HasPrefix(name, ".") || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		if len(out) == MaxFiles {
			out = append(out, File{File: name, Err: fmt.Sprintf("mehr als %d dateien, rest ignoriert", MaxFiles)})
			break
		}
		out = append(out, loadFile(filepath.Join(dir, name)))
	}
	// Namen müssen über alle Dateien eindeutig sein.
	for i := range out {
		if out[i].Playbook == nil {
			continue
		}
		for j := range out {
			if i != j && out[j].Playbook != nil && out[j].Playbook.Name == out[i].Playbook.Name {
				out[i].Err = join(out[i].Err, "name auch in "+out[j].File+" vergeben")
			}
		}
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.File, b.File) })
	return out, nil
}

func loadFile(path string) File {
	f := File{File: filepath.Base(path)}
	fh, err := os.Open(path)
	if err != nil {
		f.Err = "lesen: " + err.Error()
		return f
	}
	defer fh.Close()
	if st, err := fh.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Err = "keine normale datei"
		return f
	}
	data, err := io.ReadAll(io.LimitReader(fh, MaxFileSize+1))
	if err != nil {
		f.Err = "lesen: " + err.Error()
		return f
	}
	p, err := Parse(data)
	if err != nil {
		f.Err = err.Error()
		return f
	}
	f.Playbook = p
	if err := p.Validate(); err != nil {
		f.Err = err.Error()
	}
	return f
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}
