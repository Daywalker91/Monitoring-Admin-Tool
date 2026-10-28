// Package release beschreibt signierte Agent-Releases für das Self-Update.
//
// Ein Release besteht aus den Agent-Binaries, dem Manifest release.json
// (Version, Dateiname, Plattform, Größe, SHA-256 je Binary) und der Signatur
// release.json.sig (ed25519 über die Bytes von release.json, base64).
//
// Signiert wird im Release-Workflow mit einem privaten Schlüssel, der nur als
// GitHub-Secret existiert. Agent und Server prüfen gegen den eingebauten
// öffentlichen Schlüssel; der Server kann also keine Updates erfinden, nur
// vorhandene weiterreichen.
package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Dateinamen im Release.
const (
	ManifestName  = "release.json"
	SignatureName = "release.json.sig"
)

// Obergrenzen für Downloads (Schutz vor übergroßen Antworten).
const (
	MaxManifestBytes  = 64 << 10
	MaxSignatureBytes = 1 << 10
	MaxBinaryBytes    = 64 << 20
)

// ErrNoKey bedeutet, dass kein öffentlicher Schlüssel eingebaut ist.
var ErrNoKey = errors.New("kein signaturschlüssel eingebaut – self-update ist abgeschaltet")

// Enabled meldet, ob ein öffentlicher Schlüssel eingebaut ist.
func Enabled() bool { return publicKey != "" }

// Manifest beschreibt ein Release.
type Manifest struct {
	Version string `json:"version"` // z. B. "v0.5.0"
	Files   []File `json:"files"`
}

// File ist ein Agent-Binary für eine Plattform.
type File struct {
	Name   string `json:"name"` // z. B. "mat-agent-linux-armv7"
	OS     string `json:"os"`   // GOOS
	Arch   string `json:"arch"` // GOARCH
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // hex, klein
}

var (
	reName   = regexp.MustCompile(`^mat-agent-[a-z0-9]+-[a-z0-9]+(\.exe)?$`)
	reSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reGoName = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
)

// Validate prüft das Manifest auf gültige, eindeutige Einträge.
func (m *Manifest) Validate() error {
	if _, err := ParseVersion(m.Version); err != nil {
		return err
	}
	if len(m.Files) == 0 || len(m.Files) > 32 {
		return errors.New("manifest: ungültige anzahl dateien")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		switch {
		case !reName.MatchString(f.Name):
			return fmt.Errorf("manifest: ungültiger dateiname %q", f.Name)
		case !reGoName.MatchString(f.OS) || !reGoName.MatchString(f.Arch):
			return fmt.Errorf("manifest: ungültige plattform bei %q", f.Name)
		case f.Size <= 0 || f.Size > MaxBinaryBytes:
			return fmt.Errorf("manifest: ungültige größe bei %q", f.Name)
		case !reSHA256.MatchString(f.SHA256):
			return fmt.Errorf("manifest: ungültige prüfsumme bei %q", f.Name)
		}
		key := f.OS + "/" + f.Arch
		if seen[f.Name] || seen[key] {
			return fmt.Errorf("manifest: %q doppelt", f.Name)
		}
		seen[f.Name], seen[key] = true, true
	}
	return nil
}

// For liefert das Binary für eine Plattform.
func (m *Manifest) For(goos, goarch string) (File, bool) {
	for _, f := range m.Files {
		if f.OS == goos && f.Arch == goarch {
			return f, true
		}
	}
	return File{}, false
}

// File liefert den Eintrag mit diesem Dateinamen.
func (m *Manifest) File(name string) (File, bool) {
	for _, f := range m.Files {
		if f.Name == name {
			return f, true
		}
	}
	return File{}, false
}

// Verify prüft die Signatur des Manifests mit dem eingebauten Schlüssel und
// liefert das geprüfte Manifest.
func Verify(manifest, sig []byte) (Manifest, error) {
	if publicKey == "" {
		return Manifest{}, ErrNoKey
	}
	pub, err := ParsePublicKey(publicKey)
	if err != nil {
		return Manifest{}, fmt.Errorf("eingebauter schlüssel: %w", err)
	}
	return VerifyWith(pub, manifest, sig)
}

