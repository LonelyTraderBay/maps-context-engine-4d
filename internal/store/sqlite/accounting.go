package sqlite

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

const maxOwnedCacheFiles = 1_000_000

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

// worktreeCacheBytesFor measures every regular file below the verified
// worktree-owned cache root. It rejects reparse/symlink/mount escapes and
// relies on Root.OpenRegularFile to reject hard-linked aliases.
func (owner *storeOwnership) worktreeCacheBytesFor(worktreeID model.UUID) (int64, error) {
	return owner.worktreeCacheBytesForJournal(worktreeID, model.VolumeAdmissionJournal{})
}

func (owner *storeOwnership) worktreeCacheBytesForJournal(worktreeID model.UUID, journal model.VolumeAdmissionJournal) (int64, error) {
	return owner.measureOwnedTree("worktrees/"+string(worktreeID), worktreeID, journal)
}

func (owner *storeOwnership) measureOwnedPathBytes(relative model.AdmissionOwnedPath, journal model.VolumeAdmissionJournal) (int64, error) {
	if owner == nil || owner.root == nil || relative.Validate() != nil {
		return 0, fmt.Errorf("invalid owned path measurement: %w", ErrAdmissionOwnership)
	}
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		return 0, err
	}
	absolute := filepath.Join(rootPath, filepath.FromSlash(string(relative)))
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect owned durable path: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("durable output path is not a private directory: %w", ErrInvalidStorePath)
	}
	parts := strings.Split(string(relative), "/")
	if len(parts) < 4 || parts[0] != "worktrees" || parts[1] != string(owner.worktreeID) {
		return 0, fmt.Errorf("durable path is not scoped to the worktree: %w", ErrAdmissionOwnership)
	}
	return owner.measureOwnedTree(string(relative), owner.worktreeID, journal)
}

// renameResultBytesFor measures actual result spools and interrupted atomic
// write temporaries. Live atomic temporaries remain covered by their reservation.
func (owner *storeOwnership) renameResultBytesFor(worktreeID model.UUID) (int64, error) {
	return owner.renameResultBytesForJournal(worktreeID, model.VolumeAdmissionJournal{})
}

func (owner *storeOwnership) renameResultBytesForJournal(worktreeID model.UUID, journal model.VolumeAdmissionJournal) (int64, error) {
	return owner.measureOwnedTree("worktrees/"+string(worktreeID)+"/scratch/rename", worktreeID, journal)
}

func (owner *storeOwnership) measureOwnedTree(relativeRoot string, worktreeID model.UUID, journal model.VolumeAdmissionJournal) (int64, error) {
	if owner == nil || owner.root == nil || owner.volumeID == "" {
		return 0, fmt.Errorf("verified cache owner is required: %w", ErrAdmissionOwnership)
	}
	if err := (model.AdmissionOwnedPath(relativeRoot)).Validate(); err != nil {
		return 0, fmt.Errorf("invalid cache measurement root: %w", err)
	}
	if err := owner.root.EnsurePrivateDirs(relativeRoot); err != nil {
		return 0, fmt.Errorf("prepare verified cache measurement root: %w", err)
	}
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		return 0, err
	}
	worktreePath := filepath.Join(rootPath, filepath.FromSlash(relativeRoot))
	base, err := mceruntime.OpenRoot(worktreePath)
	if err != nil {
		return 0, fmt.Errorf("open owned cache root without following links: %w", ErrInvalidStorePath)
	}
	baseVolume, _, identityErr := base.Identity()
	basePath, pathErr := base.FinalPath()
	baseCloseErr := base.Close()
	if identityErr != nil || pathErr != nil || baseCloseErr != nil || model.Identifier(baseVolume) != owner.volumeID || !pathWithin(rootPath, basePath) {
		return 0, errors.Join(identityErr, pathErr, baseCloseErr, ErrInvalidStorePath)
	}
	var total int64
	var fileCount int
	err = filepath.WalkDir(basePath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == basePath {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0 {
			return fmt.Errorf("cache contains a link, reparse point, or special file: %w", ErrInvalidStorePath)
		}
		relative, err := filepath.Rel(rootPath, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("cache entry escaped verified home: %w", ErrInvalidStorePath)
		}
		relative = filepath.ToSlash(relative)
		if err := (model.AdmissionOwnedPath(relative)).Validate(); err != nil {
			return fmt.Errorf("cache entry has an invalid home-relative locator: %w", ErrInvalidStorePath)
		}
		if entry.IsDir() {
			if err := owner.root.EnsurePrivateDirs(relative); err != nil {
				return fmt.Errorf("cache directory is not contained beneath home: %w", ErrInvalidStorePath)
			}
			if relativeRoot == "worktrees/"+string(worktreeID) && (relative == "worktrees/"+string(worktreeID)+"/backups" || relative == "worktrees/"+string(worktreeID)+"/maintenance") {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return fmt.Errorf("cache entry is not a regular file: %w", ErrInvalidStorePath)
		}
		if isLiveRenameAtomicTemp(relative, worktreeID, journal) {
			return nil // The full peak is already charged to ACC-01's live reservation.
		}
		file, err := owner.root.OpenRegularFile(relative)
		if err != nil {
			return fmt.Errorf("securely open owned cache file %s: %w", relative, err)
		}
		opened, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
			return errors.Join(statErr, closeErr, ErrInvalidStorePath)
		}
		fileCount++
		if fileCount > maxOwnedCacheFiles {
			return fmt.Errorf("owned cache exceeds bounded file traversal: %w", ErrAdmissionResourceLimit)
		}
		var ok bool
		total, ok = addNonnegative(total, opened.Size())
		if !ok || total > model.MaxSafeInteger {
			return ErrAdmissionReservationOverflow
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure verified owned cache files: %w", err)
	}
	return total, nil
}

func isLiveRenameAtomicTemp(relative string, worktreeID model.UUID, journal model.VolumeAdmissionJournal) bool {
	prefix := "worktrees/" + string(worktreeID) + "/scratch/rename/.mce4d-atomic-"
	if !strings.HasPrefix(relative, prefix) || !strings.HasSuffix(relative, ".tmp") {
		return false
	}
	token := strings.TrimSuffix(strings.TrimPrefix(relative, prefix), ".tmp")
	if len(token) != 24 {
		return false
	}
	for _, digit := range token {
		if !((digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f')) {
			return false
		}
	}
	resultPrefix := "worktrees/" + string(worktreeID) + "/scratch/rename/"
	for _, entry := range journal.Entries {
		if entry.OperationKind != "rename_result" || entry.WorktreeID == nil || *entry.WorktreeID != worktreeID || len(entry.AllowedPaths) != 1 {
			continue
		}
		path := string(entry.AllowedPaths[0])
		if strings.HasPrefix(path, resultPrefix) && strings.HasSuffix(path, ".json") {
			return true
		}
	}
	return false
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// admissionJournalPath is intentionally only a lexical check here. T010 owns
// verified home/volume containment, the OS lease, and admission serialization.
func admissionJournalPath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("admission journal path must be absolute: %w", ErrAdmissionJournalPath)
	}
	return filepath.Clean(path), nil
}
