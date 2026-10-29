package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/alert"
)

// WithMetricsToken aktiviert /metrics (Prometheus-Textformat), geschützt durch
// ein eigenes Bearer-Token. Ohne Token antwortet der Endpunkt mit 404.
func (s *Server) WithMetricsToken(token string) *Server {
	s.metricsToken = []byte(token)
	return s
}

// metricFamily sammelt die Werte einer Metrik, damit HELP/TYPE nur einmal erscheinen.
type metricFamily struct {
	name, help, typ string
	samples         []string
}

type metricsWriter struct {
	order    []string
	families map[string]*metricFamily
}

func newMetricsWriter() *metricsWriter {
	return &metricsWriter{families: make(map[string]*metricFamily)}
}

// add hängt einen Wert an. labels ist eine Liste aus Name/Wert-Paaren.
func (mw *metricsWriter) add(name, typ, help string, value float64, labels ...string) {
	f := mw.families[name]
	if f == nil {
		f = &metricFamily{name: name, help: help, typ: typ}
		mw.families[name] = f
		mw.order = append(mw.order, name)
	}
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escapeLabel(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	f.samples = append(f.samples, b.String())
}

func (mw *metricsWriter) bytes() []byte {
	var buf bytes.Buffer
	for _, name := range mw.order {
		f := mw.families[name]
		fmt.Fprintf(&buf, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		for _, s := range f.samples {
			buf.WriteString(s)
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// escapeLabel maskiert Label-Werte nach dem Prometheus-Textformat.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if len(s.metricsToken) == 0 {
		http.NotFound(w, r)
		return
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), s.metricsToken) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	hosts, err := s.store.ListHosts(r.Context())
	if err != nil {
		s.log.Error("metrics: hosts lesen", "err", err)
		http.Error(w, "interner fehler", http.StatusInternalServerError)
		return
	}

	var firing map[string]map[string]int
	if s.alerts != nil {
		firing = s.alerts.ActiveCount()
	}

	mw := newMetricsWriter()
	for _, h := range hosts {
		if s.alerts != nil {
			for _, kind := range alert.HostKinds {
				mw.add("mat_alerts_firing", "gauge", "Anzahl aktiver Alarme je Host und Regeltyp.",
					float64(firing[h.ID][kind]), "host", h.ID, "rule", kind)
			}
		}
		host := []string{"host", h.ID}
		up := 0.0
		online := s.online(h.ID)
		if online {
			up = 1
		}
		mw.add("mat_host_up", "gauge", "1, wenn der Agent verbunden ist.", up, "host", h.ID, "hostname", h.Hostname)
		mw.add("mat_host_info", "gauge", "Stammdaten des Hosts (Wert immer 1).", 1,
			"host", h.ID, "hostname", h.Hostname, "name", h.Name(), "group", h.Group, "os", h.OS, "platform", h.Platform,
			"platform_version", h.PlatformVersion, "arch", h.Arch, "agent_version", h.AgentVersion)
		mw.add("mat_host_last_seen_timestamp_seconds", "gauge", "Zeitpunkt der letzten Nachricht.", float64(h.LastSeen), host...)

		// Messwerte nur für verbundene Hosts, sonst blieben veraltete Werte in Grafana stehen.
		if !online || len(h.Metrics) == 0 || string(h.Metrics) == "null" {
			continue
		}
		var m proto.Metrics
		if err := json.Unmarshal(h.Metrics, &m); err != nil {
			continue
		}
		mw.add("mat_host_boot_time_seconds", "gauge", "Startzeitpunkt des Systems.", float64(h.BootTime), host...)
		mw.add("mat_cpu_usage_percent", "gauge", "CPU-Auslastung gesamt in Prozent.", m.CPU.TotalPct, host...)
		for i, v := range m.CPU.PerCore {
			mw.add("mat_cpu_core_usage_percent", "gauge", "CPU-Auslastung je Kern in Prozent.", v, "host", h.ID, "core", strconv.Itoa(i))
		}
		if h.OS != "windows" { // Windows kennt keinen Load-Wert
			mw.add("mat_load1", "gauge", "Load-Durchschnitt über 1 Minute.", m.CPU.Load1, host...)
			mw.add("mat_load5", "gauge", "Load-Durchschnitt über 5 Minuten.", m.CPU.Load5, host...)
			mw.add("mat_load15", "gauge", "Load-Durchschnitt über 15 Minuten.", m.CPU.Load15, host...)
		}
		mw.add("mat_memory_total_bytes", "gauge", "Arbeitsspeicher gesamt.", float64(m.Mem.Total), host...)
		mw.add("mat_memory_used_bytes", "gauge", "Arbeitsspeicher belegt.", float64(m.Mem.Used), host...)
		mw.add("mat_swap_total_bytes", "gauge", "Swap gesamt.", float64(m.Swap.Total), host...)
		mw.add("mat_swap_used_bytes", "gauge", "Swap belegt.", float64(m.Swap.Used), host...)
		for _, n := range m.Net {
			l := []string{"host", h.ID, "interface", n.Name}
			mw.add("mat_network_receive_bytes_per_second", "gauge", "Empfangsrate in Bytes/s.", n.RxBps, l...)
			mw.add("mat_network_transmit_bytes_per_second", "gauge", "Senderate in Bytes/s.", n.TxBps, l...)
			mw.add("mat_network_receive_packets_per_second", "gauge", "Empfangene Pakete/s.", n.RxPps, l...)
			mw.add("mat_network_transmit_packets_per_second", "gauge", "Gesendete Pakete/s.", n.TxPps, l...)
			mw.add("mat_network_receive_errors_total", "counter", "Empfangsfehler seit Systemstart.", float64(n.RxErrors), l...)
			mw.add("mat_network_transmit_errors_total", "counter", "Sendefehler seit Systemstart.", float64(n.TxErrors), l...)
		}
		for _, d := range m.Disks {
			l := []string{"host", h.ID, "mountpoint", d.Mount, "fstype", d.FSType}
			mw.add("mat_filesystem_size_bytes", "gauge", "Größe des Dateisystems.", float64(d.Total), l...)
			mw.add("mat_filesystem_used_bytes", "gauge", "Belegter Platz im Dateisystem.", float64(d.Used), l...)
		}
		if m.CPUTemp != nil {
			mw.add("mat_cpu_temperature_celsius", "gauge", "CPU-Temperatur in °C.", *m.CPUTemp, host...)
		}
		for _, t := range m.Temps {
			mw.add("mat_sensor_temperature_celsius", "gauge", "Temperatur je Sensor in °C.", t.Celsius, "host", h.ID, "sensor", t.Name)
		}
	}

	s.checkMetrics(r.Context(), mw)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(mw.bytes())
}

// checkMetrics ergänzt die externen Checks.
func (s *Server) checkMetrics(ctx context.Context, mw *metricsWriter) {
	if s.checks == nil {
		return
	}
	list, err := s.store.Checks(ctx)
	if err != nil {
		s.log.Error("checks für metrics lesen", "err", err)
		return
	}
	last := s.checks.Last()
	for _, c := range list {
		res, ok := last[c.ID]
		if !c.Enabled || !ok {
			continue
		}
		l := []string{"check", strconv.FormatInt(c.ID, 10), "name", c.Name, "kind", c.Kind}
		up := 0.0
		if res.OK {
			up = 1
		}
		mw.add("mat_check_up", "gauge", "Letzter Durchlauf des Checks erfolgreich (1) oder nicht (0).", up, l...)
		if res.LatencyMS != nil {
			mw.add("mat_check_latency_seconds", "gauge", "Antwortzeit des letzten Durchlaufs.", *res.LatencyMS/1000, l...)
		}
		if res.CertNotAfter != nil {
			mw.add("mat_check_cert_expiry_timestamp_seconds", "gauge", "Ablauf des Zertifikats (HTTPS-Checks).",
				float64(*res.CertNotAfter), l...)
		}
	}
}
