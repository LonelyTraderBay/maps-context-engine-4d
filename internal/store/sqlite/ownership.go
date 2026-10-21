package sqlite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

const (
	defaultLockWaitMS         = int64(5_000)
	indexOpenPeakGrowth       = int64(16 << 20)
	manifestWriteOverhead     = int64(256 << 10)
	admissionMetadataPeak     = int64(2 << 20)
	indexAdmissionOperation   = "index"
	recordsAdmissionOperation = "record_write"
)

var (
	ErrAdmissionResourceLimit = errors.New("storage admission resource limit exceeded")
	ErrAdmissionOwnership     = errors.New("volume admission owner recovery is not verifiable")
)

type AdmissionConfigProvider func(context.Context) (model.AdmissionConfig, error)

type storeOwnership struct {
	root           *mceruntime.Root
	homeLifecycle  *mceruntime.Lease
	lifecycle      *mceruntime.Lease
	worktreeID     model.UUID
	indexRelative  string
	volumeID       model.Identifier
	homeIdentity   model.Identifier
	config         model.AdmissionConfig
	configProvider AdmissionConfigProvider
	configDigest   model.Hash
	lockWait       time.Duration
	closeOnce      sync.Once
	closeErr       error
}

type admissionReservation struct {
	owner              *storeOwnership
	operationID        model.UUID
	lease              *mceruntime.Lease
	initialSize        int64
	initialDurableSize int64
	peakBytes          int64
	cachePeakBytes     int64
	durablePath        model.AdmissionOwnedPath
	durablePaths       []model.AdmissionOwnedPath
	operationKind      string
	closeOnce          sync.Once
	closeErr           error
}

func newStoreOwnership(ctx context.Context, options Options, readOnly bool) (*storeOwnership, error) {
	if ctx == nil || strings.TrimSpace(options.HomePath) == "" {
		return nil, fmt.Errorf("verified MCE4D home and context are required: %w", ErrInvalidIndex)
	}
	worktreeID, err := model.ParseUUID(string(options.WorktreeID))
	if err != nil || worktreeID != options.WorktreeID {
		return nil, fmt.Errorf("valid worktree UUID is required for store ownership: %w", ErrInvalidIndex)
	}
	waitMS := options.LockWaitMS
	if waitMS == 0 {
		waitMS = defaultLockWaitMS
	}
	if waitMS < 1 || waitMS > math.MaxInt32 {
		return nil, fmt.Errorf("lock wait must be within 1..%d ms: %w", int64(math.MaxInt32), ErrInvalidIndex)
	}
	root, err := mceruntime.OpenRoot(options.HomePath)
	if err != nil {
		return nil, fmt.Errorf("open verified MCE4D home: %w", err)
	}
	owned := &storeOwnership{
		root: root, worktreeID: worktreeID, indexRelative: "worktrees/" + string(worktreeID) + "/index.sqlite",
		config: options.AdmissionConfig, configProvider: options.AdmissionConfigProvider, lockWait: time.Duration(waitMS) * time.Millisecond,
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = owned.Close()
		}
	}()
	if !matchesOwnedIndexPath(options.HomePath, options.Path, owned.indexRelative) {
		return nil, fmt.Errorf("index path must be the exact worktree-owned path: %w", ErrInvalidStorePath)
	}
	volumeID, homeIdentity, err := root.Identity()
	if err != nil {
		return nil, fmt.Errorf("read verified volume/home identity: %w", err)
	}
	owned.volumeID = model.Identifier(volumeID)
	owned.homeIdentity = model.Identifier(homeIdentity)
	if readOnly {
		owned.homeLifecycle, err = root.AcquireExistingLock(ctx, "lifecycle.lock", mceruntime.LockShared, owned.lockWait)
	} else {
		owned.homeLifecycle, err = mceruntime.AcquireHomeLifecycle(ctx, root, mceruntime.LockShared, owned.lockWait)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire shared home lifecycle lease: %w", err)
	}
	if !readOnly {
		if err := mceruntime.EnsureWorktreeLockLayout(root, string(worktreeID)); err != nil {
			return nil, fmt.Errorf("prepare verified store lock layout: %w", err)
		}
		owned.lifecycle, err = mceruntime.AcquireWorktreeLifecycle(ctx, root, string(worktreeID), mceruntime.LockShared, owned.lockWait)
	} else {
		owned.lifecycle, err = root.AcquireExistingLock(ctx, "worktrees/"+string(worktreeID)+"/lifecycle.lock", mceruntime.LockShared, owned.lockWait)
	}
	if err != nil {
		return nil, fmt.Errorf("acquire shared worktree lifecycle lease: %w", err)
	}
	if err := ensureNoActiveWorktreeRestore(root, worktreeID); err != nil {
		return nil, err
	}
	if !readOnly {
		configLease, lockErr := mceruntime.AcquireHomeConfig(ctx, root, mceruntime.LockShared, owned.lockWait)
		if lockErr != nil {
			return nil, fmt.Errorf("acquire shared home config lease: %w", lockErr)
		}
		config, digest, err := owned.readAdmissionConfig(ctx)
		closeErr := configLease.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, fmt.Errorf("release shared home config lease: %w", closeErr)
		}
		owned.config, owned.configDigest = config, digest
	}
	closeOnError = false
	return owned, nil
}

