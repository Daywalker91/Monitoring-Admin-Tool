// mat-agent sammelt Systemmetriken und sendet sie per WebSocket an den Server.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/client"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/collector"
	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// version wird beim Build per -ldflags "-X main.version=..." gesetzt.
var version = "dev"

// defaultMemLimit ist das weiche Speicherlimit, falls GOMEMLIMIT nicht gesetzt ist.
const defaultMemLimit = 20 << 20 // 20 MiB

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "konfiguration:", err)
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))

	// Weiches Speicherlimit: der GC arbeitet aggressiver, je näher der Heap daran kommt.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemLimit)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hello := collector.HostInfo(ctx)
	hello.AgentVersion = version
	hello.IntervalSec = cfg.IntervalSec
	hello.AgentID = cfg.AgentID
	if hello.AgentID == "" {
		hello.AgentID = config.SanitizeID(hello.Hostname)
	}
	// In M1 nur Metriken; weitere Fähigkeiten kommen mit dem Executor.
	hello.Capabilities = []proto.Capability{proto.CapMetrics}
	if err := hello.Validate(); err != nil {
		log.Error("ungültige agent-angaben", "err", err)
		os.Exit(2)
	}

	if cfg.Insecure() {
		log.Warn("unverschlüsselte verbindung (ws://) zu entferntem server – für produktiven einsatz wss:// verwenden")
	}
	log.Info("agent startet", "version", version, "agent_id", hello.AgentID,
		"interval_sec", cfg.IntervalSec, "process_interval_sec", cfg.ProcessIntervalSec)

	col := collector.New()
	cl, err := client.New(cfg, hello, col, log)
	if err != nil {
		log.Error("tls-konfiguration", "err", err)
		os.Exit(2)
	}
	col.Prime(ctx)
	cl.Run(ctx)
	log.Info("agent beendet")
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
