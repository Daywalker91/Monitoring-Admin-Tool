// mat-server nimmt Metriken der Agenten an, speichert sie und liefert die Web-GUI aus.
//
// Aufruf:
//
//	mat-server [flags]                     Server starten
//	mat-server user add|passwd <name>      Benutzer anlegen bzw. Passwort ändern
//	mat-server user list                   Benutzer auflisten
//	mat-server user del <name>             Benutzer löschen
//	mat-server gen-cert -hosts <ip,name>   selbst signiertes TLS-Zertifikat erzeugen
//	mat-server service install|uninstall|start|stop|status   Windows-Dienst verwalten
//
// Alle Befehle verstehen -env-file <datei> (MAT_*-Variablen als KEY=VALUE).
//
//	mat-server release keygen|sign|verify  signierte Agent-Releases (Self-Update)
//	mat-server version                     Version ausgeben
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/winsvc"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/api"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/checks"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/k8s"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/update"
	"github.com/Daywalker91/Monitoring-Admin-Tool/web"
)

// version wird beim Build per -ldflags "-X main.version=..." gesetzt.
var version = "dev"

func main() {
	// -env-file gilt für alle Befehle (auch user) und wird vor den Flags ausgewertet,
	// weil die Flag-Standardwerte aus der Umgebung kommen.
	args, err := applyEnvFile(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fehler:", err)
		os.Exit(2)
	}
	commands := map[string]func([]string) error{
		"version":  func([]string) error { fmt.Println(version); return nil },
		"user":     runUser,
		"gen-cert": runGenCert,
		"service":  runService,
		"release":  runRelease,
	}
	if len(args) > 0 {
		if cmd, ok := commands[args[0]]; ok {
			if err := cmd(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "fehler:", err)
				os.Exit(1)
			}
			return
		}
	}

	if winsvc.IsService() {
		os.Exit(runAsService(args))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(serve(ctx, args, os.Stderr))
}

