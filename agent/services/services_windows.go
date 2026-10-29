package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Die Startart ändert sich selten; sie wird je Dienst zwischengespeichert,
// damit nicht jede Minute ~300 Dienste einzeln abgefragt werden.
const configTTL = 10 * time.Minute

type scm struct {
	mu    sync.Mutex // list läuft im Sende-Takt und bei Jobs
	cache map[string]cachedConfig
}

type cachedConfig struct {
	startType, description string
	at                     time.Time
}

func newPlatform() platform { return &scm{cache: map[string]cachedConfig{}} }

func (p *scm) list(ctx context.Context) ([]proto.Service, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Nur Leserechte: funktioniert auch im Vordergrund ohne Administrator.
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, fmt.Errorf("dienst-manager: %w", err)
	}
	defer windows.CloseServiceHandle(h)

	var needed, count, resume uint32
	var buf []byte
	for {
		var ptr *byte
		if len(buf) > 0 {
			ptr = &buf[0]
		}
		err = windows.EnumServicesStatusEx(h, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32,
			windows.SERVICE_STATE_ALL, ptr, uint32(len(buf)), &needed, &count, &resume, nil)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_MORE_DATA) || needed <= uint32(len(buf)) {
			return nil, fmt.Errorf("dienste auflisten: %w", err)
		}
		buf = make([]byte, needed)
		resume = 0
	}
	if count == 0 {
		return nil, nil
	}
	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), count)
	now := time.Now()
	out := make([]proto.Service, 0, count)
	seen := make(map[string]bool, count)
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		name := windows.UTF16PtrToString(e.ServiceName)
		seen[name] = true
		c, ok := p.cache[name]
		if !ok || now.Sub(c.at) > configTTL {
			c = queryConfig(h, name, now)
			p.cache[name] = c
		}
		desc := windows.UTF16PtrToString(e.DisplayName)
		if c.description != "" && c.description != desc {
			desc += " – " + c.description
		}
		state := svc.State(e.ServiceStatusProcess.CurrentState)
		out = append(out, proto.Service{Name: name, Description: desc, State: windowsState(state),
			Detail: stateDetail(state), StartType: c.startType})
	}
	for name := range p.cache {
		if !seen[name] {
			delete(p.cache, name)
		}
	}
	return out, nil
}

func queryConfig(h windows.Handle, name string, now time.Time) cachedConfig {
	c := cachedConfig{at: now}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return c
	}
	sh, err := windows.OpenService(h, n, windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return c
	}
	s := &mgr.Service{Name: name, Handle: sh}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return c
	}
	switch cfg.StartType {
	case mgr.StartAutomatic:
		c.startType = "auto"
		if cfg.DelayedAutoStart {
			c.startType = "auto (verzögert)"
		}
	case mgr.StartManual:
		c.startType = "manual"
	case mgr.StartDisabled:
		c.startType = "disabled"
	case windows.SERVICE_BOOT_START, windows.SERVICE_SYSTEM_START:
		c.startType = "boot"
	}
	c.description = cfg.Description
	return c
}

func windowsState(s svc.State) string {
	switch s {
	case svc.Running:
		return proto.SvcRunning
	case svc.StartPending, svc.ContinuePending:
		return proto.SvcStarting
	case svc.StopPending, svc.PausePending:
		return proto.SvcStopping
	}
	return proto.SvcStopped
}

func stateDetail(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start_pending"
	case svc.StopPending:
		return "stop_pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue_pending"
	case svc.PausePending:
		return "pause_pending"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state_%d", s)
}

const waitTimeout = 60 * time.Second

func (p *scm) control(ctx context.Context, verb, name string) (string, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", fmt.Errorf("dienst-manager: %w", err)
	}
	defer windows.CloseServiceHandle(h)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	sh, err := windows.OpenService(h, n, windows.SERVICE_START|windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return "", fmt.Errorf("dienst %s öffnen: %w", name, err)
	}
	s := &mgr.Service{Name: name, Handle: sh}
	defer s.Close()

	if verb == "stop" || verb == "restart" {
		st, err := s.Control(svc.Stop)
		if err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return "", fmt.Errorf("dienst %s stoppen: %w", name, err)
		}
		if err == nil && st.State != svc.Stopped {
			if err := wait(ctx, s, svc.Stopped); err != nil {
				return "", err
			}
		}
	}
	if verb == "start" || verb == "restart" {
		if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return "", fmt.Errorf("dienst %s starten: %w", name, err)
		}
		if err := wait(ctx, s, svc.Running); err != nil {
			return "", err
		}
	}
	return "", nil
}

// wait wartet, bis der Dienst den Zustand want erreicht.
func wait(ctx context.Context, s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(waitTimeout)
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("dienststatus abfragen: %w", err)
		}
		if st.State == want {
			return nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return fmt.Errorf("dienst %s hat sich nach dem start wieder beendet", s.Name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dienst %s: zustand %s nicht innerhalb von %s erreicht", s.Name, stateDetail(want), waitTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
