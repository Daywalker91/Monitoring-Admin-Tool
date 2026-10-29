// Package progress reicht die Ausgabe eines laufenden Jobs an den Client
// weiter, der sie als Zwischenstand an den Server schickt. Jobs schreiben über
// Writer(ctx); ohne Zwischenstand (z. B. bei Abfragen) ist das io.Discard.
package progress

import (
	"context"
	"io"
	"sync"
	"unicode/utf8"
)

type ctxKey struct{}

// With hängt w als Ziel für Zwischenausgaben an ctx.
func With(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, ctxKey{}, w)
}

// Writer liefert das Ziel für Zwischenausgaben (io.Discard, wenn keins gesetzt).
func Writer(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(ctxKey{}).(io.Writer); ok {
		return w
	}
	return io.Discard
}

// Buffer sammelt Ausgabe, bis sie gesendet ist. Ist die Verbindung länger weg,
// bleibt nur das Ende (höchstens max Bytes) erhalten.
type Buffer struct {
	mu    sync.Mutex
	data  []byte
	start int64 // Position von data[0] in der gesamten Ausgabe
	max   int
}

// NewBuffer erzeugt einen Buffer, der höchstens max Bytes vorhält.
func NewBuffer(max int) *Buffer { return &Buffer{max: max} }

// Write hängt p an; zu viel Ungesendetes wird vorne abgeschnitten.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if over := len(b.data) - b.max; over > 0 {
		b.data = append(b.data[:0], b.data[over:]...)
		b.start += int64(over)
	}
	return len(p), nil
}

// Peek liefert höchstens n Bytes vom Anfang, ohne ein UTF-8-Zeichen zu
// zerteilen, und die Position danach. Erst Drop entfernt sie, damit nach einem
// Sendefehler nichts fehlt.
func (b *Buffer) Peek(n int) (data []byte, end int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := len(b.data)
	if cut > n {
		cut = n
		for cut > n-utf8.UTFMax && cut > 0 && !utf8.RuneStart(b.data[cut]) {
			cut--
		}
	}
	return append([]byte(nil), b.data[:cut]...), b.start + int64(cut)
}

// Drop entfernt alles vor der Position end (aus Peek).
func (b *Buffer) Drop(end int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := int(min(max(end-b.start, 0), int64(len(b.data))))
	b.data = append(b.data[:0], b.data[n:]...)
	b.start += int64(n)
}
