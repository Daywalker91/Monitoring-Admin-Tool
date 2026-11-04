package pawnio

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

//go:embed modules/IntelMSR.bin
var intelModule []byte

//go:embed modules/AMDFamily17.bin
var amdModule []byte

// Schnittstelle des Treibers (wie PawnIOLib und LibreHardwareMonitor).
const (
	devicePath  = `\\?\GLOBALROOT\Device\PawnIO`
	deviceType  = 41394 << 16
	ioctlLoad   = deviceType | 0x821<<2
	ioctlExec   = deviceType | 0x841<<2
	retryAfter  = 10 * time.Minute // Treiber fehlt: so lange nicht erneut versuchen
	pciMutex    = `Global\Access_PCI`
	pciMutexMax = 10 // ms
)

func cpuid(leaf, sub uint32) (a, b, c, d uint32)

// CPU-Temperatur aus einem einzigen Sensor; Zugriff nur unter mu.
var (
	mu      sync.Mutex
	sensor  *cpuSensor
	failed  time.Time
	lastErr string
)

// CPUTemp liefert die CPU-Temperatur in °C und den Namen des Sensors. err
// beschreibt, warum es keinen Wert gibt (nur fürs Log, z. B. "check").
func CPUTemp() (celsius float64, name string, err error) {
	mu.Lock()
	defer mu.Unlock()
	if sensor == nil {
		if !failed.IsZero() && time.Since(failed) < retryAfter {
			return 0, "", errors.New(lastErr)
		}
		s, err := openSensor()
		if err != nil {
			failed, lastErr = time.Now(), err.Error()
			return 0, "", err
		}
		sensor, failed = s, time.Time{}
	}
	c, err := sensor.read()
	if err != nil {
		// Beim nächsten Mal neu öffnen (z. B. Treiber aktualisiert).
		sensor.close()
		sensor, failed, lastErr = nil, time.Now(), err.Error()
		return 0, "", err
	}
	return c, sensor.name, nil
}

type cpuSensor struct {
	dev    windows.Handle
	name   string
	read   func() (float64, error)
	mutex  windows.Handle // nur AMD
	closed bool
}

func (s *cpuSensor) close() {
	if s.closed {
		return
	}
	s.closed = true
	windows.CloseHandle(s.dev)
	if s.mutex != 0 {
		windows.CloseHandle(s.mutex)
	}
}

func openSensor() (*cpuSensor, error) {
	_, b, c, d := cpuid(0, 0)
	vendor := string(binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(
		binary.LittleEndian.AppendUint32(nil, b), d), c))
	switch vendor {
	case "GenuineIntel":
		return openIntel()
	case "AuthenticAMD":
		return openAMD()
	}
	return nil, fmt.Errorf("cpu %q nicht unterstützt", vendor)
}

