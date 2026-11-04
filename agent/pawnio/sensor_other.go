//go:build !(windows && amd64)

package pawnio

import "errors"

// CPUTemp gibt es nur unter Windows amd64 (dort ohne PawnIO auch nicht).
func CPUTemp() (celsius float64, name string, err error) {
	return 0, "", errors.New("nur unter windows")
}
