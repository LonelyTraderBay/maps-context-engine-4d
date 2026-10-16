package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const lockProbePathEnv = "MCE4D_LOCK_PROBE_PATH"

func TestLockChildProcess(t *testing.T) {
	if os.Getenv(lockProbePathEnv) == "" {
		return
	}
	root, err := OpenRoot(os.Getenv(lockProbePathEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lease, err := root.AcquireLock(context.Background(), "worktrees/11111111-1111-4111-8111-111111111111/writer.lock", LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	_, _ = fmt.Fprintln(os.Stdout, "LOCKED")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestLockModesAreSharedExclusiveAndBounded(t *testing.T) {
	home := t.TempDir()
	root, err := OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := EnsureWorktreeLockLayout(root, "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}

	sharedA, err := AcquireWorktreeLifecycle(context.Background(), root, "11111111-1111-4111-8111-111111111111", LockShared, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sharedB, err := AcquireWorktreeLifecycle(context.Background(), root, "11111111-1111-4111-8111-111111111111", LockShared, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireWorktreeLifecycle(context.Background(), root, "11111111-1111-4111-8111-111111111111", LockExclusive, 25*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("exclusive lock while shared leases are live returned %v; want ErrLockTimeout", err)
	}
	if err := sharedA.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sharedB.Close(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := AcquireWorktreeLifecycle(context.Background(), root, "11111111-1111-4111-8111-111111111111", LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.AcquireLock(context.Background(), "worktrees/11111111-1111-4111-8111-111111111111/lifecycle.lock", LockShared, 0); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("shared lock while exclusive lease is live returned %v; want ErrLockBusy", err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "worktrees", "11111111-1111-4111-8111-111111111111", "lifecycle.lock")); err != nil {
		t.Fatalf("permanent lifecycle lockfile was removed: %v", err)
	}
}

func TestHomeConfigLockCoordinatesReadersAndWriters(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	sharedA, err := AcquireHomeConfig(context.Background(), root, LockShared, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sharedB, err := AcquireHomeConfig(context.Background(), root, LockShared, time.Second)
	if err != nil {
		_ = sharedA.Close()
		t.Fatal(err)
	}
	if _, err := AcquireHomeConfig(context.Background(), root, LockExclusive, 25*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		_ = sharedB.Close()
		_ = sharedA.Close()
		t.Fatalf("exclusive config writer passed active readers: %v", err)
	}
	if err := sharedB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sharedA.Close(); err != nil {
		t.Fatal(err)
	}

	exclusive, err := AcquireHomeConfig(context.Background(), root, LockExclusive, time.Second)
	if err != nil {
		t.Fatalf("config writer could not acquire released lock: %v", err)
	}
	if _, err := AcquireHomeConfig(context.Background(), root, LockShared, 25*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		_ = exclusive.Close()
		t.Fatalf("config reader passed active writer: %v", err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeLockLayoutRejectsNoncanonicalIdentity(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, identity := range []string{"", "11111111-1111-4111-8111-11111111111A", "../11111111-1111-4111-8111-111111111111"} {
		if err := EnsureWorktreeLockLayout(root, identity); !errors.Is(err, ErrPathDenied) {
			t.Errorf("EnsureWorktreeLockLayout(%q)=%v, want path denied", identity, err)
		}
	}
	if err := EnsureWorktreeLockLayout(nil, "11111111-1111-4111-8111-111111111111"); !errors.Is(err, ErrRootClosed) {
		t.Fatalf("nil root returned %v, want root closed", err)
	}
}

func TestLockContextCancellationReleasesAttemptHandle(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := EnsureWorktreeLockLayout(root, "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireWorktreeWriter(context.Background(), root, "11111111-1111-4111-8111-111111111111", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	if _, err := AcquireWorktreeWriter(ctx, root, "11111111-1111-4111-8111-111111111111", time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled writer wait returned %v; want context deadline", err)
	}
}

func TestProcessExitReleasesKernelLock(t *testing.T) {
	if os.Getenv(lockProbePathEnv) != "" {
		t.Skip("helper process is covered by TestLockChildProcess")
	}
	home := t.TempDir()
	root, err := OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := EnsureWorktreeLockLayout(root, "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestLockChildProcess$")
	command.Env = append(os.Environ(), lockProbePathEnv+"="+home)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "LOCKED" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("child lock handshake %q, err=%v", line, err)
	}
	if _, err := AcquireWorktreeWriter(context.Background(), root, "11111111-1111-4111-8111-111111111111", 0); !errors.Is(err, ErrLockBusy) {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("parent acquired child-owned lock: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("killed child process unexpectedly exited successfully")
	}
	_ = stdin.Close()
	lease, err := AcquireWorktreeWriter(context.Background(), root, "11111111-1111-4111-8111-111111111111", time.Second)
	if err != nil {
		t.Fatalf("kernel lock remained after owner process exit: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockfileRejectsHardlinksAndLinks(t *testing.T) {
	home := t.TempDir()
	root, err := OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.EnsurePrivateDirs("locks"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "locks", "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(home, "locks", "linked.lock")); err == nil {
		if _, err := root.OpenLockFile("locks/linked.lock"); !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("hard-linked lockfile returned %v; want ErrNotRegularFile", err)
		}
	}
	if err := os.Symlink(target, filepath.Join(home, "locks", "symlink.lock")); err == nil {
		if _, err := root.OpenLockFile("locks/symlink.lock"); err == nil {
			t.Fatal("symlink lockfile was accepted")
		}
	}
}

func TestRootIdentityAndAvailableBytesAreBoundToOpenedDirectory(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	volume, home, err := root.Identity()
	if err != nil || volume == "" || home == "" || volume == home {
		t.Fatalf("root identities volume=%q home=%q err=%v", volume, home, err)
	}
	available, err := root.AvailableBytes()
	if err != nil || available < 0 {
		t.Fatalf("available bytes=%d err=%v", available, err)
	}
}

func TestWriteAtomicFileStaysUnderVerifiedRootAndRejectsLinks(t *testing.T) {
	home := t.TempDir()
	root, err := OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.EnsurePrivateDirs("volume-admission"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "volume-admission", "journal.json")
	if err := root.WriteAtomicFile("volume-admission/journal.json", []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenRegularFile("volume-admission/journal.json")
	if err != nil {
		t.Fatal(err)
	}
	first, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(first) != `{"version":1}` {
		t.Fatalf("atomic file readback=%q read=%v close=%v", first, readErr, closeErr)
	}
	if err := root.WriteAtomicFile("volume-admission/journal.json", []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "volume-admission", "journal-link.json")); err == nil {
		if err := root.WriteAtomicFile("volume-admission/journal-link.json", []byte(`{}`), 0o600); err == nil {
			t.Fatal("atomic writer accepted an existing symbolic link")
		}
	}
	if err := root.WriteAtomicFile("../outside.json", []byte(`{}`), 0o600); !errors.Is(err, ErrPathDenied) {
		t.Fatalf("parent traversal returned %v; want ErrPathDenied", err)
	}
}
