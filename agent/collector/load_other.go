//go:build !windows

package collector

import "context"

// platformLoad: Linux liefert die Load selbst.
func platformLoad(_ context.Context, l1, l5, l15 float64) (float64, float64, float64) {
	return l1, l5, l15
}
