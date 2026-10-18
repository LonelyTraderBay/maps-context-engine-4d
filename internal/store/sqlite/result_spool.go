package sqlite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

const (
	MaxRenameResultSpoolBytes     = int64(32 << 20)
	MaxRenameResultAggregateBytes = int64(128 << 20)
	renameResultOperationKind     = "rename_result"
)

var (
	ErrRenameResultSpoolInvalid  = errors.New("rename result spool is invalid")
	ErrRenameResultSpoolExpired  = errors.New("rename result spool is missing or expired")
	ErrRenameResultSpoolConflict = errors.New("rename result identity already contains different bytes")
)

type renameResultReservation struct {
	owner       *storeOwnership
	operationID model.UUID
	lease       *mceruntime.Lease
	resultPath  model.AdmissionOwnedPath
	initialSize int64
	peakBytes   int64
	closeOnce   sync.Once
	closeErr    error
}

// WriteRenameResultSpool atomically publishes one immutable result under the
// worktree scratch root after both the shared cache and aggregate rename quota
// have admitted its complete serialized size. It does not take the SQLite writer.
func (store *Store) WriteRenameResultSpool(ctx context.Context, resultID model.UUID, raw []byte, digest model.Hash) error {
	return store.writeRenameResultSpool(ctx, resultID, raw, digest, MaxRenameResultSpoolBytes, MaxRenameResultAggregateBytes)
}

