# CLAUDE.md

Diese Datei enthält Hinweise für Claude Code (claude.ai/code) beim Arbeiten mit diesem Repository.

## Projektübersicht

Selbst gebautes Monitoring- und Administrationstool für das eigene Homelab.
Ein schlanker **Agent** läuft auf den überwachten Maschinen, ein zentraler **Server** mit Web-GUI
sammelt die Daten und steuert (später) Admin-Aktionen.

Es wird in Meilensteinen gearbeitet. Alles, was zu keinem beauftragten Meilenstein gehört, wird nur vorbereitet, nicht umgesetzt.

- **M1 (fertig):** Metriken erfassen, übertragen, speichern, anzeigen; responsive GUI
- **M2 (aktuell):** GUI-Login, Verlaufsdiagramme, `/metrics` für Prometheus, HTTPS/wss

## Tech-Stack

- **Go** für Agent und Server, **ohne CGO** (`CGO_ENABLED=0`)
- Metriken: `github.com/shirou/gopsutil/v4`
- Transport: WebSocket mit JSON (`github.com/coder/websocket`), Verbindung immer **ausgehend vom Agenten**
- Datenbank: SQLite über `modernc.org/sqlite` (reines Go)
- Frontend: reines HTML/JS (optional htmx als lokal eingebundene Datei), **kein Node-Build**; per `go:embed` im Server-Binary

## Zielplattformen

Agent:

| GOOS    | GOARCH | Zusatz     |
|---------|--------|------------|
| linux   | amd64  |            |
| linux   | arm64  |            |
| linux   | arm    | `GOARM=7`  |
| windows | amd64  |            |

Ressourcenziele für den Agenten (Geräte mit max. 1 GB RAM):

- 10–25 MB RAM im Betrieb; `GOMEMLIMIT` wird im Agenten gesetzt (Standard ca. 20 MiB, per Umgebungsvariable überschreibbar)
- kleines Binary: Build mit `-trimpath -ldflags="-s -w"`
- keine großen Abhängigkeiten im Agenten — insbesondere keine Server-Pakete (SQLite usw.) importieren
- Speicher messen über **Working Set / RSS**, nicht über „Private Bytes“: Sobald `crypto/*` gelinkt ist (WebSocket braucht es),
  enthält jedes Go-Binary einen 32-MiB-BSS-Puffer (`crypto/internal/fips140/drbg.memory`). Er wird nur im FIPS-Modus benutzt
  und belegt keinen physischen Speicher, erscheint unter Windows aber in der Commit-Größe. Gemessen (M1):
  - Linux amd64 (WSL2, Ubuntu 24.04): ca. 13 MB RSS (6 MB anonym + 7 MB Binary), < 1 s CPU-Zeit pro Minute
  - Windows 11: ca. 25–27 MB Working Set, ca. 58 MB Private Bytes
  - Raspberry Pi (arm): noch offen

Server: läuft als Docker-Container (linux/amd64 bzw. arm64) oder direkt als Binary.

## Code-Architektur

```
/agent    Agent: Collector (Metriken) und später Executor (Jobs)
/server   Server: API, WebSocket-Endpunkt, SQLite-Speicher, liefert die GUI aus
/web      Frontend-Dateien (werden per go:embed eingebettet)
/proto    gemeinsame Nachrichtentypen (JSON) zwischen Agent und Server
/deploy   Dockerfile für den Server, systemd-Unit, später Windows-Service
build.ps1 baut den Agenten für alle Zielplattformen nach /dist
```

Ein einziges Go-Modul im Repo-Root. `/proto` ist die einzige gemeinsame Abhängigkeit von Agent und Server.

Datenfluss M1:

1. Agent sammelt alle 10–15 s CPU (gesamt, pro Kern, Load), RAM/Swap, Netzwerk-Raten pro Interface,
   Speicher pro Mountpoint; alle 30 s die Top-10-Prozesse nach CPU und RAM.
2. Agent meldet sich per WebSocket mit einer `hello`-Nachricht (Host-Infos, Agent-Version, **Fähigkeiten**)
   und schickt danach Metriken. Bei Verbindungsabbruch: Reconnect mit exponentiellem Backoff.
