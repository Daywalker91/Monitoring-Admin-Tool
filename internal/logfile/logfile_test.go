package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 Byte
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	// 2 Zeilen → Rotation, 2 Zeilen → Rotation, 1 Zeile aktuell.
	if len(cur) != 40 || len(old) != 80 {
		t.Fatalf("aktuell %d byte, alt %d byte", len(cur), len(old))
	}

	// Wiederöffnen hängt an und zählt die vorhandene Größe mit.
	w, _ = Open(path, 100)
	w.Write([]byte(line))
	w.Write([]byte(line))
	w.Close()
	cur, _ = os.ReadFile(path)
	if len(cur) != 40 {
		t.Fatalf("nach wiederöffnen: %d byte", len(cur))
	}
}
