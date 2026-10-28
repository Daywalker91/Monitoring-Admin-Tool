// Package logfile schreibt Logs in eine Datei mit einfacher Größenrotation.
// Gedacht für den Betrieb als Windows-Dienst, wo es kein Terminal gibt.
package logfile

import (
	"fmt"
	"os"
	"sync"
)

// Writer hängt an eine Datei an. Überschreitet sie maxBytes, wird sie zu
// <path>.1 umbenannt (eine ältere Kopie wird ersetzt) und neu begonnen.
type Writer struct {
	path     string
	maxBytes int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open öffnet bzw. erzeugt die Logdatei.
func Open(path string, maxBytes int64) (*Writer, error) {
	w := &Writer{path: path, maxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logdatei öffnen: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logdatei prüfen: %w", err)
	}
	w.f, w.size = f, fi.Size()
	return nil
}

// Write implementiert io.Writer.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate benennt die Datei um und beginnt neu. Schlägt das fehl, wird in die
// alte Datei weitergeschrieben, damit keine Logzeilen verloren gehen.
func (w *Writer) rotate() {
	w.f.Close()
	old := w.path + ".1"
	_ = os.Remove(old)
	_ = os.Rename(w.path, old)
	if err := w.open(); err != nil {
		w.f = nil
	}
}

// Close schließt die Datei.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