// serve liest Flags und Umgebung und betreibt den Server, bis ctx endet.
// Fehler gehen nach out (Terminal bzw. Logdatei im Dienstbetrieb).
func serve(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("mat-server", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", envOr("MAT_LISTEN", ":8080"), "Adresse für HTTP (GUI, API, Agenten)")
	dbPath := fs.String("db", envOr("MAT_DB", "data/mat.db"), "Pfad zur SQLite-Datenbank")
	retention := fs.Duration("retention", 7*24*time.Hour, "Aufbewahrungsdauer der Historie")
	logLevel := fs.String("log-level", envOr("MAT_LOG_LEVEL", "info"), "Log-Level: debug, info, warn, error")
	tlsCert := fs.String("tls-cert", os.Getenv("MAT_TLS_CERT"), "TLS-Zertifikat (PEM); mit -tls-key → HTTPS/wss")
	tlsKey := fs.String("tls-key", os.Getenv("MAT_TLS_KEY"), "TLS-Schlüssel (PEM)")
	proxies := fs.String("trusted-proxies", os.Getenv("MAT_TRUSTED_PROXIES"),
		"Reverse-Proxies (IPs/Netze, Komma), deren X-Forwarded-For/-Proto gelten, z. B. 10.42.0.0/16")
	fs.String("env-file", "", "Datei mit MAT_*-Variablen (KEY=VALUE); gesetzte Umgebungsvariablen haben Vorrang")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(msg ...any) int {
		fmt.Fprintln(out, msg...)
		return 2
	}

	if (*tlsCert == "") != (*tlsKey == "") {
		return fail("-tls-cert und -tls-key müssen zusammen angegeben werden")
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(*logLevel))
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))

	// Das Token kommt bewusst nur aus der Umgebung, damit es nicht in der Prozessliste steht.
	token := os.Getenv("MAT_AGENT_TOKEN")
	if len(token) < 8 {
		return fail("MAT_AGENT_TOKEN muss gesetzt sein (mindestens 8 Zeichen)")
	}

	// Optional: Token für /metrics (Prometheus). Ohne Token ist der Endpunkt aus.
	metricsToken := os.Getenv("MAT_METRICS_TOKEN")
	if metricsToken != "" && len(metricsToken) < 16 {
		return fail("MAT_METRICS_TOKEN muss mindestens 16 Zeichen lang sein")
	}

	// Benachrichtigungskanäle (ntfy, E-Mail) nur aus der Umgebung.
	chans, warnings, err := notify.FromEnv(os.Getenv)
	if err != nil {
		return fail("benachrichtigungen:", err)
	}
	for _, w := range warnings {
		log.Warn(w)
	}

	trusted, err := api.ParseTrustedProxies(*proxies)
	if err != nil {
		return fail("-trusted-proxies:", err)
	}

	var tlsCfg *tls.Config
	if *tlsCert != "" {
		pair, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return fail("tls-zertifikat laden:", err)
		}
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}

	// Self-Update der Agenten: Releases von GitHub (MAT_UPDATE_REPO=off schaltet ab).
	// Zwischenspeicher neben der Datenbank, im Container also auf dem Volume.
	updates := update.New(os.Getenv("MAT_UPDATE_REPO"), os.Getenv("MAT_UPDATE_API"),
		filepath.Join(filepath.Dir(*dbPath), "updates"), log)

	if err := run(ctx, *listen, *dbPath, *retention, token, metricsToken, tlsCfg, chans, trusted, updates, log); err != nil {
		log.Error("server beendet mit fehler", "err", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, listen, dbPath string, retention time.Duration, token, metricsToken string, tlsCfg *tls.Config,
	chans []notify.Channel, trusted []netip.Prefix, updates *update.Checker, log *slog.Logger) error {

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if users, err := st.Users(ctx); err == nil && len(users) == 0 {
		log.Warn("noch kein benutzer für die gui angelegt – anlegen mit: mat-server user add <name>", "db", dbPath)
	}

	go api.RunRetention(ctx, st, retention, log)

	dispatcher := notify.NewDispatcher(log, chans...)
	go dispatcher.Run(ctx)
	evaluator, err := alert.New(ctx, st, api.NotifyEvent(dispatcher), log, time.Now())
	if err != nil {
		return err
	}
	var apiSrv *api.Server
	checkRunner := checks.NewRunner(st, log, func(ctx context.Context, c store.Check, r store.CheckResult) {
		apiSrv.CheckResult(ctx, c, r)
	})
	if err := checkRunner.Load(ctx); err != nil {
		return fmt.Errorf("checks laden: %w", err)
	}
	apiSrv = api.New(st, auth.New(st), token, web.Static(), log).
		WithMetricsToken(metricsToken).
		WithAlerts(evaluator, dispatcher).
		WithTrustedProxies(trusted).
		WithVersion(version).
		WithUpdates(updates).
		WithChecks(checkRunner)
	if kc, err := k8s.FromEnv(); err != nil {
		log.Warn("k3s-ansicht abgeschaltet", "err", err)
	} else if kc != nil {
		watcher := k8s.NewWatcher(kc, log, apiSrv.ClusterSnapshot)
		apiSrv.WithCluster(watcher)
		go watcher.Run(ctx)
		log.Info("k3s-ansicht aktiv")
	}
	if tlsCfg != nil && tlsCfg.Certificates[0].Leaf != nil {
		apiSrv.WithCertificate(tlsCfg.Certificates[0].Leaf)
	}
	go apiSrv.RunAlerts(ctx)
	go apiSrv.RunCertCheck(ctx)
	go updates.Run(ctx)
	go checkRunner.Run(ctx)

	srv := &http.Server{
		Addr:              listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
		// Anfrage-Kontexte hängen am Kontext von run, damit offene WebSockets beim Beenden schließen.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server startet", "version", version, "listen", listen, "db", dbPath, "retention", retention,
			"metrics", metricsToken != "", "tls", tlsCfg != nil, "notify", dispatcher.Channels(), "trusted_proxies", len(trusted),
			"self_update", updates.Status().Repo)
		if tlsCfg != nil {
			errCh <- srv.ListenAndServeTLS("", "") // Zertifikat steckt bereits in TLSConfig
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("server wird beendet")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
