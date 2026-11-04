#!/bin/sh
# Aktualisiert Instance Manager, ADS und alle Instanzen: getamp update erledigt
# das Tools-Update und danach "ampinstmgr upgradeall" als Benutzer amp.
# Ablage: /opt/mat-scripts/, Eigentümer root, chmod 750 (läuft als root).
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# Der Agent startet im Ordner des Skripts; apt (als _apt) und ampinstmgr (als amp)
# dürfen den nicht unbedingt betreten.
cd /
exec getamp update
