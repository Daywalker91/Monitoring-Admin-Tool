package packages

import (
	"os"
	"path/filepath"
)

func platformManager(run runFunc) *manager {
	base := os.Getenv("ProgramFiles")
	if base == "" {
		base = `C:\Program Files`
	}
	paths, _ := filepath.Glob(filepath.Join(base, wingetGlob))
	path := newestWinget(paths)
	if path == "" || !wingetWorks(run, path) {
		return nil
	}
	return wingetManager(path)
}
