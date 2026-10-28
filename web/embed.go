// Package web bettet die Frontend-Dateien in das Server-Binary ein.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Static liefert die Frontend-Dateien mit static/ als Wurzel.
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // kann nur bei fehlerhaftem Embed-Pfad passieren
	}
	return sub
}
