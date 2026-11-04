#!/bin/sh
# Aktualisiert Instance Manager, ADS und alle Instanzen: getamp update erledigt
# das Tools-Update und danach "ampinstmgr upgradeall" als Benutzer amp.
# Ablage: /opt/mat-scripts/, Eigentümer root, chmod 750 (läuft als root).
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# Der Agent startet im Ordner des Skripts; apt (als _apt) und ampinstmgr (als amp)
# dürfen den nicht unbedingt betreten.
cd /
# getamp wechselt für apt zu _apt und für upgradeall zu amp. Fehlt dem Dienst
# CAP_SETUID (Bit 7 in CapEff), ginge das halb durch: lieber gar nicht anfangen.
capeff=$(awk '/^CapEff:/ {print $2}' /proc/self/status 2>/dev/null || true)
if [ -n "$capeff" ] && [ $(( 0x$capeff >> 7 & 1 )) -eq 0 ]; then
  echo "CAP_SETUID fehlt: getamp kann nicht zum Benutzer amp wechseln, nichts geändert."
  echo "Ursache am Host beheben (alle systemd-Dienste ohne CAP_SETUID)."
  exit 2
fi
exec getamp update
