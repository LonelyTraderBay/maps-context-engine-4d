package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
)

// EnsureWorktreeLockLayout prepares only the private directories used by the
// lifecycle, writer, admission, and operation leases for one owned worktree.
func EnsureWorktreeLockLayout(root *Root, worktreeID string) error {
	if root == nil {
		return ErrRootClosed
	}
	if !canonicalUUID(worktreeID) {
		return fmt.Errorf("invalid worktree lock owner: %w", ErrPathDenied)
	}
	return root.EnsurePrivateDirs("worktrees/"+worktreeID, "volume-admission", "operation-leases")
}

func AcquireHomeLifecycle(ctx context.Context, root *Root, mode LockMode, wait time.Duration) (*Lease, error) {
	return root.AcquireLock(ctx, "lifecycle.lock", mode, wait)
}

// AcquireHomeConfig coordinates short reads and replacements of the canonical
// home RuntimeConfig. Acquire it before and release it after volume admission;
// never wait for it while holding the volume admission lease.
func AcquireHomeConfig(ctx context.Context, root *Root, mode LockMode, wait time.Duration) (*Lease, error) {
	return root.AcquireLock(ctx, "config.lock", mode, wait)
}

func AcquireWorktreeLifecycle(ctx context.Context, root *Root, worktreeID string, mode LockMode, wait time.Duration) (*Lease, error) {
	if !canonicalUUID(worktreeID) {
		return nil, fmt.Errorf("invalid worktree lifecycle identity: %w", ErrPathDenied)
	}
	return root.AcquireLock(ctx, "worktrees/"+worktreeID+"/lifecycle.lock", mode, wait)
}

func AcquireWorktreeWriter(ctx context.Context, root *Root, worktreeID string, wait time.Duration) (*Lease, error) {
	if !canonicalUUID(worktreeID) {
		return nil, fmt.Errorf("invalid worktree writer identity: %w", ErrPathDenied)
	}
	return root.AcquireLock(ctx, "worktrees/"+worktreeID+"/writer.lock", LockExclusive, wait)
}

func AcquireVolumeAdmission(ctx context.Context, root *Root, volumeID string, wait time.Duration) (*Lease, error) {
	if strings.TrimSpace(volumeID) == "" {
		return nil, fmt.Errorf("invalid volume admission identity: %w", ErrPathDenied)
	}
	path := "volume-admission/" + volumeID + ".lock"
	if err := paths.ValidateRelative(path); err != nil {
		return nil, fmt.Errorf("invalid volume admission lock identity: %w", ErrPathDenied)
	}
	return root.AcquireLock(ctx, path, LockExclusive, wait)
}

func AcquireOperationLease(ctx context.Context, root *Root, operationID string, wait time.Duration) (*Lease, error) {
	if !canonicalUUID(operationID) {
		return nil, fmt.Errorf("invalid operation lease identity: %w", ErrPathDenied)
	}
	return root.AcquireLock(ctx, "operation-leases/"+operationID+".lock", LockExclusive, wait)
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
