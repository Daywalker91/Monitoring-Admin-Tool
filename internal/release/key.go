package release

// publicKey ist der öffentliche Signaturschlüssel der Releases (base64, 32 Byte),
// erzeugt mit `mat-server release keygen`. Der private Schlüssel liegt nur als
// Secret RELEASE_SIGNING_KEY in der GitHub-Environment "release".
//
// Leer = Self-Update ist abgeschaltet. Für Test-Builds überschreibbar per
// -ldflags "-X github.com/Daywalker91/Monitoring-Admin-Tool/internal/release.publicKey=…".
//
// Schlüsselwechsel: neuen Schlüssel hier eintragen und ein Release bauen. Agenten
// mit dem alten Schlüssel nehmen es nicht an und müssen einmal von Hand
// aktualisiert werden.
var publicKey = "evfJAffftJjfPumFViqOnH4S7HMsUdBta6CLSmMIwo0="
