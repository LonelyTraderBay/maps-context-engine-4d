package sqlite

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// indexCacheMeasurement describes the private SQLite portion of ACC-01 cache
// accounting. Allocated bytes are deliberately absent: they require a verified
// filesystem-specific measurement and cannot be inferred from logical lengths.
type indexCacheMeasurement struct {
	IndexFileBytes       int64
	DatabaseFileBytes    int64
	WALBytes             int64
	SHMBytes             int64
	RollbackJournalBytes int64
	LivePageBytes        int64
	FreePageBytes        int64
	ObservedAt           time.Time
}

func (store *Store) measureIndexCache(ctx context.Context) (indexCacheMeasurement, error) {
	var measurement indexCacheMeasurement
	if store == nil || store.readers == nil || ctx == nil || store.path == "" {
		return measurement, fmt.Errorf("invalid SQLite cache measurement request: %w", ErrInvalidIndex)
	}

	var pageSize, pageCount, freePages int64
	for _, query := range []struct {
		statement string
		dest      *int64
	}{
		{`PRAGMA page_size`, &pageSize},
		{`PRAGMA page_count`, &pageCount},
		{`PRAGMA freelist_count`, &freePages},
	} {
		if err := store.readers.QueryRowContext(ctx, query.statement).Scan(query.dest); err != nil {
			return measurement, fmt.Errorf("read SQLite cache metric %s: %w", query.statement, err)
		}
	}
	if pageSize < 1 || pageCount < 0 || freePages < 0 || freePages > pageCount {
		return measurement, fmt.Errorf("invalid SQLite page accounting size=%d pages=%d free=%d: %w", pageSize, pageCount, freePages, ErrInvalidIndex)
	}
	liveBytes, ok := multiplyNonnegative(pageSize, pageCount-freePages)
	if !ok {
		return measurement, fmt.Errorf("SQLite live page bytes overflow: %w", ErrInvalidIndex)
	}
	freeBytes, ok := multiplyNonnegative(pageSize, freePages)
	if !ok {
		return measurement, fmt.Errorf("SQLite free page bytes overflow: %w", ErrInvalidIndex)
	}

	lengths := []struct {
		path string
		dest *int64
	}{
		{store.path, &measurement.DatabaseFileBytes},
		{store.path + "-wal", &measurement.WALBytes},
		{store.path + "-shm", &measurement.SHMBytes},
		{store.path + "-journal", &measurement.RollbackJournalBytes},
	}
	var identities []os.FileInfo
	for _, file := range lengths {
		length, info, err := regularFileLengthNoFollow(file.path, file.path != store.path)
		if err != nil {
			return measurement, err
		}
		*file.dest = length
		if info == nil {
			continue
		}
		duplicate := false
		for _, identity := range identities {
			if os.SameFile(identity, info) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			measurement.IndexFileBytes, ok = addNonnegative(measurement.IndexFileBytes, length)
			if !ok {
				return measurement, fmt.Errorf("SQLite index file byte total overflow: %w", ErrInvalidIndex)
			}
			identities = append(identities, info)
		}
	}
	measurement.LivePageBytes = liveBytes
	measurement.FreePageBytes = freeBytes
	measurement.ObservedAt = time.Now().UTC()
	return measurement, nil
}

func regularFileLengthNoFollow(path string, optional bool) (int64, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, fmt.Errorf("inspect SQLite cache file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 {
		return 0, nil, fmt.Errorf("SQLite cache entry is not a regular file: %w", ErrInvalidStorePath)
	}
	return info.Size(), info, nil
}

func addNonnegative(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}

func multiplyNonnegative(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || (right != 0 && left > math.MaxInt64/right) {
		return 0, false
	}
	return left * right, true
}

// admissionJournalPath is intentionally only a lexical check here. T010 owns
// verified home/volume containment, the OS lease, and admission serialization.
func admissionJournalPath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("admission journal path must be absolute: %w", ErrAdmissionJournalPath)
	}
	return filepath.Clean(path), nil
}
