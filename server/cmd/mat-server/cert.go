package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const certUsage = `Selbst signiertes TLS-Zertifikat für den Server erzeugen:
  mat-server gen-cert -hosts 192.168.100.10,monitor.lan [-out data] [-days 1825] [-force]

Erzeugt <out>/server.crt und <out>/server.key (nur für den Besitzer lesbar).
server.crt auf die Agenten kopieren und dort mit -ca bzw. "ca_file" angeben.`

func runGenCert(args []string) error {
	fs := flag.NewFlagSet("gen-cert", flag.ContinueOnError)
	hostList := fs.String("hosts", "", "IP-Adressen und Namen des Servers, kommagetrennt (Pflicht)")
	out := fs.String("out", "data", "Zielverzeichnis")
	days := fs.Int("days", 5*365, "Gültigkeit in Tagen")
	force := fs.Bool("force", false, "vorhandene Dateien überschreiben")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, certUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	var hosts []string
	for _, h := range strings.Split(*hostList, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		fs.Usage()
		return errors.New("-hosts fehlt")
	}
	if *days < 1 || *days > 10*365 {
		return errors.New("-days muss zwischen 1 und 3650 liegen")
	}

	certPath := filepath.Join(*out, "server.crt")
	keyPath := filepath.Join(*out, "server.key")
	if !*force {
		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s existiert bereits (überschreiben mit -force)", p)
			}
		}
	}

	certPEM, keyPEM, err := generateCert(hosts, time.Duration(*days)*24*time.Hour, time.Now())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	fmt.Printf("zertifikat: %s\nschlüssel:  %s\ngültig für: %s (%d tage)\n", certPath, keyPath, strings.Join(hosts, ", "), *days)
	return nil
}

// generateCert erzeugt ein selbst signiertes ECDSA-P-256-Zertifikat. Es ist
// zugleich CA, damit Agenten es direkt als Vertrauensanker nutzen können.
func generateCert(hosts []string, validFor time.Duration, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"Monitoring-Admin-Tool"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}
