//go:build linux

package exec

import (
	"errors"
	"os"
	osexec "os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxProcessTreeAttachesToExitedChild(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := osexec.Command(executable, "-test.run=^TestLinuxProcessTreeExitHelper$")
	guard, err := newProcessTreeGuard(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = guard.Close()
		_ = command.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		state, _, _, detailErr := linuxProcessDetails(command.Process.Pid)
		if detailErr == nil && state == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not reach zombie state before Attach: state=%q err=%v", state, detailErr)
		}
		time.Sleep(time.Millisecond)
	}

	if err := guard.Attach(command); err != nil {
		t.Fatalf("Attach rejected an exited child with a verified process group: %v", err)
	}
	exited, err := guard.ParentExited()
	if err != nil || !exited {
		t.Fatalf("ParentExited() = %t, %v; want true, nil", exited, err)
	}
}

func TestLinuxProcessTreeExitHelper(t *testing.T) {}

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
