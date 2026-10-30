# Monitoring and Administration Tool (MAT)

Selbst gebautes Monitoring- und Administrationstool fürs Homelab: ein schlanker Agent auf jeder Maschine,
ein zentraler Server mit Web-GUI.

**Stand:** M1 fertig – Metriken erfassen (CPU, RAM/Swap, Netzwerk, Speicher, Top-Prozesse),
per WebSocket übertragen, in SQLite speichern (7 Tage Historie) und als responsive Host-Liste anzeigen.
M2 fertig – GUI-Login, Verlaufsdiagramme, Prometheus-Endpunkt, HTTPS/wss.
M3 fertig – Alarme mit Schwellwerten, Anzeige in der GUI, Benachrichtigung per ntfy und E-Mail.
M4 fertig – Installation als Dienst (Windows, systemd), Server in k3s, Alarme je Host, Stummschalten, Erinnerungen.
M5 fertig – Self-Update der Agenten aus signierten GitHub-Releases, Warnung vor Ablauf des Server-Zertifikats.
M6 fertig – CPU-Temperatur, externe Checks (HTTP(S), Ping, TCP), k3s-Ansicht; Hosts umbenennen (Anzeigename,
auch in Alarmen) und in Gruppen ordnen – intern bleibt die Agent-ID die Kennung.
M7 fertig – Admin-Aktionen als Jobs mit Verlauf, Dienste überwachen und steuern,
offene Updates (apt) und Neustart, neue k3s-Versionen im Tab „Cluster“, freigegebene Logs lesen, SMART der Datenträger; Alarme quittieren, Wartungsfenster, Abhängigkeiten.
M8 fertig – Prognose „Platte voll“ je Mountpoint (mit Alarm), eigene Befehle mit fester Befehlszeile aus `agent.json`; Live-Ausgabe laufender Jobs.
M9 fertig – Neustart von k3s-Hosts mit drain (absperren, Pods verdrängen, Neustart, freigeben); „Node nicht bereit“ folgt dem Wartungsfenster des Hosts.

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
Das Token kann auch über `MAT_AGENT_TOKEN` gesetzt werden. Ohne `-config` und `-server` liest der Agent
die Konfiguration vom festen Ort (`%ProgramData%\mat-agent\agent.json` bzw. `/etc/mat-agent/agent.json`).
`mat-agent check [flags]` prüft die Konfiguration, ohne zu verbinden.

### Admin-Aktionen freischalten

Ändernde Aktionen führt der Agent nur aus, wenn sie in seiner `agent.json` freigeschaltet sind – der
Server kann sie sonst nicht auslösen, auch nicht, wenn er übernommen wird:

```json
"jobs": ["services", "packages", "reboot"]
```

Beim Einrichten geht das direkt mit dem Install-Skript (nur zusammen mit `--server`, sonst
`agent.json` von Hand ändern und den Dienst neu starten):

```sh
sudo sh install-agent.sh --server wss://… --fingerprint … --jobs services,packages,reboot --log-sources journal:sshd,journal:kernel
```
```powershell
.\install-agent.ps1 -Server wss://… -Fingerprint … -Jobs services,reboot -LogSources eventlog:System
```

Unter Windows ändert `-Jobs`/`-LogSources` auch eine vorhandene Konfiguration (`none` = keine).

| Eintrag    | erlaubt                                         |
|------------|-------------------------------------------------|
| `services` | Dienste starten, stoppen, neu starten (`systemctl` bzw. Windows-Dienst-Manager) |
| `packages` | Paketlisten holen und Updates installieren (`apt-get update` / `apt-get -y upgrade`) |
| `reboot`   | Neustart (`systemctl reboot` bzw. `shutdown.exe /r`) |

Jede Aktion landet mit Benutzer, Zeit und Ausgabe im Verlauf („Aktionen“ in den Host-Details, 90 Tage). Ein Klick
auf einen Eintrag öffnet die Ausgabe; bei Paket-Jobs läuft sie ab Agent v1.3.0 live mit.
Der Agent prüft die Parameter selbst noch einmal, steuert nur Dienste aus seiner eigenen Liste und
nie sich selbst. Lesend arbeitet er immer: Dienstliste, offene Updates (`apt-get -s upgrade`,
höchstens alle 30 min) und ob ein Neustart nötig ist (`/var/run/reboot-required` bzw. Registry).
Unter Windows gibt es nur den Neustart-Hinweis, keine Liste offener Windows-Updates.

