//go:build !linux && !windows

package services

func newPlatform() platform { return nil }