func openIntel() (*cpuSensor, error) {
	a, _, _, _ := cpuid(6, 0)
	if a&1 == 0 { // kein digitaler Temperatursensor
		return nil, errors.New("cpu ohne digitalen temperatursensor")
	}
	dev, err := openModule(intelModule)
	if err != nil {
		return nil, err
	}
	s := &cpuSensor{dev: dev}
	target, err := s.call("ioctl_read_msr", msrTemperatureTarget)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("tjmax lesen: %w", err)
	}
	tj := tjMax(target)
	if a&(1<<6) != 0 { // Package Thermal Management: ein Wert für die ganze CPU
		s.name = "CPU Package"
		s.read = func() (float64, error) {
			v, err := s.call("ioctl_read_msr", msrPackageThermStatus)
			if err != nil {
				return 0, err
			}
			c, _ := intelTemp(tj, v, false)
			return checked(c)
		}
	} else {
		s.name = "CPU Kerne (max)"
		s.read = func() (float64, error) { return s.maxCoreTemp(tj) }
	}
	if _, err := s.read(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// maxCoreTemp liest IA32_THERM_STATUS auf jedem logischen Prozessor (der Treiber
// liest das MSR der CPU, auf der der Aufruf läuft) und liefert das Maximum.
func (s *cpuSensor) maxCoreTemp(tj float64) (float64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread := windows.CurrentThread()
	n := min(runtime.NumCPU(), 64)
	best, found := 0.0, false
	var prev uintptr
	for i := range n {
		old, _, e := procSetThreadAffinityMask.Call(uintptr(thread), uintptr(1)<<i)
		if old == 0 {
			return 0, fmt.Errorf("kern %d wählen: %w", i, e)
		}
		if i == 0 {
			prev = old
		}
		v, err := s.call("ioctl_read_msr", msrThermStatus)
		if err != nil {
			continue
		}
		if c, ok := intelTemp(tj, v, true); ok && (!found || c > best) {
			best, found = c, true
		}
	}
	procSetThreadAffinityMask.Call(uintptr(thread), prev)
	if !found {
		return 0, errors.New("kein gültiger kernwert")
	}
	return checked(best)
}

var procSetThreadAffinityMask = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadAffinityMask")

func openAMD() (*cpuSensor, error) {
	dev, err := openModule(amdModule) // das Modul prüft selbst Family 17h–1Ah
	if err != nil {
		return nil, err
	}
	name, _ := windows.UTF16PtrFromString(pciMutex)
	// Den Mutex teilen sich alle Programme, die über den PCI-Bus lesen
	// (LibreHardwareMonitor, HWiNFO, …). Gibt es ihn schon, kommt trotzdem ein
	// gültiges Handle und ERROR_ALREADY_EXISTS.
	m, err := windows.CreateMutex(nil, false, name)
	if m == 0 {
		windows.CloseHandle(dev)
		return nil, fmt.Errorf("mutex %s: %w", pciMutex, err)
	}
	s := &cpuSensor{dev: dev, mutex: m}
	offset := amdOffset(brandString())
	s.name = "Tctl"
	if offset != 0 {
		s.name = "Tdie"
	}
	s.read = func() (float64, error) {
		raw, err := s.readSMN(smnThmTconCurTmp)
		if err != nil {
			return 0, err
		}
		return checked(amdTemp(uint32(raw), offset))
	}
	if _, err := s.read(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// readSMN liest ein SMN-Register; dabei muss der gemeinsame PCI-Mutex gehalten
// werden (Index- und Datenregister dürfen nicht von zwei Programmen zugleich
// benutzt werden). Mutexe gehören einem Thread, deshalb LockOSThread.
func (s *cpuSensor) readSMN(addr uint64) (uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ev, err := windows.WaitForSingleObject(s.mutex, pciMutexMax)
	if ev != windows.WAIT_OBJECT_0 && ev != windows.WAIT_ABANDONED {
		return 0, fmt.Errorf("pci-mutex belegt: %v", err)
	}
	defer windows.ReleaseMutex(s.mutex)
	return s.call("ioctl_read_smn", addr)
}

// call führt eine Funktion des geladenen Moduls mit einem Argument aus.
func (s *cpuSensor) call(fn string, arg uint64) (uint64, error) {
	in := execInput(fn, arg)
	out := make([]byte, 8)
	var n uint32
	if err := windows.DeviceIoControl(s.dev, ioctlExec, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &n, nil); err != nil {
		return 0, fmt.Errorf("%s: %w", fn, err)
	}
	if n < 8 {
		return 0, fmt.Errorf("%s: keine antwort", fn)
	}
	return le64(out), nil
}

// openModule öffnet den Treiber und lädt ein Modul; ein Handle trägt genau ein Modul.
func openModule(blob []byte) (windows.Handle, error) {
	p, _ := windows.UTF16PtrFromString(devicePath)
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("pawnio nicht installiert oder nicht erreichbar: %w", err)
	}
	var n uint32
	if err := windows.DeviceIoControl(h, ioctlLoad, &blob[0], uint32(len(blob)), nil, 0, &n, nil); err != nil {
		windows.CloseHandle(h)
		return 0, fmt.Errorf("pawnio-modul laden: %w", err)
	}
	return h, nil
}

// brandString liefert den Prozessornamen aus CPUID 0x80000002–4.
func brandString() string {
	if a, _, _, _ := cpuid(0x80000000, 0); a < 0x80000004 {
		return ""
	}
	var buf [48]byte
	for i := range uint32(3) {
		a, b, c, d := cpuid(0x80000002+i, 0)
		for j, r := range []uint32{a, b, c, d} {
			binary.LittleEndian.PutUint32(buf[i*16+uint32(j)*4:], r)
		}
	}
	return strings.TrimSpace(strings.TrimRight(string(buf[:]), "\x00"))
}

func checked(c float64) (float64, error) {
	if !plausible(c) {
		return 0, fmt.Errorf("unplausibler wert %.1f °C", c)
	}
	return c, nil
}