// writeRenameResultSpool keeps custom bounds package-private for quota tests;
// production callers share the same fixed limits across every process.
func (store *Store) writeRenameResultSpool(ctx context.Context, resultID model.UUID, raw []byte, digest model.Hash, perResultLimit, aggregateLimit int64) error {
	path, err := store.renameResultPath(resultID)
	if err != nil || store == nil || store.ownership == nil || ctx == nil || len(raw) == 0 ||
		int64(len(raw)) > perResultLimit || perResultLimit < 1 || perResultLimit > MaxRenameResultSpoolBytes ||
		aggregateLimit < perResultLimit || aggregateLimit > MaxRenameResultAggregateBytes ||
		model.SHA256(raw) != digest {
		return fmt.Errorf("invalid rename spool write or digest: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	owner := store.ownership
	if err := owner.root.EnsurePrivateDirs(filepath.ToSlash(filepath.Dir(string(path)))); err != nil {
		return fmt.Errorf("prepare owned rename spool directory: %w", err)
	}
	resultLease, err := mceruntime.AcquireOperationLease(ctx, owner.root, string(resultID), owner.lockWait)
	if err != nil {
		return fmt.Errorf("acquire immutable result identity lease: %w", err)
	}
	defer resultLease.Close()
	if existing, readErr := readRenameResultFile(owner, path, perResultLimit); readErr == nil {
		if model.SHA256(existing) == digest && equalBytes(existing, raw) {
			return nil
		}
		return ErrRenameResultSpoolConflict
	} else if !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, ErrRenameResultSpoolExpired) {
		return readErr
	}
	reservation, err := owner.admitRenameResultGrowth(ctx, resultID, int64(len(raw)), aggregateLimit)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, reservation.Close())
	}
	writeErr := owner.root.WriteAtomicFile(string(path), raw, 0o600)
	settleErr := reservation.Close()
	if writeErr != nil || settleErr != nil {
		return errors.Join(fmt.Errorf("publish immutable rename result spool: %w", writeErr), settleErr)
	}
	readback, err := readRenameResultFile(owner, path, perResultLimit)
	if err != nil || model.SHA256(readback) != digest || !equalBytes(readback, raw) {
		return fmt.Errorf("verify immutable rename spool readback: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	return nil
}

// ReadRenameResultSpool takes only a per-result shared OS lease and a verified
// file handle. Page reads do not acquire the SQLite writer or a global lock.
func (store *Store) ReadRenameResultSpool(ctx context.Context, resultID model.UUID, digest model.Hash) ([]byte, error) {
	return store.readRenameResultSpool(ctx, resultID, digest, MaxRenameResultSpoolBytes)
}

func (store *Store) readRenameResultSpool(ctx context.Context, resultID model.UUID, digest model.Hash, maxBytes int64) ([]byte, error) {
	path, err := store.renameResultPath(resultID)
	if err != nil || store == nil || store.ownership == nil || ctx == nil || maxBytes < 1 || maxBytes > MaxRenameResultSpoolBytes {
		return nil, fmt.Errorf("invalid rename spool read: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	if _, err := model.ParseHash(string(digest)); err != nil {
		return nil, ErrRenameResultSpoolInvalid
	}
	lease, err := store.ownership.root.AcquireLock(ctx, "operation-leases/"+string(resultID)+".lock", mceruntime.LockShared, store.ownership.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire shared immutable result lease: %w", err)
	}
	defer lease.Close()
	raw, err := readRenameResultFile(store.ownership, path, maxBytes)
	if err != nil {
		return nil, err
	}
	if model.SHA256(raw) != digest {
		return nil, fmt.Errorf("rename spool digest does not match immutable result identity: %w", ErrRenameResultSpoolInvalid)
	}
	return raw, nil
}

// RemoveRenameResultSpool evicts only the exact owned result under its
// exclusive result lease. It makes no secure-erasure claim.
func (store *Store) RemoveRenameResultSpool(ctx context.Context, resultID model.UUID) error {
	path, err := store.renameResultPath(resultID)
	if err != nil || store == nil || store.ownership == nil || ctx == nil {
		return fmt.Errorf("invalid rename spool removal: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	owner := store.ownership
	if err := owner.root.EnsurePrivateDirs(filepath.ToSlash(filepath.Dir(string(path)))); err != nil {
		return fmt.Errorf("verify owned rename spool parent: %w", err)
	}
	lease, err := mceruntime.AcquireOperationLease(ctx, owner.root, string(resultID), owner.lockWait)
	if err != nil {
		return fmt.Errorf("acquire exclusive result eviction lease: %w", err)
	}
	defer lease.Close()
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		return err
	}
	fullPath := filepath.Join(rootPath, filepath.FromSlash(string(path)))
	before, err := os.Lstat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return ErrRenameResultSpoolExpired
	}
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("inspect rename spool before eviction: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	file, err := owner.root.OpenRegularFile(string(path))
	if err != nil {
		return fmt.Errorf("securely open rename spool before eviction: %w", err)
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(before, opened) {
		return errors.Join(statErr, closeErr, ErrRenameResultSpoolInvalid)
	}
	if err := os.Remove(fullPath); err != nil {
		return fmt.Errorf("evict expired owned rename spool: %w", err)
	}
	if _, err := os.Lstat(fullPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rename spool remains after eviction: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	return nil
}

func (store *Store) renameResultPath(resultID model.UUID) (model.AdmissionOwnedPath, error) {
	if store == nil || store.ownership == nil {
		return "", ErrInvalidIndex
	}
	parsed, err := model.ParseUUID(string(resultID))
	if err != nil || parsed != resultID {
		return "", fmt.Errorf("result identity must be a canonical UUID: %w", ErrRenameResultSpoolInvalid)
	}
	path := model.AdmissionOwnedPath("worktrees/" + string(store.ownership.worktreeID) + "/scratch/rename/" + string(resultID) + ".json")
	if err := path.Validate(); err != nil {
		return "", fmt.Errorf("invalid owned rename spool path: %w", err)
	}
	return path, nil
}

func readRenameResultFile(owner *storeOwnership, path model.AdmissionOwnedPath, maxBytes int64) ([]byte, error) {
	file, err := owner.root.OpenRegularFile(string(path))
	if errors.Is(err, mceruntime.ErrOpenFailed) {
		rootPath, pathErr := owner.root.FinalPath()
		if pathErr == nil {
			_, statErr := os.Lstat(filepath.Join(rootPath, filepath.FromSlash(string(path))))
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, ErrRenameResultSpoolExpired
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("securely open immutable rename spool: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxBytes {
		return nil, fmt.Errorf("rename spool size is invalid or expired: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(raw)) != info.Size() || int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("read bounded immutable rename spool: %w", errors.Join(err, ErrRenameResultSpoolInvalid))
	}
	return raw, nil
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (owner *storeOwnership) admitRenameResultGrowth(ctx context.Context, resultID model.UUID, peakBytes, aggregateLimit int64) (*renameResultReservation, error) {
	if owner == nil || owner.root == nil || owner.lifecycle == nil || ctx == nil || peakBytes < 1 || peakBytes > MaxRenameResultSpoolBytes ||
		aggregateLimit < peakBytes || aggregateLimit > MaxRenameResultAggregateBytes {
		return nil, fmt.Errorf("invalid rename result quota request: %w", ErrAdmissionResourceLimit)
	}
	resultPath := model.AdmissionOwnedPath("worktrees/" + string(owner.worktreeID) + "/scratch/rename/" + string(resultID) + ".json")
	if err := resultPath.Validate(); err != nil {
		return nil, fmt.Errorf("invalid rename result owned path: %w", ErrAdmissionOwnership)
	}
	configLease, err := mceruntime.AcquireHomeConfig(ctx, owner.root, mceruntime.LockShared, owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire shared config before rename admission: %w", err)
	}
	defer configLease.Close()
	config, configDigest, err := owner.readAdmissionConfig(ctx)
	if err != nil {
		return nil, err
	}
	volumeLease, err := mceruntime.AcquireVolumeAdmission(ctx, owner.root, string(owner.volumeID), owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire shared volume admission for rename spool: %w", err)
	}
	defer volumeLease.Close()
	journal, err := owner.readJournalAnyConfig()
	if err != nil {
		return nil, err
	}
	journal, err = owner.recoverDeadOwners(ctx, journal)
	if err != nil {
		return nil, err
	}
	if journal.AdmissionConfigDigest != configDigest {
		if len(journal.Entries) != 0 {
			return nil, ErrAdmissionConfigConflict
		}
		journal.AdmissionConfigDigest = configDigest
	}
	currentCacheBytes, err := owner.worktreeCacheBytesForJournal(owner.worktreeID, journal)
	if err != nil {
		return nil, fmt.Errorf("measure complete owned cache before rename spool: %w", err)
	}
	totals, err := sumAdmissionReservations(journal)
	if err != nil {
		return nil, err
	}
	cacheUse, ok := addNonnegative(currentCacheBytes, totals.CacheByWorktree[owner.worktreeID])
	if !ok || cacheUse > config.MaxWorktreeCacheBytes || peakBytes > config.MaxWorktreeCacheBytes-cacheUse {
		return nil, fmt.Errorf("rename spool exceeds effective worktree cache quota: %w", ErrAdmissionResourceLimit)
	}
	if err := owner.root.EnsurePrivateDirs(filepath.ToSlash(filepath.Dir(string(resultPath)))); err != nil {
		return nil, fmt.Errorf("prepare owned rename spool directory after admission check: %w", err)
	}
	renameBytes, err := owner.renameResultBytesForJournal(owner.worktreeID, journal)
	if err != nil {
		return nil, fmt.Errorf("measure aggregate retained rename results: %w", err)
	}
	renameUse, ok := addNonnegative(renameBytes, totals.RenameByWorktree[owner.worktreeID])
	if !ok || renameUse > aggregateLimit || peakBytes > aggregateLimit-renameUse {
		return nil, fmt.Errorf("rename result aggregate quota would be exceeded: %w", ErrAdmissionResourceLimit)
	}
	available, err := owner.root.AvailableBytes()
	if err != nil {
		return nil, fmt.Errorf("measure verified volume headroom for rename spool: %w", err)
	}
	volumeNeeded, ok := addNonnegative(totals.VolumeRemainingBytes, peakBytes)
	if !ok {
		return nil, ErrAdmissionReservationOverflow
	}
	volumeNeeded, ok = addNonnegative(volumeNeeded, admissionMetadataPeak)
	if !ok {
		return nil, ErrAdmissionReservationOverflow
	}
	volumeNeeded, ok = addNonnegative(volumeNeeded, config.VolumeReserveBytes)
	if !ok || available < volumeNeeded {
		return nil, fmt.Errorf("rename spool would breach verified volume reserve: %w", ErrAdmissionResourceLimit)
	}
	if len(journal.Entries) >= 256 {
		return nil, fmt.Errorf("volume admission entry cap reached: %w", ErrAdmissionResourceLimit)
	}
	operationID, err := newOperationUUID()
	if err != nil {
		return nil, fmt.Errorf("create rename admission owner identity: %w", err)
	}
	operationLease, err := mceruntime.AcquireOperationLease(ctx, owner.root, string(operationID), 0)
	if err != nil {
		return nil, fmt.Errorf("acquire rename admission owner lease: %w", err)
	}
	worktreeID := owner.worktreeID
	entry := model.VolumeAdmissionEntry{
		OperationID: operationID, ScopeKind: "worktree", WorktreeID: &worktreeID,
		OwnerIdentity:                model.Identifier("lease-" + string(operationID)),
		OwnershipRef:                 model.AdmissionOwnedPath("operation-leases/" + string(operationID) + ".lock"),
		OperationKind:                renameResultOperationKind,
		AllowedPaths:                 []model.AdmissionOwnedPath{resultPath},
		VolumeReservedRemainingBytes: peakBytes, CacheReservedRemainingBytes: peakBytes,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := entry.Validate(); err != nil {
		_ = operationLease.Close()
		return nil, fmt.Errorf("validate rename quota entry: %w", err)
	}
	initialSize, err := owner.ownedPathBytes(resultPath)
	if err != nil {
		_ = operationLease.Close()
		return nil, err
	}
	journal.Entries = append(journal.Entries, entry)
	if err := owner.writeJournal(journal); err != nil {
		_ = operationLease.Close()
		return nil, fmt.Errorf("persist rename spool quota reservation: %w", err)
	}
	owner.config, owner.configDigest = config, configDigest
	return &renameResultReservation{owner: owner, operationID: operationID, lease: operationLease, resultPath: resultPath, initialSize: initialSize, peakBytes: peakBytes}, nil
}

func (owner *storeOwnership) ownedPathBytes(path model.AdmissionOwnedPath) (int64, error) {
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		return 0, err
	}
	fullPath := filepath.Join(rootPath, filepath.FromSlash(string(path)))
	info, err := os.Lstat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 {
		return 0, fmt.Errorf("owned reservation path is not a regular file: %w", errors.Join(err, ErrInvalidStorePath))
	}
	file, err := owner.root.OpenRegularFile(string(path))
	if err != nil {
		return 0, fmt.Errorf("securely open reserved rename result: %w", err)
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(info, opened) {
		return 0, errors.Join(statErr, closeErr, ErrInvalidStorePath)
	}
	return opened.Size(), nil
}

func (reservation *renameResultReservation) Close() error {
	if reservation == nil {
		return nil
	}
	reservation.closeOnce.Do(func() {
		owner := reservation.owner
		defer func() {
			if reservation.lease != nil {
				reservation.closeErr = errors.Join(reservation.closeErr, reservation.lease.Close())
			}
		}()
		configLease, configErr := mceruntime.AcquireHomeConfig(context.Background(), owner.root, mceruntime.LockShared, owner.lockWait)
		if configErr == nil {
			defer func() { reservation.closeErr = errors.Join(reservation.closeErr, configLease.Close()) }()
		}
		volumeLease, err := mceruntime.AcquireVolumeAdmission(context.Background(), owner.root, string(owner.volumeID), owner.lockWait)
		if err != nil {
			reservation.closeErr = errors.Join(reservation.closeErr, fmt.Errorf("reacquire admission lock to settle rename result: %w", err))
			return
		}
		defer func() { reservation.closeErr = errors.Join(reservation.closeErr, volumeLease.Close()) }()
		journal, err := owner.readJournalAnyConfig()
		if err != nil {
			reservation.closeErr = errors.Join(reservation.closeErr, err)
			return
		}
		actualBytes, err := owner.ownedPathBytes(reservation.resultPath)
		if err != nil {
			reservation.closeErr = errors.Join(reservation.closeErr, err)
		} else if actualBytes > reservation.initialSize && actualBytes-reservation.initialSize > reservation.peakBytes {
			reservation.closeErr = errors.Join(reservation.closeErr, fmt.Errorf("rename spool growth exceeded admitted peak: %w", ErrAdmissionResourceLimit))
		}
		filtered := journal.Entries[:0]
		found := false
		for _, entry := range journal.Entries {
			if entry.OperationID == reservation.operationID {
				found = true
				continue
			}
			filtered = append(filtered, entry)
		}
		if !found {
			reservation.closeErr = errors.Join(reservation.closeErr, ErrAdmissionOwnership)
			return
		}
		journal.Entries = filtered
		if configErr == nil && len(journal.Entries) == 0 {
			if config, digest, readErr := owner.readAdmissionConfig(context.Background()); readErr == nil {
				journal.AdmissionConfigDigest = digest
				owner.config, owner.configDigest = config, digest
			}
		}
		if err := owner.writeJournal(journal); err != nil {
			reservation.closeErr = errors.Join(reservation.closeErr, fmt.Errorf("settle rename result reservation: %w", err))
		}
	})
	return reservation.closeErr
}
