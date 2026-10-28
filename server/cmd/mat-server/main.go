// mat-server nimmt Metriken der Agenten an, speichert sie und liefert die Web-GUI aus.
//
// Aufruf:
//
//	mat-server [flags]                     Server starten
//	mat-server user add|passwd <name>      Benutzer anlegen bzw. Passwort ändern
//	mat-server user list                   Benutzer auflisten
//	mat-server user del <name>             Benutzer löschen
//	mat-server gen-cert -hosts <ip,name>   selbst signiertes TLS-Zertifikat erzeugen
//	mat-server version                     Version ausgeben
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/api"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/auth"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/store"
	"github.com/Daywalker91/Monitoring-Admin-Tool/web"
)

// version wird beim Build per -ldflags "-X main.version=..." gesetzt.
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version)
			return
		case "user":
			if err := runUser(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "fehler:", err)
				os.Exit(1)
			}
			return
		case "gen-cert":
			if err := runGenCert(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "fehler:", err)
				os.Exit(1)
			}
			return
		}
	}

	listen := flag.String("listen", envOr("MAT_LISTEN", ":8080"), "Adresse für HTTP (GUI, API, Agenten)")
	dbPath := flag.String("db", envOr("MAT_DB", "data/mat.db"), "Pfad zur SQLite-Datenbank")
	retention := flag.Duration("retention", 7*24*time.Hour, "Aufbewahrungsdauer der Historie")
	logLevel := flag.String("log-level", envOr("MAT_LOG_LEVEL", "info"), "Log-Level: debug, info, warn, error")
	tlsCert := flag.String("tls-cert", os.Getenv("MAT_TLS_CERT"), "TLS-Zertifikat (PEM); mit -tls-key → HTTPS/wss")
	tlsKey := flag.String("tls-key", os.Getenv("MAT_TLS_KEY"), "TLS-Schlüssel (PEM)")
	flag.Parse()

	if (*tlsCert == "") != (*tlsKey == "") {
		fmt.Fprintln(os.Stderr, "-tls-cert und -tls-key müssen zusammen angegeben werden")
		os.Exit(2)
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(*logLevel))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Das Token kommt bewusst nur aus der Umgebung, damit es nicht in der Prozessliste steht.
	token := os.Getenv("MAT_AGENT_TOKEN")
	if len(token) < 8 {
		fmt.Fprintln(os.Stderr, "MAT_AGENT_TOKEN muss gesetzt sein (mindestens 8 Zeichen)")
		os.Exit(2)
	}

	// Optional: Token für /metrics (Prometheus). Ohne Token ist der Endpunkt aus.
	metricsToken := os.Getenv("MAT_METRICS_TOKEN")
	if metricsToken != "" && len(metricsToken) < 16 {
		fmt.Fprintln(os.Stderr, "MAT_METRICS_TOKEN muss mindestens 16 Zeichen lang sein")
		os.Exit(2)
	}

	var tlsCfg *tls.Config
	if *tlsCert != "" {
		pair, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tls-zertifikat laden:", err)
			os.Exit(2)
		}
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}

	if err := run(*listen, *dbPath, *retention, token, metricsToken, tlsCfg, log); err != nil {
		log.Error("server beendet mit fehler", "err", err)
		os.Exit(1)
	}
}

func run(listen, dbPath string, retention time.Duration, token, metricsToken string, tlsCfg *tls.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if users, err := st.Users(ctx); err == nil && len(users) == 0 {
		log.Warn("noch kein benutzer für die gui angelegt – anlegen mit: mat-server user add <name>", "db", dbPath)
	}

	go api.RunRetention(ctx, st, retention, log)

	srv := &http.Server{
		Addr:              listen,
		Handler:           api.New(st, auth.New(st), token, web.Static(), log).WithMetricsToken(metricsToken).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
		// Anfrage-Kontexte hängen am Signal-Kontext, damit offene WebSockets beim Beenden schließen.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("server startet", "version", version, "listen", listen, "db", dbPath, "retention", retention,
			"metrics", metricsToken != "", "tls", tlsCfg != nil)
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
