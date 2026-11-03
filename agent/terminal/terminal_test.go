package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Daywalker91/Monitoring-Admin-Tool/agent/config"
	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

type msg struct {
	typ  websocket.MessageType
	data []byte
}

// fakeConn ersetzt die WebSocket-Verbindung zum Server.
type fakeConn struct {
	in     chan msg // vom Browser
	out    chan msg // zum Browser
	closed chan struct{}
	once   sync.Once
}

func newFakeConn() *fakeConn {
	return &fakeConn{in: make(chan msg, 16), out: make(chan msg, 256), closed: make(chan struct{})}
}

func (c *fakeConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case m := <-c.in:
		return m.typ, m.data, nil
	case <-c.closed:
		return 0, nil, errors.New("geschlossen")
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c *fakeConn) Write(ctx context.Context, typ websocket.MessageType, p []byte) error {
	select {
	case <-c.closed:
		return errors.New("geschlossen")
	default:
	}
	c.out <- msg{typ, append([]byte(nil), p...)}
	return nil
}

func (c *fakeConn) Close(websocket.StatusCode, string) error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// exitReason wartet auf die exit-Nachricht und liefert den Grund; Ausgaben
// davor landen in output.
func (c *fakeConn) exitReason(t *testing.T) (reason, output string) {
	t.Helper()
	var out strings.Builder
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-c.out:
			if m.typ == websocket.MessageBinary {
				out.Write(m.data)
				continue
			}
			var ctl proto.TerminalControl
			if err := json.Unmarshal(m.data, &ctl); err != nil || ctl.Validate(true) != nil {
				t.Fatalf("ungültige steuernachricht %s", m.data)
			}
			return ctl.Reason, out.String()
		case <-timeout:
			t.Fatal("keine exit-nachricht")
		}
	}
}

// fakeProc ersetzt die Shell am PTY.
type fakeProc struct {
	mu      sync.Mutex
	input   strings.Builder
	size    [2]int
	output  chan []byte
	exit    chan struct{} // geschlossen = Shell beendet sich
	closed  chan struct{}
	closeMu sync.Once
}

func newFakeProc() *fakeProc {
	return &fakeProc{output: make(chan []byte, 16), exit: make(chan struct{}), closed: make(chan struct{})}
}

func (p *fakeProc) Read(b []byte) (int, error) {
	select {
	case d := <-p.output:
		return copy(b, d), nil
	case <-p.exit:
		return 0, io.EOF
	case <-p.closed:
		return 0, io.EOF
	}
}

func (p *fakeProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.input.Write(b)
	return len(b), nil
}

func (p *fakeProc) Resize(cols, rows int) error {
	p.mu.Lock()
	p.size = [2]int{cols, rows}
	p.mu.Unlock()
	return nil
}

func (p *fakeProc) Close() error {
	p.closeMu.Do(func() { close(p.closed) })
	return nil
}

func (p *fakeProc) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

type fakeSpawner struct {
	mu    sync.Mutex
	procs []*fakeProc
	err   error
}

func (s *fakeSpawner) Start(id string, cols, rows int) (Proc, error) {
	if s.err != nil {
		return nil, s.err
	}
	p := newFakeProc()
	p.size = [2]int{cols, rows}
	s.mu.Lock()
	s.procs = append(s.procs, p)
	s.mu.Unlock()
	return p, nil
}