3. Server speichert den letzten Stand pro Host sowie eine kompakte Historie (Retention 7 Tage) in SQLite
   und zeigt eine Host-Liste mit aktuellen Werten in der Web-GUI.

Vorbereitet für später (nicht umsetzen, solange nicht beauftragt):

- **Fähigkeiten** (`capabilities`, z. B. `packages`, `users`, `reboot`): der Agent meldet sie, die GUI zeigt nur passende Aktionen
- **Jobs**: Admin-Aktionen mit festem Aktionsnamen und validierten Parametern
- **`/metrics`-Endpoint** im Prometheus-Format für Grafana

## Sicherheitsregeln

Der Agent läuft als root bzw. SYSTEM, deshalb gilt:

- **Keine freie Shell.** Admin-Aktionen gibt es nur als Jobs mit Aktionsnamen aus einer festen Whitelist und
  streng validierten Parametern. Niemals `sh -c`, `cmd /c`, `powershell -Command` oder String-Verkettung
  zu Befehlszeilen; Programme mit fester Argumentliste aufrufen.
- Der Agent führt nur Jobs aus, deren Fähigkeit er selbst gemeldet hat, und prüft Parameter **selbst** noch einmal —
  er vertraut dem Server nicht blind.
- Der Agent öffnet **keine eingehenden Ports**; er verbindet sich nur ausgehend zum Server.
- Agenten authentifizieren sich mit einem Token (`Authorization: Bearer …`). Ohne konfiguriertes Token startet der Server nicht.
  Außerhalb von localhost/LAN-Tests ist `wss://` (TLS) Pflicht.
- Alle Netzwerkeingaben gelten als nicht vertrauenswürdig: Größenlimit für WebSocket-Nachrichten, Felder validieren,
  SQL nur mit Platzhaltern, in der GUI Werte per `textContent` setzen (kein `innerHTML` mit Fremddaten).
- **GUI-Login**: Alles außer Login-Seite, `/ws/agent` (Agent-Token) und `/healthz` erfordert eine Session.
  Passwörter mit bcrypt (Kosten 12, min. 10 Zeichen); Session-Token zufällig (32 Byte), in der DB nur als SHA-256;
  Cookie `HttpOnly`, `SameSite=Strict`, `Secure` bei TLS; 7 Tage gültig; 5 Fehlversuche → 5 Minuten Sperre pro IP.
  Benutzer werden nur per CLI angelegt (`mat-server user add`), nie über die GUI oder Umgebungsvariablen.
- Ändernde Anfragen (POST/DELETE) prüfen zusätzlich den `Origin`-Header (Same-Origin).
- Zugangsdaten, Tokens und Passwörter niemals committen und niemals loggen — Konfiguration über `.env` /
  lokale Config-Dateien (in `.gitignore`).

## Build- & Run-Befehle

```powershell
# Tests und Prüfung
go vet ./...
go test ./...

# Agent für alle Zielplattformen nach .\dist bauen
.\build.ps1

# Lokal testen (Server + Agent auf demselben Rechner)
go run ./server/cmd/mat-server user add admin -db .\data\mat.db   # einmalig, Passwort wird abgefragt
$env:MAT_AGENT_TOKEN = "dev-token"
go run ./server/cmd/mat-server -listen 127.0.0.1:8080 -db .\data\mat.db
go run ./agent/cmd/mat-agent -server ws://127.0.0.1:8080/ws/agent -token dev-token
# GUI: http://127.0.0.1:8080/

# Server-Image bauen
docker build -f deploy/Dockerfile -t mat-server .
```

## Arbeitsweise

- Erst einen kurzen Plan zeigen, dann umsetzen.
- Kleine, nachvollziehbare Commits.
- Vor jedem Commit `go vet ./...` und `go test ./...` ausführen.
- Lokal unter Windows testen (Agent und Server auf demselben Rechner).
- **Keine systemverändernden Befehle** ausführen (keine Dienste installieren, keine Systemeinstellungen ändern usw.).

## Konventionen

- Sprache für Dokumentation und Kommentare: Deutsch
- Git-Standard-Branch: `main`
- Go-Code: `gofmt`, Fehler mit Kontext wrappen (`fmt.Errorf("…: %w", err)`), Logging über `log/slog`
