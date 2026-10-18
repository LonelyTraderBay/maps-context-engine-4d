//go:build windows

package exec

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("open fixture process %d: %v", pid, err)
	}
	defer windows.CloseHandle(process)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result, err := windows.WaitForSingleObject(process, 10)
		if err != nil {
			t.Fatalf("wait for fixture process %d: %v", pid, err)
		}
		if result == windows.WAIT_OBJECT_0 {
			return
		}
		if result != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("unexpected process wait result %#x", result)
		}
	}
	t.Fatalf("fixture process %d remained alive after tree cancellation", pid)
}
