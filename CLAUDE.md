# CLAUDE.md

Diese Datei enthält Hinweise für Claude Code (claude.ai/code) beim Arbeiten mit diesem Repository.

## Projektübersicht

Selbst gebautes Monitoring- und Administrationstool für das eigene Homelab: **Monitoring and Administration Tool (MAT)**.
Repo, Go-Modul, Image und Release-URLs heißen weiter `Monitoring-Admin-Tool` (nicht umbenennen, sonst brechen Self-Update und Install-Skripte).
Ein schlanker **Agent** läuft auf den überwachten Maschinen, ein zentraler **Server** mit Web-GUI
sammelt die Daten und steuert (später) Admin-Aktionen.

Es wird in Meilensteinen gearbeitet. Alles, was zu keinem beauftragten Meilenstein gehört, wird nur vorbereitet, nicht umgesetzt.

- **M1 (fertig):** Metriken erfassen, übertragen, speichern, anzeigen; responsive GUI
- **M2 (fertig):** GUI-Login, Verlaufsdiagramme, `/metrics` für Prometheus, HTTPS/wss
- **M3 (fertig):** Alarme (Schwellwerte, GUI-Anzeige, Benachrichtigung per ntfy und E-Mail)
- **M4 (fertig):** Installation als Dienst (Windows, systemd), Server in k3s (ghcr.io, kustomize),
  Alarme je Host, Stummschalten, Erinnerungen
- **M5 (fertig):** Self-Update der Agenten (Job `agent.update`, signierte GitHub-Releases),
  Release-Workflow, Warnung vor Ablauf des Server-Zertifikats
- **M6 (fertig):** CPU-Temperatur, externe Checks (HTTP(S), Ping, TCP), k3s-Ansicht (nur lesend), Anzeigename/Gruppe je Host
- **M7 (fertig):** Admin-Jobs mit Audit-Log (Dienste, apt-Updates, Neustart, Logs lesen), Dienstüberwachung,
  Update-Status, SMART, k3s-Versionen; Alarme quittieren, Wartungsfenster, Abhängigkeiten
- **M8 (geplant):** Prognose „Platte voll“ (Tabelle `disk_history`, Trend über 7 Tage, Alarmtyp `disk_forecast`),
  danach `commands`-Whitelist in `agent.json` (feste Argumentliste, keine Parameter aus der GUI, keine Shell,
  Job `command.run`)
- **Später:** Playbooks (Abläufe über mehrere Hosts) – vorher gemeinsam einen genauen Plan erarbeiten

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

Server: läuft als Pod in k3s (Nodes sind arm64; Image multi-arch amd64/arm64 auf ghcr.io), als Windows-Dienst
oder direkt als Binary. Agenten laufen immer direkt auf den Hosts (Windows-Dienst bzw. systemd), nie als Pod.

## Code-Architektur

```
/agent    Agent: collector (Metriken), client (WebSocket, Jobs), services, packages (apt), power (Neustart),
          logs (journalctl/wevtutil), smart (smartctl), update (Self-Update)
/server   Server: API, WebSocket-Endpunkt, SQLite-Speicher, liefert die GUI aus;
          server/alert wertet Alarmregeln aus, server/notify verschickt Nachrichten (ntfy, SMTP),
          server/checks führt externe Checks aus (HTTP(S), Ping, TCP),
          server/k8s liest den k3s-Cluster (nur lesend, REST ohne client-go) und die k3s-Release-Kanäle
/web      Frontend-Dateien (werden per go:embed eingebettet)
/proto    gemeinsame Nachrichtentypen (JSON) zwischen Agent und Server
/internal kleine gemeinsame Hilfspakete ohne schwere Abhängigkeiten (winsvc: Windows-Dienst, logfile,
          release: signierte Release-Manifeste)
/deploy   Dockerfile, k8s-Manifeste (k3s), systemd-Unit, install-agent.sh/.ps1, env-Vorlage
.github   Workflow: Multi-Arch-Server-Image nach ghcr.io
build.ps1 baut den Agenten für alle Zielplattformen nach /dist
```

