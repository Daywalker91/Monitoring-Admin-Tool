#!/bin/sh
# Aktualisiert Instance Manager, ADS und alle Instanzen: getamp update erledigt
# das Tools-Update und danach "ampinstmgr upgradeall" als Benutzer amp.
# Ablage: /opt/mat-scripts/, Eigentümer root, chmod 750 (läuft als root).
set -eu
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
exec getamp update
