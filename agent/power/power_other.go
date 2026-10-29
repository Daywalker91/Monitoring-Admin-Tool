//go:build !linux && !windows

package power

import "errors"

func rebootRequired() bool { return false }
func supported() bool      { return false }
func precheck() error      { return errors.New("neustart wird auf dieser plattform nicht unterstützt") }
func reboot() (string, error) {
	return "", precheck()
}
