package api

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/server/notify"
)

func TestCertStage(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		left time.Duration
		want int
	}{
		{60 * day, -1}, {30 * day, -1}, {29*day + time.Hour, 0}, {13 * day, 1}, {6 * day, 2},
		{12 * time.Hour, 3}, {0, 4}, {-day, 4},
	} {
		if got := certStage(c.left); got != c.want {
			t.Errorf("certStage(%v) = %d, will %d", c.left, got, c.want)
		}
	}
}

func TestCheckCertNotifiesOncePerStage(t *testing.T) {
	rec := &recordChannel{got: make(chan notify.Message, 10)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := notify.NewDispatcher(log, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s := &Server{log: log, notifier: d, nowFor: func() time.Time { return now }}
	s.WithCertificate(now.Add(20 * 24 * time.Hour))

	notified := s.checkCert(-1)
	notified = s.checkCert(notified) // gleiche Stufe: keine zweite Nachricht
	now = now.Add(15 * 24 * time.Hour)
	notified = s.checkCert(notified)
	now = now.Add(10 * 24 * time.Hour)
	s.checkCert(notified)

	var titles []string
	for i := 0; i < 3; i++ {
		select {
		case m := <-rec.got:
			titles = append(titles, m.Title)
		case <-time.After(5 * time.Second):
			t.Fatalf("zu wenige nachrichten: %v", titles)
		}
	}
	select {
	case m := <-rec.got:
		t.Fatalf("doppelte nachricht: %q", m.Title)
	case <-time.After(200 * time.Millisecond):
	}
	if titles[0] != "Server-Zertifikat läuft in 20 Tagen ab" || !strings.Contains(titles[1], "in 5 Tagen") ||
		titles[2] != "Server-Zertifikat ist abgelaufen" {
		t.Fatalf("titel = %q", titles)
	}
}
