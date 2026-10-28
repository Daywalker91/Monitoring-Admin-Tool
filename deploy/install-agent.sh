#!/bin/sh
# Installiert den Homelab-Monitor-Agenten als systemd-Dienst (Linux, auch Raspberry Pi).
#
#   sudo sh install-agent.sh --server wss://monitor.lan:8443/ws/agent [--ca server.crt] [--id name]
#   sudo sh install-agent.sh                 # nur Programm aktualisieren, Konfiguration bleibt
#   sudo sh install-agent.sh --uninstall [--purge]
#   weitere Optionen: --version v0.5.0 | --download | --binary <datei>
#
# Das Token wird abgefragt (oder aus MAT_AGENT_TOKEN gelesen), damit es nicht in
# der Prozessliste oder der Shell-History landet. Das passende Programm
# (mat-agent-linux-amd64|arm64|armv7) wird neben diesem Skript oder in ../dist
# gesucht; fehlt es (oder mit --download/--version), lädt das Skript es aus dem
# GitHub-Release (neueste Version bzw. --version) und prüft die SHA-256-Summe aus
# release.json. Nur über HTTPS (curl oder wget).
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
	sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

# Quelle der Releases; MAT_RELEASE_URL nur für Tests oder Forks.
RELEASES=${MAT_RELEASE_URL:-https://github.com/Daywalker91/Monitoring-Admin-Tool/releases}

server= ca= id= binary= uninstall= purge= version= download=
while [ $# -gt 0 ]; do
	case "$1" in
	--server) server=${2:?}; shift 2 ;;
	--ca) ca=${2:?}; shift 2 ;;
	--id) id=${2:?}; shift 2 ;;
	--binary) binary=${2:?}; shift 2 ;;
	--version) version=${2:?}; download=1; shift 2 ;;
	--download) download=1; shift ;;
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
if [ -n "$version" ] && ! printf '%s' "$version" | grep -Eq '^v[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$'; then
	die "--version im format v1.2.3 angeben"
fi
if [ -n "$binary" ] && [ -n "$download" ]; then
	die "--binary und --download/--version schließen sich aus"
fi
if [ -z "$server" ] && [ ! -f "$CONF" ]; then
	die "noch keine konfiguration vorhanden – --server angeben"
fi

# --- Programm für diese Architektur finden oder laden ------------------------
tmpdir=
cleanup() { [ -z "$tmpdir" ] || rm -rf "$tmpdir"; }
trap cleanup EXIT

# fetch lädt eine Release-Datei; ohne MAT_RELEASE_URL nur über HTTPS.
fetch() {
	if [ -n "$version" ]; then url="$RELEASES/download/$version/$1"; else url="$RELEASES/latest/download/$1"; fi
	if command -v curl >/dev/null 2>&1; then
		if [ -n "${MAT_RELEASE_URL:-}" ]; then
			curl -fsSL -o "$2" "$url"
		else
			curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$url"
		fi
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$url"
	else
		die "weder curl noch wget gefunden – programm von hand laden und --binary angeben"
	fi || die "laden fehlgeschlagen: $url"
}

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
armv7l | armv8l) arch=armv7 ;;
*) die "architektur $(uname -m) wird nicht unterstützt (amd64, arm64, armv7)" ;;
esac
name="mat-agent-linux-$arch"

if [ -z "$binary" ] && [ -z "$download" ]; then
	for dir in "$SCRIPT_DIR" "$SCRIPT_DIR/../dist"; do
		if [ -f "$dir/$name" ]; then
			binary="$dir/$name"
			break
		fi
	done
fi
if [ -z "$binary" ]; then
	tmpdir=$(mktemp -d)
	info "lade $name (${version:-neueste version}) von $RELEASES"
	fetch release.json "$tmpdir/release.json"
	fetch "$name" "$tmpdir/$name"
	# Prüfsumme aus release.json (ein Feld je Zeile, "sha256" folgt auf "name").
	want=$(awk -v n="\"$name\"" '$1 == "\"name\":" && $2 ~ "^" n {f=1} f && $1 == "\"sha256\":" {gsub(/[",]/, "", $2); print $2; exit}' "$tmpdir/release.json")
	got=$(sha256sum "$tmpdir/$name" | cut -d' ' -f1)
	[ -n "$want" ] || die "$name steht nicht in release.json"
	[ "$want" = "$got" ] || die "prüfsumme von $name stimmt nicht – abbruch"
	rel=$(awk '$1 == "\"version\":" {gsub(/[",]/, "", $2); print $2; exit}' "$tmpdir/release.json")
	info "prüfsumme ok ($rel)"
	chmod 0755 "$tmpdir/$name"
	binary="$tmpdir/$name"
fi
[ -f "$binary" ] || die "$binary nicht gefunden"

unit_src=
for f in "$SCRIPT_DIR/mat-agent.service" "$SCRIPT_DIR/systemd/mat-agent.service"; do
	if [ -f "$f" ]; then unit_src=$f; break; fi
done
if [ -z "$unit_src" ]; then
	[ -n "$tmpdir" ] || tmpdir=$(mktemp -d)
	fetch mat-agent.service "$tmpdir/mat-agent.service"
	unit_src="$tmpdir/mat-agent.service"
fi

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