func ensureNoActiveWorktreeRestore(root *mceruntime.Root, worktreeID model.UUID) error {
	marker := "worktrees/" + string(worktreeID) + "/maintenance/restore-active.json"
	raw, err := readOwnedFileBounded(root, marker, 4<<10)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot verify worktree restore state: %w: %v", ErrStoreRecoveryRequired, err)
	}
	active, err := decodeRestoreActiveMarker(raw, worktreeID)
	if err != nil {
		return fmt.Errorf("cannot verify worktree restore marker: %w", errors.Join(ErrStoreRecoveryRequired, err))
	}
	journal, err := readWorktreeRestoreJournal(root, active.JournalPath, active.OperationID, worktreeID)
	if err != nil {
		return fmt.Errorf("cannot verify worktree restore journal: %w", errors.Join(ErrStoreRecoveryRequired, err))
	}
	if journal.Phase == "VERIFIED" && (journal.Outcome == "restored" || journal.Outcome == "rolled_back") {
		return nil
	}
	return fmt.Errorf("worktree %s has an incomplete maintenance operation: %w", worktreeID, ErrStoreRecoveryRequired)
}

// newMaintenanceOwnership opens the verified home and shared configuration
// authority without taking a worktree SHARED lifecycle lease. The caller must
// acquire the worktree EXCLUSIVE maintenance lease before opening any store.
func newMaintenanceOwnership(ctx context.Context, options Options) (*storeOwnership, error) {
	if ctx == nil || strings.TrimSpace(options.HomePath) == "" {
		return nil, fmt.Errorf("verified MCE4D home and context are required: %w", ErrInvalidIndex)
	}
	path, _, err := normalizeOptions(&options)
	if err != nil {
		return nil, err
	}
	worktreeID, err := model.ParseUUID(string(options.WorktreeID))
	if err != nil || worktreeID != options.WorktreeID {
		return nil, fmt.Errorf("valid worktree UUID is required for maintenance ownership: %w", ErrInvalidIndex)
	}
	waitMS := options.LockWaitMS
	if waitMS == 0 {
		waitMS = defaultLockWaitMS
	}
	if waitMS < 1 || waitMS > math.MaxInt32 {
		return nil, fmt.Errorf("lock wait must be within 1..%d ms: %w", int64(math.MaxInt32), ErrInvalidIndex)
	}
	indexRelative := "worktrees/" + string(worktreeID) + "/index.sqlite"
	if !matchesOwnedIndexPath(options.HomePath, path, indexRelative) {
		return nil, fmt.Errorf("maintenance index path must be the exact worktree-owned path: %w", ErrInvalidStorePath)
	}
	root, err := mceruntime.OpenRoot(options.HomePath)
	if err != nil {
		return nil, fmt.Errorf("open verified MCE4D home for maintenance: %w", err)
	}
	owner := &storeOwnership{
		root: root, worktreeID: worktreeID, indexRelative: indexRelative,
		config: options.AdmissionConfig, configProvider: options.AdmissionConfigProvider,
		lockWait: time.Duration(waitMS) * time.Millisecond,
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = owner.Close()
		}
	}()
	volumeID, homeIdentity, err := root.Identity()
	if err != nil {
		return nil, fmt.Errorf("read verified volume/home identity: %w", err)
	}
	owner.volumeID = model.Identifier(volumeID)
	owner.homeIdentity = model.Identifier(homeIdentity)
	owner.homeLifecycle, err = mceruntime.AcquireHomeLifecycle(ctx, root, mceruntime.LockShared, owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire shared home lifecycle lease for maintenance: %w", err)
	}
	if err := mceruntime.EnsureWorktreeLockLayout(root, string(worktreeID)); err != nil {
		return nil, fmt.Errorf("prepare verified maintenance lock layout: %w", err)
	}
	configLease, err := mceruntime.AcquireHomeConfig(ctx, root, mceruntime.LockShared, owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire shared home config lease for maintenance: %w", err)
	}
	config, digest, configErr := owner.readAdmissionConfig(ctx)
	closeErr := configLease.Close()
	if configErr != nil || closeErr != nil {
		return nil, errors.Join(configErr, closeErr)
	}
	owner.config, owner.configDigest = config, digest
	closeOnError = false
	return owner, nil
}

