package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// BuildManifest erstellt das Manifest für alle Agent-Binaries in dir
// (Dateinamen wie von build.ps1, z. B. mat-agent-linux-arm64).
func BuildManifest(version, dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("verzeichnis lesen: %w", err)
	}
	m := Manifest{Version: version}
	for _, e := range entries {
		goos, goarch, ok := platformOf(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		sum, size, err := hashFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m.Files = append(m.Files, File{Name: e.Name(), OS: goos, Arch: goarch, Size: size, SHA256: sum})
	}
	if len(m.Files) == 0 {
		return nil, errors.New("keine agent-binaries gefunden")
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	if err := m.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("manifest serialisieren: %w", err)
	}
	return append(data, '\n'), nil
}

// hashFile liefert SHA-256 (hex) und Größe einer Datei.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("datei öffnen: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("datei lesen: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// CheckFile prüft Größe und SHA-256 einer Datei gegen den Manifest-Eintrag.
func CheckFile(path string, f File) error {
	sum, size, err := hashFile(path)
	if err != nil {
		return err
	}
	if size != f.Size || sum != f.SHA256 {
		return fmt.Errorf("%s: prüfsumme oder größe stimmt nicht", f.Name)
	}
	return nil
}
