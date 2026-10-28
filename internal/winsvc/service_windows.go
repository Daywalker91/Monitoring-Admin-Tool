//go:build windows

package winsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// IsService meldet, ob der Prozess vom Dienst-Manager gestartet wurde.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// Run führt fn als Dienst aus. Der Kontext wird beendet, wenn Windows den
// Dienst stoppt oder herunterfährt. Run kehrt zurück, wenn fn zurückkehrt.
func Run(name string, fn func(ctx context.Context)) error {
	return svc.Run(name, &handler{fn: fn})
}

type handler struct {
	fn func(ctx context.Context)
}

// Execute implementiert svc.Handler.
func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.fn(ctx)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case <-done:
			// fn hat sich selbst beendet (z. B. Konfigurationsfehler): Exit-Code 1,
			// damit die Wiederherstellungsaktionen greifen.
			status <- svc.Status{State: svc.StopPending}
			return false, 1
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 15000}
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// Install legt den Dienst an (Autostart, Neustart nach Absturz) oder
// aktualisiert Pfad und Argumente eines bestehenden Dienstes.
func Install(s Spec) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	cfg := mgr.Config{
		DisplayName:  s.DisplayName,
		Description:  s.Description,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}
	service, err := m.OpenService(s.Name)
	if err == nil {
		// Bestehenden Dienst aktualisieren.
		cur, err := service.Config()
		if err != nil {
			service.Close()
			return fmt.Errorf("dienstkonfiguration lesen: %w", err)
		}
		cur.DisplayName, cur.Description, cur.StartType = cfg.DisplayName, cfg.Description, cfg.StartType
		cur.BinaryPathName = commandLine(s.ExePath, s.Args)
		if err := service.UpdateConfig(cur); err != nil {
			service.Close()
			return fmt.Errorf("dienst aktualisieren: %w", err)
		}
	} else {
		service, err = m.CreateService(s.Name, s.ExePath, cfg, s.Args...)
		if err != nil {
			return fmt.Errorf("dienst anlegen: %w", err)
		}
	}
	defer service.Close()

	// Neustart nach 10 s, 30 s, 60 s; Zähler wird nach einem Tag zurückgesetzt.
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}
	if err := service.SetRecoveryActions(actions, 86400); err != nil {
		return fmt.Errorf("wiederherstellung einstellen: %w", err)
	}
	// Auch bei Exit-Code ≠ 0 ohne Absturz neu starten.
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("wiederherstellung einstellen: %w", err)
	}
	return nil
}

// commandLine baut die Befehlszeile so, wie CreateService sie ablegt.
func commandLine(exe string, args []string) string {
	s := windows.EscapeArg(exe)
	for _, a := range args {
		s += " " + windows.EscapeArg(a)
	}
	return s
}

// Uninstall stoppt und entfernt den Dienst.
func Uninstall(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	service, err := open(m, name)
	if err != nil {
		return err
	}
	defer service.Close()
	_ = stopAndWait(service)
	if err := service.Delete(); err != nil {
		return fmt.Errorf("dienst entfernen: %w", err)
	}
	return nil
}

// Start startet den Dienst.
func Start(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	service, err := open(m, name)
	if err != nil {
		return err
	}
	defer service.Close()
	if err := service.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return nil
		}
		return fmt.Errorf("dienst starten: %w", err)
	}
	return nil
}

// Stop stoppt den Dienst und wartet bis zu 30 s.
func Stop(name string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	service, err := open(m, name)
	if err != nil {
		return err
	}
	defer service.Close()
	return stopAndWait(service)
}

// Status liefert den Zustand als Text, z. B. "läuft". Braucht nur Leserechte,
// funktioniert also auch ohne Administrator.
func Status(name string) (string, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", fmt.Errorf("dienst-manager: %w", err)
	}
	defer windows.CloseServiceHandle(m)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	h, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return "", ErrNotInstalled
		}
		return "", fmt.Errorf("dienst öffnen: %w", err)
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return "", fmt.Errorf("dienststatus abfragen: %w", err)
	}
	return stateText(svc.State(st.CurrentState)), nil
}

func stateText(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "gestoppt"
	case svc.StartPending:
		return "startet"
	case svc.StopPending:
		return "wird gestoppt"
	case svc.Running:
		return "läuft"
	case svc.Paused:
		return "pausiert"
	}
	return fmt.Sprintf("zustand %d", s)
}

func stopAndWait(service *mgr.Service) error {
	st, err := service.Control(svc.Stop)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return nil
		}
		return fmt.Errorf("dienst stoppen: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return errors.New("dienst hat sich nicht innerhalb von 30 s beendet")
		}
		time.Sleep(300 * time.Millisecond)
		if st, err = service.Query(); err != nil {
			return fmt.Errorf("dienststatus abfragen: %w", err)
		}
	}
	return nil
}

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, errors.New("keine berechtigung – als administrator ausführen")
		}
		return nil, fmt.Errorf("dienst-manager: %w", err)
	}
	return m, nil
}

func open(m *mgr.Mgr, name string) (*mgr.Service, error) {
	service, err := m.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, ErrNotInstalled
		}
		return nil, fmt.Errorf("dienst öffnen: %w", err)
	}
	return service, nil
}

// Nur SYSTEM und Administratoren, keine geerbten Rechte. "OICI" vererbt die
// Rechte an neu angelegte Dateien und Unterordner.
const (
	adminOnlyDir  = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	adminOnlyFile = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
)

// RestrictToAdmins beschränkt den Zugriff auf path auf SYSTEM und Administratoren.
// Gedacht für Verzeichnisse und Dateien mit Tokens und Passwörtern.
func RestrictToAdmins(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	sddl := adminOnlyFile
	if fi.IsDir() {
		sddl = adminOnlyDir
	}
	return setDACL(path, sddl)
}

func setDACL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("sicherheitsbeschreibung: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("dacl lesen: %w", err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return fmt.Errorf("zugriffsrechte setzen für %s: %w", path, err)
	}
	return nil
}

// IsAdmin meldet, ob der Prozess mit Administratorrechten läuft.
func IsAdmin() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	var elevation uint32
	var n uint32
	err := windows.GetTokenInformation(token, windows.TokenElevation, (*byte)(unsafe.Pointer(&elevation)),
		uint32(unsafe.Sizeof(elevation)), &n)
	return err == nil && elevation != 0
}
