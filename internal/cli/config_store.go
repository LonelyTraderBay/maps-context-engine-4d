package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	appconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/config"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

const maxRuntimeConfigBytes = 1 << 20

type homeConfigStore struct {
	rootPath string
	pipeline *runtimeconfig.Pipeline
}

var _ appconfig.Store = (*homeConfigStore)(nil)

func newHomeConfigStore(rootPath string, pipeline *runtimeconfig.Pipeline) (*homeConfigStore, error) {
	if !filepath.IsAbs(rootPath) || pipeline == nil {
		return nil, fmt.Errorf("absolute MCE4D home and config pipeline are required")
	}
	return &homeConfigStore{rootPath: filepath.Clean(rootPath), pipeline: pipeline}, nil
}

func (store *homeConfigStore) ReadCanonical(ctx context.Context) (raw []byte, present bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	root, exists, err := store.openRoot(false)
	if err != nil || !exists {
		return nil, false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	lease, err := mceruntime.AcquireHomeConfig(ctx, root, mceruntime.LockShared, configLockWait())
	if err != nil {
		return nil, false, fmt.Errorf("acquire shared runtime config lock: %w", err)
	}
	raw, present, err = readCanonicalAt(root)
	if closeErr := lease.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("release shared runtime config lock: %w", closeErr))
	}
	if err == nil {
		err = ctx.Err()
	}
	return raw, present, err
}

func (store *homeConfigStore) CompareAndSwapCanonical(ctx context.Context, expected *model.Hash, canonicalJSON []byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	candidate, err := store.pipeline.ParseSource(canonicalJSON, true)
	if err != nil {
		return fmt.Errorf("validate canonical runtime config candidate: %w", err)
	}
	if !bytes.Equal(candidate.CanonicalJSON(), canonicalJSON) {
		return fmt.Errorf("runtime config candidate is not canonical")
	}
	if err := os.MkdirAll(store.rootPath, 0o700); err != nil {
		return fmt.Errorf("create MCE4D home for config replacement: %w", err)
	}
	root, err := mceruntime.OpenRoot(store.rootPath)
	if err != nil {
		return fmt.Errorf("open verified MCE4D home for config replacement: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	lease, err := mceruntime.AcquireHomeConfig(ctx, root, mceruntime.LockExclusive, configLockWait())
	if err != nil {
		return fmt.Errorf("acquire exclusive runtime config lock: %w", err)
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("release exclusive runtime config lock: %w", closeErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	currentRaw, present, err := readCanonicalAt(root)
	if err != nil {
		return err
	}
	current, err := store.pipeline.ParseSource(currentRaw, present)
	if err != nil {
		return fmt.Errorf("parse canonical runtime config before replacement: %w", err)
	}
	if !sameDigest(current.Digest(), expected) {
		return appconfig.ErrConflict
	}
	if err := root.WriteAtomicFile("runtime.json", candidate.CanonicalJSON(), 0o600); err != nil {
		return fmt.Errorf("atomically replace canonical runtime config: %w", err)
	}
	return nil
}

func (store *homeConfigStore) openRoot(create bool) (*mceruntime.Root, bool, error) {
	if create {
		if err := os.MkdirAll(store.rootPath, 0o700); err != nil {
			return nil, false, fmt.Errorf("create MCE4D home: %w", err)
		}
	} else {
		if _, err := os.Lstat(store.rootPath); errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		} else if err != nil {
			return nil, false, fmt.Errorf("inspect MCE4D home: %w", err)
		}
	}
	root, err := mceruntime.OpenRoot(store.rootPath)
	if err != nil {
		return nil, false, fmt.Errorf("open verified MCE4D home: %w", err)
	}
	return root, true, nil
}

func readCanonicalAt(root *mceruntime.Root) (raw []byte, present bool, err error) {
	file, err := root.OpenRegularFile("runtime.json")
	if err != nil {
		finalPath, pathErr := root.FinalPath()
		if pathErr != nil {
			return nil, false, errors.Join(err, pathErr)
		}
		_, statErr := os.Lstat(filepath.Join(finalPath, "runtime.json"))
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, false, nil
		}
		if statErr != nil {
			return nil, false, errors.Join(err, statErr)
		}
		return nil, false, fmt.Errorf("open canonical runtime config: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, false, errors.Join(statErr, file.Close())
	}
	if info.Size() > maxRuntimeConfigBytes {
		return nil, false, errors.Join(fmt.Errorf("runtime config exceeds %d bytes", maxRuntimeConfigBytes), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxRuntimeConfigBytes+1))
	closeErr := file.Close()
	if len(data) > maxRuntimeConfigBytes {
		return nil, false, errors.Join(fmt.Errorf("runtime config exceeds %d bytes", maxRuntimeConfigBytes), readErr, closeErr)
	}
	if readErr != nil || closeErr != nil {
		return nil, false, errors.Join(readErr, closeErr)
	}
	return data, true, nil
}

func sameDigest(left, right *model.Hash) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func configLockWait() time.Duration {
	milliseconds := runtimeconfig.DefaultRuntimeConfig().Core.WriterLockWaitMS
	if milliseconds < 1 {
		milliseconds = 1
	}
	return time.Duration(milliseconds) * time.Millisecond
}