Ein einziges Go-Modul im Repo-Root. Agent und Server teilen sich nur `/proto` und `/internal`; `/internal`
darf außer `golang.org/x/sys` nichts importieren (der Agent soll klein bleiben).

Datenfluss M1:

1. Agent sammelt alle 10–15 s CPU (gesamt, pro Kern, Load), RAM/Swap, Netzwerk-Raten pro Interface,
   Speicher pro Mountpoint; alle 30 s die Top-10-Prozesse nach CPU und RAM.
2. Agent meldet sich per WebSocket mit einer `hello`-Nachricht (Host-Infos, Agent-Version, **Fähigkeiten**)
   und schickt danach Metriken. Bei Verbindungsabbruch: Reconnect mit exponentiellem Backoff.
3. Server speichert den letzten Stand pro Host sowie eine kompakte Historie (Retention 7 Tage) in SQLite
   und zeigt eine Host-Liste mit aktuellen Werten in der Web-GUI.

Alarme (M3): feste Regeltypen (`offline`, `cpu`, `mem`, `swap`, `disk` je Mountpoint, `temp` in °C seit M6;
seit M7 `service`, `unit_failed`, `smart`; seit M8 `disk_forecast` aus `disk_history`, stündlich; dazu Checks und Cluster),
keine frei formulierbaren Ausdrücke. Auswertung im Server bei jeder Metrik-Nachricht, Offline-Prüfung alle 30 s.
Auslösen nach ununterbrochener Mindestdauer, Ende mit 5 Prozentpunkten (bzw. 5 °C) Hysterese, je Alarm genau
eine Nachricht beim Auslösen und eine beim Ende. Offene Alarme stehen in SQLite und überstehen Neustarts.
Stummschaltung, Wartungsfenster und Abhängigkeiten (`Evaluator.SetQuiet`) verhindern nur das Melden: Alarme
werden erfasst und, falls danach noch aktiv, nachgemeldet. Quittierte Alarme bekommen keine Erinnerungen.

Jobs (M5/M7): Aktionen und ihre Parameter stehen fest in `proto` (`ActionCapability`, `actionParams`,
`actionTimeout`, `readOnly`). Jeder Job steht mit Benutzer und Ergebnis in der Tabelle `jobs` (Audit, 90 Tage).
Agent-Jobs laufen unabhängig von der Verbindung; Ergebnisse gehen nach dem nächsten hello raus. Paket-Jobs schicken
ihre Ausgabe zusätzlich alle 2 s als `job_progress` (nur im Server-Speicher, für die Live-Anzeige).

Vorbereitet für später (nicht umsetzen, solange nicht beauftragt):

- weitere Benachrichtigungskanäle (Telegram, Gotify)
- Fähigkeit `users` (Benutzer verwalten)

## Sicherheitsregeln

Der Agent läuft als root bzw. SYSTEM, deshalb gilt:

- **Keine freie Shell.** Admin-Aktionen gibt es nur als Jobs mit Aktionsnamen aus einer festen Whitelist und
  streng validierten Parametern. Niemals `sh -c`, `cmd /c`, `powershell -Command` oder String-Verkettung
  zu Befehlszeilen; Programme mit fester Argumentliste aufrufen.
- Der Agent führt nur Jobs aus, deren Fähigkeit er selbst gemeldet hat, und prüft Parameter **selbst** noch einmal —
  er vertraut dem Server nicht blind.
- **Ändernde Fähigkeiten nur per Opt-in am Host:** `"jobs"` in `agent.json` (bzw. `--jobs`/`-Jobs` beim Installieren);
  nie per Server, GUI oder normalem Start-Flag. Logs nur aus `"log_sources"`. Ohne Eintrag meldet der Agent die
  Fähigkeit nicht. Dienste: nur aus der eigenen Liste, nie der Agent selbst, unter Linux nur `<name>.service`.
  Geräte für smartctl nur aus dessen Scan. Programme mit festem Pfad (`System32\…`) bzw. fester Argumentliste.
