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
- **M8 (fertig):** Prognose „Platte voll“ (Tabelle `disk_history`, Trend über 7 Tage, Alarmtyp `disk_forecast`),
  `commands`-Whitelist in `agent.json` (feste Argumentliste, keine Parameter aus der GUI, keine Shell,
  Job `command.run`)
- **M9 (fertig):** Neustart mit drain für k3s-Hosts (Aktion `k3s.drain_reboot` im Server, Tabelle `node_ops`,
  optionale ClusterRole `mat-server-drain`); „Node nicht bereit“ folgt dem Wartungsfenster des gleichnamigen Hosts
- **M10 (fertig):** Pakete installieren (`package.install`, Fähigkeit `packages`) und Benutzer anlegen (`user.create`,
  Fähigkeit `users`); Paketverwaltungen apt, dnf, zypper, apk an festen Pfaden, unter Windows nur Installation per winget
- **M12a (fertig):** Playbooks, Grundversion: YAML-Dateien aus `MAT_PLAYBOOK_DIR` (nur Server, `go.yaml.in/yaml/v3`),
  manuell starten, serial/parallel, `on_error`, `when` (Exit-Code oder ok eines früheren Schritts), Wartezeiten,
  Tabellen `playbook_runs`/`playbook_steps`, Fortsetzen nach Server-Neustart; Agent meldet echte Exit-Codes (v1.6.0)
- **M11 (fertig):** Web-Terminal (Block `terminal` in `agent.json`, Shell als unprivilegierter Benutzer über
  `systemd-run --pty --uid`, Ticket nach erneuter Passworteingabe, Tabelle `terminal_sessions`, xterm.js lokal);
  nur Linux mit systemd, Agent v1.7.0
- **M12b/c (fertig):** Gates (`host_healthy`, `cluster_healthy`, `check_ok`; 30 s stabil, nur Daten nach Schrittbeginn),
  Probelauf (Einschätzung Host × Schritt, sendet nichts), Freigabe-Schritte (`approve`, `once`, Frist), Zeitplan
  (`schedule`: daily/weekly/monthly inkl. „first sun“, Zeitzone `TZ`, Tabelle `playbook_schedules`, in der GUI nur pausieren)
- **M13 (in Arbeit):** winget-Updates unter Windows (`winget upgrade` als Update-Liste, `source update`,
  `upgrade --all` mit derselben Auswahl, nur Quelle `winget`; Ausnahmen per `winget pin` am Host), Agent v1.8.0
- **Zurückgestellt:** Load und Temperatur unter Windows (gopsutil liefert Load 0, WMI-Thermozonen fehlen) – erst recherchieren

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
/agent    Agent: collector (Metriken), client (WebSocket, Jobs), services, packages (apt, dnf, zypper, apk, winget),
          users (Benutzer anlegen), power (Neustart),
          logs (journalctl/wevtutil), smart (smartctl), update (Self-Update), commands (eigene Befehle),
          progress (Live-Ausgabe von Jobs), terminal (Web-Terminal, PTY über github.com/creack/pty)
/server   Server: API, WebSocket-Endpunkt, SQLite-Speicher, liefert die GUI aus;
          server/alert wertet Alarmregeln aus, server/notify verschickt Nachrichten (ntfy, SMTP),
          server/checks führt externe Checks aus (HTTP(S), Ping, TCP),
          server/k8s liest den k3s-Cluster (REST ohne client-go) und die k3s-Release-Kanäle; schreibend nur
          cordon/uncordon und Eviction für den Neustart mit drain,
          server/forecast schätzt aus der Disk-Historie, wann ein Mountpoint voll ist,
          server/heartbeat meldet einem externen Wächter regelmäßig ein Lebenszeichen,
          server/playbook liest und prüft Playbooks (Parsen, Prüfen, Schreiben getrennt; Runner in server/api),
          Web-Terminal: Tickets und Vermittlung in server/api/terminal.go
