package notify

import (
	"fmt"
	"strconv"
	"strings"
)

// FromEnv liest die Kanäle aus Umgebungsvariablen. Zugangsdaten kommen bewusst
// nur aus der Umgebung, nie aus der GUI oder der Datenbank.
//
//	MAT_NTFY_URL     Topic-URL, z. B. https://ntfy.sh/mein-topic
//	MAT_NTFY_TOKEN   optional: Access-Token
//	MAT_SMTP_HOST    Mailserver; ohne Host kein E-Mail-Versand
//	MAT_SMTP_PORT    Standard 587 (starttls) bzw. 465 (tls)
//	MAT_SMTP_TLS     starttls (Standard), tls oder none (nur localhost)
//	MAT_SMTP_USER, MAT_SMTP_PASS   optional: Anmeldung
//	MAT_SMTP_FROM    Absender
//	MAT_SMTP_TO      Empfänger, durch Komma getrennt
//
// warnings enthält Hinweise, die den Start nicht verhindern.
func FromEnv(getenv func(string) string) (chans []Channel, warnings []string, err error) {
	if u := getenv("MAT_NTFY_URL"); u != "" {
		n, insecure, err := NewNtfy(u, getenv("MAT_NTFY_TOKEN"))
		if err != nil {
			return nil, nil, err
		}
		if insecure {
			warnings = append(warnings, "MAT_NTFY_URL ist unverschlüsselt (http) – nur im LAN verwenden")
		}
		chans = append(chans, n)
	} else if getenv("MAT_NTFY_TOKEN") != "" {
		return nil, nil, fmt.Errorf("MAT_NTFY_TOKEN ohne MAT_NTFY_URL gesetzt")
	}

	if host := getenv("MAT_SMTP_HOST"); host != "" {
		mode := strings.ToLower(getenv("MAT_SMTP_TLS"))
		if mode == "" {
			mode = SMTPStartTLS
		}
		port := 587
		if mode == SMTPTLS {
			port = 465
		}
		if p := getenv("MAT_SMTP_PORT"); p != "" {
			if port, err = strconv.Atoi(p); err != nil {
				return nil, nil, fmt.Errorf("MAT_SMTP_PORT ist keine zahl")
			}
		}
		var to []string
		if t := getenv("MAT_SMTP_TO"); t != "" {
			to = strings.Split(t, ",")
		}
		s, err := NewSMTP(SMTPConfig{Host: host, Port: port, Mode: mode, Username: getenv("MAT_SMTP_USER"),
			Password: getenv("MAT_SMTP_PASS"), From: getenv("MAT_SMTP_FROM"), To: to})
		if err != nil {
			return nil, nil, err
		}
		chans = append(chans, s)
	}
	return chans, warnings, nil
}
