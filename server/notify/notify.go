// Package notify verschickt Alarmmeldungen über externe Kanäle (ntfy, E-Mail).
//
// Jeder Kanal hat eine eigene Warteschlange und einen eigenen Worker, damit ein
// hängender Kanal die anderen nicht aufhält. Fehlgeschlagene Sendungen werden
// mit wachsendem Abstand wiederholt.
package notify

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

// Message ist eine Meldung, unabhängig vom Kanal.
type Message struct {
	Title string
	Body  string
	High  bool // hohe Priorität (ausgelöster Alarm)
	Tag   string
}

// Channel ist ein Benachrichtigungskanal.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

const (
	queueSize   = 100
	sendTimeout = 20 * time.Second
)

// retryDelays sind die Wartezeiten vor dem 2., 3. und 4. Versuch.
var retryDelays = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}

type worker struct {
	ch    Channel
	queue chan Message
}

// Dispatcher verteilt Meldungen an alle Kanäle.
type Dispatcher struct {
	workers []*worker
	log     *slog.Logger
}

// NewDispatcher erzeugt einen Dispatcher. Ohne Kanäle werden Meldungen verworfen
// (Alarme erscheinen dann nur in der GUI).
func NewDispatcher(log *slog.Logger, chans ...Channel) *Dispatcher {
	d := &Dispatcher{log: log}
	for _, c := range chans {
		d.workers = append(d.workers, &worker{ch: c, queue: make(chan Message, queueSize)})
	}
	return d
}

// Channels liefert die Namen der konfigurierten Kanäle.
func (d *Dispatcher) Channels() []string {
	names := make([]string, len(d.workers))
	for i, w := range d.workers {
		names[i] = w.ch.Name()
	}
	return names
}

// Enqueue reiht eine Meldung ein, ohne zu blockieren.
func (d *Dispatcher) Enqueue(m Message) {
	m = sanitize(m)
	for _, w := range d.workers {
		select {
		case w.queue <- m:
		default:
			d.log.Warn("benachrichtigung verworfen, warteschlange voll", "channel", w.ch.Name(), "title", m.Title)
		}
	}
}

// Run startet die Worker und blockiert, bis ctx beendet ist.
func (d *Dispatcher) Run(ctx context.Context) {
	done := make(chan struct{})
	for _, w := range d.workers {
		go func() {
			d.work(ctx, w)
			done <- struct{}{}
		}()
	}
	for range d.workers {
		<-done
	}
}

func (d *Dispatcher) work(ctx context.Context, w *worker) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-w.queue:
			d.deliver(ctx, w.ch, m)
		}
	}
}

// deliver versucht eine Meldung mehrfach zuzustellen.
func (d *Dispatcher) deliver(ctx context.Context, ch Channel, m Message) {
	for attempt := 0; ; attempt++ {
		err := send(ctx, ch, m)
		if err == nil {
			d.log.Debug("benachrichtigung gesendet", "channel", ch.Name(), "title", m.Title)
			return
		}
		if attempt >= len(retryDelays) {
			d.log.Error("benachrichtigung endgültig fehlgeschlagen", "channel", ch.Name(), "title", m.Title, "err", err)
			return
		}
		d.log.Warn("benachrichtigung fehlgeschlagen", "channel", ch.Name(), "err", err, "retry_in", retryDelays[attempt])
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelays[attempt]):
		}
	}
}

func send(ctx context.Context, ch Channel, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return ch.Send(ctx, m)
}

// Result ist das Ergebnis einer Testsendung.
type Result struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// Test sendet eine Meldung sofort und ohne Wiederholung an alle Kanäle.
func (d *Dispatcher) Test(ctx context.Context, m Message) []Result {
	m = sanitize(m)
	results := make([]Result, len(d.workers))
	done := make(chan struct{})
	for i, w := range d.workers {
		go func() {
			defer func() { done <- struct{}{} }()
			results[i] = Result{Channel: w.ch.Name(), OK: true}
			if err := send(ctx, w.ch, m); err != nil {
				results[i] = Result{Channel: w.ch.Name(), Error: err.Error()}
			}
		}()
	}
	for range d.workers {
		<-done
	}
	return results
}

// sanitize entfernt Steuerzeichen. Texte enthalten Hostnamen und Mountpoints,
// die von Agenten stammen; Zeilenumbrüche bleiben nur im Text erhalten.
func sanitize(m Message) Message {
	clean := func(s string, keepNewline bool, limit int) string {
		s = strings.Map(func(r rune) rune {
			if r == '\n' && keepNewline {
				return r
			}
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, s)
		if len(s) > limit {
			s = strings.ToValidUTF8(s[:limit], "") + "…"
		}
		return s
	}
	m.Title = clean(m.Title, false, 200)
	m.Body = clean(m.Body, true, 4000)
	m.Tag = clean(m.Tag, false, 50)
	return m
}