/web      Frontend-Dateien (werden per go:embed eingebettet; xterm.js unter web/static/vendor/xterm)
/proto    gemeinsame Nachrichtentypen (JSON) zwischen Agent und Server
/internal kleine gemeinsame Hilfspakete ohne schwere Abhängigkeiten (winsvc: Windows-Dienst, logfile,
          release: signierte Release-Manifeste)
/deploy   Dockerfile, k8s-Manifeste (k3s), systemd-Unit, install-agent.sh/.ps1, env-Vorlage,
          playbooks (Beispiele und AMP-Hilfsskripte)
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

Playbooks (M12a): Jeder Schritt läuft über `startJob` bzw. `startDrain` mit `checkJob` (dieselben Prüfungen wie die
GUI). Die Job-ID steht im Schritt, bevor der Job gesendet wird; nach einem Neustart wartet der Runner auf diesen Job
statt ihn neu auszulösen, fehlt er, endet der Lauf als „unterbrochen“. Nach einem Fehler mit `stop` startet kein
weiterer Host. Vor einem Schritt bis zu 2 min auf die Verbindung warten, nach `system.reboot` auf die Rückkehr.
Gates, Freigaben und Zeitpläne (M12b/c) stehen in `server/api/playbook_{gates,approve,schedule}.go`. Ein Zeitplan
merkt sich den Termin vor dem Start (nie doppelt), startet neue Zeitpläne nie rückwirkend und holt verpasste Termine
nur innerhalb 1 h nach. once-Freigaben werden nach einem Neustart aus der Meldung des Schritts wiederhergestellt.

Vorbereitet für später (nicht umsetzen, solange nicht beauftragt):

- weitere Benachrichtigungskanäle (Telegram, Gotify)

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
  Eigene Befehle nur aus `"commands"` (feste Argumentliste, absoluter Pfad, keine Shell außer PowerShell
  `-File <Skript>`); der Server wählt nur den Namen, Parameter aus der GUI gibt es nicht.
- **Pakete und Benutzer:** Parameter prüfen `proto` **und** Agent. Paketnamen ohne führendes oder endendes `-` (`foo-`
  entfernt bei apt und zypper das Paket!), ohne Pfade, Versionen und Platzhalter; apt schlägt den Namen vorher exakt nach.
  Es wird nichts entfernt (`--no-remove`, `--no-force-resolution`). Nie ein Passwort über Server, GUI, Job oder Log:
  Linux-Konten bleiben gesperrt, Windows-Konten deaktiviert. sudo nur über die Gruppe, nie über `/etc/sudoers`.
- **Playbooks** bringen keine neuen Möglichkeiten: nur Aktionen aus `proto.ActionCapability` (ohne `agent.update`)
  plus `k3s.drain_reboot`, Parameter mit denselben Prüfern wie Jobs, keine Skripte, Ausdrücke oder Variablen aus
  Ausgaben. Dateien nur aus `MAT_PLAYBOOK_DIR` (kein Editor, kein Upload über die GUI). `on_error: continue` ist bei
  `system.reboot`, `k3s.drain_reboot` und `packages.upgrade` verboten. Gates und Freigaben sind feste Typen ohne
  Ausdrücke. Ein Zeitplan lässt sich nur in der Datei einschalten, in der GUI nur pausieren.
