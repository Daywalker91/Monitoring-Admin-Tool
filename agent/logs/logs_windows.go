package logs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// wevtutil liegt fest im System-Verzeichnis (nicht über PATH suchen).
func wevtutil() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "wevtutil.exe")
}

// cpACP ist die ANSI-Codepage des Systems (fehlt in x/sys/windows).
const cpACP = 0

// fromANSI wandelt die Ausgabe von wevtutil (ANSI-Codepage, z. B. 1252) in
// UTF-8 um und entfernt NUL-Bytes und Wagenrückläufe.
func fromANSI(b []byte) string {
	b = bytes.ReplaceAll(b, []byte{0}, nil)
	if len(b) == 0 {
		return ""
	}
	n, err := windows.MultiByteToWideChar(cpACP, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n == 0 {
		return string(b)
	}
	w := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cpACP, 0, &b[0], int32(len(b)), &w[0], n); err != nil {
		return string(b)
	}
	return strings.ReplaceAll(string(utf16.Decode(w)), "\r\n", "\n")
}

func supported() bool {
	_, err := os.Stat(wevtutil())
	return err == nil
}

func platformKind(source string) bool { return strings.HasPrefix(source, "eventlog:") }

// read liefert die neuesten Ereignisse zuerst (/rd:true); bei zu langer
// Ausgabe bleibt deshalb der Anfang.
func read(ctx context.Context, source string, lines int) (string, error) {
	name := strings.TrimPrefix(source, "eventlog:")
	out, err := exec.CommandContext(ctx, wevtutil(), "qe", name, "/c:"+strconv.Itoa(lines), "/rd:true", "/f:text").CombinedOutput()
	text := proto.Truncate(fromANSI(out), proto.MaxJobOutputLen)
	if err != nil {
		return text, fmt.Errorf("wevtutil: %w", err)
	}
	return text, nil
}
