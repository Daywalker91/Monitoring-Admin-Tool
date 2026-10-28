package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Daywalker91/Monitoring-Admin-Tool/internal/release"
)

const releaseUsage = `Signierte Agent-Releases (Self-Update):
  mat-server release keygen -out <datei>
      Schlüsselpaar erzeugen. Der private Schlüssel landet in <datei> (nur für den
      Besitzer lesbar) und gehört als Secret RELEASE_SIGNING_KEY in die
      GitHub-Environment "release"; danach die Datei löschen. Der öffentliche
      Schlüssel wird ausgegeben und kommt nach internal/release/key.go.
  mat-server release sign -version v0.5.0 [-dir dist]
      release.json und release.json.sig für die Agent-Binaries in <dir> erzeugen.
      Privater Schlüssel aus der Umgebungsvariable MAT_RELEASE_KEY oder -key-file.
  mat-server release verify [-dir dist] [-pub <schlüssel>]
      Signatur und Prüfsummen prüfen (Standard: eingebauter Schlüssel).`

func runRelease(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, releaseUsage)
		return errors.New("unterbefehl fehlt")
	}
	switch args[0] {
	case "keygen":
		return releaseKeygen(args[1:])
	case "sign":
		return releaseSign(args[1:])
	case "verify":
		return releaseVerify(args[1:])
	case "-h", "-help", "--help", "help":
		fmt.Println(releaseUsage)
		return nil
	}
	fmt.Fprintln(os.Stderr, releaseUsage)
	return fmt.Errorf("unbekannter unterbefehl %q", args[0])
}

func releaseKeygen(args []string) error {
	fs := flag.NewFlagSet("release keygen", flag.ContinueOnError)
	out := fs.String("out", "", "Datei für den privaten Schlüssel (Pflicht)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out fehlt")
	}
	priv, pub, err := release.GenerateKey()
	if err != nil {
		return err
	}
	// O_EXCL: einen vorhandenen Schlüssel nie überschreiben. 0600 wirkt unter
	// Unix; unter Windows gelten die Rechte des Zielordners (z. B. Benutzerprofil).
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("schlüsseldatei anlegen: %w", err)
	}
	if _, err := f.WriteString(priv + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("schlüssel schreiben: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("schlüssel schreiben: %w", err)
	}
	fmt.Printf("privater schlüssel: %s (geheim halten, nach dem hinterlegen löschen)\n", *out)
	fmt.Printf("öffentlicher schlüssel: %s\n", pub)
	return nil
}

func releaseSign(args []string) error {
	fs := flag.NewFlagSet("release sign", flag.ContinueOnError)
	version := fs.String("version", "", "Release-Version, z. B. v0.5.0 (Pflicht)")
	dir := fs.String("dir", "dist", "Verzeichnis mit den Agent-Binaries")
	keyFile := fs.String("key-file", "", "Datei mit dem privaten Schlüssel (sonst MAT_RELEASE_KEY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keyText := os.Getenv("MAT_RELEASE_KEY")
	if *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			return fmt.Errorf("schlüssel lesen: %w", err)
		}
		keyText = string(data)
	}
	if keyText == "" {
		return errors.New("kein privater schlüssel (MAT_RELEASE_KEY oder -key-file)")
	}
	priv, err := release.ParsePrivateKey(keyText)
	if err != nil {
		return err
	}
	manifest, err := release.BuildManifest(*version, *dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, release.ManifestName), manifest, 0o644); err != nil {
		return fmt.Errorf("manifest schreiben: %w", err)
	}
	if err := os.WriteFile(filepath.Join(*dir, release.SignatureName), release.Sign(priv, manifest), 0o644); err != nil {
		return fmt.Errorf("signatur schreiben: %w", err)
	}
	fmt.Printf("signiert: %s (%s)\n", *version, filepath.Join(*dir, release.ManifestName))
	return nil
}

func releaseVerify(args []string) error {
	fs := flag.NewFlagSet("release verify", flag.ContinueOnError)
	dir := fs.String("dir", "dist", "Verzeichnis mit Manifest, Signatur und Binaries")
	pubText := fs.String("pub", "", "öffentlicher Schlüssel (Standard: eingebauter)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(*dir, release.ManifestName))
	if err != nil {
		return fmt.Errorf("manifest lesen: %w", err)
	}
	sig, err := os.ReadFile(filepath.Join(*dir, release.SignatureName))
	if err != nil {
		return fmt.Errorf("signatur lesen: %w", err)
	}
	var m release.Manifest
	if *pubText != "" {
		pub, err := release.ParsePublicKey(*pubText)
		if err != nil {
			return err
		}
		m, err = release.VerifyWith(pub, manifest, sig)
		if err != nil {
			return err
		}
	} else if m, err = release.Verify(manifest, sig); err != nil {
		return err
	}
	for _, f := range m.Files {
		if err := release.CheckFile(filepath.Join(*dir, f.Name), f); err != nil {
			return err
		}
	}
	fmt.Printf("ok: %s, %d binaries\n", m.Version, len(m.Files))
	return nil
}
