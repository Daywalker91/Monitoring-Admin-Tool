// mat-agent sammelt Systemmetriken und sendet sie per WebSocket an den Server.
//
// Aufruf:
//
//	mat-agent [flags]                      im Vordergrund laufen
//	mat-agent service install [flags]      als Windows-Dienst einrichten
//	mat-agent service uninstall|start|stop|status
//	mat-agent check [flags]                Konfiguration prüfen, ohne zu verbinden
//	mat-agent version                      Version ausgeben
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"syscall"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/client"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/collector"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/commands"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/logs"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/packages"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/power"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/services"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/smart"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/update"
	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/logfile"
	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/winsvc"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// version wird beim Build per -ldflags "-X main.version=..." gesetzt.
var version = "dev"

// defaultMemLimit ist das weiche Speicherlimit, falls GOMEMLIMIT nicht gesetzt ist.
const defaultMemLimit = 20 << 20 // 20 MiB

// logMaxBytes begrenzt die Logdatei im Dienstbetrieb (eine ältere Kopie bleibt).
const logMaxBytes = 5 << 20

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version)
			return
		case "check":
			if err := runCheck(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "konfiguration:", err)
				os.Exit(2)
			}
			return
		case "service":
			if err := runService(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "fehler:", err)
				os.Exit(1)
			}
			return
		}
	}

	// Weiches Speicherlimit: der GC arbeitet aggressiver, je näher der Heap daran kommt.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemLimit)
	}

	if winsvc.IsService() {
		os.Exit(runAsService())
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "konfiguration:", err)
		os.Exit(2)
	}
	log := newLogger(os.Stderr, cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runAgent(ctx, cfg, log); err != nil {
		log.Error("agent kann nicht starten", "err", err)
		os.Exit(2)
	}
}

// runAsService läuft unter dem Windows-Dienst-Manager. Es gibt kein Terminal,
// deshalb geht das Log in eine Datei neben der Konfiguration.
func runAsService() int {
	logPath := filepath.Join(winsvc.DataDir(serviceName), "agent.log")
	lf, err := logfile.Open(logPath, logMaxBytes)
	if err != nil {
		return 3
	}
	defer lf.Close()

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		newLogger(lf, "info").Error("konfiguration", "err", err)
		return 2
	}
	log := newLogger(lf, cfg.LogLevel)
	var runErr error
	if err := winsvc.Run(serviceName, func(ctx context.Context) { runErr = runAgent(ctx, cfg, log) }); err != nil {
		log.Error("dienst", "err", err)
		return 1
	}
	if runErr != nil {
		log.Error("agent kann nicht starten", "err", runErr)
		return 2
	}
	return 0
}

