//go:build !windows

package commands

import "io"

// outputWriter: Unter Linux schreiben Programme UTF-8, nichts umzuwandeln.
func outputWriter(w io.Writer) io.Writer { return w }
