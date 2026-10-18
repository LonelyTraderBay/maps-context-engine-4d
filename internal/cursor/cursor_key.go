package cursor

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

const cursorKeyPath = "cursor-keys/active.key"

// LoadOrCreateCursorKey stores one random 32-byte key below the verified home.
// A kernel lease serializes first creation and explicit rotations across processes.
func LoadOrCreateCursorKey(ctx context.Context, root *mceruntime.Root, wait time.Duration) ([]byte, error) {
	return loadCursorKey(ctx, root, wait, false)
}

// RotateCursorKey deliberately invalidates every token signed by the prior key.
func RotateCursorKey(ctx context.Context, root *mceruntime.Root, wait time.Duration) ([]byte, error) {
	return loadCursorKey(ctx, root, wait, true)
}

func loadCursorKey(ctx context.Context, root *mceruntime.Root, wait time.Duration, rotate bool) ([]byte, error) {
	if ctx == nil || root == nil || wait < 0 {
		return nil, fmt.Errorf("invalid cursor key-store request: %w", ErrCursorInvalid)
	}
	if err := root.EnsurePrivateDirs("cursor-keys"); err != nil {
		return nil, fmt.Errorf("prepare private cursor key directory: %w", err)
	}
	lease, err := root.AcquireLock(ctx, "cursor-keys/key.lock", mceruntime.LockExclusive, wait)
	if err != nil {
		return nil, fmt.Errorf("acquire cursor key rotation lease: %w", err)
	}
	defer lease.Close()
	if !rotate {
		key, readErr := readCursorKey(root)
		if readErr == nil {
			return key, nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return nil, readErr
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate cursor signing key: %w", err)
	}
	if err := root.WriteAtomicFile(cursorKeyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("persist private cursor signing key: %w", err)
	}
	readback, err := readCursorKey(root)
	if err != nil || !equalSecret(key, readback) {
		return nil, fmt.Errorf("verify private cursor signing key readback: %w", errors.Join(err, ErrCursorInvalid))
	}
	return readback, nil
}

func readCursorKey(root *mceruntime.Root) ([]byte, error) {
	rootPath, err := root.FinalPath()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(rootPath, filepath.FromSlash(cursorKeyPath))
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != 32 {
		return nil, fmt.Errorf("cursor signing key must be one regular 32-byte file: %w", ErrCursorInvalid)
	}
	if runtime.GOOS != "windows" && before.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("cursor signing key permissions are not owner-only: %w", ErrCursorInvalid)
	}
	file, err := root.OpenRegularFile(cursorKeyPath)
	if err != nil {
		return nil, fmt.Errorf("securely open cursor signing key: %w", err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() != 32 {
		return nil, fmt.Errorf("cursor signing key changed during open: %w", errors.Join(err, ErrCursorInvalid))
	}
	key, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("cursor signing key length differs from 32 bytes: %w", errors.Join(err, ErrCursorInvalid))
	}
	return key, nil
}

func equalSecret(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var mismatch byte
	for index := range left {
		mismatch |= left[index] ^ right[index]
	}
	return mismatch == 0
}