**Logs** gibt der Agent nur aus den Quellen in `log_sources` heraus; die GUI bietet genau diese an
(Abschnitt „Logs“ in den Host-Details):

```json
"log_sources": ["journal:sshd", "journal:k3s", "journal:kernel", "eventlog:System"]
```

`journal` ist das ganze Journal, `journal:<unit>` eine Unit, `journal:kernel` die Kernel-Meldungen
(`journalctl`), `eventlog:<name>` ein Windows-Ereignisprotokoll (`wevtutil`). Gelesen werden bis zu
1000 Zeilen bzw. Ereignisse, auch während ein Upgrade läuft. Wer welches Log gelesen hat, steht im
Verlauf; den Inhalt löscht der Server nach einem Tag.

**Eigene Befehle** (ab Agent v1.4.0) stehen mit fester Argumentliste in `commands`; die GUI zeigt
sie als Knöpfe unter „Aktionen“, die Ausgabe läuft live mit. Der Server wählt nur den Namen, Parameter
aus der GUI gibt es nicht – eine Variante ist ein weiterer Eintrag:

```json
"commands": {
  "amp-backup":  { "argv": ["/opt/amp/backup.sh", "--all"], "timeout_sec": 600, "description": "AMP-Backup" },
  "amp-restart": { "argv": ["/usr/bin/systemctl", "restart", "ampinstmgr"], "description": "AMP neu starten" },
  "aufraeumen":  { "argv": ["C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
                            "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:\\Skripte\\aufraeumen.ps1"] }
}
```

- Name: `a-z`, `0-9`, `-`, `_` (höchstens 32 Zeichen), höchstens 32 Befehle; `description` erscheint auf dem Knopf.
- Das Programm braucht einen absoluten Pfad. Skripte direkt aufrufen (Linux: ausführbar mit Shebang-Zeile;
  Windows: `.exe`, `.bat`, `.cmd`).
- Shells als Programm lehnt der Agent ab (`sh`, `bash`, `cmd.exe`, `powershell` …). Einzige Ausnahme ist
  PowerShell mit `-File <absoluter Pfad>`; davor sind nur `-NoProfile`, `-NonInteractive`, `-NoLogo` und
  `-ExecutionPolicy Bypass|RemoteSigned|AllSigned` erlaubt.
- `timeout_sec`: Standard 300, höchstens 3600. Bei Überschreitung beendet der Agent den Befehl (unter Linux
  samt Kindprozessen). Arbeitsverzeichnis ist der Ordner des Programms; es läuft als root bzw. SYSTEM.
- Nach einer Änderung den Dienst neu starten; `mat-agent check -config …` zeigt die erkannten Befehle.

**SMART:** Ist smartmontools installiert (`apt install smartmontools`, Windows: smartmontools.org),
meldet der Agent alle 30 min die Werte aller Datenträger aus `smartctl --scan-open` (`-n standby`
weckt keine schlafenden Platten). SD-Karten und eMMC liefern meist keine Werte.

k3s selbst ist per Install-Skript installiert, nicht über apt: `packages.upgrade` aktualisiert nur
das Betriebssystem. Vor Upgrade oder Neustart eines Nodes am besten `kubectl drain`, und immer nur
einen Node gleichzeitig.

## Installation als Dienst

