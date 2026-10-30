package commands

import (
	"bytes"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

// cpOEMCP ist die OEM-Codepage, in der Konsolenprogramme ohne Konsole schreiben
// (z. B. 850 bei deutschem Windows).
const cpOEMCP = 1

// outputWriter wandelt die Ausgabe nach UTF-8: Was schon gültiges UTF-8 ist,
// bleibt; sonst gilt die OEM-Codepage. Zeilenenden werden zu "\n".
func outputWriter(w io.Writer) io.Writer { return oemWriter{w} }

type oemWriter struct{ w io.Writer }

func (o oemWriter) Write(p []byte) (int, error) {
	b := p
	if !utf8.Valid(b) {
		b = fromOEM(b)
	}
	if _, err := o.w.Write(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func fromOEM(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	n, err := windows.MultiByteToWideChar(cpOEMCP, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n == 0 {
		return b
	}
	w := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cpOEMCP, 0, &b[0], int32(len(b)), &w[0], n); err != nil {
		return b
	}
	return []byte(string(utf16.Decode(w)))
}
