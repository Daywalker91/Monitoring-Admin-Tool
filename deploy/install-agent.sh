#!/bin/sh
# Installiert den Homelab-Monitor-Agenten als systemd-Dienst (Linux, auch Raspberry Pi).
#
#   sudo sh install-agent.sh --server wss://monitor.lan:8443/ws/agent [--ca server.crt] [--id name]
#   sudo sh install-agent.sh                 # nur Programm aktualisieren, Konfiguration bleibt
#   sudo sh install-agent.sh --uninstall [--purge]
#
# Das Token wird abgefragt (oder aus MAT_AGENT_TOKEN gelesen), damit es nicht in
# der Prozessliste oder der Shell-History landet. Das passende Programm
# (mat-agent-linux-amd64|arm64|armv7) wird neben diesem Skript oder in ../dist gesucht.
#
# Zum Testen ohne root: DESTROOT=/tmp/x MAT_NO_SYSTEMCTL=1 sh install-agent.sh …
set -eu

DESTROOT=${DESTROOT:-}
BIN="$DESTROOT/usr/local/bin/mat-agent"
CONF_DIR="$DESTROOT/etc/mat-agent"
CONF="$CONF_DIR/agent.json"
UNIT="$DESTROOT/etc/systemd/system/mat-agent.service"
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

die() { echo "fehler: $*" >&2; exit 1; }
info() { echo "==> $*"; }

systemctl_() {
	if [ "${MAT_NO_SYSTEMCTL:-}" = 1 ]; then
		echo "(übersprungen) systemctl $*"
	else
		systemctl "$@"
	fi
}

usage() {
	sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

server= ca= id= binary= uninstall= purge=
while [ $# -gt 0 ]; do
	case "$1" in
	--server) server=${2:?}; shift 2 ;;
	--ca) ca=${2:?}; shift 2 ;;
	--id) id=${2:?}; shift 2 ;;
	--binary) binary=${2:?}; shift 2 ;;
	--uninstall) uninstall=1; shift ;;
	--purge) purge=1; shift ;;
	-h | --help) usage ;;
	*) echo "unbekannte option: $1" >&2; usage 2 ;;
	esac
done

if [ -z "$DESTROOT" ] && [ "$(id -u)" -ne 0 ]; then
	die "bitte als root ausführen (sudo)"
fi
if [ "${MAT_NO_SYSTEMCTL:-}" != 1 ] && ! command -v systemctl >/dev/null 2>&1; then
	die "systemd nicht gefunden"
fi

# --- Deinstallation ---------------------------------------------------------
if [ -n "$uninstall" ]; then
	info "dienst stoppen und entfernen"
	if [ -f "$UNIT" ]; then
		systemctl_ disable --now mat-agent || true
		rm -f "$UNIT"
		systemctl_ daemon-reload
	fi
	rm -f "$BIN"
	if [ -n "$purge" ]; then
		rm -rf "$CONF_DIR"
		info "konfiguration $CONF_DIR gelöscht"
	else
		info "konfiguration bleibt in $CONF_DIR (löschen mit --purge)"
	fi
	exit 0
fi

# --- Eingaben prüfen (sie landen in einer JSON-Datei) -----------------------
case "$server" in
"") ;;
ws://* | wss://*) ;;
*) die "--server muss mit ws:// oder wss:// beginnen" ;;
esac
# Keine Anführungszeichen, Backslashes oder Leerzeichen: so bleibt das JSON gültig.
if printf '%s' "$server" | grep -q '["\\[:space:][:cntrl:]]'; then
	die "--server enthält unzulässige zeichen"
fi
if [ -n "$id" ] && ! printf '%s' "$id" | grep -Eq '^[A-Za-z0-9._-]{1,64}$'; then
	die "--id darf nur buchstaben, ziffern, '.', '_' und '-' enthalten (max. 64)"
fi
if [ -n "$ca" ] && [ ! -f "$ca" ]; then
	die "ca-datei $ca nicht gefunden"