- **Web-Terminal – die einzige, bewusste Ausnahme von „keine freie Shell“** (M11). Begründung: Für Fehlersuche
  braucht es manchmal eine echte Shell, und SSH soll nicht von außen offen sein. Deshalb gilt zusätzlich:
  Freischaltung nur über den Block `terminal` in `agent.json` (eigene Fähigkeit, nicht über `jobs`, nie per
  Server/GUI/Flag); Shell nur als dort eingetragener Benutzer, nie root/UID 0, Shell absolut und in `/etc/shells`;
  Start nur über `systemd-run` mit fester Argumentliste (eigene Unit `mat-term-<id>`, wird beim Ende gestoppt);
  erneute Passworteingabe je Sitzung, einmaliges Ticket (30 s, an Benutzer, Host und Session gebunden), Origin-
  Prüfung auch beim WebSocket; Leerlauf-, Gesamtzeit-, Sitzungs- und Größenlimits im Agenten; Audit ohne Ein-
  und Ausgabe. Der Server reicht nur Bytes durch und erzeugt keine Eingaben; reißt die Browser-Verbindung ab,
  hält er die Sitzung 5 min offen und puffert die Ausgabe nur im Speicher (höchstens 256 KB). Wiederaufnehmen
  ohne Passwort nur mit einmaligem Token (je Wiederaufnahme neu, an Sitzung, Benutzer und GUI-Session
  gebunden); „close“ vom Browser beendet sofort und geht nicht an den Agenten. Bekannte Grenze: Ein übernommener
  Server kann auf freigeschalteten Hosts selbst Sitzungen öffnen (in der README unter „Risiken“).
- Der Agent öffnet **keine eingehenden Ports**; er verbindet sich nur ausgehend zum Server.
- **Cluster-Zugriff des Servers:** lesend (ClusterRole `mat-server-read`); schreibend nur Node absperren/freigeben
  (`patch` auf Nodes) und Pods über die Eviction-API verdrängen – mit eigener, optionaler ClusterRole
  `mat-server-drain`, nur für den Neustart mit drain. Kein Löschen von Pods, keine Secrets. Freigegeben wird nur ein
  Node mit MATs eigener Annotation (`mat/cordoned-by` = Job-ID).
- Agenten authentifizieren sich mit einem Token (`Authorization: Bearer …`). Ohne konfiguriertes Token startet der Server nicht.
  Außerhalb von localhost/LAN-Tests ist `wss://` (TLS) Pflicht. Selbst signierte Zertifikate (`mat-server gen-cert`)
  werden im Agenten per `ca_file` gepinnt — `InsecureSkipVerify` ist verboten. TLS mindestens 1.2.
  Einzige Ausnahme: externe HTTPS-Checks (server/checks), wenn der Nutzer „Zertifikat nicht prüfen“ für
  einen selbst signierten Dienst ausdrücklich ankreuzt — dort wird nur Erreichbarkeit gemessen, nichts vertraut.
- Alle Netzwerkeingaben gelten als nicht vertrauenswürdig: Größenlimit für WebSocket-Nachrichten, Felder validieren,
  SQL nur mit Platzhaltern, in der GUI Werte per `textContent` setzen (kein `innerHTML` mit Fremddaten).
  CSP `default-src 'self'`; einzige Lockerung `style-src-elem 'unsafe-inline'` für die `<style>`-Elemente von xterm.js.
- **GUI-Login**: Alles außer Login-Seite, `/ws/agent` (Agent-Token) und `/healthz` erfordert eine Session.
  Passwörter mit bcrypt (Kosten 12, min. 10 Zeichen); Session-Token zufällig (32 Byte), in der DB nur als SHA-256;
  Cookie `HttpOnly`, `SameSite=Strict`, `Secure` bei TLS; 7 Tage gültig; 5 Fehlversuche → 5 Minuten Sperre pro IP.
  Benutzer werden nur per CLI angelegt (`mat-server user add`), nie über die GUI oder Umgebungsvariablen.
- Ändernde Anfragen (POST/DELETE) prüfen zusätzlich den `Origin`-Header (Same-Origin).
- Benachrichtigungskanäle (`MAT_NTFY_*`, `MAT_SMTP_*`) und der Heartbeat (`MAT_HEARTBEAT_*`) nur über
  Umgebungsvariablen, nie über GUI oder DB. Heartbeat-URL und -Token nie loggen; der Status-Text enthält keine Hostnamen.
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
