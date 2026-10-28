//go:build windows

package winsvc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func TestHandlerStop(t *testing.T) {
	stopped := make(chan struct{})
	h := &handler{fn: func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	}}
	req := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 10)
	result := make(chan uint32)
	go func() {
		_, code := h.Execute(nil, req, status)
		result <- code
	}()

	if s := (<-status).State; s != svc.StartPending {
		t.Fatalf("erster status: %v", s)
	}
	if s := <-status; s.State != svc.Running || s.Accepts&svc.AcceptStop == 0 {
		t.Fatalf("zweiter status: %+v", s)
	}
	req <- svc.ChangeRequest{Cmd: svc.Stop}
	select {
	case code := <-result:
		if code != 0 {
			t.Errorf("exit-code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler beendet sich nicht")
	}
	select {
	case <-stopped:
	default:
		t.Error("kontext wurde nicht beendet")
	}
}

func TestHandlerFnExits(t *testing.T) {
	h := &handler{fn: func(context.Context) {}}
	status := make(chan svc.Status, 10)
	_, code := h.Execute(nil, make(chan svc.ChangeRequest), status)
	if code != 1 {
		t.Fatalf("exit-code %d, erwartet 1 (wiederherstellung soll greifen)", code)
	}
}

func TestRestrictToAdmins(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conf")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "agent.json")
	os.WriteFile(file, []byte("{}"), 0o600)
	// Als Besitzer darf der Test die Rechte danach wieder öffnen, damit TempDir aufräumen kann.
	t.Cleanup(func() {
		setDACL(file, "D:(A;;FA;;;WD)")
		setDACL(dir, "D:(A;OICI;FA;;;WD)")
	})

	for _, p := range []string{file, dir} {
		if err := RestrictToAdmins(p); err != nil {
			t.Fatal(err)
		}
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		s := sd.String()
		// Geschützt (P), nur SYSTEM (SY) und Administratoren (BA).
		if !strings.HasPrefix(s, "D:P") || !strings.Contains(s, ";SY)") || !strings.Contains(s, ";BA)") ||
			strings.Count(s, "(A;") != 2 {
			t.Errorf("%s: dacl %s", p, s)
		}
	}
}
