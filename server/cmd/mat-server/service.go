package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/logfile"
	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/winsvc"
)

const serviceName = "mat-server"

const serviceUsage = `Windows-Dienst verwalten (als Administrator ausführen):
  mat-server service install -env-file <datei> [-import-db <pfad>]
  mat-server service uninstall [-purge]
  mat-server service start|stop|status

install kopiert die env-Datei nach %ProgramData%\mat-server\server.env (nur für
SYSTEM und Administratoren lesbar) und das Programm nach %ProgramFiles%\mat-server.
Die Datenbank liegt dort als mat.db, sofern die env-Datei kein MAT_DB setzt;
-import-db übernimmt eine bestehende Datenbank (Server vorher beenden).
Erneutes install aktualisiert Programm und env-Datei.

Benutzer anlegen (Administrator-Konsole):
  mat-server user add <name> -env-file %ProgramData%\mat-server\server.env`

// restrict schützt Dateien mit Zugangsdaten; im Test austauschbar.
var restrict = winsvc.RestrictToAdmins

func runService(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, serviceUsage)
		return errors.New("unterbefehl fehlt")
	}
	if runtime.GOOS != "windows" {
		return errors.New("nur unter windows – sonst docker/kubernetes (siehe deploy/)")
	}
	switch args[0] {
	case "install":
		return installService(args[1:])
	case "uninstall":
		return uninstallService(args[1:])
	case "start":
		return report(winsvc.Start(serviceName), "dienst gestartet")
	case "stop":
		return report(winsvc.Stop(serviceName), "dienst gestoppt")
	case "status":
		st, err := winsvc.Status(serviceName)
		return report(err, st)
	case "-h", "-help", "--help", "help":
		fmt.Println(serviceUsage)
		return nil
	}
	fmt.Fprintln(os.Stderr, serviceUsage)
	return fmt.Errorf("unbekannter unterbefehl %q", args[0])
}

func report(err error, msg string) error {
	if err == nil {
		fmt.Println(msg)
	}
	return err
}

func installService(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	importDB := fs.String("import-db", "", "bestehende Datenbank übernehmen (z. B. data\\mat.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !winsvc.IsAdmin() {
		return errors.New("keine berechtigung – in einer administrator-konsole ausführen")
	}

	dataDir := winsvc.DataDir(serviceName)
	envPath := filepath.Join(dataDir, "server.env")
	dbPath := filepath.Join(dataDir, "mat.db")
	if err := prepareData(dataDir, envPath, dbPath, *importDB); err != nil {
		return err
	}

	if err := winsvc.Stop(serviceName); err != nil && !errors.Is(err, winsvc.ErrNotInstalled) {
		return err
	}
	exe, err := winsvc.InstallBinary(winsvc.ProgramDir(serviceName), serviceName)
	if err != nil {
		return err
	}
	svcArgs := []string{"-env-file", envPath}
	if os.Getenv("MAT_DB") == "" {
		svcArgs = append(svcArgs, "-db", dbPath)
	}
	err = winsvc.Install(winsvc.Spec{
		Name:        serviceName,
		DisplayName: "MAT Server (Monitoring and Administration Tool)",
		Description: "Nimmt Metriken der Agenten an und liefert die Web-GUI aus.",
		ExePath:     exe,
		Args:        svcArgs,
	})
	if err != nil {
		return err
	}
	if err := winsvc.Start(serviceName); err != nil {
		return err
	}
	fmt.Printf("dienst %s eingerichtet und gestartet\n  programm:  %s\n  env-datei: %s\n  log:       %s\n",
		serviceName, exe, envPath, filepath.Join(dataDir, "server.log"))
	return nil
}

// prepareData legt das Datenverzeichnis an, übernimmt env-Datei und ggf. die
// Datenbank und prüft die Einstellungen, die der Dienst braucht.
func prepareData(dataDir, envPath, dbPath, importDB string) error {
	if envFilePath == "" && !fileExists(envPath) {
		return errors.New("-env-file angeben (mindestens MAT_AGENT_TOKEN)")
	}
	// Die Variablen sind bereits aus -env-file geladen (applyEnvFile).
	if len(os.Getenv("MAT_AGENT_TOKEN")) < 8 {
		return errors.New("MAT_AGENT_TOKEN fehlt in der env-datei (mindestens 8 zeichen)")
	}
	// Der Dienst läuft mit anderem Arbeitsverzeichnis: Pfade müssen absolut sein.
	for _, k := range []string{"MAT_DB", "MAT_TLS_CERT", "MAT_TLS_KEY"} {
		if v := os.Getenv(k); v != "" && !filepath.IsAbs(v) {
			return fmt.Errorf("%s muss ein absoluter pfad sein: %s", k, v)
		}
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("datenverzeichnis anlegen: %w", err)
	}
	if err := restrict(dataDir); err != nil {
		return err
	}
	if envFilePath != "" && !winsvc.SamePath(envFilePath, envPath) {
		if err := winsvc.CopyFile(envFilePath, envPath); err != nil {
			return fmt.Errorf("env-datei übernehmen: %w", err)
		}
	}
	if err := restrict(envPath); err != nil {
		return err
	}

	if importDB != "" {
		if fileExists(dbPath) {
			return fmt.Errorf("%s existiert bereits – nicht überschrieben", dbPath)
		}
		// Bei sauber beendetem Server ist die WAL-Datei leer oder fehlt; sonst mitnehmen.
		for _, suffix := range []string{"", "-wal"} {
			if src := importDB + suffix; fileExists(src) {
				if err := winsvc.CopyFile(src, dbPath+suffix); err != nil {
					return fmt.Errorf("datenbank übernehmen: %w", err)
				}
			}
		}
		fmt.Println("datenbank übernommen:", dbPath)
	}
	return nil
}

func uninstallService(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "auch env-datei, datenbank und log löschen")
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
		fmt.Println("dienst, programm und daten (inkl. datenbank) entfernt")
		return nil
	}
	fmt.Printf("dienst und programm entfernt; datenbank und env-datei liegen weiter in %s (löschen mit -purge)\n", dataDir)
	return nil
}

// runAsService läuft unter dem Windows-Dienst-Manager; das Log geht in eine Datei.
func runAsService(args []string) int {
	lf, err := logfile.Open(filepath.Join(winsvc.DataDir(serviceName), "server.log"), 10<<20)
	if err != nil {
		return 3
	}
	defer lf.Close()
	code := 0
	if err := winsvc.Run(serviceName, func(ctx context.Context) { code = serve(ctx, args, lf) }); err != nil {
		fmt.Fprintln(lf, "dienst:", err)
		return 1
	}
	return code
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
