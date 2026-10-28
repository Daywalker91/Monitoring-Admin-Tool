package api

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
)

// certStages sind die Resttage, an denen vor dem Ablauf des Server-Zertifikats
// gewarnt wird (je Stufe eine Nachricht; danach "abgelaufen").
var certStages = []int{30, 14, 7, 1}

// certCheckInterval: so oft wird die Restlaufzeit geprüft.
const certCheckInterval = time.Hour

// WithCertificate übernimmt Ablaufdatum und SHA-256-Fingerabdruck des eigenen
// TLS-Zertifikats. Der Fingerabdruck steht in der GUI im Installationsbefehl;
// die Skripte pinnen damit das Zertifikat, ohne dass server.crt kopiert werden muss.
func (s *Server) WithCertificate(cert *x509.Certificate) *Server {
	s.certNotAfter = cert.NotAfter
	sum := sha256.Sum256(cert.Raw)
	s.certFingerprint = hex.EncodeToString(sum[:])
	return s
}

// certStage liefert die Warnstufe: -1 = noch kein Hinweis, 0..len-1 = Index in
// certStages, len(certStages) = abgelaufen.
func certStage(left time.Duration) int {
	if left <= 0 {
		return len(certStages)
	}
	days := int(left / (24 * time.Hour))
	stage := -1
	for i, d := range certStages {
		if days < d {
			stage = i
		}
	}
	return stage
}

// RunCertCheck warnt per Log und Benachrichtigung, wenn das Zertifikat bald
// abläuft. Jede Stufe wird einmal gemeldet (nach einem Neustart erneut).
func (s *Server) RunCertCheck(ctx context.Context) {
	if s.certNotAfter.IsZero() {
		return
	}
	notified := -1
	t := time.NewTicker(certCheckInterval)
	defer t.Stop()
	for {
		notified = s.checkCert(notified)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkCert meldet eine neue Warnstufe und liefert die zuletzt gemeldete.
func (s *Server) checkCert(notified int) int {
	left := s.certNotAfter.Sub(s.nowFor())
	stage := certStage(left)
	if stage <= notified {
		return notified
	}
	until := s.certNotAfter.Local().Format("02.01.2006 15:04")
	days := int(math.Ceil(left.Hours() / 24))
	title := fmt.Sprintf("Server-Zertifikat läuft in %d Tagen ab", days)
	if days == 1 {
		title = "Server-Zertifikat läuft morgen ab"
	}
	if stage == len(certStages) {
		title = "Server-Zertifikat ist abgelaufen"
	}
	body := "Gültig bis " + until + ". Neues Zertifikat erzeugen (mat-server gen-cert) und server.crt " +
		"auf allen Agenten als ca_file hinterlegen – sonst verbinden sie sich nicht mehr."
	s.log.Warn(title, "not_after", s.certNotAfter)
	if s.notifier != nil {
		s.notifier.Enqueue(notify.Message{Title: title, Body: body, High: stage >= 2, Tag: "warning"})
	}
	return stage
}
