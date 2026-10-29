package progress

import (
	"context"
	"io"
	"testing"
)

func TestWriter(t *testing.T) {
	if Writer(context.Background()) != io.Discard {
		t.Fatal("ohne ziel nicht io.Discard")
	}
	b := NewBuffer(10)
	if Writer(With(context.Background(), b)) != b {
		t.Fatal("ziel nicht übernommen")
	}
}

func TestBuffer(t *testing.T) {
	b := NewBuffer(10)
	b.Write([]byte("abcdef"))
	data, end := b.Peek(4)
	if string(data) != "abcd" || end != 4 {
		t.Fatalf("peek = %q, %d", data, end)
	}
	b.Drop(end)
	if data, _ := b.Peek(100); string(data) != "ef" {
		t.Fatalf("nach drop = %q", data)
	}

	// Überlauf zwischen Peek und Drop: Drop darf keine neuen Bytes entfernen.
	data, end = b.Peek(100) // "ef", end 6
	b.Write([]byte("0123456789X"))
	b.Drop(end)
	if data, _ := b.Peek(100); string(data) != "123456789X" {
		t.Fatalf("nach überlauf = %q", data)
	}
}

func TestBufferUTF8(t *testing.T) {
	b := NewBuffer(100)
	b.Write([]byte("aä")) // ä = 2 Bytes
	if data, end := b.Peek(2); string(data) != "a" || end != 1 {
		t.Fatalf("zeichen zerteilt: %q, %d", data, end)
	}
}
