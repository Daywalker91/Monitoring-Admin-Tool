package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/winsvc"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

const serviceName = "mat-agent"

// restrict schützt Dateien mit Zugangsdaten; im Test austauschbar.
var restrict = winsvc.RestrictToAdmins

const serviceUsage = `Windows-Dienst verwalten (als Administrator ausführen):
  mat-agent service install [agent-flags]   einrichten bzw. aktualisieren und starten
  mat-agent service uninstall [-purge]       entfernen (-purge löscht auch Konfiguration und Log)
  mat-agent service start|stop|status

install übernimmt die normalen Agent-Flags (-server, -token, -ca, -id, -interval …)
und speichert sie in %ProgramData%\mat-agent\agent.json (nur für SYSTEM und
Administratoren lesbar). Ohne -server wird eine vorhandene Konfiguration
übernommen; einzelne Flags überschreiben sie. Das Programm wird nach
%ProgramFiles%\mat-agent kopiert.

Nur bei install: -jobs services,packages,reboot und -log-sources eventlog:System,…
schalten Admin-Aktionen frei (leer = keine); ohne die Flags bleiben die
Freischaltungen der vorhandenen Konfiguration.

Unter Linux übernimmt deploy/install-agent.sh diese Aufgabe (systemd).`

func runService(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, serviceUsage)
		return errors.New("unterbefehl fehlt")
	}
	if runtime.GOOS != "windows" {
		return errors.New("nur unter windows – unter linux deploy/install-agent.sh verwenden")
	}
	switch args[0] {
	case "install":
		return installService(args[1:])
	case "uninstall":
		return uninstallService(args[1:])
	case "start":
		if err := winsvc.Start(serviceName); err != nil {
			return err
		}
		fmt.Println("dienst gestartet")
		return nil
	case "stop":
		if err := winsvc.Stop(serviceName); err != nil {
			return err
		}
		fmt.Println("dienst gestoppt")
		return nil
	case "status":
		st, err := winsvc.Status(serviceName)
		if err != nil {
			return err
		}
		fmt.Println(st)
		return nil
	case "-h", "-help", "--help", "help":
		fmt.Println(serviceUsage)
		return nil
	}
	fmt.Fprintln(os.Stderr, serviceUsage)
	return fmt.Errorf("unbekannter unterbefehl %q", args[0])
}

func installService(args []string) error {
	if !winsvc.IsAdmin() {
		return errors.New("keine berechtigung – in einer administrator-konsole ausführen")
	}
	args, jobs, jobsSet := cutListFlag(args, "jobs")
	args, sources, sourcesSet := cutListFlag(args, "log-sources")
	cfg, err := config.Load(args) // prüft die Konfiguration vollständig
	if err != nil {
		return fmt.Errorf("konfiguration: %w", err)
	}
	if jobsSet {
		cfg.Jobs = nil
		for _, j := range jobs {
			cfg.Jobs = append(cfg.Jobs, proto.Capability(j))
		}
	}
	if sourcesSet {
		cfg.LogSources = sources
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("konfiguration: %w", err)
	}

	dataDir := winsvc.DataDir(serviceName)
	cfgPath := filepath.Join(dataDir, "agent.json")
	if err := prepareConfig(&cfg, dataDir, cfgPath); err != nil {
		return err
	}

	// Laufenden Dienst vor dem Austausch des Programms stoppen.
	if err := winsvc.Stop(serviceName); err != nil && !errors.Is(err, winsvc.ErrNotInstalled) {
		return err
	}
	progDir := winsvc.ProgramDir(serviceName)
	exe, err := winsvc.InstallBinary(progDir, serviceName)
	if err != nil {
		return err
	}

	err = winsvc.Install(winsvc.Spec{
		Name:        serviceName,
		DisplayName: "MAT Agent (Monitoring and Administration Tool)",
		Description: "Sendet Systemmetriken an den Server des Monitoring and Administration Tool und führt freigegebene Admin-Jobs aus.",
		ExePath:     exe,
		Args:        []string{"-config", cfgPath},
	})
	if err != nil {
		return err
	}
	if err := winsvc.Start(serviceName); err != nil {
		return err
	}
	fmt.Printf("dienst %s eingerichtet und gestartet\n  programm:      %s\n  konfiguration: %s\n  log:           %s\n",
		serviceName, exe, cfgPath, filepath.Join(dataDir, "agent.log"))
	return nil
}

// prepareConfig legt das Datenverzeichnis an, übernimmt die CA-Datei und
// schreibt die Konfiguration mit Zugriff nur für SYSTEM und Administratoren.
// cutListFlag entfernt -name/--name (mit "=wert" oder als nächstes Argument)
// aus args und liefert die kommagetrennten Werte. Diese Freischaltungen gibt
// es nur bei "service install", nicht beim normalen Start.
func cutListFlag(args []string, name string) (rest, values []string, set bool) {
	for i := 0; i < len(args); i++ {
		a := strings.TrimPrefix(strings.TrimPrefix(args[i], "-"), "-")
		v, hasValue := "", false
		switch {
		case a == name && i+1 < len(args):
			v, hasValue = args[i+1], true
			i++
		case strings.HasPrefix(a, name+"="):
			v, hasValue = strings.TrimPrefix(a, name+"="), true
		}
		if !hasValue {
			rest = append(rest, args[i])
			continue
		}
		set = true
		values = nil
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				values = append(values, part)
			}
		}
	}
	return rest, values, set
}

func prepareConfig(cfg *config.Config, dataDir, cfgPath string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("datenverzeichnis anlegen: %w", err)
	}
	if err := restrict(dataDir); err != nil {
		return err
	}
	// Der Dienst läuft als SYSTEM mit anderem Arbeitsverzeichnis: die CA-Datei
	// wird deshalb ins Datenverzeichnis kopiert.
	if cfg.CAFile != "" {
		target := filepath.Join(dataDir, "server.crt")
		if !winsvc.SamePath(cfg.CAFile, target) {
			if err := winsvc.CopyFile(cfg.CAFile, target); err != nil {
				return fmt.Errorf("ca-datei übernehmen: %w", err)
			}
		}
		cfg.CAFile = target
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("konfiguration schreiben: %w", err)
	}
	return restrict(cfgPath)
}

func uninstallService(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "auch Konfiguration und Log löschen")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := winsvc.Uninstall(serviceName); err != nil && !errors.Is(err, winsvc.ErrNotInstalled) {
		return err
	}
	if err := os.RemoveAll(winsvc.ProgramDir(serviceName)); err != nil {
		return fmt.Errorf("programmverzeichnis löschen: %w", err)
	}
	dataDir := winsvc.DataDir(serviceName)
	if *purge {
		if err := os.RemoveAll(dataDir); err != nil {
			return fmt.Errorf("datenverzeichnis löschen: %w", err)
		}
		fmt.Println("dienst, programm, konfiguration und log entfernt")
		return nil
	}
	fmt.Printf("dienst und programm entfernt; konfiguration und log liegen weiter in %s (löschen mit -purge)\n", dataDir)
	return nil
}
