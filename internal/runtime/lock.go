package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	ErrInvalidLockMode = errors.New("invalid OS lock mode")
	ErrLockBusy        = errors.New("OS lock is held by another owner")
	ErrLockTimeout     = errors.New("OS lock wait exceeded its deadline")
)

type LockMode uint8

const (
	LockShared LockMode = iota + 1
	LockExclusive
)

// Lease owns one kernel lock and its permanent lockfile handle. Closing it
// unlocks the range before closing the handle; the lockfile itself is retained.
type Lease struct {
	file     *os.File
	unlock   func() error
	closeOne sync.Once
	closeErr error
}

// AcquireLock takes a shared or exclusive kernel lock beneath a verified root.
// Wait is bounded; zero performs one nonblocking attempt for recovery probes.
func (root *Root) AcquireLock(ctx context.Context, relative string, mode LockMode, wait time.Duration) (*Lease, error) {
	return root.acquireLock(ctx, relative, mode, wait, true)
}

// AcquireExistingLock acquires a kernel lock only when the permanent lockfile
// already exists. It is for read-only operations that must not bootstrap state.
func (root *Root) AcquireExistingLock(ctx context.Context, relative string, mode LockMode, wait time.Duration) (*Lease, error) {
	return root.acquireLock(ctx, relative, mode, wait, false)
}

func (root *Root) acquireLock(ctx context.Context, relative string, mode LockMode, wait time.Duration, create bool) (*Lease, error) {
	if root == nil || ctx == nil || wait < 0 || (mode != LockShared && mode != LockExclusive) {
		return nil, fmt.Errorf("invalid OS lock request: %w", ErrInvalidLockMode)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var file *os.File
	var err error
	if create {
		file, err = root.OpenLockFile(relative)
	} else {
		file, err = root.OpenExistingLockFile(relative)
	}
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	delay := time.Millisecond
	for {
		unlock, lockErr := tryFileLock(file, mode)
		if lockErr == nil {
			return &Lease{file: file, unlock: unlock}, nil
		}
		if !errors.Is(lockErr, ErrLockBusy) {
			_ = file.Close()
			return nil, lockErr
		}
		if wait == 0 {
			_ = file.Close()
			return nil, ErrLockBusy
		}
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = file.Close()
			return nil, ErrLockTimeout
		}
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if delay < 25*time.Millisecond {
			delay *= 2
			if delay > 25*time.Millisecond {
				delay = 25 * time.Millisecond
			}
		}
	}
}

func (lease *Lease) Close() error {
	if lease == nil {
		return nil
	}
	lease.closeOne.Do(func() {
		if lease.unlock != nil {
			lease.closeErr = lease.unlock()
		}
		if lease.file != nil {
			if err := lease.file.Close(); lease.closeErr == nil {
				lease.closeErr = err
			}
		}
	})
	return lease.closeErr
}
