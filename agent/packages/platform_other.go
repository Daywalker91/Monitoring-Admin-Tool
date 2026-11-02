//go:build !windows

package packages

func platformManager(runFunc) *manager { return nil }
