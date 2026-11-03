# Playbooks

Ein Playbook ist eine feste Reihenfolge freigeschalteter Aktionen über mehrere Hosts. Es bringt keine neuen
Möglichkeiten: Jeder Schritt ist ein Job aus der Whitelist, mit denselben Prüfungen wie ein Klick in der GUI.
Der Agent prüft weiterhin selbst, ob die Fähigkeit am Host freigeschaltet ist.

Die Dateien hier sind Beispiele. Der Server liest `*.yaml`/`*.yml` aus dem Ordner `MAT_PLAYBOOK_DIR`
(nicht rekursiv), beim Start und per „Neu laden“ im Tab „Playbooks“.

## Format

```yaml
name: amp-update-linux          # a-z, 0-9, -; eindeutig über alle Dateien
description: …                  # optional
targets: [amp-server, k3s]      # Host-ID (genau), Anzeigename oder Gruppe (ohne Groß-/Kleinschreibung)
mode: serial                    # serial (Standard) | parallel
max_parallel: 3                 # nur bei parallel, 1–10 (Standard 10)
on_error: stop                  # Standard für alle Schritte: stop (Standard) | continue
schedule: monthly first sun 03:00  # optional, siehe „Zeitplan“
steps:
  - id: pruefen                 # a-z, 0-9, -; eindeutig im Playbook
    action: command.run         # Aktion aus der Whitelist
    params: {name: amp-update-check}
    ok_exit_codes: [0, 10]      # nur bei command.run: diese Exit-Codes gelten als ok
  - id: update
    action: command.run
    params: {name: amp-update}
    when: {step: pruefen, exit_code: 0}   # oder {step: …, ok: true|false}
  - id: pause
    wait: 60s                   # Wartezeit, höchstens 1h
    on_error: continue          # überschreibt on_error des Playbooks
  - id: gesund
    gate: cluster_healthy       # warten, bis eine feste Prüfung erfüllt ist (siehe „Gates“)
    timeout: 20m                # Standard 10m, höchstens 1h
```

**Gates** warten, bis eine Prüfung 30 s lang ununterbrochen erfüllt ist, und schlagen nach `timeout` fehl
(dann gilt `on_error`). Gezählt werden nur Daten, die nach dem Beginn des Schritts eingegangen sind:

| Gate | erfüllt, wenn |
|------|---------------|
| `host_healthy` | Host verbunden, überwachte Dienste laufen, keine systemd-Unit `failed`, kein offener SMART- oder Platten-Alarm |
| `cluster_healthy` | alle Nodes bereit, alle Workloads mit gewünschter Zahl bereiter Pods, kein Pod im Fehlerzustand (Ausnahmen wie bei den Cluster-Alarmen) |
| `check_ok` + `check: <Name>` | der externe Check mit diesem Namen meldet ok |

Kann ein Gate nie erfüllt werden (keine k3s-Anbindung, Check unbekannt oder abgeschaltet), schlägt es sofort fehl.

**Freigaben** halten den Lauf an, bis jemand in der GUI (Laufansicht) „Freigeben“ oder „Ablehnen“ klickt:

```yaml
  - id: freigabe
    approve: "node1 ist durch – mit den übrigen weitermachen?"   # höchstens 200 Zeichen
    once: true          # eine Entscheidung gilt für alle Hosts des Laufs (sonst je Host)
    timeout: 24h        # Standard 24h, höchstens 7 Tage
```

Beim Warten geht eine Nachricht über ntfy/E-Mail raus (bei `once` einmal je Lauf). Ablehnen und Fristablauf
gelten als Fehler (`on_error` entscheidet). Wer entschieden hat, steht beim Schritt und im Log. Freigeben kann
jeder angemeldete Benutzer; ein erneutes Passwort wird nicht verlangt (wie beim Start eines Playbooks).

**Aktionen:** `service.start|stop|restart`, `packages.refresh`, `packages.upgrade`, `package.install`,
`user.create`, `system.reboot`, `log.read`, `command.run` und `k3s.drain_reboot`. Die Parameter prüft der
Server genau wie bei einem Job aus der GUI. `agent.update` ist nicht erlaubt (eigener Ablauf).

**Bedingungen** gibt es nur in zwei Formen und nur auf frühere Schritte desselben Hosts: Exit-Code eines
`command.run`-Schritts oder `ok: true/false`. Ein übersprungener Schritt erfüllt keine Bedingung und zählt
nicht als Fehler. Freie Ausdrücke, Variablen und Schleifen gibt es nicht.

**Grenzen:** 50 Schritte, 50 Ziele, 64 Dateien zu je 64 KiB. `on_error: continue` ist bei `system.reboot`,
`k3s.drain_reboot` und `packages.upgrade` verboten: Nach einem fehlgeschlagenen Upgrade soll nie einfach
neu gestartet werden.

**Zeitplan:** `daily 04:30`, `weekly sun 03:00` (auch `weekly sat,sun 03:00`), `monthly 15 02:00` (Tag 1–28) oder
`monthly first sun 03:00` (`first`, `second`, `third`, `fourth`, `last`). Kein cron: Dort werden Tag im Monat und
Wochentag mit ODER verknüpft, „erster Sonntag“ geht nicht. Es gilt die Zeitzone des Servers (`TZ`, wie bei den
Wartungsfenstern). Gestartet wird als Benutzer `zeitplan` mit denselben Prüfungen wie ein Start in der GUI;
verhindert etwas den Start (z. B. ein laufender Job), fällt der Termin mit einer Nachricht aus. Ein neuer Zeitplan
startet erst zum nächsten Termin, nie rückwirkend. War der Server zum Termin aus, läuft der Plan noch, wenn er
innerhalb einer Stunde zurück ist; sonst fällt der Termin mit einer Nachricht aus. Einschalten geht nur über die
Datei; in der GUI lässt sich ein Zeitplan nur pausieren und fortsetzen. Ein Zeitplan läuft auch außerhalb von
Wartungsfenstern – wer das nicht will, setzt `gate: host_healthy` und `approve` an den Anfang.

