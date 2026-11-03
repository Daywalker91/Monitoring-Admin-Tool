package client

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// TestMain: Mit MAT_TEST_EXIT beendet sich das Testprogramm sofort mit diesem
// Code (Hilfsprozess für TestExitCode).
func TestMain(m *testing.M) {
	if code := os.Getenv("MAT_TEST_EXIT"); code != "" {
		var n int
		fmt.Sscan(code, &n)
		os.Exit(n)
	}
	os.Exit(m.Run())
}

func TestExitCode(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "MAT_TEST_EXIT=10")
	err := cmd.Run()
	if got := exitCode(fmt.Errorf("amp-update-check: %w", err)); got != 10 {
		t.Fatalf("exit-code %d, err %v", got, err)
	}
	if got := exitCode(errors.New("befehl nicht freigegeben")); got != 1 {
		t.Fatalf("ohne programm: %d", got)
	}
}
