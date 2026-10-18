//go:build linux

package exec

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := unix.Kill(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			return
		}
		if err != nil && !errors.Is(err, unix.EPERM) {
			t.Fatalf("probe fixture process %d: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture process %d remained alive after tree cancellation", pid)
}