func (s *fakeSpawner) proc(t *testing.T, i int) *fakeProc {
	t.Helper()
	for range 200 {
		s.mu.Lock()
		if len(s.procs) > i {
			p := s.procs[i]
			s.mu.Unlock()
			return p
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("shell nicht gestartet")
	return nil
}

func testManager(sp Spawner, maxSessions int) *Manager {
	m := newManager(&config.Terminal{User: "hans", Shell: "/bin/bash", MaxSessions: maxSessions,
		IdleTimeoutSec: 60, MaxDurationSec: 60}, sp, slog.New(slog.DiscardHandler))
	return m
}

var openReq = proto.TerminalOpen{SessionID: proto.TerminalIDPrefix + strings.Repeat("ab", 16), Cols: 80, Rows: 24, User: "admin"}

func serve(m *Manager, conn Conn) chan struct{} {
	done := make(chan struct{})
	go func() {
		m.Serve(context.Background(), openReq, conn)
		close(done)
	}()
	return done
}

func TestSessionRelayAndShellExit(t *testing.T) {
	sp := &fakeSpawner{}
	m := testManager(sp, 1)
	conn := newFakeConn()
	done := serve(m, conn)
	p := sp.proc(t, 0)

	conn.in <- msg{websocket.MessageBinary, []byte("ls -la\r")}
	resize, _ := json.Marshal(proto.TerminalControl{Type: proto.TerminalResize, Cols: 132, Rows: 50})
	conn.in <- msg{websocket.MessageText, resize}
	conn.in <- msg{websocket.MessageText, []byte(`{"type":"exit","reason":"x"}`)} // vom Browser ignoriert
	p.output <- []byte("total 0\r\n")
	time.Sleep(50 * time.Millisecond)
	close(p.exit)

	reason, out := conn.exitReason(t)
	<-done
	if reason != ReasonShellExit || out != "total 0\r\n" {
		t.Fatalf("grund %q, ausgabe %q", reason, out)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.input.String() != "ls -la\r" || p.size != [2]int{132, 50} {
		t.Fatalf("eingabe %q, größe %v", p.input.String(), p.size)
	}
	if !p.isClosed() || m.active.Load() != 0 {
		t.Fatal("shell nicht beendet oder zähler falsch")
	}
}

func TestSessionBrowserGone(t *testing.T) {
	sp := &fakeSpawner{}
	m := testManager(sp, 1)
	conn := newFakeConn()
	done := serve(m, conn)
	p := sp.proc(t, 0)
	conn.Close(websocket.StatusNormalClosure, "")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sitzung endet nicht")
	}
	if !p.isClosed() {
		t.Fatal("shell läuft nach trennung weiter")
	}
}

func TestSessionTimeouts(t *testing.T) {
	for name, set := range map[string]func(m *Manager){
		ReasonIdle:    func(m *Manager) { m.idle = 80 * time.Millisecond },
		ReasonMaxTime: func(m *Manager) { m.maxDuration = 80 * time.Millisecond },
	} {
		sp := &fakeSpawner{}
		m := testManager(sp, 1)
		set(m)
		conn := newFakeConn()
		done := serve(m, conn)
		if name == ReasonMaxTime { // Eingabe verhindert nicht das Zeitlimit
			go func() {
				for range 10 {
					conn.in <- msg{websocket.MessageBinary, []byte("a")}
					time.Sleep(10 * time.Millisecond)
				}
			}()
		}
		if reason, _ := conn.exitReason(t); reason != name {
			t.Errorf("%s: grund %q", name, reason)
		}
		<-done
		if !sp.proc(t, 0).isClosed() {
			t.Errorf("%s: shell nicht beendet", name)
		}
	}
}

func TestMaxSessions(t *testing.T) {
	sp := &fakeSpawner{}
	m := testManager(sp, 1)
	first := newFakeConn()
	done := serve(m, first)
	sp.proc(t, 0)

	second := newFakeConn()
	serve(m, second)
	if reason, _ := second.exitReason(t); !strings.Contains(reason, "höchstens 1") {
		t.Fatalf("grund %q", reason)
	}
	sp.mu.Lock()
	n := len(sp.procs)
	sp.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d shells gestartet", n)
	}
	first.Close(websocket.StatusNormalClosure, "")
	<-done
	// danach ist wieder Platz
	third := newFakeConn()
	serve(m, third)
	sp.proc(t, 1)
	third.Close(websocket.StatusNormalClosure, "")
}

func TestSpawnError(t *testing.T) {
	m := testManager(&fakeSpawner{err: errors.New("kaputt")}, 1)
	conn := newFakeConn()
	<-serve(m, conn)
	if reason, _ := conn.exitReason(t); !strings.Contains(reason, "nicht gestartet") {
		t.Fatalf("grund %q", reason)
	}
	if m.active.Load() != 0 {
		t.Fatal("zähler nicht zurückgesetzt")
	}
}

func TestShutdown(t *testing.T) {
	sp := &fakeSpawner{}
	m := testManager(sp, 2)
	conn := newFakeConn()
	done := serve(m, conn)
	sp.proc(t, 0)
	m.Shutdown()
	<-done
	if reason, _ := conn.exitReason(t); reason != ReasonShutdown {
		t.Fatalf("grund %q", reason)
	}
}

func TestOversizedInputEndsSession(t *testing.T) {
	sp := &fakeSpawner{}
	m := testManager(sp, 1)
	conn := newFakeConn()
	done := serve(m, conn)
	sp.proc(t, 0)
	conn.in <- msg{websocket.MessageBinary, make([]byte, proto.TerminalMaxFrame+1)}
	<-done
	p := sp.proc(t, 0)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.input.Len() != 0 {
		t.Fatal("übergroße eingabe weitergegeben")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(1000, 100)
	ctx := context.Background()
	start := time.Now()
	if err := l.wait(ctx, 100); err != nil || time.Since(start) > 20*time.Millisecond {
		t.Fatal("burst nicht sofort erlaubt")
	}
	if err := l.wait(ctx, 50); err != nil || time.Since(start) < 30*time.Millisecond {
		t.Fatalf("nicht gebremst: %v", time.Since(start))
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := l.wait(cctx, 1000); err == nil {
		t.Fatal("abbruch ignoriert")
	}
}