Die Installationsskripte laden das passende Programm aus dem neuesten GitHub-Release und prüfen
dessen SHA-256-Summe aus `release.json` (nur HTTPS). Mit `--version`/`-Version` eine bestimmte
Version, mit `--binary`/`-Binary` ein vorhandenes Programm (z. B. aus `.\build.ps1` → `dist\`).
Spätere Updates laufen über die GUI (siehe Agent-Updates).

Am einfachsten: In der GUI unter **Einstellungen → Agent installieren** steht der fertige Befehl für
Windows und Linux, mit Server-URL und dem SHA-256-Fingerabdruck des Server-Zertifikats. Damit holt
das Skript das Zertifikat selbst vom Server und übernimmt es nur, wenn der Fingerabdruck stimmt –
`server.crt` muss nicht auf die Hosts kopiert werden. Statt `-Fingerprint`/`--fingerprint` geht
weiterhin `-Ca`/`--ca` mit der Datei. Ohne beides fragt das Skript bei einem nicht
vertrauenswürdigen Zertifikat nach und zeigt den Fingerabdruck zum Vergleich an.

### Agent unter Windows

In einer **Administrator-PowerShell** (Befehl aus der GUI):

```powershell
$f = "$env:TEMP\install-agent.ps1"; Invoke-WebRequest -UseBasicParsing https://github.com/Daywalker91/Monitoring-Admin-Tool/releases/latest/download/install-agent.ps1 -OutFile $f
powershell -ExecutionPolicy Bypass -File $f -Server wss://192.168.100.50/ws/agent -Fingerprint <sha256>
```

Das Token wird verdeckt abgefragt (oder aus `MAT_AGENT_TOKEN` genommen). Das Skript ruft
`mat-agent service install` auf: Programm nach `C:\Program Files\mat-agent`, Konfiguration nach
`C:\ProgramData\mat-agent\agent.json` (nur SYSTEM und Administratoren lesbar, CA-Datei daneben), Dienst
mit Autostart und Neustart nach Fehlern. Log: `C:\ProgramData\mat-agent\agent.log`.
Ohne `-Server` aktualisiert es nur das Programm. Entfernen: `.\install-agent.ps1 -Uninstall [-Purge]`.

Von Hand geht es auch direkt mit dem Programm:

```powershell
$env:MAT_AGENT_TOKEN = "<token>"     # nicht als -token, sonst steht es in der Prozessliste
.\mat-agent-windows-amd64.exe service install -server wss://192.168.100.50/ws/agent -ca server.crt
mat-agent service status              # geht auch ohne Administrator
```

### Agent unter Linux / Raspberry Pi

```sh
curl -fsSLO https://github.com/Daywalker91/Monitoring-Admin-Tool/releases/latest/download/install-agent.sh
sudo sh install-agent.sh --server wss://192.168.100.50/ws/agent --fingerprint <sha256>
```

Für `--fingerprint` braucht das Skript `openssl`. Es erkennt die Architektur (amd64, arm64, armv7), lädt das Programm (liegt es neben dem
Skript oder in `../dist`, wird dieses genommen), fragt das Token verdeckt ab (oder nimmt
`MAT_AGENT_TOKEN`), schreibt `/etc/mat-agent/agent.json` mit 0600, prüft sie und startet den Dienst.
Ohne `--server` aktualisiert es nur Programm und systemd-Unit. Log: `journalctl -u mat-agent -f`.
Entfernen: `sudo sh install-agent.sh --uninstall [--purge]`.

Installationen vor v1.3.0 haben `MemoryMax=64M` in der Unit. Die Grenze gilt auch für `apt-get`/`dpkg` aus
Paket-Jobs und kann ein Upgrade mittendrin abbrechen. Das Self-Update tauscht nur das Programm, daher einmal
`sudo sh install-agent.sh` (ohne `--server`) ausführen, um die neue Unit einzuspielen.

### Server unter Windows

Einstellungen in eine env-Datei (Vorlage: [deploy/server.env.example](deploy/server.env.example)), dann in
einer Administrator-PowerShell:

```powershell
.\dist\mat-server-windows-amd64.exe service install -env-file data\server.env -import-db data\mat.db
mat-server user add admin -env-file C:\ProgramData\mat-server\server.env -db C:\ProgramData\mat-server\mat.db
```

`-import-db` übernimmt die bisherige Datenbank (den Server vorher beenden). Danach liegen env-Datei,
Datenbank und Log (`server.log`) in `C:\ProgramData\mat-server`, nur für Administratoren lesbar.
Ohne Dienst lässt sich die env-Datei auch direkt nutzen: `mat-server -env-file data\server.env`.

### Server in Kubernetes (k3s) oder Docker

Siehe [deploy/k8s/README.md](deploy/k8s/README.md). Das Image `ghcr.io/daywalker91/mat-server`
(amd64 + arm64) baut der Workflow `.github/workflows/server-image.yml` bei jedem Push auf `main`.
Hinter einem Reverse-Proxy `MAT_TRUSTED_PROXIES` setzen (z. B. `10.42.0.0/16`), sonst sieht der Server
nur die IP des Proxys.

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
| `POST /api/hosts/{id}/meta`     | Anzeigename und Gruppe (`{"display_name":"…","group":"…"}`, leer = zurücksetzen) |
| `GET/POST /api/hosts/{id}/jobs` | Verlauf der Aktionen / Aktion starten (`{"action":"service.restart","params":{"name":"nginx"}}`) |
| `GET /api/jobs/{id}`            | eine Aktion samt Ausgabe                   |
| `GET /api/hosts/{id}/services`  | letzte Dienstliste und überwachte Dienste  |
| `GET /api/hosts/{id}/updates`   | letzter Update-Stand mit Paketliste        |
| `GET /api/hosts/{id}/smart`     | letzter SMART-Stand der Datenträger        |
| `POST /api/hosts/{id}/services/watch` | Dienst überwachen (`{"name":"nginx","watch":true}`) |
| `GET /api/alerts`               | aktive Alarme, `?all=1` mit Verlauf (7 Tage) |
| `GET /api/alert-rules`          | Regeln und konfigurierte Kanäle            |
| `POST /api/alert-rules`         | Regeln speichern (`{"rules":[…]}`)         |
| `POST /api/alerts/test`         | Testnachricht an alle Kanäle               |
| `POST/DELETE /api/alerts/{id}/ack` | Alarm quittieren (`{"comment":"…"}`) / Quittierung aufheben |
| `GET/POST /api/maintenance`     | Wartungsfenster / anlegen                  |
| `PUT/DELETE /api/maintenance/{id}` | Wartungsfenster ändern / löschen        |
| `GET/POST /api/hosts/{id}/alert-rules` | abweichende Regeln eines Hosts (leer = Standard) |
| `POST/DELETE /api/hosts/{id}/mute`     | stummschalten (`{"minutes": N}`, 0 = unbefristet) / aufheben |
| `GET /api/update`               | Release-Status, Server-Version, Zertifikatsablauf |
| `POST /api/update/check`        | sofort auf ein neues Release prüfen (höchstens 1×/min) |
| `POST /api/hosts/{id}/update`   | Agent-Update eines Hosts starten           |
| `POST /api/update/all`          | alle veralteten Agenten aktualisieren      |
| `GET/POST /api/checks`          | externe Checks mit letztem Ergebnis / Check anlegen |
| `PUT/DELETE /api/checks/{id}`   | Check ändern / samt Verlauf löschen        |
| `GET /api/checks/{id}/history`  | Antwortzeit und Verfügbarkeit, `?hours=1..168`, `?points=` |
| `GET /api/cluster`              | k3s-Stand: Nodes, Workloads, Pods, ArgoCD-Apps |
| `GET /api/cluster/logs`         | Pod-Logs als Text, `?ns=&pod=&container=&lines=1..5000&previous=1` |
| `GET/POST /api/cluster/ignore`  | Ausnahmen für Cluster-Alarme (`{"patterns":["ns/name*"]}`) |
| `GET /agent/update/{version}/{datei}` | Release-Dateien für Agenten (Agent-Token) |
| `GET /healthz`                  | Health-Check                               |
| `GET /metrics`                  | Prometheus-Format (eigenes Bearer-Token)   |

## Alarme

Der Server prüft feste Regeltypen; Schwelle und Mindestdauer lassen sich in der GUI unter „Regeln“ ändern:

| Regel   | Standard               | Bedeutung                                   |
|---------|------------------------|---------------------------------------------|
| Offline | 2 min                  | Agent nicht verbunden                       |
| CPU     | 90 % für 5 min         | CPU-Auslastung gesamt                       |
| RAM     | 90 % für 5 min         | Arbeitsspeicher belegt                      |
| Swap    | 80 % für 10 min        | Swap belegt (nur Hosts mit Swap)            |
| Disk    | 90 % für 1 min         | Belegung je Mountpoint                      |
| Disk-Prognose | < 7 Tage         | Mountpoint ist bei gleichem Trend (lineare Regression der letzten 7 Tage, stündliche Messpunkte, mind. 24 h) bald voll; endet erst über dem 1,5-Fachen der Schwelle |
| CPU-Temperatur | 80 °C für 5 min | höchster CPU-Sensor (nur Linux-Hosts mit Sensor) |
| Check   | 2 min                  | externer Check schlägt ununterbrochen fehl  |
| Zertifikat | < 14 Tage           | Zertifikat eines HTTPS-Checks läuft bald ab |
| Node    | 2 min                  | Kubernetes-Node nicht bereit                |
| Workload | 5 min                 | Deployment/StatefulSet/DaemonSet nicht bereit, Pod in CrashLoopBackOff u. ä. |
| ArgoCD  | 10 min                 | App Degraded, Missing oder OutOfSync        |
| Dienst  | 1 min                  | überwachter Dienst läuft nicht (Auswahl in den Host-Details unter „Dienste“) |
| Unit    | sofort                 | systemd-Unit ist fehlgeschlagen (Linux)     |
| SMART   | sofort                 | Datenträger FAILED, kritische NVMe-Warnung, ausstehende oder nicht korrigierbare Sektoren (wieder zugewiesene Sektoren nur Anzeige) |

Ein Alarm löst aus, wenn der Wert die Mindestdauer lang ununterbrochen über der Schwelle liegt, und endet,
wenn er 5 Prozentpunkte (Temperatur: 5 °C) unter die Schwelle fällt.

Die **CPU-Temperatur** liest der Agent unter Linux aus `/sys/class/thermal` (Raspberry Pi, Rockchip,
`x86_pkg_temp`) und aus den hwmon-Treibern `coretemp`, `k10temp` und `zenpower`. VMs und Windows-Hosts
haben keinen Wert (in der GUI „–“). In den Host-Details stehen alle gefundenen Sensoren. Pro Alarm gibt es genau zwei Nachrichten: beim Auslösen
und beim Ende. Aktive Alarme stehen oben in der GUI und unter „Alarme“, zusammen mit dem Verlauf der letzten 7 Tage.

In den Host-Details unter „Alarme für diesen Host“:

- **Eigene Regeln:** pro Regeltyp abweichende Werte (z. B. Disk 95 % fürs NAS, Offline aus für den Laptop);
  ohne Haken gilt der Standard, der daneben steht.
- **Stummschalten** für 1 h, 4 h, 24 h oder unbefristet (Wartung): Alarme werden weiter erfasst und angezeigt,
  aber nicht gemeldet. Ist ein Alarm nach dem Ende der Stummschaltung noch aktiv, wird er nachgemeldet;
  sein Ende wird nur gemeldet, wenn auch der Beginn gemeldet wurde.

Unter „Regeln“ lässt sich eine **Erinnerung** einstellen: Alarme, die nach N Stunden noch aktiv sind,
werden erneut gemeldet (0 = aus).

**Quittieren:** Aktive Alarme lassen sich in der Alarmansicht mit Kommentar quittieren. Danach
gibt es keine Erinnerungen mehr; das Ende wird trotzdem gemeldet.

**Wartungsfenster** (Einstellungen): einmalig oder wöchentlich, für alle, eine Gruppe oder einen
Host. Alarme werden erfasst, aber nicht gemeldet; was am Ende noch aktiv ist, wird nachgemeldet.
Wöchentliche Fenster gelten in der Zeitzone des Servers (`TZ`, im k3s-Manifest `Europe/Berlin`).
Checks und Cluster-Alarme erfasst nur „alle“; Ausnahme: „Node nicht bereit“ folgt dem Fenster des
gleichnamigen Hosts (z. B. Gruppe `k3s`).

**Abhängigkeiten** (Host-Details, „Hängt ab von“): Ein Host kann von einem anderen Host oder einem
externen Check abhängen, z. B. alles hinter dem Router. Ist das Ziel ausgefallen (Host offline bzw.
Check fehlgeschlagen), werden Alarme des Hosts erfasst, aber nicht gemeldet – statt zwanzig
Meldungen kommt nur die des Routers. Schleifen lehnt der Server ab.

### Benachrichtigungen

Kanäle werden nur über Umgebungsvariablen des Servers eingerichtet (Zugangsdaten nie in GUI oder Datenbank).
Ohne Kanal erscheinen Alarme nur in der GUI. Funktionstest: „Regeln“ → „Testnachricht senden“.

**ntfy** (Push aufs Handy, [ntfy.sh](https://ntfy.sh) oder selbst gehostet):

```powershell
$env:MAT_NTFY_URL   = "https://ntfy.sh/mein-geheimes-topic"   # Topic-Name schwer erratbar wählen
$env:MAT_NTFY_TOKEN = "tk_..."                                # optional, bei geschütztem Topic
```

In der ntfy-App das Topic abonnieren. `http://` geht auch, ist aber unverschlüsselt (nur im LAN).

**E-Mail** (SMTP mit STARTTLS oder TLS; unverschlüsselt nur zu localhost):

```powershell
$env:MAT_SMTP_HOST = "smtp.example.com"
$env:MAT_SMTP_PORT = "587"                # Standard: 587 bei starttls, 465 bei tls
$env:MAT_SMTP_TLS  = "starttls"           # starttls (Standard), tls oder none
$env:MAT_SMTP_USER = "monitor@example.com"
$env:MAT_SMTP_PASS = "..."
$env:MAT_SMTP_FROM = "MAT <monitor@example.com>"
$env:MAT_SMTP_TO   = "ich@example.com,du@example.com"
```

Fehlgeschlagene Sendungen werden nach 10 s, 1 min und 5 min erneut versucht.

### Heartbeat (externer Wächter)

Fällt der Router, das Internet oder der Server selbst aus, kann MAT nichts mehr melden. Dafür
schickt der Server regelmäßig ein Lebenszeichen an einen externen Dienst (z. B. Push-Healthcheck
bei [ipv64.net](https://ipv64.net/healthcheck_updater_api) oder healthchecks.io). Bleibt es aus,
alarmiert dieser Dienst von außen – Benachrichtigung dort über Mail o. Ä. einrichten, nicht über
einen Kanal hinter dem eigenen Router.

```powershell
$env:MAT_HEARTBEAT_URL      = "https://ipv64.net/health.php"
$env:MAT_HEARTBEAT_TOKEN    = "..."     # optional, als Authorization: Bearer (statt ?token= in der URL)
$env:MAT_HEARTBEAT_INTERVAL = "3m"      # Standard 3m, mindestens 30s
$env:MAT_HEARTBEAT_PAYLOAD  = "off"     # optional: nur GET ohne Status
```

Die erste Meldung geht 1 min nach dem Start raus, damit die Agenten verbunden sind.

Standard ist ein POST mit dem Feld `payload`, z. B. „MAT 1.5.0 · 5/6 Hosts online · 2 Alarme“
(keine Hostnamen); ipv64 zeigt ihn im Event-Log. Ist die Datenbank nicht lesbar, bleibt das
Lebenszeichen aus. URL und Token werden nie geloggt. Kontingent beachten: bei ipv64 zählt jeder
Ping (3m = 480/Tag); beim Wächter Zeitraum und Karenzzeit größer als das Intervall wählen
(z. B. 5 + 5 min).

## Externe Checks

Unter „Checks“ legt man Prüfungen an, die der Server selbst ausführt (der Agent bleibt dabei außen vor):

- **HTTP(S):** URL abrufen; erfolgreich bei Status 200–399 oder dem eingetragenen Status (z. B. 401 für
  eine Login-Seite). Bei HTTPS wird der Ablauf des Zertifikats ausgewertet, auch wenn es abgelaufen ist.
  „Zertifikat nicht prüfen“ erlaubt selbst signierte Dienste im LAN; der Ablauf wird trotzdem überwacht.
  Höchstens 5 Weiterleitungen, kein Proxy, Zugangsdaten in der URL sind nicht erlaubt.
- **Ping:** ICMP-Echo. Im Container braucht das unprivilegiertes ICMP (`net.ipv4.ping_group_range`
  muss die Gruppe des Servers enthalten, unter containerd üblich); sonst meldet der Check
  „ping nicht möglich“ – dann den TCP-Check nehmen.
- **TCP-Port:** Verbindung zu `host:port`, z. B. `nas.lan:22`.

Intervall 30–3600 s (Standard 60 s), Timeout 1–30 s. Die GUI zeigt letztes Ergebnis, Antwortzeit,
Restlaufzeit des Zertifikats, Verfügbarkeit der letzten 24 h und ein Diagramm der Antwortzeit. Die
Ergebnisse werden wie die Host-Historie 7 Tage aufbewahrt. Alarme: „Check“ (schlägt die Mindestdauer
lang fehl) und „Zertifikat“ (läuft in weniger als N Tagen ab) unter Einstellungen.

## k3s-Ansicht

Läuft der Server im Cluster (mit `deploy/k8s/rbac.yaml`), zeigt der Tab „Cluster“ alle 30 s den
Stand: Nodes mit CPU/RAM (metrics-server), ArgoCD-Apps mit Sync- und Health-Status, Workloads
und Pods (Filter nach Namespace, Suche, „nur Probleme“). Ein Klick auf einen Pod zeigt seine
letzten Log-Zeilen, auch die des vorherigen Containers nach einem Absturz. Die Ansicht ist nur
lesend; die API wird ohne client-go per HTTP angesprochen. Schreibend ist allein der optionale
Neustart mit drain (siehe unten).

Läuft auf einem Node zusätzlich ein Agent (systemd, wie auf jedem anderen Host), ordnet der Server
ihn über den Hostnamen (oder die Agent-ID) dem Node zu: Der Tab „Cluster“ zeigt dann CPU, RAM und
Temperatur vom Agenten, die Host-Liste markiert den Host mit „k3s“ samt Ready-Status.

**Neustart mit drain** (optional, `deploy/k8s/rbac-drain.yaml`): Mit der ClusterRole
`mat-server-drain` (nur `patch` auf Nodes und `create` auf `pods/eviction`) zeigt die GUI bei
k3s-Hosts mit freigeschaltetem Neustart „Neu starten mit drain“. Der Server sperrt den Node ab
(Annotation `mat/cordoned-by`), verdrängt alle Pods außer DaemonSets über die Eviction-API –
PodDisruptionBudgets (z. B. Longhorn) gelten, höchstens 10 Minuten –, startet den Host über den
Agenten neu, wartet auf neue Boot-Zeit und Node `Ready` (höchstens 15 Minuten) und gibt ihn wieder
frei. Klappt der drain nicht, wird der Node freigegeben und nicht neu gestartet; kommt der Host
nicht zurück, bleibt der Node abgesperrt (Nachricht). Währenddessen bleiben Alarme des Hosts und
seines Nodes still. Der Stand steht in der Datenbank: Muss der MAT-Server selbst umziehen, setzt
er nach dem Start fort. Freigegeben wird nur, was MAT selbst abgesperrt hat; ein schon
abgesperrter Node (z. B. durch den system-upgrade-controller) wird abgelehnt. Ob die Rechte da
sind, prüft der Server beim Start und stündlich (SelfSubjectAccessReview).

Alarme gibt es für Nodes, Workloads/Pods und ArgoCD-Apps. **Ausnahmen** (Muster `namespace/name`,
z. B. `kube-system/svclb-*`, voreingestellt für den k3s-ServiceLB neben MetalLB) werden weiter
angezeigt, lösen aber nichts aus. Zum Testen außerhalb des Clusters: `kubectl proxy` starten und
den Server mit `MAT_K8S_API=http://127.0.0.1:8001` aufrufen.

Neue k3s-Versionen holt der Server alle 6 h von `update.k3s.io` (`MAT_K3S_CHANNELS=off` schaltet ab):
Der Tab zeigt die Node-Versionen, den neuesten Patch im selben Zweig und `stable`. Aktualisiert wird
k3s nicht über das Tool, sondern z. B. mit dem system-upgrade-controller.

## HTTPS / wss

```powershell
# 1. Zertifikat erzeugen (IP und/oder Name, unter dem Agenten und Browser den Server erreichen)
mat-server gen-cert -hosts 192.168.100.10,monitor.lan -out data

# 2. Server mit TLS starten (alternativ MAT_TLS_CERT / MAT_TLS_KEY)
mat-server -listen 0.0.0.0:8443 -tls-cert data/server.crt -tls-key data/server.key

# 3. data/server.crt auf die Agenten kopieren und vertrauen
mat-agent -server wss://192.168.100.10:8443/ws/agent -ca server.crt
```

Das Zertifikat ist selbst signiert: Der Agent vertraut mit `-ca` genau diesem Zertifikat (keine
Prüfung wird abgeschaltet). Im Browser erscheint beim ersten Aufruf eine Warnung, bis das Zertifikat
dort als vertrauenswürdig importiert wird. Bei TLS bekommt das Session-Cookie automatisch `Secure`.

Läuft das Zertifikat in weniger als 30 Tagen ab, zeigt die GUI einen Hinweis; bei 30, 14, 7 und 1 Tag(en)
Rest und nach dem Ablauf geht je eine Nachricht an die Benachrichtigungskanäle. Ein neues Zertifikat
muss auf allen Agenten als `ca_file` hinterlegt werden (Self-Update verteilt es nicht).

## Agent-Updates (Self-Update)

Neue Agent-Versionen kommen als **signierte GitHub-Releases**. Der Server fragt alle 6 Stunden
`releases/latest` ab (oder sofort mit „Jetzt prüfen“ unter *Einstellungen*), prüft die Signatur und zeigt
veraltete Hosts an. Aktualisiert wird nur auf Knopfdruck – pro Host oder „Alle aktualisieren“.

Ablauf: Der Server lädt das passende Binary von GitHub und reicht es weiter; der Agent lädt es vom
Server (eigenes Token, gepinntes Zertifikat), **prüft die Signatur selbst** gegen den eingebauten
öffentlichen Schlüssel, außerdem Plattform, Größe, SHA-256 und dass die Version neuer ist (kein
Downgrade), ruft das neue Binary probehalber mit `version` auf, tauscht es aus (das alte bleibt als
`mat-agent.old` bzw. `mat-agent.old.exe` liegen) und beendet sich. Dienst-Manager bzw. systemd starten ihn neu.
Startet die neue Version dreimal, ohne eine Minute stabil verbunden zu sein, legt der Agent das alte
Binary zurück und meldet das in der GUI.

- Nur Agenten, die als Windows-Dienst oder unter systemd laufen, melden die Fähigkeit `update`;
  im Vordergrund gestartete Agenten bieten kein Update an.
- Agenten vor v1.0.0 können sich noch nicht selbst aktualisieren: einmal von Hand neu installieren
  (`service install` bzw. `install-agent.sh` mit dem neuen Binary).
- Der Server braucht ausgehend Zugriff auf `api.github.com` und `objects.githubusercontent.com`.
  `MAT_UPDATE_REPO=off` schaltet die Prüfung ab, `MAT_UPDATE_REPO=<owner>/<repo>` nimmt ein anderes Repo.
  Binaries werden neben der Datenbank zwischengespeichert (`<db-ordner>/updates/`).

### Release erstellen

```powershell
git tag v0.5.1
git push origin v0.5.1
```

Der Workflow [release.yml](.github/workflows/release.yml) testet, wartet auf die Freigabe in der
Environment `release`, baut alle Agenten, signiert `release.json` mit dem Secret `RELEASE_SIGNING_KEY`,
prüft die Signatur gegen den eingebauten Schlüssel und veröffentlicht den Release. Parallel baut
[server-image.yml](.github/workflows/server-image.yml) das Server-Image `:0.5.1`.

Einmalige Einrichtung des Schlüssels:

```powershell
go run ./server/cmd/mat-server release keygen -out $env:TEMP\release.key
# Ausgabe "öffentlicher schlüssel: …" in internal/release/key.go eintragen und committen
Get-Content $env:TEMP\release.key | gh secret set RELEASE_SIGNING_KEY --env release   # über stdin, nicht als Argument
Remove-Item $env:TEMP\release.key
```

In den Repo-Einstellungen: Environment `release` mit *Required reviewers* (nur du) und eine Tag-Regel
für `v*`, damit nur du Release-Tags anlegen kannst. Wer den privaten Schlüssel hat, kann allen Agenten
Code unterschieben – er liegt deshalb nur in dieser Environment.

## Prometheus / Grafana

`/metrics` ist nur aktiv, wenn der Server mit `MAT_METRICS_TOKEN` (mind. 16 Zeichen) gestartet wird.
Messwerte gibt es nur für verbundene Hosts; `mat_host_up` zeigt 0/1 für alle.

```yaml
scrape_configs:
  - job_name: monitoring-admin-tool
    scrape_interval: 15s
    authorization:
      credentials: "<MAT_METRICS_TOKEN>"
    static_configs:
      - targets: ["192.168.100.10:8080"]
```

Metriken (Präfix `mat_`): `alerts_firing` (je Host und Regel), `host_up`, `host_info`, `host_last_seen_timestamp_seconds`, `host_boot_time_seconds`,
`cpu_usage_percent`, `cpu_core_usage_percent`, `load1/5/15` (nicht unter Windows), `memory_total/used_bytes`,
`swap_total/used_bytes`, `network_{receive,transmit}_{bytes,packets}_per_second`, `network_{receive,transmit}_errors_total`,
`filesystem_size/used_bytes`, `filesystem_full_in_days` (Prognose, nur wachsende Mountpoints), `cpu_temperature_celsius` und `sensor_temperature_celsius` (je Sensor, nur Linux-Hosts mit Sensor).
Updates (M7): `updates_pending`, `updates_security_pending` (nur Hosts mit apt) und `reboot_required`.
SMART (Labels `device`, `model`): `smart_healthy`, `smart_temperature_celsius`, `smart_power_on_hours`,
`smart_reallocated_sectors`, `smart_pending_sectors`, `smart_percentage_used`, `smart_media_errors`.
Externe Checks: `check_up`, `check_latency_seconds`, `check_cert_expiry_timestamp_seconds` (Labels `check`, `name`, `kind`).

Architektur, Zielplattformen und Sicherheitsregeln: siehe [CLAUDE.md](CLAUDE.md).
