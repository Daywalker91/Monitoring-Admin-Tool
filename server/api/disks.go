package api

import (
	"context"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
	"github.com/Daywalker91/Monitoring-Admin-Tool/server/forecast"
)

const (
	// diskSampleEvery: so oft wird die Belegung je Mountpoint gespeichert und
	// die Prognose neu berechnet.
	diskSampleEvery = time.Hour
	// DiskRetention: so lange bleiben diese Messpunkte erhalten.
	DiskRetention = 30 * 24 * time.Hour
)

// observeDisks speichert höchstens stündlich die Belegung je Mountpoint,
// berechnet daraus die Prognose „Platte voll“ und gibt sie an die Alarme.
func (s *Server) observeDisks(ctx context.Context, hostID, hostname string, disks []proto.DiskUsage, now time.Time) {
	s.diskMu.Lock()
	if s.diskAt == nil {
		s.diskAt = make(map[string]time.Time)
		s.forecasts = make(map[string]map[string]float64)
	}
	last, known := s.diskAt[hostID]
	if !known {
		// Nach einem Neustart des Servers nicht sofort einen zweiten Punkt anlegen.
		var err error
		if last, err = s.store.LastDiskSample(ctx, hostID); err != nil {
			s.log.Error("disk-historie lesen", "err", err)
		}
	}
	due := now.Sub(last) >= diskSampleEvery
	if due {
		s.diskAt[hostID] = now
	} else {
		s.diskAt[hostID] = last
	}
	_, computed := s.forecasts[hostID]
	s.diskMu.Unlock()
	if !due && computed {
		return
	}

	if due {
		if err := s.store.SaveDiskSamples(ctx, hostID, disks, now); err != nil {
			s.log.Error("disk-historie speichern", "err", err)
			return
		}
	}
	hist, err := s.store.DiskHistory(ctx, hostID, now.Add(-forecast.Window))
	if err != nil {
		s.log.Error("disk-historie lesen", "err", err)
		return
	}
	days := make(map[string]float64)
	for _, d := range disks {
		if v, ok := forecast.DaysUntilFull(hist[d.Mount], now); ok {
			days[d.Mount] = v
		}
	}
	s.diskMu.Lock()
	s.forecasts[hostID] = days
	s.diskMu.Unlock()
	if s.alerts != nil {
		s.alerts.ObserveForecast(ctx, hostID, hostname, days, now)
	}
}

// diskForecast liefert die zuletzt berechnete Prognose eines Hosts (Tage bis
// voll je Mountpoint; nil = keine).
func (s *Server) diskForecast(hostID string) map[string]float64 {
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	if len(s.forecasts[hostID]) == 0 {
		return nil
	}
	return s.forecasts[hostID]
}
