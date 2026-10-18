package maintenance

import (
	"context"
	"errors"
	"testing"
	"time"

	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

func TestWorktreeMaintenanceDrainsOnlyTargetWorktree(t *testing.T) {
	root, err := mceruntime.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("open verified home: %v", err)
	}
	defer root.Close()
	const first = "51000000-0000-4000-8000-000000000001"
	const second = "51000000-0000-4000-8000-000000000002"
	if err := mceruntime.EnsureWorktreeLockLayout(root, first); err != nil {
		t.Fatalf("prepare first worktree locks: %v", err)
	}
	if err := mceruntime.EnsureWorktreeLockLayout(root, second); err != nil {
		t.Fatalf("prepare second worktree locks: %v", err)
	}

	reader, err := mceruntime.AcquireWorktreeLifecycle(context.Background(), root, first, mceruntime.LockShared, time.Second)
	if err != nil {
		t.Fatalf("acquire simulated pool lifecycle lease: %v", err)
	}
	if _, err := AcquireWorktree(context.Background(), root, first, 20*time.Millisecond); !errors.Is(err, mceruntime.ErrLockTimeout) {
		t.Fatalf("maintenance with live pool error=%v; want ErrLockTimeout", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("release simulated pool lifecycle lease: %v", err)
	}

	lease, err := AcquireWorktree(context.Background(), root, first, time.Second)
	if err != nil {
		t.Fatalf("acquire maintenance lease after pool closed: %v", err)
	}
	if lease.WorktreeID() != first {
		t.Fatalf("maintenance lease worktree=%q; want %q", lease.WorktreeID(), first)
	}
	if _, err := mceruntime.AcquireWorktreeLifecycle(context.Background(), root, first, mceruntime.LockShared, 20*time.Millisecond); !errors.Is(err, mceruntime.ErrLockTimeout) {
		t.Fatalf("same-worktree reader while maintenance active error=%v; want ErrLockTimeout", err)
	}
	otherReader, err := mceruntime.AcquireWorktreeLifecycle(context.Background(), root, second, mceruntime.LockShared, time.Second)
	if err != nil {
		t.Fatalf("different worktree was blocked by maintenance: %v", err)
	}
	if err := otherReader.Close(); err != nil {
		t.Fatalf("release other worktree reader: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("release maintenance lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("idempotent maintenance close: %v", err)
	}
	reader, err = mceruntime.AcquireWorktreeLifecycle(context.Background(), root, first, mceruntime.LockShared, time.Second)
	if err != nil {
		t.Fatalf("lifecycle lease remained held after maintenance close: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("release reader after maintenance: %v", err)
	}
}

func TestWorktreeMaintenanceHonorsCancellationAndValidation(t *testing.T) {
	if _, err := AcquireWorktree(nil, nil, "", 0); err == nil {
		t.Fatal("maintenance accepted missing context and verified root")
	}
	root, err := mceruntime.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("open verified home: %v", err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AcquireWorktree(ctx, root, "51000000-0000-4000-8000-000000000003", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled maintenance error=%v; want context.Canceled", err)
	}
}
