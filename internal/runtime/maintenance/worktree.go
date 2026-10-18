package maintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

// WorktreeLease drains every cooperating store pool before a store file may be
// replaced. It owns the lifecycle lease before the writer lease, and releases
// them in reverse order.
type WorktreeLease struct {
	worktreeID string
	lifecycle  *mceruntime.Lease
	writer     *mceruntime.Lease
	once       sync.Once
	err        error
}

func AcquireWorktree(ctx context.Context, root *mceruntime.Root, worktreeID string, wait time.Duration) (*WorktreeLease, error) {
	if ctx == nil || root == nil || wait < 0 {
		return nil, fmt.Errorf("invalid worktree maintenance request")
	}
	if err := mceruntime.EnsureWorktreeLockLayout(root, worktreeID); err != nil {
		return nil, fmt.Errorf("prepare worktree maintenance locks: %w", err)
	}
	lifecycle, err := mceruntime.AcquireWorktreeLifecycle(ctx, root, worktreeID, mceruntime.LockExclusive, wait)
	if err != nil {
		return nil, fmt.Errorf("acquire exclusive worktree lifecycle lease: %w", err)
	}
	writer, err := mceruntime.AcquireWorktreeWriter(ctx, root, worktreeID, wait)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("acquire worktree writer lease for maintenance: %w", err), lifecycle.Close())
	}
	return &WorktreeLease{worktreeID: worktreeID, lifecycle: lifecycle, writer: writer}, nil
}

func (lease *WorktreeLease) WorktreeID() string {
	if lease == nil {
		return ""
	}
	return lease.worktreeID
}

func (lease *WorktreeLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.once.Do(func() {
		if lease.writer != nil {
			lease.err = lease.writer.Close()
		}
		if lease.lifecycle != nil {
			lease.err = errors.Join(lease.err, lease.lifecycle.Close())
		}
	})
	return lease.err
}