// SQLite receives the caller's path spelling, so bind it to the same HomePath
// spelling used to open the verified root. Windows final-handle paths may use
// a different DOS/8.3 spelling for the same directory.
func matchesOwnedIndexPath(homePath, candidate, expectedRelative string) bool {
	absoluteHome, err := filepath.Abs(homePath)
	if err != nil {
		return false
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteHome, absoluteCandidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return filepath.Clean(relative) == filepath.Clean(filepath.FromSlash(expectedRelative))
}

func comparableOwnedPath(path string) string {
	if strings.HasPrefix(path, `\\?\UNC\`) {
		path = `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
	} else if strings.HasPrefix(path, `\\?\`) {
		path = strings.TrimPrefix(path, `\\?\`)
	}
	return filepath.Clean(path)
}

func (owner *storeOwnership) Close() error {
	if owner == nil {
		return nil
	}
	owner.closeOnce.Do(func() {
		if owner.lifecycle != nil {
			owner.closeErr = owner.lifecycle.Close()
		}
		if owner.homeLifecycle != nil {
			if err := owner.homeLifecycle.Close(); owner.closeErr == nil {
				owner.closeErr = err
			}
		}
		if owner.root != nil {
			if err := owner.root.Close(); owner.closeErr == nil {
				owner.closeErr = err
			}
		}
	})
	return owner.closeErr
}

func (owner *storeOwnership) acquireWriter(ctx context.Context) (*mceruntime.Lease, error) {
	if owner == nil || owner.root == nil || owner.lifecycle == nil {
		return nil, fmt.Errorf("store ownership is closed: %w", ErrInvalidIndex)
	}
	return mceruntime.AcquireWorktreeWriter(ctx, owner.root, string(owner.worktreeID), owner.lockWait)
}

func (owner *storeOwnership) readAdmissionConfig(ctx context.Context) (model.AdmissionConfig, model.Hash, error) {
	config := owner.config
	if owner.configProvider != nil {
		var err error
		config, err = owner.configProvider(ctx)
		if err != nil {
			return model.AdmissionConfig{}, "", fmt.Errorf("read shared admission config: %w", err)
		}
	}
	digest, err := config.Digest()
	if err != nil {
		return model.AdmissionConfig{}, "", fmt.Errorf("validate shared admission config: %w", err)
	}
	return config, digest, nil
}

func (owner *storeOwnership) admitIndexGrowth(ctx context.Context, peakBytes int64) (*admissionReservation, error) {
	if owner == nil || owner.root == nil || owner.lifecycle == nil || ctx == nil || peakBytes < 1 || peakBytes > model.MaxSafeInteger {
		return nil, fmt.Errorf("invalid index admission request: %w", ErrInvalidIndex)
	}
	configLease, err := mceruntime.AcquireHomeConfig(ctx, owner.root, mceruntime.LockShared, owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire shared home config lease: %w", err)
	}
	defer configLease.Close()
	config, configDigest, err := owner.readAdmissionConfig(ctx)
	if err != nil {
		return nil, err
	}
	volumeLease, err := mceruntime.AcquireVolumeAdmission(ctx, owner.root, string(owner.volumeID), owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire short volume admission lease: %w", err)
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
		if _, err := owner.worktreeCacheBytesForJournal(owner.worktreeID, journal); err != nil {
			return nil, fmt.Errorf("recheck owned store sizes after config drift: %w", err)
		}
		journal.AdmissionConfigDigest = configDigest
	}
	owner.config, owner.configDigest = config, configDigest
	currentCacheBytes, err := owner.worktreeCacheBytesForJournal(owner.worktreeID, journal)
	if err != nil {
		return nil, fmt.Errorf("measure owned SQLite files before write: %w", err)
	}
	totals, err := sumAdmissionReservations(journal)
	if err != nil {
		return nil, err
	}
	cacheReserved := totals.CacheByWorktree[owner.worktreeID]
	cacheUse, ok := addNonnegative(currentCacheBytes, cacheReserved)
	if !ok || cacheUse > config.MaxWorktreeCacheBytes || peakBytes > config.MaxWorktreeCacheBytes-cacheUse {
		return nil, fmt.Errorf("worktree cache cap would be exceeded: %w", ErrAdmissionResourceLimit)
	}
	available, err := owner.root.AvailableBytes()
	if err != nil {
		return nil, fmt.Errorf("measure verified volume headroom: %w", err)
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
		return nil, fmt.Errorf("verified volume reserve would be breached: %w", ErrAdmissionResourceLimit)
	}
	if len(journal.Entries) >= 256 {
		return nil, fmt.Errorf("volume admission entry cap reached: %w", ErrAdmissionResourceLimit)
	}

	operationID, err := newOperationUUID()
	if err != nil {
		return nil, fmt.Errorf("create operation lease identity: %w", err)
	}
	operationLease, err := mceruntime.AcquireOperationLease(ctx, owner.root, string(operationID), 0)
	if err != nil {
		return nil, fmt.Errorf("acquire operation owner lease: %w", err)
	}
	entry := model.VolumeAdmissionEntry{
		OperationID: operationID, ScopeKind: "worktree", WorktreeID: &owner.worktreeID,
		OwnerIdentity: model.Identifier("lease-" + string(operationID)),
		OwnershipRef:  model.AdmissionOwnedPath("operation-leases/" + string(operationID) + ".lock"),
		OperationKind: indexAdmissionOperation,
		AllowedPaths: []model.AdmissionOwnedPath{
			model.AdmissionOwnedPath(owner.indexRelative), model.AdmissionOwnedPath(owner.indexRelative + "-wal"),
			model.AdmissionOwnedPath(owner.indexRelative + "-shm"), model.AdmissionOwnedPath(owner.indexRelative + "-journal"),
		},
		VolumeReservedRemainingBytes: peakBytes, CacheReservedRemainingBytes: peakBytes,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	journal.Entries = append(journal.Entries, entry)
	if err := owner.writeJournal(journal); err != nil {
		_ = operationLease.Close()
		return nil, fmt.Errorf("persist index growth reservation: %w", err)
	}
	return &admissionReservation{owner: owner, operationID: operationID, lease: operationLease,
		initialSize: currentCacheBytes, peakBytes: peakBytes, cachePeakBytes: peakBytes}, nil
}

func durableOperationID(worktreeID model.UUID, operationKind string, ownedPath model.AdmissionOwnedPath) (model.UUID, error) {
	if operationKind == recordsAdmissionOperation {
		if ownedPath != model.AdmissionOwnedPath(recordsStorePath(worktreeID)) {
			return "", fmt.Errorf("records write must target the worktree records database: %w", ErrAdmissionOwnership)
		}
		operationID, err := newOperationUUID()
		if err != nil {
			return "", fmt.Errorf("create records write operation identity: %w", err)
		}
		return operationID, nil
	}
	area := "backups"
	if operationKind == "maintenance" {
		area = "maintenance"
	}
	parts := strings.Split(string(ownedPath), "/")
	if len(parts) != 4 || parts[0] != "worktrees" || parts[1] != string(worktreeID) || parts[2] != area {
		return "", fmt.Errorf("durable output must be scoped to one worktree operation: %w", ErrAdmissionOwnership)
	}
	operationID, err := model.ParseUUID(parts[3])
	if err != nil || string(operationID) != parts[3] {
		return "", fmt.Errorf("durable output directory must use a canonical operation UUID: %w", ErrAdmissionOwnership)
	}
	return operationID, nil
}

func (owner *storeOwnership) admitDurableGrowth(ctx context.Context, operationKind string, ownedPath model.AdmissionOwnedPath, peakBytes int64) (*admissionReservation, error) {
	if owner == nil || owner.root == nil || owner.homeLifecycle == nil || ctx == nil || peakBytes < 1 || peakBytes > model.MaxSafeInteger ||
		(operationKind != "backup" && operationKind != "maintenance" && operationKind != recordsAdmissionOperation) {
		return nil, fmt.Errorf("invalid durable admission request: %w", ErrInvalidIndex)
	}
	if err := ownedPath.Validate(); err != nil || !strings.HasPrefix(string(ownedPath), "worktrees/"+string(owner.worktreeID)+"/") {
		return nil, fmt.Errorf("durable output is outside the owned worktree: %w", ErrAdmissionOwnership)
	}
	allowedPaths := []model.AdmissionOwnedPath{ownedPath}
	if operationKind == recordsAdmissionOperation {
		if ownedPath != model.AdmissionOwnedPath(recordsStorePath(owner.worktreeID)) {
			return nil, fmt.Errorf("records write must target the worktree records database: %w", ErrAdmissionOwnership)
		}
		allowedPaths = recordsStorePaths(owner.worktreeID)
	}
	configLease, err := mceruntime.AcquireHomeConfig(ctx, owner.root, mceruntime.LockShared, owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire shared home config lease: %w", err)
	}
	defer configLease.Close()
	config, configDigest, err := owner.readAdmissionConfig(ctx)
	if err != nil {
		return nil, err
	}
	volumeLease, err := mceruntime.AcquireVolumeAdmission(ctx, owner.root, string(owner.volumeID), owner.lockWait)
	if err != nil {
		return nil, fmt.Errorf("acquire short volume admission lease: %w", err)
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
		if _, err := owner.worktreeCacheBytesForJournal(owner.worktreeID, journal); err != nil {
			return nil, fmt.Errorf("recheck owned store sizes after config drift: %w", err)
		}
		journal.AdmissionConfigDigest = configDigest
	}
	owner.config, owner.configDigest = config, configDigest
	currentCacheBytes, err := owner.worktreeCacheBytesForJournal(owner.worktreeID, journal)
	if err != nil {
		return nil, fmt.Errorf("measure owned worktree cache before durable write: %w", err)
	}
	totals, err := sumAdmissionReservations(journal)
	if err != nil {
		return nil, err
	}
	cacheUse, ok := addNonnegative(currentCacheBytes, totals.CacheByWorktree[owner.worktreeID])
	if !ok || cacheUse > config.MaxWorktreeCacheBytes {
		return nil, fmt.Errorf("existing worktree cache cap is exceeded: %w", ErrAdmissionResourceLimit)
	}
	available, err := owner.root.AvailableBytes()
	if err != nil {
		return nil, fmt.Errorf("measure verified volume headroom: %w", err)
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
		return nil, fmt.Errorf("verified volume reserve would be breached: %w", ErrAdmissionResourceLimit)
	}
	if len(journal.Entries) >= 256 {
		return nil, fmt.Errorf("volume admission entry cap reached: %w", ErrAdmissionResourceLimit)
	}
	initialDurableSize, err := owner.measureAdmittedDurableBytes(operationKind, allowedPaths, journal)
	if err != nil {
		return nil, fmt.Errorf("measure durable output before write: %w", err)
	}
	operationID, err := durableOperationID(owner.worktreeID, operationKind, ownedPath)
	if err != nil {
		return nil, err
	}
	operationLease, err := mceruntime.AcquireOperationLease(ctx, owner.root, string(operationID), 0)
	if err != nil {
		return nil, fmt.Errorf("acquire operation owner lease: %w", err)
	}
	entry := model.VolumeAdmissionEntry{
		OperationID: operationID, ScopeKind: "worktree", WorktreeID: &owner.worktreeID,
		OwnerIdentity: model.Identifier("lease-" + string(operationID)),
		OwnershipRef:  model.AdmissionOwnedPath("operation-leases/" + string(operationID) + ".lock"),
		OperationKind: operationKind, AllowedPaths: allowedPaths,
		VolumeReservedRemainingBytes: peakBytes, CacheReservedRemainingBytes: 0,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	journal.Entries = append(journal.Entries, entry)
	if err := owner.writeJournal(journal); err != nil {
		_ = operationLease.Close()
		return nil, fmt.Errorf("persist durable growth reservation: %w", err)
	}
	return &admissionReservation{owner: owner, operationID: operationID, lease: operationLease,
		initialSize: currentCacheBytes, initialDurableSize: initialDurableSize, peakBytes: peakBytes,
		durablePath: ownedPath, durablePaths: allowedPaths, operationKind: operationKind}, nil
}

func (reservation *admissionReservation) Close() error {
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
		configLease, configLockErr := mceruntime.AcquireHomeConfig(context.Background(), owner.root, mceruntime.LockShared, owner.lockWait)
		if configLockErr == nil {
			defer func() { reservation.closeErr = errors.Join(reservation.closeErr, configLease.Close()) }()
		}
		volumeLease, err := mceruntime.AcquireVolumeAdmission(context.Background(), owner.root, string(owner.volumeID), owner.lockWait)
		if err != nil {
			reservation.closeErr = fmt.Errorf("reacquire volume admission to settle reservation: %w", err)
			return
		}
		defer func() { reservation.closeErr = errors.Join(reservation.closeErr, volumeLease.Close()) }()
		journal, readErr := owner.readJournalAnyConfig()
		if readErr != nil {
			reservation.closeErr = readErr
			return
		}
		actualBytes, measureErr := owner.worktreeCacheBytesForJournal(owner.worktreeID, journal)
		if measureErr != nil {
			reservation.closeErr = measureErr
			return
		}
		if actualBytes > reservation.initialSize && actualBytes-reservation.initialSize > reservation.cachePeakBytes {
			reservation.closeErr = fmt.Errorf("observed index growth exceeded admitted peak %d bytes: %w", reservation.peakBytes, ErrAdmissionResourceLimit)
		}
		if reservation.durablePath != "" {
			actualDurableBytes, durableErr := owner.measureAdmittedDurableBytes(reservation.operationKind, reservation.durablePaths, journal)
			if durableErr != nil {
				reservation.closeErr = errors.Join(reservation.closeErr, fmt.Errorf("measure durable output after write: %w", durableErr))
			} else if actualDurableBytes > reservation.initialDurableSize && actualDurableBytes-reservation.initialDurableSize > reservation.peakBytes {
				reservation.closeErr = errors.Join(reservation.closeErr, fmt.Errorf("observed durable growth exceeded admitted peak %d bytes: %w", reservation.peakBytes, ErrAdmissionResourceLimit))
			}
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
		} else {
			journal.Entries = filtered
			if configLockErr == nil && len(journal.Entries) == 0 {
				if config, digest, configErr := owner.readAdmissionConfig(context.Background()); configErr == nil {
					journal.AdmissionConfigDigest = digest
					owner.config = config
					owner.configDigest = digest
				}
			}
			if err := owner.writeJournal(journal); err != nil {
				reservation.closeErr = errors.Join(reservation.closeErr, fmt.Errorf("settle index growth reservation: %w", err))
			}
		}
	})
	return reservation.closeErr
}

func (owner *storeOwnership) readJournalAnyConfig() (model.VolumeAdmissionJournal, error) {
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		return model.VolumeAdmissionJournal{}, err
	}
	path := filepath.Join(rootPath, "volume-admission", string(owner.volumeID)+".json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return model.VolumeAdmissionJournal{
			FormatVersion: 1, VolumeID: owner.volumeID, HomeIdentity: owner.homeIdentity,
			AdmissionConfigDigest: owner.configDigest, Entries: []model.VolumeAdmissionEntry{},
		}, nil
	} else if err != nil {
		return model.VolumeAdmissionJournal{}, fmt.Errorf("inspect admission journal: %w", err)
	}
	file, err := owner.root.OpenRegularFile("volume-admission/" + string(owner.volumeID) + ".json")
	if err != nil {
		return model.VolumeAdmissionJournal{}, fmt.Errorf("securely open volume admission journal: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxAdmissionJournalBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return model.VolumeAdmissionJournal{}, errors.Join(readErr, closeErr)
	}
	journal, err := decodeAdmissionJournal(raw)
	if err != nil {
		return model.VolumeAdmissionJournal{}, err
	}
	if journal.VolumeID != owner.volumeID || journal.HomeIdentity != owner.homeIdentity {
		return model.VolumeAdmissionJournal{}, ErrAdmissionIdentityMismatch
	}
	return journal, nil
}

func (owner *storeOwnership) writeJournal(journal model.VolumeAdmissionJournal) error {
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		return err
	}
	if err := owner.root.EnsurePrivateDirs("volume-admission"); err != nil {
		return err
	}
	relative := "volume-admission/" + string(owner.volumeID) + ".json"
	path := filepath.Join(rootPath, filepath.FromSlash(relative))
	if _, err := os.Lstat(path); err == nil {
		file, openErr := owner.root.OpenRegularFile(relative)
		if openErr != nil {
			return openErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := encodeAdmissionJournal(journal)
	if err != nil {
		return err
	}
	if err := owner.root.WriteAtomicFile(relative, raw, 0o600); err != nil {
		return err
	}
	file, err := owner.root.OpenRegularFile(relative)
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err = io.ReadAll(io.LimitReader(file, maxAdmissionJournalBytes+1))
	if err != nil {
		return err
	}
	readback, err := decodeAdmissionJournal(raw)
	if err != nil {
		return err
	}
	if readback.VolumeID != owner.volumeID || readback.HomeIdentity != owner.homeIdentity {
		return ErrAdmissionIdentityMismatch
	}
	return nil
}

func (owner *storeOwnership) recoverDeadOwners(ctx context.Context, journal model.VolumeAdmissionJournal) (model.VolumeAdmissionJournal, error) {
	if len(journal.Entries) == 0 {
		return journal, nil
	}
	kept := journal.Entries[:0]
	changed := false
	for _, entry := range journal.Entries {
		leaseRef := "operation-leases/" + string(entry.OperationID) + ".lock"
		if entry.OwnershipRef != model.AdmissionOwnedPath(leaseRef) {
			return model.VolumeAdmissionJournal{}, fmt.Errorf("entry operation lease locator mismatch: %w", ErrAdmissionOwnership)
		}
		worktreeID, err := owner.verifyRecoverableEntry(entry)
		if err != nil {
			return model.VolumeAdmissionJournal{}, err
		}
		if err := owner.measureRecoverableEntry(worktreeID, entry, journal); err != nil {
			return model.VolumeAdmissionJournal{}, fmt.Errorf("verify reserved owner paths before recovery: %w", err)
		}
		lease, err := mceruntime.AcquireOperationLease(ctx, owner.root, string(entry.OperationID), 0)
		if errors.Is(err, mceruntime.ErrLockBusy) {
			kept = append(kept, entry)
			continue
		}
		if err != nil {
			return model.VolumeAdmissionJournal{}, fmt.Errorf("inspect operation OS lease: %w", err)
		}
		if err := owner.measureRecoverableEntry(worktreeID, entry, journal); err != nil {
			_ = lease.Close()
			return model.VolumeAdmissionJournal{}, fmt.Errorf("measure paths before reclaiming dead reservation: %w", err)
		}
		if err := lease.Close(); err != nil {
			return model.VolumeAdmissionJournal{}, err
		}
		changed = true
	}
	if changed {
		journal.Entries = kept
		if err := owner.writeJournal(journal); err != nil {
			return model.VolumeAdmissionJournal{}, fmt.Errorf("persist recovered admission journal: %w", err)
		}
	}
	return journal, nil
}

func (owner *storeOwnership) verifyRecoverableEntry(entry model.VolumeAdmissionEntry) (model.UUID, error) {
	if entry.ScopeKind != "worktree" || entry.WorktreeID == nil ||
		(entry.OperationKind != indexAdmissionOperation && entry.OperationKind != "rename_result" && entry.OperationKind != "backup" && entry.OperationKind != "maintenance" && entry.OperationKind != recordsAdmissionOperation) {
		return "", fmt.Errorf("stale entry is outside the verified SQLite owner: %w", ErrAdmissionOwnership)
	}
	if entry.OwnerIdentity != model.Identifier("lease-"+string(entry.OperationID)) {
		return "", fmt.Errorf("stale entry owner identity mismatch: %w", ErrAdmissionOwnership)
	}
	worktreeID, err := model.ParseUUID(string(*entry.WorktreeID))
	if err != nil || worktreeID != *entry.WorktreeID {
		return "", fmt.Errorf("stale entry has invalid worktree identity: %w", ErrAdmissionOwnership)
	}
	indexRelative := "worktrees/" + string(worktreeID) + "/index.sqlite"
	want := []string{indexRelative, indexRelative + "-wal", indexRelative + "-shm", indexRelative + "-journal"}
	if entry.OperationKind == "rename_result" {
		if len(entry.AllowedPaths) != 1 {
			return "", fmt.Errorf("rename reservation has an invalid owned-path count: %w", ErrAdmissionOwnership)
		}
		prefix := "worktrees/" + string(worktreeID) + "/scratch/rename/"
		path := string(entry.AllowedPaths[0])
		base := strings.TrimPrefix(path, prefix)
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(base, ".json") {
			return "", fmt.Errorf("rename reservation escaped its result spool root: %w", ErrAdmissionOwnership)
		}
		idText := strings.TrimSuffix(base, ".json")
		resultID, err := model.ParseUUID(idText)
		if err != nil || string(resultID) != idText {
			return "", fmt.Errorf("rename reservation result identity is invalid: %w", ErrAdmissionOwnership)
		}
		if err := entry.AllowedPaths[0].Validate(); err != nil {
			return "", fmt.Errorf("invalid rename spool locator: %w", ErrAdmissionOwnership)
		}
		return worktreeID, nil
	}
	if entry.OperationKind == "backup" || entry.OperationKind == "maintenance" {
		if len(entry.AllowedPaths) != 1 {
			return "", fmt.Errorf("durable reservation has an invalid owned-path count: %w", ErrAdmissionOwnership)
		}
		path := string(entry.AllowedPaths[0])
		base := "worktrees/" + string(worktreeID) + "/"
		area := "backups/"
		if entry.OperationKind == "maintenance" {
			area = "maintenance/"
		}
		prefix := base + area
		operationPath := strings.TrimPrefix(path, prefix)
		operationID, parseErr := model.ParseUUID(operationPath)
		if !strings.HasPrefix(path, prefix) || parseErr != nil || operationID != entry.OperationID || string(operationID) != operationPath {
			return "", fmt.Errorf("durable reservation path does not match its operation identity: %w", ErrAdmissionOwnership)
		}
		if err := entry.AllowedPaths[0].Validate(); err != nil {
			return "", fmt.Errorf("invalid durable output locator: %w", ErrAdmissionOwnership)
		}
		if entry.CacheReservedRemainingBytes != 0 {
			return "", fmt.Errorf("durable reservation must not charge cache quota: %w", ErrAdmissionOwnership)
		}
		return worktreeID, nil
	}
	if entry.OperationKind == recordsAdmissionOperation {
		wantRecords := recordsStorePaths(worktreeID)
		if len(entry.AllowedPaths) != len(wantRecords) || entry.CacheReservedRemainingBytes != 0 || entry.VolumeReservedRemainingBytes <= 0 {
			return "", fmt.Errorf("records reservation has an invalid path or quota shape: %w", ErrAdmissionOwnership)
		}
		for index, expected := range wantRecords {
			if entry.AllowedPaths[index] != expected || expected.Validate() != nil {
				return "", fmt.Errorf("records reservation escaped its owned database paths: %w", ErrAdmissionOwnership)
			}
		}
		return worktreeID, nil
	}
	if len(entry.AllowedPaths) != len(want) {
		return "", fmt.Errorf("stale entry is outside the verified SQLite owner: %w", ErrAdmissionOwnership)
	}
	for index, expected := range want {
		if string(entry.AllowedPaths[index]) != expected {
			return "", fmt.Errorf("stale entry path differs from verified SQLite owner: %w", ErrAdmissionOwnership)
		}
		if err := entry.AllowedPaths[index].Validate(); err != nil {
			return "", fmt.Errorf("invalid stale owned path: %w", ErrAdmissionOwnership)
		}
	}
	return worktreeID, nil
}

func (owner *storeOwnership) measureRecoverableEntry(worktreeID model.UUID, entry model.VolumeAdmissionEntry, journal model.VolumeAdmissionJournal) error {
	if entry.OperationKind == "backup" || entry.OperationKind == "maintenance" {
		if len(entry.AllowedPaths) != 1 {
			return ErrAdmissionOwnership
		}
		_, err := owner.measureOwnedPathBytes(entry.AllowedPaths[0], journal)
		return err
	}
	if entry.OperationKind == recordsAdmissionOperation {
		_, err := owner.measureRecordsStoreBytes(worktreeID, journal)
		return err
	}
	_, err := owner.worktreeCacheBytesForJournal(worktreeID, journal)
	return err
}

func newOperationUUID() (model.UUID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return model.ParseUUID(string(encoded))
}
