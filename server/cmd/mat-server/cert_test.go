package main

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
)

func TestGenerateCert(t *testing.T) {
	now := time.Now()
	certPEM, keyPEM, err := generateCert([]string{"192.168.0.10", "monitor.lan"}, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("schlüsselpaar passt nicht: %v", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// Das Zertifikat muss sich selbst als Vertrauensanker für beide Namen bestätigen.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	for _, name := range []string{"192.168.0.10", "monitor.lan"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, CurrentTime: now}); err != nil {
			t.Errorf("verify %s: %v", name, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "anderer.lan", CurrentTime: now}); err == nil {
		t.Error("fremder name akzeptiert")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "monitor.lan", CurrentTime: now.Add(48 * time.Hour)}); err == nil {
		t.Error("abgelaufenes zertifikat akzeptiert")
	}
}
