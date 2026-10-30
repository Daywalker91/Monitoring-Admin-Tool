package api

import (
	"context"
	"fmt"
)

// HeartbeatStatus liefert den Status für den externen Wächter, z. B.
// "MAT 1.5.0 · 5/6 Hosts online · 2 Alarme". Keine Hostnamen oder Inhalte:
// Der Text landet bei einem fremden Dienst. Ist die Datenbank nicht lesbar,
// kommt ein Fehler und der Heartbeat bleibt aus.
func (s *Server) HeartbeatStatus(ctx context.Context) (string, error) {
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return "", fmt.Errorf("hosts lesen: %w", err)
	}
	online := 0
	for _, h := range hosts {
		if s.online(h.ID) {
			online++
		}
	}
	text := fmt.Sprintf("MAT %s · %d/%d Hosts online", s.version, online, len(hosts))
	if s.alerts != nil {
		n := 0
		for _, kinds := range s.alerts.ActiveCount() {
			for _, c := range kinds {
				n += c
			}
		}
		switch n {
		case 0:
			text += " · keine Alarme"
		case 1:
			text += " · 1 Alarm"
		default:
			text += fmt.Sprintf(" · %d Alarme", n)
		}
	}
	return text, nil
}