// runAgent verbindet sich mit dem Server und sendet Metriken, bis ctx endet
// oder ein Self-Update einen Neustart verlangt. Danach beendet sich der Prozess;
// Dienst-Manager bzw. systemd starten ihn neu.
func runAgent(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	exe, selfUpdate := selfUpdatePath(log)
	var note string
	if selfUpdate {
		var restart bool
		if note, restart = update.Startup(exe, version, log); restart {
			log.Warn("agent beendet sich, damit das alte binary startet")
			return nil
		}
	}

	hello := collector.HostInfo(ctx)
	hello.AgentVersion = version
	hello.IntervalSec = cfg.IntervalSec
	hello.AgentID = cfg.AgentID
	if hello.AgentID == "" {
		hello.AgentID = config.SanitizeID(hello.Hostname)
	}
	hello.UpdateNote = note
	hello.Capabilities = []proto.Capability{proto.CapMetrics}
	if selfUpdate {
		hello.Capabilities = append(hello.Capabilities, proto.CapUpdate)
	}
	// Dienste liest der Agent immer; steuern nur mit Freischaltung in agent.json.
	svcs := services.New(serviceName)
	if svcs.Supported() && cfg.Allows(proto.CapServices) {
		hello.Capabilities = append(hello.Capabilities, proto.CapServices)
	}
	// Ebenso Update-Stand und Neustart-Hinweis; Upgrade und Neustart nur freigeschaltet.
	pkgs := packages.New(power.RebootRequired)
	if pkgs.Supported() && cfg.Allows(proto.CapPackages) {
		hello.Capabilities = append(hello.Capabilities, proto.CapPackages)
	}
	if power.Supported() && cfg.Allows(proto.CapReboot) {
		hello.Capabilities = append(hello.Capabilities, proto.CapReboot)
	}
	// Logs nur aus den freigegebenen Quellen, die es auf dieser Plattform gibt.
	logReader := logs.New(cfg.LogSources)
	if hello.LogSources = logReader.Sources(); len(hello.LogSources) > 0 {
		hello.Capabilities = append(hello.Capabilities, proto.CapLogs)
	}
	// Eigene Befehle nur aus der commands-Liste in agent.json.
	cmds := commands.New(cfg.Commands)
	if hello.Commands = cmds.Infos(); len(hello.Commands) > 0 {
		hello.Capabilities = append(hello.Capabilities, proto.CapCommands)
	}
	if err := hello.Validate(); err != nil {
		return fmt.Errorf("ungültige agent-angaben: %w", err)
	}

	if cfg.Insecure() {
		log.Warn("unverschlüsselte verbindung (ws://) zu entferntem server – für produktiven einsatz wss:// verwenden")
	}
	log.Info("agent startet", "version", version, "agent_id", hello.AgentID,
		"interval_sec", cfg.IntervalSec, "process_interval_sec", cfg.ProcessIntervalSec, "service", winsvc.IsService(),
		"capabilities", hello.Capabilities)

	col := collector.New()
	cl, err := client.New(cfg, hello, col, log)
	if err != nil {
		return fmt.Errorf("tls-konfiguration: %w", err)
	}
	if selfUpdate {
		base, err := update.BaseURL(cfg.ServerURL)
		if err != nil {
			return err
		}
		u := &update.Updater{Exe: exe, Version: version, BaseURL: base, Token: cfg.Token,
			HTTPClient: cl.HTTPClient(), Log: log}
		cl.HandleJob(proto.ActionAgentUpdate, func(ctx context.Context, p map[string]string) (string, bool, error) {
			out, err := u.Apply(ctx, p)
			return out, err == nil, err
		})
		cl.OnStable(func() { update.Confirm(exe, version, log) })
	}
	if svcs.Supported() {
		cl.WithServices(svcs.List)
		for _, a := range []string{proto.ActionServiceStart, proto.ActionServiceStop, proto.ActionServiceRestart} {
			cl.HandleJob(a, svcs.Job(a)) // ohne gemeldete Fähigkeit lehnt der Client ab
		}
	}
	cl.WithUpdates(pkgs.Status)
	cl.WithSmart(smart.New().Read)
	cl.HandleJob(proto.ActionPackagesRefresh, pkgs.Refresh)
	cl.HandleJob(proto.ActionPackagesUpgrade, pkgs.Upgrade)
	cl.HandleJob(proto.ActionReboot, power.Job(log))
	cl.HandleJob(proto.ActionLogRead, logReader.Job)
	cl.HandleJob(proto.ActionCommandRun, cmds.Run)
	col.Prime(ctx)
	if err := cl.Run(ctx); errors.Is(err, client.ErrRestart) {
		log.Info("agent startet für das update neu")
		return nil
	}
	log.Info("agent beendet")
	return nil
}

// selfUpdatePath liefert den Pfad des eigenen Binaries und ob Self-Update
// möglich ist: nur mit eingebautem Signaturschlüssel und nur, wenn ein
// Dienst-Manager den Agenten nach dem Austausch neu startet (Windows-Dienst
// oder systemd, erkennbar an INVOCATION_ID).
func selfUpdatePath(log *slog.Logger) (string, bool) {
	if !release.Enabled() || (!winsvc.IsService() && os.Getenv("INVOCATION_ID") == "") {
		return "", false
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		log.Warn("self-update aus: eigener pfad unbekannt", "err", err)
		return "", false
	}
	return exe, true
}

// runCheck lädt die Konfiguration samt CA-Datei und gibt sie ohne Token aus.
func runCheck(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	if _, err := cfg.TLSConfig(); err != nil {
		return err
	}
	id := cfg.AgentID
	if id == "" {
		id = "(hostname)"
	}
	fmt.Printf("ok: server=%s agent_id=%s interval=%ds ca_file=%q jobs=%v commands=%v\n",
		cfg.ServerURL, id, cfg.IntervalSec, cfg.CAFile, cfg.Jobs, commandNames(cfg))
	if cfg.Insecure() {
		fmt.Println("warnung: unverschlüsselte verbindung (ws://) zu entferntem server")
	}
	return nil
}

func newLogger(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: parseLevel(level)}))
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// commandNames liefert die Namen der freigegebenen Befehle, sortiert.
func commandNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Commands))
	for n := range cfg.Commands {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
