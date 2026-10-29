//go:build !linux && !windows

package logs

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("logs werden auf dieser plattform nicht unterstützt")

func supported() bool          { return false }
func platformKind(string) bool { return false }
func read(context.Context, string, int) (string, error) {
	return "", errUnsupported
}
