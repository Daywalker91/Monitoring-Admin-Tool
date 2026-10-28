package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envFilePath ist die mit -env-file angegebene Datei (für "service install").
var envFilePath string

var envKey = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// applyEnvFile entfernt -env-file <datei> (bzw. -env-file=<datei>) aus args und
// setzt die Variablen der Datei. Bereits gesetzte Umgebungsvariablen haben
// Vorrang, damit sich einzelne Werte beim Start überschreiben lassen.
func applyEnvFile(args []string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "env-file" {
			rest = append(rest, a)
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("-env-file: datei fehlt")
			}
			i++
			value = args[i]
		}
		envFilePath = value
	}
	if envFilePath == "" {
		return rest, nil
	}
	vars, err := readEnvFile(envFilePath)
	if err != nil {
		return nil, err
	}
	for k, v := range vars {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return rest, nil
}

// readEnvFile liest KEY=VALUE-Zeilen. Leere Zeilen und Zeilen mit # werden
// übersprungen, umschließende Anführungszeichen entfernt. Es gibt bewusst keine
// Variablenersetzung oder Escape-Sequenzen.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("env-datei öffnen: %w", err)
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // BOM von Windows-Editoren
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || !envKey.MatchString(k) {
			return nil, fmt.Errorf("%s:%d: erwartet KEY=VALUE", path, n)
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vars[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("env-datei lesen: %w", err)
	}
	return vars, nil
}