## Ablauf

- Die Ziele werden beim Start zu Hosts aufgelöst und festgehalten; der Lauf arbeitet mit einer Kopie des
  Playbooks, eine geänderte Datei ändert ihn nicht.
- `serial`: ein Host nach dem anderen. `parallel`: bis `max_parallel` Hosts gleichzeitig. Auf einem Host laufen
  die Schritte immer nacheinander.
- Fehler mit `stop`: Der Host endet, und es startet **kein weiterer Host**; laufende Hosts machen ihren Schritt
  fertig. Der Lauf endet als „angehalten“. Mit `continue` wird der Fehler vermerkt und es geht weiter.
- Ist ein Host offline, wartet der Schritt bis zu 2 Minuten auf die Verbindung und schlägt dann fehl. Fehlt
  eine Fähigkeit oder ein Befehl, schlägt er sofort fehl (der Probelauf zeigt das vorher).
- **Probelauf** (Knopf im Tab, auch im Startdialog): Tabelle Host × Schritt mit der Einschätzung nach heutigem
  Stand – Fähigkeit und Befehl vorhanden, Host online, Node für den drain, ausstehende Updates, Gates jetzt
  erfüllt – und was einen Start gerade verhindert. Es wird nichts gesendet und nichts gespeichert.
- Nach `system.reboot` wartet der Lauf, bis der Host mit neuer Boot-Zeit zurück ist (höchstens 15 min).
- Pro Playbook läuft höchstens ein Lauf, ein Host gehört zu höchstens einem Lauf. Ein offener ändernder Job auf
  einem Zielhost oder ein laufender Neustart mit drain verhindert den Start.
- „Abbrechen“ beendet den Lauf nach dem aktuellen Schritt; ein laufender Job läuft zu Ende, eine Wartezeit endet sofort.
- Der Stand steht in der Datenbank. Startet der Server neu (z. B. weil sein Pod beim drain umzieht), setzt er den
  Lauf fort und wartet auf den bereits gestarteten Job, statt ihn erneut auszulösen. Lässt sich das nicht sicher
  feststellen, endet der Lauf als „unterbrochen“.
- Jeder Schritt steht als Job im Verlauf des Hosts (Von: Benutzer, der den Lauf gestartet hat). Läufe bleiben
  90 Tage. Am Ende geht eine Nachricht über ntfy/E-Mail raus (bei Fehlern mit hoher Priorität).

## AMP aktualisieren

### Windows (gameserver3)

`amp-update.yaml` ruft das Skript `C:\mat-scripts\amp-update.ps1` auf (nicht in diesem Repo). Es prüft selbst,
ob SYSTEM die Instanzen sieht, ob die MSI gültig signiert ist und ob sie neuer ist; sonst ändert es nichts.
Exit-Codes: 0 = ok bzw. nichts zu tun, 1 = Fehler, 2 = Vorprüfung fehlgeschlagen (nichts geändert).

```json
"commands": {
  "amp-update": {
    "argv": ["C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
             "-NoProfile", "-NonInteractive", "-NoLogo", "-ExecutionPolicy", "RemoteSigned",
             "-File", "C:\\mat-scripts\\amp-update.ps1"],
    "timeout_sec": 1800,
    "description": "AMP aktualisieren"
  }
}
```

`C:\mat-scripts` darf nur für SYSTEM und Administratoren beschreibbar sein, sonst kann jeder mit Schreibrecht
Code als SYSTEM ausführen lassen.

### Linux

`amp-update-linux.yaml` mit den Skripten aus `scripts/`:

| Skript | Exit-Codes |
|--------|------------|
| `amp-update-check.sh` | 0 = Update für `ampinstmgr` verfügbar (oder anderer Release-Stream als Mainline), 10 = aktuell, 2 = Fehler (kein apt, `apt-get update` fehlgeschlagen, nicht installiert) |
| `amp-update.sh` | Exit-Code von `getamp update` (Tools-Update und danach `ampinstmgr upgradeall` als Benutzer `amp`) |

Einrichten (als root):

```sh
install -d -o root -g root -m 755 /opt/mat-scripts
install -o root -g root -m 750 amp-update-check.sh amp-update.sh /opt/mat-scripts/
```

```json
"commands": {
  "amp-update-check": { "argv": ["/opt/mat-scripts/amp-update-check.sh"], "timeout_sec": 300,  "description": "AMP: Update verfügbar?" },
  "amp-update":       { "argv": ["/opt/mat-scripts/amp-update.sh"],       "timeout_sec": 1800, "description": "AMP aktualisieren" }
}
```

Danach den Agent-Dienst neu starten; `mat-agent check -config …` zeigt die erkannten Befehle. Die Exit-Codes
meldet der Agent ab v1.6.0; ältere melden jeden Fehler als 1, dann hält der Lauf bei „kein Update“ an.

Bekannte Lücke: Löst das AMP-Webinterface das Tools-Update selbst aus, meldet die Prüfung „aktuell“, obwohl
`upgradeall` für ADS und Instanzen vielleicht noch aussteht. Dann den Prüfschritt weglassen: `getamp update` ist
wiederholbar. Ob ein Update laufende Spielserver stoppt, ist nicht geprüft – zuerst auf einer Testinstanz und
wenn niemand spielt.
