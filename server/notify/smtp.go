package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// TLS-Modi für SMTP.
const (
	SMTPStartTLS = "starttls" // Klartext-Verbindung, dann STARTTLS (Pflicht), typisch Port 587
	SMTPTLS      = "tls"      // TLS ab dem ersten Byte, typisch Port 465
	SMTPNone     = "none"     // unverschlüsselt, nur zu localhost erlaubt
)

// SMTPConfig beschreibt den Mailversand.
type SMTPConfig struct {
	Host     string
	Port     int
	Mode     string // SMTPStartTLS, SMTPTLS oder SMTPNone
	Username string // optional; ohne Benutzer keine Anmeldung
	Password string
	From     string
	To       []string

	rootCAs *x509.CertPool // nur für Tests; nil = System-Zertifikate
}

// SMTP verschickt Meldungen per E-Mail.
type SMTP struct {
	cfg  SMTPConfig
	from *mail.Address
	to   []*mail.Address
}

// NewSMTP prüft die Konfiguration und erzeugt den Kanal.
func NewSMTP(cfg SMTPConfig) (*SMTP, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("smtp: host fehlt")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("smtp: ungültiger port %d", cfg.Port)
	}
	switch cfg.Mode {
	case SMTPStartTLS, SMTPTLS:
	case SMTPNone:
		if !isLoopback(cfg.Host) {
			return nil, fmt.Errorf("smtp: unverschlüsselt nur zu localhost erlaubt")
		}
	default:
		return nil, fmt.Errorf("smtp: ungültiger tls-modus %q (starttls, tls, none)", cfg.Mode)
	}
	if (cfg.Username == "") != (cfg.Password == "") {
		return nil, fmt.Errorf("smtp: benutzer und passwort nur zusammen angeben")
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("smtp: ungültige absenderadresse: %w", err)
	}
	s := &SMTP{cfg: cfg, from: from}
	for _, t := range cfg.To {
		a, err := mail.ParseAddress(strings.TrimSpace(t))
		if err != nil {
			return nil, fmt.Errorf("smtp: ungültige empfängeradresse %q: %w", t, err)
		}
		s.to = append(s.to, a)
	}
	if len(s.to) == 0 || len(s.to) > 20 {
		return nil, fmt.Errorf("smtp: 1 bis 20 empfänger angeben")
	}
	return s, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Name implementiert Channel.
func (s *SMTP) Name() string { return "email" }

// Send implementiert Channel.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	if err := s.send(ctx, m); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return nil
}

func (s *SMTP) send(ctx context.Context, m Message) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12, RootCAs: s.cfg.rootCAs}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if s.cfg.Mode == SMTPTLS {
		conn = tls.Client(conn, tlsCfg)
	}

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("mat-server"); err != nil {
		return err
	}
	if s.cfg.Mode == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("server bietet kein STARTTLS an")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if s.cfg.Username != "" {
		// PlainAuth verweigert selbst die Anmeldung über unverschlüsselte Verbindungen (außer localhost).
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return err
	}
	for _, t := range s.to {
		if err := c.Rcpt(t.Address); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(s.compose(m, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// compose baut die Nachricht. Betreff wird nach RFC 2047, der Text als
// quoted-printable kodiert; Steuerzeichen hat sanitize bereits entfernt.
func (s *SMTP) compose(m Message, now time.Time) []byte {
	var b bytes.Buffer
	to := make([]string, len(s.to))
	for i, a := range s.to {
		to[i] = a.String()
	}
	subject := m.Title
	if m.High {
		subject = "[Alarm] " + subject
	}
	id := make([]byte, 12)
	rand.Read(id)
	domain := s.from.Address[strings.LastIndex(s.from.Address, "@")+1:]

	fmt.Fprintf(&b, "From: %s\r\n", s.from.String())
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n") + "\r\n"))
	qp.Close()
	return b.Bytes()
}