fi
if [ -n "$ca" ]; then
	case "$server" in
	wss://*) ;;
	"") ;;
	*) die "--ca ist nur mit wss:// sinnvoll" ;;
	esac
fi
if [ -z "$server" ] && [ ! -f "$CONF" ]; then
	die "noch keine konfiguration vorhanden – --server angeben"
fi

# --- Programm für diese Architektur finden ----------------------------------
if [ -z "$binary" ]; then
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	armv7l | armv8l) arch=armv7 ;;
	*) die "architektur $(uname -m) wird nicht unterstützt (amd64, arm64, armv7)" ;;
	esac
	for dir in "$SCRIPT_DIR" "$SCRIPT_DIR/../dist"; do
		if [ -f "$dir/mat-agent-linux-$arch" ]; then
			binary="$dir/mat-agent-linux-$arch"
			break
		fi
	done
	[ -n "$binary" ] || die "mat-agent-linux-$arch nicht gefunden (neben dem skript oder in ../dist; sonst --binary)"
fi
[ -f "$binary" ] || die "$binary nicht gefunden"

unit_src=
for f in "$SCRIPT_DIR/mat-agent.service" "$SCRIPT_DIR/systemd/mat-agent.service"; do
	if [ -f "$f" ]; then unit_src=$f; break; fi
done
[ -n "$unit_src" ] || die "mat-agent.service nicht gefunden (neben dem skript oder in systemd/)"

# --- Konfiguration -----------------------------------------------------------
umask 077
mkdir -p "$CONF_DIR"
chmod 0700 "$CONF_DIR"

if [ -n "$server" ]; then
	token=${MAT_AGENT_TOKEN:-}
	if [ -z "$token" ]; then
		[ -t 0 ] || die "kein terminal für die token-abfrage – MAT_AGENT_TOKEN setzen"
		printf 'agent-token: '
		stty -echo
		read -r token || true
		stty echo
		echo
	fi
	[ -n "$token" ] || die "token fehlt"
	if printf '%s' "$token" | grep -q '["\\[:cntrl:]]'; then
		die "token enthält unzulässige zeichen"
	fi

	ca_json=
	if [ -n "$ca" ]; then
		install -m 0644 "$ca" "$CONF_DIR/server.crt"
		# Pfad im Zielsystem, ohne DESTROOT.
		ca_json=",
  \"ca_file\": \"/etc/mat-agent/server.crt\""
	fi
	id_json=
	if [ -n "$id" ]; then
		id_json=",
  \"agent_id\": \"$id\""
	fi
	tmp="$CONF.tmp"
	cat >"$tmp" <<EOF
{
  "server_url": "$server",
  "token": "$token"$id_json$ca_json
}
EOF
	chmod 0600 "$tmp"
	mv "$tmp" "$CONF"
	info "konfiguration geschrieben: $CONF"
else
	info "vorhandene konfiguration bleibt: $CONF"
fi

# Konfiguration mit dem neuen Programm prüfen, bevor etwas ersetzt wird.
# Im Testmodus (DESTROOT) zeigt ca_file auf den Zielpfad; für die Prüfung umschreiben.
check_conf=$CONF
if [ -n "$DESTROOT" ] && grep -q '"/etc/mat-agent/server.crt"' "$CONF"; then
	check_conf="$CONF.check"
	sed "s#\"/etc/mat-agent/server.crt\"#\"$CONF_DIR/server.crt\"#" "$CONF" >"$check_conf"
fi
"$binary" check -config "$check_conf" || die "konfiguration ungültig"
[ "$check_conf" = "$CONF" ] || rm -f "$check_conf"

# --- Programm und Dienst ------------------------------------------------------
umask 022
info "programm installieren: $BIN"
mkdir -p "$(dirname "$BIN")" "$(dirname "$UNIT")"
install -m 0755 "$binary" "$BIN.new"
mv "$BIN.new" "$BIN"
install -m 0644 "$unit_src" "$UNIT"

systemctl_ daemon-reload
systemctl_ enable mat-agent
systemctl_ restart mat-agent
info "fertig – status: systemctl status mat-agent, log: journalctl -u mat-agent -f"