- Der Agent öffnet **keine eingehenden Ports**; er verbindet sich nur ausgehend zum Server.
- Agenten authentifizieren sich mit einem Token (`Authorization: Bearer …`). Ohne konfiguriertes Token startet der Server nicht.
  Außerhalb von localhost/LAN-Tests ist `wss://` (TLS) Pflicht. Selbst signierte Zertifikate (`mat-server gen-cert`)
  werden im Agenten per `ca_file` gepinnt — `InsecureSkipVerify` ist verboten. TLS mindestens 1.2.
  Einzige Ausnahme: externe HTTPS-Checks (server/checks), wenn der Nutzer „Zertifikat nicht prüfen“ für
  einen selbst signierten Dienst ausdrücklich ankreuzt — dort wird nur Erreichbarkeit gemessen, nichts vertraut.
- Alle Netzwerkeingaben gelten als nicht vertrauenswürdig: Größenlimit für WebSocket-Nachrichten, Felder validieren,
  SQL nur mit Platzhaltern, in der GUI Werte per `textContent` setzen (kein `innerHTML` mit Fremddaten).
- **GUI-Login**: Alles außer Login-Seite, `/ws/agent` (Agent-Token) und `/healthz` erfordert eine Session.
  Passwörter mit bcrypt (Kosten 12, min. 10 Zeichen); Session-Token zufällig (32 Byte), in der DB nur als SHA-256;
  Cookie `HttpOnly`, `SameSite=Strict`, `Secure` bei TLS; 7 Tage gültig; 5 Fehlversuche → 5 Minuten Sperre pro IP.
  Benutzer werden nur per CLI angelegt (`mat-server user add`), nie über die GUI oder Umgebungsvariablen.
- Ändernde Anfragen (POST/DELETE) prüfen zusätzlich den `Origin`-Header (Same-Origin).
- Benachrichtigungskanäle (`MAT_NTFY_*`, `MAT_SMTP_*`) nur über Umgebungsvariablen, nie über GUI oder DB.
  SMTP nur mit STARTTLS/TLS (Klartext nur zu localhost). Texte in Nachrichten stammen teils von Agenten
  (Hostname, Mountpoint): Steuerzeichen werden entfernt, Betreff RFC-2047-kodiert (keine Header-Injection).
- **Self-Update:** Releases sind mit ed25519 signiert (`release.json` + `.sig`); der öffentliche Schlüssel
  ist in `internal/release/key.go` eingebaut, der private liegt nur als Secret der GitHub-Environment
  `release` (Freigabe Pflicht). Agent **und** Server prüfen die Signatur; der Agent prüft zusätzlich
  Plattform, Größe, SHA-256 und verweigert Downgrades. Der Server liefert nur Dateien der geprüften Version aus.
- `/metrics` hat ein eigenes Token (`MAT_METRICS_TOKEN`, Bearer) und ist ohne Token abgeschaltet (404) — nie offen.
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

# Server-Image bauen (lokal; im CI baut .github/workflows/server-image.yml multi-arch)
docker build -f deploy/Dockerfile -t mat-server .

# k8s-Manifeste prüfen (nur lesen / Probelauf)
kubectl kustomize deploy/k8s
```

## Arbeitsweise

- Erst einen kurzen Plan zeigen, dann umsetzen.
- Kleine, nachvollziehbare Commits.
- Vor jedem Commit `go vet ./...` und `go test ./...` ausführen.
- Lokal unter Windows testen (Agent und Server auf demselben Rechner).
- **Keine systemverändernden Befehle** ausführen (keine Dienste installieren, keine Systemeinstellungen ändern usw.).
  Das gilt auch für den Cluster: `kubectl` nur lesend oder mit `--dry-run`; `apply`/`delete` nur nach Rückfrage.

## Konventionen

- Sprache für Dokumentation und Kommentare: Deutsch
- Git-Standard-Branch: `main`
- Go-Code: `gofmt`, Fehler mit Kontext wrappen (`fmt.Errorf("…: %w", err)`), Logging über `log/slog`
