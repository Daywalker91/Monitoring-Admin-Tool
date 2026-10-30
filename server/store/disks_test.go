package store

import (
	"context"
	"testing"
	"time"

	"github.com/Daywalker91/Monitoring-Admin-Tool/proto"
)

func TestDiskHistory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	if last, err := s.LastDiskSample(ctx, "h1"); err != nil || !last.IsZero() {
		t.Fatalf("leer: %v %v", last, err)
	}
	for i, used := range []uint64{10, 20, 30} {
		at := now.Add(time.Duration(i-2) * time.Hour)
		disks := []proto.DiskUsage{{Mount: "/", Used: used, Total: 100}, {Mount: "/var", Used: used * 2, Total: 200}}
		if err := s.SaveDiskSamples(ctx, "h1", disks, at); err != nil {
			t.Fatal(err)
		}
	}
	s.SaveDiskSamples(ctx, "h2", []proto.DiskUsage{{Mount: "/", Used: 1, Total: 2}}, now)

	if last, _ := s.LastDiskSample(ctx, "h1"); !last.Equal(now) {
		t.Errorf("letzter punkt: %v", last)
	}
	hist, err := s.DiskHistory(ctx, "h1", now.Add(-90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || len(hist["/"]) != 2 || hist["/"][0].Used != 20 || hist["/"][1].Used != 30 || hist["/var"][1].Total != 200 {
		t.Fatalf("historie: %+v", hist)
	}

	if n, err := s.PruneDiskHistory(ctx, now.Add(-90*time.Minute)); err != nil || n != 2 {
		t.Errorf("prune: %d %v", n, err)
	}
	if _, err := s.DeleteHost(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if hist, _ := s.DiskHistory(ctx, "h1", time.Time{}); len(hist) != 0 {
		t.Errorf("nach löschen: %+v", hist)
	}
	if hist, _ := s.DiskHistory(ctx, "h2", time.Time{}); len(hist["/"]) != 1 {
		t.Errorf("anderer host betroffen: %+v", hist)
	}
}
