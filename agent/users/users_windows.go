package users

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxWindowsName ist die Grenze für lokale Konten unter Windows.
const maxWindowsName = 20

// netExe liegt fest im System-Verzeichnis (nicht über PATH suchen).
func netExe() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "net.exe")
}

func supported(s system) bool { return s.exists(netExe()) }

// create legt ein deaktiviertes Konto ohne Passwort an. Aktivieren und ein
// Passwort setzen muss ein Administrator am Host; verlangt die Richtlinie ein
// Passwort, schlägt das Anlegen fehl.
func create(ctx context.Context, s system, w io.Writer, name string, sudo bool) error {
	if sudo {
		return errors.New("sudo gibt es unter windows nicht, nichts geändert")
	}
	if len(name) > maxWindowsName {
		return fmt.Errorf("unter windows höchstens %d zeichen, nichts geändert", maxWindowsName)
	}
	if err := s.run(ctx, w, netExe(), "user", name, "/add", "/active:no"); err != nil {
		return fmt.Errorf("net user: %w", err)
	}
	fmt.Fprintf(w, "Konto %s angelegt (deaktiviert, ohne Passwort). Aktivieren muss es ein Administrator am Host.\n", name)
	return nil
}
