# Monitoring-Admin-Tool

Selbst gebautes Monitoring- und Administrationstool fürs Homelab: ein schlanker Agent auf jeder Maschine,
ein zentraler Server mit Web-GUI.

**Stand:** M1 fertig – Metriken erfassen (CPU, RAM/Swap, Netzwerk, Speicher, Top-Prozesse),
per WebSocket übertragen, in SQLite speichern (7 Tage Historie) und als responsive Host-Liste anzeigen.
M2 fertig – GUI-Login, Verlaufsdiagramme, Prometheus-Endpunkt, HTTPS/wss.

## Schnellstart (lokal)

```powershell
go run ./server/cmd/mat-server user add admin        # einmalig: GUI-Benutzer anlegen
$env:MAT_AGENT_TOKEN = "ein-langes-geheimes-token"
go run ./server/cmd/mat-server -listen 127.0.0.1:8080
go run ./agent/cmd/mat-agent -server ws://127.0.0.1:8080/ws/agent
```

GUI: <http://127.0.0.1:8080/>

## GUI-Benutzer

```powershell
mat-server user add <name>      # anlegen, Passwort wird verdeckt abgefragt (mind. 10 Zeichen)
mat-server user passwd <name>   # Passwort ändern, beendet alle Sessions des Benutzers
mat-server user list
mat-server user del <name>
```

Im Docker-Container: `docker exec -it <container> mat-server user add admin`.
Nach 5 Fehlversuchen ist die Anmeldung für die IP 5 Minuten gesperrt.

## Bauen

```powershell
.\build.ps1           # Agent für linux/amd64, linux/arm64, linux/armv7, windows/amd64 → .\dist
.\build.ps1 -Server   # zusätzlich Server-Binaries
docker build -f deploy/Dockerfile -t mat-server .
```

## Agent konfigurieren

Per Flags (`-server`, `-token`, `-id`, `-interval`, `-process-interval`, `-log-level`, `-ca`) oder JSON-Datei
(`-config agent.json`, Vorlage: [deploy/agent.example.json](deploy/agent.example.json)).
Das Token kann auch über `MAT_AGENT_TOKEN` gesetzt werden.

Linux: systemd-Unit unter [deploy/systemd/mat-agent.service](deploy/systemd/mat-agent.service).

## API

Alle `/api/*`-Endpunkte außer Login/Logout erfordern eine Session.

| Endpunkt                        | Beschreibung                               |
|---------------------------------|--------------------------------------------|
| `GET /ws/agent`                 | WebSocket für Agenten (Bearer-Token)       |
| `POST /api/login`               | Anmelden (`{"username","password"}`)       |
| `POST /api/logout`              | Abmelden                                   |
| `GET /api/me`                   | angemeldeter Benutzer                      |
| `GET /api/hosts`                | alle Hosts mit letztem Stand               |
| `GET /api/hosts/{id}/history`   | Historie, `?hours=1..168`, `?points=0..2000` verdichtet |
| `DELETE /api/hosts/{id}`        | Host samt Historie entfernen (nur offline) |
| `GET /healthz`                  | Health-Check                               |
| `GET /metrics`                  | Prometheus-Format (eigenes Bearer-Token)   |

## HTTPS / wss

```powershell
# 1. Zertifikat erzeugen (IP und/oder Name, unter dem Agenten und Browser den Server erreichen)
mat-server gen-cert -hosts 192.168.0.10,monitor.lan -out data

# 2. Server mit TLS starten (alternativ MAT_TLS_CERT / MAT_TLS_KEY)
mat-server -listen 0.0.0.0:8443 -tls-cert data/server.crt -tls-key data/server.key

# 3. data/server.crt auf die Agenten kopieren und vertrauen
mat-agent -server wss://192.168.0.10:8443/ws/agent -ca server.crt
```

Das Zertifikat ist selbst signiert: Der Agent vertraut mit `-ca` genau diesem Zertifikat (keine
Prüfung wird abgeschaltet). Im Browser erscheint beim ersten Aufruf eine Warnung, bis das Zertifikat
dort als vertrauenswürdig importiert wird. Bei TLS bekommt das Session-Cookie automatisch `Secure`.

## Prometheus / Grafana

`/metrics` ist nur aktiv, wenn der Server mit `MAT_METRICS_TOKEN` (mind. 16 Zeichen) gestartet wird.
Messwerte gibt es nur für verbundene Hosts; `mat_host_up` zeigt 0/1 für alle.

```yaml
scrape_configs:
  - job_name: homelab-monitor
    scrape_interval: 15s
    authorization:
      credentials: "<MAT_METRICS_TOKEN>"
    static_configs:
      - targets: ["192.168.0.10:8080"]
```

Metriken (Präfix `mat_`): `host_up`, `host_info`, `host_last_seen_timestamp_seconds`, `host_boot_time_seconds`,
`cpu_usage_percent`, `cpu_core_usage_percent`, `load1/5/15` (nicht unter Windows), `memory_total/used_bytes`,
`swap_total/used_bytes`, `network_{receive,transmit}_{bytes,packets}_per_second`, `network_{receive,transmit}_errors_total`,
`filesystem_size/used_bytes`.

Architektur, Zielplattformen und Sicherheitsregeln: siehe [CLAUDE.md](CLAUDE.md).
