package proto

import (
	"strings"
	"time"
)

// Aktionen, die Parameter aus der GUI annehmen. Die Parameter landen in
// Befehlszeilen von Paketverwaltungen und Benutzerprogrammen, deshalb sind die
// Prüfungen eng; der Agent prüft zusätzlich je Paketverwaltung bzw. Plattform.
const (
	// ActionPackageInstall installiert ein Paket (Fähigkeit CapPackages). Parameter: name.
	ActionPackageInstall = "package.install"
	// ActionUserCreate legt einen Benutzer ohne nutzbares Passwort an (Fähigkeit
	// CapUsers). Parameter: name, optional sudo ("true"/"false"). Passwörter
	// gehen nie über Server, GUI oder Job-Parameter.
	ActionUserCreate = "user.create"
)

const (
	maxPackageName = 100
	maxUserName    = 32
)

func init() {
	ActionCapability[ActionPackageInstall] = CapPackages
	ActionCapability[ActionUserCreate] = CapUsers
	actionParams[ActionPackageInstall] = map[string]Param{"name": {Valid: ValidPackageName}}
	actionParams[ActionUserCreate] = map[string]Param{
		"name": {Valid: ValidUserName},
		"sudo": {Valid: validBool, Optional: true},
	}
	actionTimeout[ActionPackageInstall] = 30 * time.Minute
	actionTimeout[ActionUserCreate] = 2 * time.Minute
}

// ValidPackageName prüft einen Paketnamen. Nicht erlaubt sind führende
// Bindestriche (Optionen), ein Bindestrich am Ende (apt und zypper entfernen
// das Paket dann!), Platzhalter, Pfade, Versionsangaben und Paketdateien.
// Groß/klein und ":" regelt der Agent je Paketverwaltung.
func ValidPackageName(s string) bool {
	if s == "" || len(s) > maxPackageName {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '+' || r == '.' || r == '_' || r == ':' || r == '-'):
		default:
			return false
		}
	}
	if strings.ContainsRune("-.:", rune(s[len(s)-1])) {
		return false
	}
	lower := strings.ToLower(s)
	for _, ext := range []string{".deb", ".rpm", ".apk", ".msi", ".exe"} {
		if strings.HasSuffix(lower, ext) {
			return false
		}
	}
	return true
}

// reservedUsers sind Namen, die nie über einen Job angelegt werden (Linux und Windows).
var reservedUsers = map[string]bool{
	"root": true, "daemon": true, "bin": true, "sys": true, "sync": true, "games": true, "man": true, "lp": true,
	"mail": true, "news": true, "uucp": true, "proxy": true, "www-data": true, "backup": true, "list": true,
	"irc": true, "nobody": true, "nogroup": true, "sshd": true, "messagebus": true, "polkitd": true, "wheel": true,
	"sudo": true, "adm": true, "admin": true, "administrator": true, "administratoren": true, "guest": true,
	"gast": true, "system": true, "defaultaccount": true, "wdagutilityaccount": true,
}

// ValidUserName prüft einen Benutzernamen: ^[a-z_][a-z0-9_-]{0,31}$, ohne
// reservierte Namen. Der Zeichenvorrat enthält nichts, was unter Windows
// Sonderbedeutung hat; dort gilt im Agenten zusätzlich die Grenze von 20 Zeichen.
func ValidUserName(s string) bool {
	if s == "" || len(s) > maxUserName || reservedUsers[s] {
		return false
	}
	if strings.HasPrefix(s, "systemd-") || strings.HasPrefix(s, "_") {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func validBool(s string) bool { return s == "true" || s == "false" }
