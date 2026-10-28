// Package collector erfasst Systemmetriken mit gopsutil.
//
// Raten (Netzwerk, Prozess-CPU) werden aus der Differenz zweier aufeinander
// folgender Messungen berechnet. Der Collector ist nicht nebenläufig sicher und
// wird nur aus einer Goroutine heraus benutzt.
package collector

import (
	"context"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

// Dateisysteme, die für die Speicheranzeige uninteressant sind.
var ignoredFSTypes = map[string]bool{
	"squashfs": true, "overlay": true, "tmpfs": true, "devtmpfs": true,
	"iso9660": true, "udf": true, "autofs": true,
}

// netSample ist ein Zählerstand eines Interfaces.
type netSample struct {
	rxBytes, txBytes, rxPkts, txPkts uint64
}

// Collector hält den Zustand der letzten Messung.
type Collector struct {
	lastNet     map[string]netSample
	lastNetTime time.Time

	lastProcCPU  map[int32]float64 // CPU-Sekunden (user+system) je PID
	lastProcTime time.Time
}

// New erzeugt einen Collector.
func New() *Collector {
	return &Collector{}
}

// Prime nimmt eine erste Messung als Ausgangsbasis für Raten vor, damit schon
// die ersten gesendeten Werte aussagekräftig sind.
func (c *Collector) Prime(ctx context.Context) {
	_, _ = c.Metrics(ctx)
	_, _ = c.Processes(ctx, 1)
}

// HostInfo liefert die Host-Angaben für die Hello-Nachricht.
func HostInfo(ctx context.Context) proto.Hello {
	h := proto.Hello{
		ProtoVersion: proto.Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
	}
	if info, err := host.InfoWithContext(ctx); err == nil {
		h.Hostname = info.Hostname
		h.Platform = info.Platform
		h.PlatformVer = info.PlatformVersion
		h.KernelVer = info.KernelVersion
		h.BootTime = int64(info.BootTime)
	}
	if n, err := cpu.CountsWithContext(ctx, true); err == nil {
		h.CPUCores = n
	}
	return h
}

// Metrics erfasst CPU, RAM/Swap, Netzwerk und Speicher. Teilfehler werden
// übergangen, damit ein fehlender Wert nicht die ganze Messung verhindert.
func (c *Collector) Metrics(ctx context.Context) (proto.Metrics, error) {
	now := time.Now()
	m := proto.Metrics{Time: now.UTC()}

	// Intervall 0: Auslastung seit dem letzten Aufruf (gopsutil merkt sich den Stand).
	if total, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(total) > 0 {
		m.CPU.TotalPct = round2(total[0])
	}
	if per, err := cpu.PercentWithContext(ctx, 0, true); err == nil {
		m.CPU.PerCore = make([]float64, len(per))
		for i, v := range per {
			m.CPU.PerCore[i] = round2(v)
		}
	}
	if avg, err := load.AvgWithContext(ctx); err == nil {
		m.CPU.Load1, m.CPU.Load5, m.CPU.Load15 = round2(avg.Load1), round2(avg.Load5), round2(avg.Load15)
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		m.Mem = proto.Memory{Total: vm.Total, Used: vm.Used, UsedPct: round2(vm.UsedPercent)}
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		m.Swap = proto.Memory{Total: sw.Total, Used: sw.Used, UsedPct: round2(sw.UsedPercent)}
	}

	if counters, err := net.IOCountersWithContext(ctx, true); err == nil {
		m.Net = c.netRates(counters, now)
	}

	m.Disks = diskUsage(ctx)
	return m, nil
}

// netRates berechnet Raten pro Interface gegenüber der letzten Messung.
func (c *Collector) netRates(counters []net.IOCountersStat, now time.Time) []proto.NetIface {
	secs := now.Sub(c.lastNetTime).Seconds()
	first := c.lastNet == nil
	cur := make(map[string]netSample, len(counters))
	out := make([]proto.NetIface, 0, len(counters))

	for _, ct := range counters {
		if isLoopback(ct.Name) {
			continue
		}
		// Interfaces ganz ohne Verkehr (z. B. inaktive virtuelle Adapter) weglassen.
		if ct.BytesRecv == 0 && ct.BytesSent == 0 {
			continue
		}
		s := netSample{ct.BytesRecv, ct.BytesSent, ct.PacketsRecv, ct.PacketsSent}
		cur[ct.Name] = s

		iface := proto.NetIface{Name: ct.Name, RxErrors: ct.Errin, TxErrors: ct.Errout}
		if prev, ok := c.lastNet[ct.Name]; ok && !first {
			iface.RxBps = rate(prev.rxBytes, s.rxBytes, secs)
			iface.TxBps = rate(prev.txBytes, s.txBytes, secs)
			iface.RxPps = rate(prev.rxPkts, s.rxPkts, secs)
			iface.TxPps = rate(prev.txPkts, s.txPkts, secs)
		}
		out = append(out, iface)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	c.lastNet = cur
	c.lastNetTime = now
	return out
}

// rate berechnet (cur-prev)/secs. Bei Zählerüberlauf oder -reset wird 0 geliefert.
func rate(prev, cur uint64, secs float64) float64 {
	if secs <= 0 || cur < prev {
		return 0
	}
	return round2(float64(cur-prev) / secs)
}

func isLoopback(name string) bool {
	n := strings.ToLower(name)
	return n == "lo" || strings.HasPrefix(n, "loopback")
}

// diskUsage liefert die Belegung aller relevanten Mountpoints.
func diskUsage(ctx context.Context) []proto.DiskUsage {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool, len(parts))
	out := make([]proto.DiskUsage, 0, len(parts))
	for _, p := range parts {
		if seen[p.Mountpoint] || ignoredFSTypes[strings.ToLower(p.Fstype)] {
			continue
		}
		seen[p.Mountpoint] = true
		u, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue // z. B. leeres CD-Laufwerk
		}
		out = append(out, proto.DiskUsage{
			Mount: p.Mountpoint, FSType: p.Fstype,
			Total: u.Total, Used: u.Used, UsedPct: round2(u.UsedPercent),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// procSample ist der Rohwert eines Prozesses während einer Messung.
type procSample struct {
	p      *process.Process
	cpuPct float64
	rss    uint64
}

// Processes liefert die Top-n-Prozesse nach CPU und nach RAM. Die CPU-Werte
// beziehen sich auf den Zeitraum seit dem letzten Aufruf.
func (c *Collector) Processes(ctx context.Context, n int) (proto.Processes, error) {
	now := time.Now()
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return proto.Processes{}, err
	}

	secs := now.Sub(c.lastProcTime).Seconds()
	first := c.lastProcCPU == nil
	cur := make(map[int32]float64, len(procs))
	samples := make([]procSample, 0, len(procs))

	for _, p := range procs {
		s := procSample{p: p}
		// Zugriffsfehler (fremde/Systemprozesse ohne Rechte) einfach überspringen.
		if t, err := p.TimesWithContext(ctx); err == nil {
			total := t.User + t.System
			cur[p.Pid] = total
			if prev, ok := c.lastProcCPU[p.Pid]; ok && !first && secs > 0 && total >= prev {
				s.cpuPct = round2((total - prev) / secs * 100)
			}
		}
		if mi, err := p.MemoryInfoWithContext(ctx); err == nil {
			s.rss = mi.RSS
		}
		samples = append(samples, s)
	}
	c.lastProcCPU = cur
	c.lastProcTime = now

	topCPU := topN(samples, n, func(a, b procSample) bool { return a.cpuPct > b.cpuPct })
	topMem := topN(samples, n, func(a, b procSample) bool { return a.rss > b.rss })

	// Name und Benutzer nur für die ausgewählten Prozesse abfragen (spart Zeit und Speicher).
	details := make(map[int32]proto.Process)
	convert := func(list []procSample) []proto.Process {
		out := make([]proto.Process, 0, len(list))
		for _, s := range list {
			pr, ok := details[s.p.Pid]
			if !ok {
				pr = proto.Process{PID: s.p.Pid, Name: "?"}
				if name, err := s.p.NameWithContext(ctx); err == nil && name != "" {
					pr.Name = name
				}
				if user, err := s.p.UsernameWithContext(ctx); err == nil {
					pr.User = user
				}
				details[s.p.Pid] = pr
			}
			pr.CPUPct, pr.RSS = s.cpuPct, s.rss
			out = append(out, pr)
		}
		return out
	}

	return proto.Processes{Time: now.UTC(), TopCPU: convert(topCPU), TopMem: convert(topMem)}, nil
}

// topN sortiert eine Kopie und liefert die ersten n Einträge.
func topN(samples []procSample, n int, less func(a, b procSample) bool) []procSample {
	sorted := make([]procSample, len(samples))
	copy(sorted, samples)
	sort.SliceStable(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