// VerifyWith prüft die Signatur mit einem bestimmten Schlüssel.
func VerifyWith(pub ed25519.PublicKey, manifest, sig []byte) (Manifest, error) {
	var m Manifest
	if len(manifest) > MaxManifestBytes || len(sig) > MaxSignatureBytes {
		return m, errors.New("manifest oder signatur zu groß")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return m, errors.New("signatur unlesbar")
	}
	if !ed25519.Verify(pub, manifest, raw) {
		return m, errors.New("signatur ungültig")
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return m, fmt.Errorf("manifest lesen: %w", err)
	}
	return m, m.Validate()
}

// Sign signiert die Manifest-Bytes und liefert die Signaturdatei (base64 + Zeilenende).
func Sign(priv ed25519.PrivateKey, manifest []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)) + "\n")
}

// GenerateKey erzeugt ein Schlüsselpaar im Textformat (je base64): den privaten
// Schlüssel als 32-Byte-Seed, den öffentlichen als 32 Byte.
func GenerateKey() (priv, pub string, err error) {
	p, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("schlüssel erzeugen: %w", err)
	}
	return base64.StdEncoding.EncodeToString(k.Seed()), base64.StdEncoding.EncodeToString(p), nil
}

// ParsePrivateKey liest einen privaten Schlüssel (base64-Seed).
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("privater schlüssel unlesbar")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// ParsePublicKey liest einen öffentlichen Schlüssel (base64).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("öffentlicher schlüssel unlesbar")
	}
	return ed25519.PublicKey(raw), nil
}

// Version ist eine Release-Version major.minor.patch.
type Version [3]int

var reVersion = regexp.MustCompile(`^v?(\d{1,4})\.(\d{1,4})\.(\d{1,4})$`)

// ParseVersion liest eine Release-Version wie "v0.5.0" oder "0.5.0". Zusätze
// (z. B. "-rc1") sind nicht erlaubt: Self-Update verteilt nur echte Releases.
func ParseVersion(s string) (Version, error) {
	m := reVersion.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("ungültige version %q", s)
	}
	var v Version
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, nil
}

// Compare liefert -1, 0 oder 1.
func (v Version) Compare(o Version) int {
	for i := range v {
		switch {
		case v[i] < o[i]:
			return -1
		case v[i] > o[i]:
			return 1
		}
	}
	return 0
}

// String liefert "vX.Y.Z".
func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]) }

// Newer meldet, ob candidate (eine Release-Version) neuer ist als current.
// current darf ein git-describe-Stand sein ("v0.5.0-3-gabc1234-dirty"): dann
// zählt die Release-Version davor. Unlesbare Stände ("dev") gelten als älter
// als jedes Release.
func Newer(candidate, current string) bool {
	c, err := ParseVersion(candidate)
	if err != nil {
		return false
	}
	base := current
	if i := strings.IndexByte(current, '-'); i >= 0 {
		base = current[:i]
	}
	cur, err := ParseVersion(base)
	if err != nil {
		return true
	}
	return c.Compare(cur) > 0
}

// BinaryName liefert den Dateinamen des Agenten für eine Plattform, so wie
// build.ps1 ihn erzeugt (arm wird als GOARM=7 gebaut).
func BinaryName(goos, goarch string) string {
	name := "mat-agent-" + goos + "-" + goarch
	if goarch == "arm" {
		name += "v7"
	}
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// platformOf ist die Umkehrung von BinaryName.
func platformOf(name string) (goos, goarch string, ok bool) {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(name, ".exe"), "mat-agent-")
	if !ok {
		return "", "", false
	}
	goos, goarch, ok = strings.Cut(rest, "-")
	if goarch == "armv7" {
		goarch = "arm"
	}
	return goos, goarch, ok && BinaryName(goos, goarch) == name
}
