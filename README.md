# Monitoring-Admin-Tool

Selbst gebautes Monitoring- und Administrationstool fürs Homelab: ein schlanker Agent auf jeder Maschine,
ein zentraler Server mit Web-GUI.

**Stand: Meilenstein M1** – Metriken erfassen (CPU, RAM/Swap, Netzwerk, Speicher, Top-Prozesse),
per WebSocket übertragen, in SQLite speichern (7 Tage Historie) und als Host-Liste anzeigen.

## Schnellstart (lokal)

```powershell
$env:MAT_AGENT_TOKEN = "ein-langes-geheimes-token"
go run ./server/cmd/mat-server -listen 127.0.0.1:8080
go run ./agent/cmd/mat-agent -server ws://127.0.0.1:8080/ws/agent
```

GUI: <http://127.0.0.1:8080/>

## Bauen

```powershell
.\build.ps1           # Agent für linux/amd64, linux/arm64, linux/armv7, windows/amd64 → .\dist
.\build.ps1 -Server   # zusätzlich Server-Binaries
docker build -f deploy/Dockerfile -t mat-server .
```

## Agent konfigurieren

Per Flags (`-server`, `-token`, `-id`, `-interval`, `-process-interval`, `-log-level`) oder JSON-Datei
(`-config agent.json`, Vorlage: [deploy/agent.example.json](deploy/agent.example.json)).
Das Token kann auch über `MAT_AGENT_TOKEN` gesetzt werden.

Linux: systemd-Unit unter [deploy/systemd/mat-agent.service](deploy/systemd/mat-agent.service).

## API

| Endpunkt                        | Beschreibung                               |
|---------------------------------|--------------------------------------------|
| `GET /ws/agent`                 | WebSocket für Agenten (Bearer-Token)       |
| `GET /api/hosts`                | alle Hosts mit letztem Stand               |
| `GET /api/hosts/{id}/history`   | kompakte Historie, `?hours=1..168`         |
| `DELETE /api/hosts/{id}`        | Host samt Historie entfernen (nur offline) |
| `GET /healthz`                  | Health-Check                               |

Architektur, Zielplattformen und Sicherheitsregeln: siehe [CLAUDE.md](CLAUDE.md).
