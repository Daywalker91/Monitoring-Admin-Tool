package api

import (
	"time"
	"unicode/utf8"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// liveJob hält die Zwischenausgabe eines laufenden Jobs. Sie lebt nur im
// Speicher; in die Datenbank kommt allein das Endergebnis.
type liveJob struct {
	host     string
	seq      int
	out      []byte
	deadline time.Time
}

// liveStart merkt einen gestarteten Job für Zwischenstände vor und räumt
// Einträge abgelaufener Jobs weg.
func (s *Server) liveStart(hostID, jobID string, deadline time.Time) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live == nil {
		s.live = make(map[string]*liveJob)
	}
	now := s.nowFor()
	for id, l := range s.live {
		if now.After(l.deadline) {
			delete(s.live, id)
		}
	}
	s.live[jobID] = &liveJob{host: hostID, deadline: deadline}
}

// liveEnd verwirft die Zwischenausgabe, sobald das Ergebnis da ist.
func (s *Server) liveEnd(jobID string) {
	s.liveMu.Lock()
	delete(s.live, jobID)
	s.liveMu.Unlock()
}

// jobProgress hängt einen Zwischenstand an. Angenommen wird er nur für einen
// laufenden Job desselben Hosts und in aufsteigender Reihenfolge; behalten wird
// höchstens das Ende von MaxJobOutputLen Bytes.
func (s *Server) jobProgress(hostID string, p proto.JobProgress) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	l, ok := s.live[p.JobID]
	if !ok || l.host != hostID || p.Seq <= l.seq {
		return
	}
	l.seq = p.Seq
	l.out = append(l.out, p.Data...)
	if over := len(l.out) - proto.MaxJobOutputLen; over > 0 {
		for over < len(l.out) && !utf8.RuneStart(l.out[over]) {
			over++
		}
		l.out = append(l.out[:0], l.out[over:]...)
	}
}

// liveOutput liefert die bisherige Ausgabe eines laufenden Jobs ("" = keine).
func (s *Server) liveOutput(jobID string) string {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if l, ok := s.live[jobID]; ok {
		return string(l.out)
	}
	return ""
}

// liveSet ersetzt die Zwischenausgabe eines laufenden Jobs (Abläufe im Server).
func (s *Server) liveSet(jobID, out string) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if l, ok := s.live[jobID]; ok {
		l.out = []byte(out)
	}
}
