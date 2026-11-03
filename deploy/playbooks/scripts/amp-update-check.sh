#!/bin/sh
# Prüft, ob es ein Update für den AMP Instance Manager gibt (nur apt-Systeme).
# Exit 0 = Update verfügbar, 10 = nichts zu tun, alles andere = Fehler.
# Ablage: /opt/mat-scripts/, Eigentümer root, chmod 750 (läuft als root).
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# Andere Release-Streams als Mainline stehen nicht im Paketmanager: kein
# Vergleich möglich, ein Update wird angenommen (getamp update ist wiederholbar).
STREAM_FILE=/home/amp/.amp-tools-update-stream
if [ -f "$STREAM_FILE" ]; then
  stream=$(tr -dc '[:alnum:]' < "$STREAM_FILE")
  if [ -n "$stream" ] && [ "$stream" != "Mainline" ]; then
    echo "Stream $stream: kein Vergleich möglich, Update wird angenommen"; exit 0
  fi
fi

command -v apt-get >/dev/null 2>&1 || { echo "kein apt: Skript nicht unterstützt"; exit 2; }
if ! out=$(apt-get update -qq 2>&1); then
  echo "apt-get update fehlgeschlagen:"; printf '%s\n' "$out" | tail -n 20; exit 2
fi
inst=$(apt-cache policy ampinstmgr | awk '/Installed:/ {print $2}')
cand=$(apt-cache policy ampinstmgr | awk '/Candidate:/ {print $2}')
if [ -z "$inst" ] || [ "$inst" = "(none)" ]; then echo "ampinstmgr nicht installiert"; exit 2; fi
if [ -n "$cand" ] && [ "$cand" != "(none)" ] && [ "$inst" != "$cand" ]; then
  echo "ampinstmgr: $inst -> $cand"; exit 0
fi
echo "ampinstmgr aktuell ($inst)"; exit 10
