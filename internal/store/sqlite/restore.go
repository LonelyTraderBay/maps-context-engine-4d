package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/maintenance"
)

const (
	maxWorktreeBackupManifestBytes = 64 << 20
	maxWorktreeRestoreJournalBytes = 256 << 20
)

type WorktreeRestoreOptions struct {
	HomePath                string
	ManifestPath            string
	WorktreeID              model.UUID
	Confirm                 bool
	LockWaitMS              int64
	BusyTimeoutMS           int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
}

type WorktreeRestoreResult struct {
	OperationID model.UUID `json:"operation_id"`
	WorktreeID  model.UUID `json:"worktree_id"`
	Phase       string     `json:"phase"`
	Outcome     string     `json:"outcome"`
	Stores      []string   `json:"stores"`
}

type WorktreeRestoreRecoveryOptions struct {
	HomePath                string
	WorktreeID              model.UUID
	Apply                   bool
	Confirm                 bool
	LockWaitMS              int64
	BusyTimeoutMS           int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
}

type WorktreeRestoreRecoveryReport struct {
	Operation  string     `json:"operation"`
	WorktreeID model.UUID `json:"worktree_id"`
	Pending    bool       `json:"pending"`
	Applied    bool       `json:"applied"`
	Phase      string     `json:"phase,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
	Completed  []string   `json:"completed_steps,omitempty"`
	Stores     []string   `json:"stores,omitempty"`
}

type restoreJournalStore struct {
	Kind                  string     `json:"kind"`
	WorktreeID            model.UUID `json:"worktree_id"`
	OldIncarnation        model.UUID `json:"old_incarnation"`
	NewIncarnation        model.UUID `json:"new_incarnation"`
	RollbackIncarnation   model.UUID `json:"rollback_incarnation"`
	SchemaVersion         int64      `json:"schema_version"`
	RollbackSchemaVersion int64      `json:"rollback_schema_version"`
	StagedFile            string     `json:"staged_file"`
	StagedByteLength      int64      `json:"staged_byte_length"`
	StagedSHA256          model.Hash `json:"staged_sha256"`
	RollbackFile          string     `json:"rollback_file"`
	RollbackByteLength    int64      `json:"rollback_byte_length"`
	RollbackSHA256        model.Hash `json:"rollback_sha256"`
	RollbackReadyFile     string     `json:"rollback_ready_file"`
	RollbackReadyLength   int64      `json:"rollback_ready_byte_length"`
	RollbackReadySHA256   model.Hash `json:"rollback_ready_sha256"`
}

type restoreJournalArtifact struct {
	SourcePath string     `json:"source_path"`
	StagedFile string     `json:"staged_file"`
	ByteLength int64      `json:"byte_length"`
	SHA256     model.Hash `json:"sha256"`
}

type worktreeRestoreJournal struct {
	FormatVersion int64                    `json:"format_version"`
	OperationID   model.UUID               `json:"operation_id"`
	WorktreeID    model.UUID               `json:"worktree_id"`
	Phase         string                   `json:"phase"`
	Outcome       string                   `json:"outcome"`
	ManifestSHA   model.Hash               `json:"manifest_sha256"`
	Stores        []restoreJournalStore    `json:"stores"`
	Artifacts     []restoreJournalArtifact `json:"artifacts"`
	Completed     []string                 `json:"completed_steps"`
	UpdatedAt     string                   `json:"updated_at"`
}

type restoreActiveMarker struct {
	FormatVersion int64      `json:"format_version"`
	OperationID   model.UUID `json:"operation_id"`
	WorktreeID    model.UUID `json:"worktree_id"`
	JournalPath   string     `json:"journal_path"`
}

type restoreSourceStore struct {
	descriptor WorktreeBackupStore
}

func ReadWorktreeBackupManifest(path string) (WorktreeBackupManifest, error) {
	var manifest WorktreeBackupManifest
	if !filepath.IsAbs(path) || filepath.Base(path) != "manifest.json" {
		return manifest, fmt.Errorf("backup manifest must be an absolute manifest.json path: %w", ErrInvalidIndex)
	}
	root, err := mceruntime.OpenRoot(filepath.Dir(path))
	if err != nil {
		return manifest, fmt.Errorf("open backup manifest directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	raw, err := readOwnedFileBounded(root, "manifest.json", maxWorktreeBackupManifestBytes)
	if err != nil {
		return manifest, err
	}
	return decodeWorktreeBackupManifest(raw)
}

func RestoreWorktree(ctx context.Context, options WorktreeRestoreOptions) (result WorktreeRestoreResult, retErr error) {
	if ctx == nil || !options.Confirm || !filepath.IsAbs(options.HomePath) || !filepath.IsAbs(options.ManifestPath) {
		return result, fmt.Errorf("restore requires an absolute home, an absolute manifest path, confirmation, and context: %w", ErrInvalidIndex)
	}
	if filepath.Base(options.ManifestPath) != "manifest.json" {
		return result, fmt.Errorf("restore manifest must be named manifest.json: %w", ErrInvalidIndex)
	}
	if options.BusyTimeoutMS == 0 {
		options.BusyTimeoutMS = defaultBusyTimeout
	}
	if options.BusyTimeoutMS < 1 || options.BusyTimeoutMS > math.MaxInt32 {
		return result, fmt.Errorf("SQLite busy timeout is outside the supported range: %w", ErrInvalidIndex)
	}
	worktreeID, err := model.ParseUUID(string(options.WorktreeID))
	if err != nil || worktreeID != options.WorktreeID {
		return result, fmt.Errorf("restore requires a canonical worktree UUID: %w", ErrInvalidIndex)
	}
	indexPath := filepath.Join(options.HomePath, "worktrees", string(worktreeID), "index.sqlite")
	owner, err := newMaintenanceOwnership(ctx, Options{
		Path: indexPath, HomePath: options.HomePath, WorktreeID: worktreeID,
		BusyTimeoutMS: options.BusyTimeoutMS, LockWaitMS: options.LockWaitMS,
		AdmissionConfig: options.AdmissionConfig, AdmissionConfigProvider: options.AdmissionConfigProvider,
	})
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	maintenanceLease, err := maintenance.AcquireWorktree(ctx, owner.root, string(worktreeID), owner.lockWait)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, maintenanceLease.Close()) }()
	if err := ensureNoActiveWorktreeRestore(owner.root, worktreeID); err != nil {
		return result, err
	}
	if err := clearTerminalRestoreGate(owner.root, worktreeID); err != nil {
		return result, err
	}

	sourceRoot, err := mceruntime.OpenRoot(filepath.Dir(options.ManifestPath))
	if err != nil {
		return result, fmt.Errorf("open approved backup source directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, sourceRoot.Close()) }()
	manifestRaw, err := readOwnedFileBounded(sourceRoot, "manifest.json", maxWorktreeBackupManifestBytes)
	if err != nil {
		return result, err
	}
	manifest, err := decodeWorktreeBackupManifest(manifestRaw)
	if err != nil {
		return result, err
	}
	if manifest.WorktreeID != worktreeID {
		return result, fmt.Errorf("backup belongs to a different worktree: %w", ErrWorktreeMismatch)
	}
	sourceStores, err := validateWorktreeBackupSource(sourceRoot, manifest)
	if err != nil {
		return result, err
	}
	currentStores, err := listWorktreeBackupStores(ctx, owner.root,
		"worktrees/"+string(worktreeID)+"/index.sqlite", options.BusyTimeoutMS, worktreeID)
	if err != nil {
		return result, err
	}
	currentByKind := make(map[string]verifiedSQLiteStore, len(currentStores))
	for _, store := range currentStores {
		currentByKind[store.kind] = store
	}
	if hasBackupStore(sourceStores, "records") {
		if _, exists := currentByKind["records"]; !exists {
			return result, fmt.Errorf("paired records restore requires the current records store for rollback: %w", ErrInvalidIndex)
		}
	}

	operationID, err := newOperationUUID()
	if err != nil {
		return result, fmt.Errorf("create worktree restore operation identity: %w", err)
	}
	operationRoot := "worktrees/" + string(worktreeID) + "/maintenance/" + string(operationID)
	cachePeak, volumePeak, err := estimateRestorePeaks(owner.root, worktreeID, sourceStores, currentStores, manifest)
	if err != nil {
		return result, err
	}
	reservation, err := owner.admitDurableGrowth(ctx, "maintenance", model.AdmissionOwnedPath(operationRoot), volumePeak)
	if err != nil {
		return result, fmt.Errorf("admit restore staging, rollback evidence, and artifact cache: %w", err)
	}
	reservation.cachePeakBytes = cachePeak
	currentCache, cacheErr := owner.worktreeCacheBytesFor(worktreeID)
	if cacheErr != nil || currentCache > owner.config.MaxWorktreeCacheBytes || cachePeak > owner.config.MaxWorktreeCacheBytes-currentCache {
		_ = reservation.Close()
		return result, errors.Join(fmt.Errorf("restore artifact or SQLite replacement peak exceeds the worktree cache cap: %w", ErrAdmissionResourceLimit), cacheErr)
	}
	reservationActive := true
	defer func() {
		if reservationActive {
			retErr = errors.Join(retErr, reservation.Close())
		}
	}()
	if reservation.initialDurableSize != 0 {
		return result, fmt.Errorf("restore operation output already exists: %w", ErrInvalidStorePath)
	}
	if err := owner.root.EnsurePrivateDirs(operationRoot, operationRoot+"/restore-artifacts", "worktrees/"+string(worktreeID)+"/maintenance"); err != nil {
		return result, fmt.Errorf("prepare private restore output: %w", err)
	}
	manifestDigest := model.SHA256(manifestRaw)
	if _, err := owner.root.PublishAtomicFile(operationRoot+"/restore-source-manifest.json", manifestRaw, 0o600); err != nil {
		return result, fmt.Errorf("preserve approved restore manifest without overwrite: %w", err)
	}

	journal := worktreeRestoreJournal{
		FormatVersion: 1, OperationID: operationID, WorktreeID: worktreeID,
		Phase: "PREPARED", Outcome: "pending", ManifestSHA: manifestDigest,
		Stores: []restoreJournalStore{}, Artifacts: []restoreJournalArtifact{},
		Completed: []string{"source_manifest_preserved"}, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	journalActive := false
	for _, source := range sourceStores {
		current := currentByKind[source.descriptor.Kind]
		stageFile := "stage-" + source.descriptor.Kind + ".sqlite"
		rollbackFile := "rollback-" + source.descriptor.Kind + ".sqlite"
		rollbackReadyFile := "rollback-ready-" + source.descriptor.Kind + ".sqlite"
		stagePath := operationRoot + "/" + stageFile
		stageHash, stageLength, err := copyVerifiedFile(sourceRoot, source.descriptor.File, owner.root, stagePath, source.descriptor.ByteLength, source.descriptor.SHA256)
		if err != nil {
			return result, fmt.Errorf("copy verified %s backup database into owned staging: %w", source.descriptor.Kind, err)
		}
		stageInfo, err := inspectSQLiteBackup(ctx, owner.root, stagePath, source.descriptor.Kind, options.BusyTimeoutMS, worktreeID)
		if err != nil || stageInfo.incarnation != source.descriptor.Incarnation || stageInfo.schemaVersion != source.descriptor.SchemaVersion ||
			stageHash != source.descriptor.SHA256 || stageLength != source.descriptor.ByteLength {
			return result, errors.Join(fmt.Errorf("staged %s database failed identity/schema verification: %w", source.descriptor.Kind, ErrInvalidIndex), err)
		}
		newIncarnation, err := distinctIncarnation(source.descriptor.Incarnation, current.incarnation)
		if err != nil {
			return result, err
		}
		if err := rotateStoreIncarnation(ctx, owner.root, stagePath, source.descriptor.Kind, worktreeID, newIncarnation, options.BusyTimeoutMS); err != nil {
			return result, err
		}
		if err := verifySQLiteFileIntegrity(ctx, owner.root, stagePath, options.BusyTimeoutMS); err != nil {
			return result, fmt.Errorf("verify staged %s database integrity: %w", source.descriptor.Kind, err)
		}
		stageHash, stageLength, err = hashOwnedRegularFile(owner.root, stagePath)
		if err != nil {
			return result, err
		}

		rollbackPath := operationRoot + "/" + rollbackFile
		rollbackHash, rollbackLength, err := backupSQLiteFile(ctx, owner.root, current.relativePath, rollbackPath, options.BusyTimeoutMS)
		if err != nil {
			return result, fmt.Errorf("preserve current %s rollback database: %w", source.descriptor.Kind, err)
		}
		rollbackReadyPath := operationRoot + "/" + rollbackReadyFile
		if _, _, err := backupSQLiteFile(ctx, owner.root, rollbackPath, rollbackReadyPath, options.BusyTimeoutMS); err != nil {
			return result, fmt.Errorf("stage %s rollback database: %w", source.descriptor.Kind, err)
		}
		rollbackIncarnation, err := distinctIncarnation(current.incarnation, newIncarnation)
		if err != nil {
			return result, err
		}
		if err := rotateStoreIncarnation(ctx, owner.root, rollbackReadyPath, source.descriptor.Kind, worktreeID, rollbackIncarnation, options.BusyTimeoutMS); err != nil {
			return result, err
		}
		if err := verifySQLiteFileIntegrity(ctx, owner.root, rollbackReadyPath, options.BusyTimeoutMS); err != nil {
			return result, fmt.Errorf("verify staged %s rollback integrity: %w", source.descriptor.Kind, err)
		}
		rollbackReadyHash, rollbackReadyLength, err := hashOwnedRegularFile(owner.root, rollbackReadyPath)
		if err != nil {
			return result, err
		}
		journal.Stores = append(journal.Stores, restoreJournalStore{
			Kind: source.descriptor.Kind, WorktreeID: worktreeID,
			OldIncarnation: current.incarnation, NewIncarnation: newIncarnation,
			RollbackIncarnation: rollbackIncarnation, SchemaVersion: source.descriptor.SchemaVersion,
			RollbackSchemaVersion: current.schemaVersion,
			StagedFile:            stageFile, StagedByteLength: stageLength, StagedSHA256: stageHash,
			RollbackFile: rollbackFile, RollbackByteLength: rollbackLength, RollbackSHA256: rollbackHash,
			RollbackReadyFile: rollbackReadyFile, RollbackReadyLength: rollbackReadyLength, RollbackReadySHA256: rollbackReadyHash,
		})
		journal.Completed = append(journal.Completed, "stage:"+source.descriptor.Kind, "rollback-stage:"+source.descriptor.Kind)
	}
	for _, artifact := range manifest.Artifacts {
		stagedFile := "restore-artifacts/" + artifact.SourcePath
		if _, _, err := copyVerifiedFile(sourceRoot, artifact.BackupPath, owner.root, operationRoot+"/"+stagedFile, artifact.ByteLength, artifact.SHA256); err != nil {
			return result, fmt.Errorf("preserve backup artifact %s for recovery: %w", artifact.SourcePath, err)
		}
		journal.Artifacts = append(journal.Artifacts, restoreJournalArtifact{
			SourcePath: artifact.SourcePath, StagedFile: stagedFile, ByteLength: artifact.ByteLength, SHA256: artifact.SHA256,
		})
	}
	journal.Completed = append(journal.Completed, "artifacts_staged")
	if err := verifyStagedBackupManifestState(ctx, owner.root, operationRoot, journal, manifest, options.BusyTimeoutMS); err != nil {
		return result, fmt.Errorf("staged backup set differs from its manifest metadata: %w", err)
	}
	journalPath := operationRoot + "/restore-journal.json"
	journalBytes, err := encodeWorktreeRestoreJournal(journal)
	if err != nil {
		return result, err
	}
	if _, err := owner.root.PublishAtomicFile(journalPath, journalBytes, 0o600); err != nil {
		return result, fmt.Errorf("publish prepared restore journal: %w", err)
	}
	marker := restoreActiveMarker{FormatVersion: 1, OperationID: operationID, WorktreeID: worktreeID, JournalPath: journalPath}
	markerBytes, err := encodeRestoreActiveMarker(marker)
	if err != nil {
		return result, err
	}
	activePath := "worktrees/" + string(worktreeID) + "/maintenance/restore-active.json"
	markerResult, markerErr := owner.root.PublishAtomicFile(activePath, markerBytes, 0o600)
	if markerResult.Published {
		journalActive = true
	}
	if markerErr != nil {
		return result, fmt.Errorf("activate worktree restore recovery gate (published=%t): %w", markerResult.Published, markerErr)
	}
	if err := validateRestoreEvidence(ctx, owner.root, journalPath, &journal, options.BusyTimeoutMS); err != nil {
		_ = writeRestoreJournal(owner.root, journalPath, &journal, "NEEDS_RECOVERY")
		return result, err
	}
	if err := runForwardRestore(ctx, owner.root, &journal, options.BusyTimeoutMS); err != nil {
		if journalActive {
			_ = writeRestoreJournal(owner.root, journalPath, &journal, "NEEDS_RECOVERY")
		}
		return result, err
	}
	if err := reservation.Close(); err != nil {
		_ = writeRestoreJournal(owner.root, journalPath, &journal, "NEEDS_RECOVERY")
		return result, fmt.Errorf("settle restore storage reservation before reopening store: %w", err)
	}
	reservationActive = false
	if err := releaseRestoreGate(owner.root, activePath, journalPath, operationID, worktreeID); err != nil {
		return result, fmt.Errorf("release completed worktree restore gate: %w", err)
	}
	return WorktreeRestoreResult{
		OperationID: operationID, WorktreeID: worktreeID, Phase: "VERIFIED", Outcome: "restored",
		Stores: journalStoreKinds(journal.Stores),
	}, nil
}

func InspectWorktreeRestore(ctx context.Context, homePath string, worktreeID model.UUID, wait time.Duration) (report WorktreeRestoreRecoveryReport, err error) {
	report = WorktreeRestoreRecoveryReport{Operation: "store_recover", WorktreeID: worktreeID, Completed: []string{}, Stores: []string{}}
	if ctx == nil || !filepath.IsAbs(homePath) || wait < 0 {
		return report, fmt.Errorf("invalid worktree restore inspection: %w", ErrInvalidIndex)
	}
	if err := validateWorktreeIdentity(worktreeID); err != nil {
		return report, err
	}
	root, err := mceruntime.OpenRoot(homePath)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	homeLease, err := root.AcquireExistingLock(ctx, "lifecycle.lock", mceruntime.LockShared, wait)
	if err != nil {
		return report, fmt.Errorf("acquire existing home lifecycle lock for recovery report: %w", err)
	}
	defer func() { err = errors.Join(err, homeLease.Close()) }()
	worktreeLease, err := root.AcquireExistingLock(ctx, "worktrees/"+string(worktreeID)+"/lifecycle.lock", mceruntime.LockShared, wait)
	if err != nil {
		return report, fmt.Errorf("acquire existing worktree lifecycle lock for recovery report: %w", err)
	}
	defer func() { err = errors.Join(err, worktreeLease.Close()) }()
	markerPath := "worktrees/" + string(worktreeID) + "/maintenance/restore-active.json"
	markerRaw, readErr := readOwnedFileBounded(root, markerPath, 4<<10)
	if errors.Is(readErr, os.ErrNotExist) {
		return report, nil
	}
	if readErr != nil {
		return report, readErr
	}
	marker, err := decodeRestoreActiveMarker(markerRaw, worktreeID)
	if err != nil {
		return report, err
	}
	journal, err := readWorktreeRestoreJournal(root, marker.JournalPath, marker.OperationID, worktreeID)
	if err != nil {
		return report, err
	}
	report.Pending = journal.Phase != "VERIFIED" || (journal.Outcome != "restored" && journal.Outcome != "rolled_back")
	report.Phase, report.Outcome = journal.Phase, journal.Outcome
	report.Completed, report.Stores = append([]string(nil), journal.Completed...), journalStoreKinds(journal.Stores)
	return report, nil
}

func RecoverWorktreeRestore(ctx context.Context, options WorktreeRestoreRecoveryOptions) (report WorktreeRestoreRecoveryReport, retErr error) {
	if !options.Apply {
		return InspectWorktreeRestore(ctx, options.HomePath, options.WorktreeID, time.Duration(options.LockWaitMS)*time.Millisecond)
	}
	if !options.Confirm {
		return report, fmt.Errorf("applying worktree restore recovery requires --confirm: %w", ErrInvalidIndex)
	}
	if ctx == nil || !filepath.IsAbs(options.HomePath) {
		return report, fmt.Errorf("invalid worktree restore recovery request: %w", ErrInvalidIndex)
	}
	if options.BusyTimeoutMS == 0 {
		options.BusyTimeoutMS = defaultBusyTimeout
	}
	if options.BusyTimeoutMS < 1 || options.BusyTimeoutMS > math.MaxInt32 {
		return report, fmt.Errorf("SQLite busy timeout is outside the supported range: %w", ErrInvalidIndex)
	}
	if err := validateWorktreeIdentity(options.WorktreeID); err != nil {
		return report, err
	}
	indexPath := filepath.Join(options.HomePath, "worktrees", string(options.WorktreeID), "index.sqlite")
	owner, err := newMaintenanceOwnership(ctx, Options{
		Path: indexPath, HomePath: options.HomePath, WorktreeID: options.WorktreeID,
		BusyTimeoutMS: options.BusyTimeoutMS, LockWaitMS: options.LockWaitMS,
		AdmissionConfig: options.AdmissionConfig, AdmissionConfigProvider: options.AdmissionConfigProvider,
	})
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	maintenanceLease, err := maintenance.AcquireWorktree(ctx, owner.root, string(options.WorktreeID), owner.lockWait)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, maintenanceLease.Close()) }()
	markerPath := "worktrees/" + string(options.WorktreeID) + "/maintenance/restore-active.json"
	markerRaw, err := readOwnedFileBounded(owner.root, markerPath, 4<<10)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WorktreeRestoreRecoveryReport{Operation: "store_recover", WorktreeID: options.WorktreeID, Completed: []string{}, Stores: []string{}}, nil
		}
		return report, err
	}
	marker, err := decodeRestoreActiveMarker(markerRaw, options.WorktreeID)
	if err != nil {
		return report, err
	}
	journal, err := readWorktreeRestoreJournal(owner.root, marker.JournalPath, marker.OperationID, options.WorktreeID)
	if err != nil {
		return report, err
	}
	cachePeak, volumePeak, err := estimateRecoveryPeaks(owner.root, marker.JournalPath, journal)
	if err != nil {
		return report, err
	}
	operationRoot := filepath.ToSlash(filepath.Dir(filepath.FromSlash(marker.JournalPath)))
	reservation, err := owner.admitDurableGrowth(ctx, "maintenance", model.AdmissionOwnedPath(operationRoot), volumePeak)
	if err != nil {
		return report, fmt.Errorf("admit pending restore recovery: %w", err)
	}
	reservation.cachePeakBytes = cachePeak
	currentCache, cacheErr := owner.worktreeCacheBytesFor(options.WorktreeID)
	if cacheErr != nil || currentCache > owner.config.MaxWorktreeCacheBytes || cachePeak > owner.config.MaxWorktreeCacheBytes-currentCache {
		_ = reservation.Close()
		return report, errors.Join(fmt.Errorf("restore recovery peak exceeds the worktree cache cap: %w", ErrAdmissionResourceLimit), cacheErr)
	}
	reservationActive := true
	defer func() {
		if reservationActive {
			retErr = errors.Join(retErr, reservation.Close())
		}
	}()
	if journal.Phase != "VERIFIED" {
		if err := validateRestoreEvidence(ctx, owner.root, marker.JournalPath, &journal, options.BusyTimeoutMS); err != nil {
			_ = writeRestoreJournal(owner.root, marker.JournalPath, &journal, "NEEDS_RECOVERY")
			return report, err
		}
	}
	if err := runForwardRestore(ctx, owner.root, &journal, options.BusyTimeoutMS); err != nil {
		_ = writeRestoreJournal(owner.root, marker.JournalPath, &journal, "NEEDS_RECOVERY")
		return report, err
	}
	if err := reservation.Close(); err != nil {
		_ = writeRestoreJournal(owner.root, marker.JournalPath, &journal, "NEEDS_RECOVERY")
		return report, err
	}
	reservationActive = false
	if err := releaseRestoreGate(owner.root, markerPath, marker.JournalPath, marker.OperationID, options.WorktreeID); err != nil {
		return report, fmt.Errorf("release recovered worktree restore gate: %w", err)
	}
	return WorktreeRestoreRecoveryReport{
		Operation: "store_recover", WorktreeID: options.WorktreeID, Pending: false, Applied: true,
		Phase: journal.Phase, Outcome: journal.Outcome,
		Completed: append([]string(nil), journal.Completed...), Stores: journalStoreKinds(journal.Stores),
	}, nil
}

func releaseRestoreGate(root *mceruntime.Root, markerPath, journalPath string, operationID, worktreeID model.UUID) error {
	if err := root.RemoveRegularFile(markerPath); err == nil {
		return nil
	} else {
		journal, journalErr := readWorktreeRestoreJournal(root, journalPath, operationID, worktreeID)
		if journalErr != nil || journal.Phase != "VERIFIED" || (journal.Outcome != "restored" && journal.Outcome != "rolled_back") {
			return errors.Join(err, journalErr)
		}
		if _, markerErr := readOwnedFileBounded(root, markerPath, 4<<10); errors.Is(markerErr, os.ErrNotExist) {
			return nil
		} else if markerErr != nil {
			return errors.Join(err, markerErr)
		}
		return err
	}
}

func clearTerminalRestoreGate(root *mceruntime.Root, worktreeID model.UUID) error {
	markerPath := "worktrees/" + string(worktreeID) + "/maintenance/restore-active.json"
	raw, err := readOwnedFileBounded(root, markerPath, 4<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	marker, err := decodeRestoreActiveMarker(raw, worktreeID)
	if err != nil {
		return err
	}
	journal, err := readWorktreeRestoreJournal(root, marker.JournalPath, marker.OperationID, worktreeID)
	if err != nil {
		return err
	}
	if journal.Phase != "VERIFIED" || (journal.Outcome != "restored" && journal.Outcome != "rolled_back") {
		return fmt.Errorf("worktree %s has an incomplete maintenance operation: %w", worktreeID, ErrStoreRecoveryRequired)
	}
	return releaseRestoreGate(root, markerPath, marker.JournalPath, marker.OperationID, worktreeID)
}

func validateWorktreeIdentity(worktreeID model.UUID) error {
	parsed, err := model.ParseUUID(string(worktreeID))
	if err != nil || parsed != worktreeID {
		return fmt.Errorf("canonical worktree UUID is required: %w", ErrInvalidIndex)
	}
	return nil
}

func validateWorktreeBackupSource(root *mceruntime.Root, manifest WorktreeBackupManifest) ([]restoreSourceStore, error) {
	canonical, err := encodeWorktreeBackupManifest(manifest)
	if err != nil {
		return nil, err
	}
	raw, err := readOwnedFileBounded(root, "manifest.json", maxWorktreeBackupManifestBytes)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, raw) {
		return nil, fmt.Errorf("backup manifest is not canonical MCE-MANIFEST-JSON1: %w", ErrInvalidIndex)
	}
	sourceStores := make([]restoreSourceStore, 0, len(manifest.Stores))
	seen := make(map[string]struct{}, len(manifest.Stores))
	for _, descriptor := range manifest.Stores {
		if _, duplicate := seen[descriptor.Kind]; duplicate {
			return nil, fmt.Errorf("backup repeats a store kind: %w", ErrInvalidIndex)
		}
		seen[descriptor.Kind] = struct{}{}
		digest, length, err := hashOwnedRegularFile(root, descriptor.File)
		if err != nil || digest != descriptor.SHA256 || length != descriptor.ByteLength {
			return nil, errors.Join(fmt.Errorf("backup %s database checksum or length mismatch: %w", descriptor.Kind, ErrInvalidIndex), err)
		}
		sourceStores = append(sourceStores, restoreSourceStore{descriptor: descriptor})
	}
	if _, exists := seen["index"]; !exists {
		return nil, fmt.Errorf("worktree backup is missing its index store: %w", ErrInvalidIndex)
	}
	if len(sourceStores) == 2 && (manifest.Consistency != "quiesced_worktree_set" || !hasBackupStore(sourceStores, "records")) {
		return nil, fmt.Errorf("index and records backup must be marked as one quiesced worktree set: %w", ErrInvalidIndex)
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.BackupPath != "artifacts/"+artifact.SourcePath || (model.AdmissionOwnedPath(artifact.SourcePath)).Validate() != nil {
			return nil, fmt.Errorf("backup artifact path is outside its content-addressed manifest scope: %w", ErrInvalidIndex)
		}
		digest, length, err := hashOwnedRegularFile(root, artifact.BackupPath)
		if err != nil || digest != artifact.SHA256 || length != artifact.ByteLength {
			return nil, errors.Join(fmt.Errorf("backup artifact checksum or length mismatch: %w", ErrInvalidIndex), err)
		}
	}
	indexDescriptor := manifestStore(manifest, "index")
	if indexDescriptor == nil {
		return nil, ErrInvalidIndex
	}
	if indexDescriptor := manifestStore(manifest, "index"); indexDescriptor == nil {
		return nil, fmt.Errorf("worktree backup is missing its index store: %w", ErrInvalidIndex)
	}
	if manifestStore(manifest, "records") == nil && (manifest.RecordEpoch != 0 || manifest.RecordVersionCount != 0) {
		return nil, fmt.Errorf("index-only backup must not claim records revisions: %w", ErrInvalidIndex)
	}
	return sourceStores, nil
}

func verifyStagedBackupManifestState(ctx context.Context, root *mceruntime.Root, operationRoot string, journal worktreeRestoreJournal, manifest WorktreeBackupManifest, timeout int64) error {
	var stagedIndex, stagedRecords *restoreJournalStore
	for index := range journal.Stores {
		store := &journal.Stores[index]
		switch store.Kind {
		case "index":
			stagedIndex = store
		case "records":
			stagedRecords = store
		}
	}
	if stagedIndex == nil {
		return fmt.Errorf("staged backup set has no index database: %w", ErrInvalidIndex)
	}
	rootPath, err := root.FinalPath()
	if err != nil {
		return err
	}
	indexRelative := operationRoot + "/" + stagedIndex.StagedFile
	db, err := openPool(ctx, comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(indexRelative))), "ro", timeout, true, 1)
	if err != nil {
		return fmt.Errorf("open owned staged index for metadata verification: %w", err)
	}
	refs, refsErr := readCaptureManifestReferences(ctx, db)
	policies, policiesErr := readPolicyRevisions(ctx, db)
	closeErr := db.Close()
	bindings, bindingsErr := readBindingRevisionCount(ctx, root, indexRelative, timeout)
	if refsErr != nil || policiesErr != nil || closeErr != nil || bindingsErr != nil {
		return errors.Join(refsErr, policiesErr, closeErr, bindingsErr)
	}
	if !reflect.DeepEqual(policies, manifest.PolicyRevisions) || bindings != manifest.BindingRevisionCount {
		return fmt.Errorf("staged index revision metadata differs from its manifest: %w", ErrInvalidIndex)
	}
	artifactByPath := make(map[string]WorktreeBackupArtifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		artifactByPath[artifact.SourcePath] = artifact
	}
	if len(refs) != len(artifactByPath) {
		return fmt.Errorf("staged index artifact set differs from its references: %w", ErrInvalidIndex)
	}
	stagedArtifactByPath := make(map[string]restoreJournalArtifact, len(journal.Artifacts))
	for _, artifact := range journal.Artifacts {
		stagedArtifactByPath[artifact.SourcePath] = artifact
	}
	for _, ref := range refs {
		artifact, ok := artifactByPath[ref.Path]
		staged, stagedOK := stagedArtifactByPath[ref.Path]
		if !ok || !stagedOK || artifact.SHA256 != ref.ArtifactSHA256 || artifact.ByteLength != ref.ByteLength ||
			staged.SHA256 != artifact.SHA256 || staged.ByteLength != artifact.ByteLength {
			return fmt.Errorf("staged artifact set omits or mismatches a referenced capture manifest: %w", ErrInvalidIndex)
		}
		digest, length, err := hashOwnedRegularFile(root, operationRoot+"/"+staged.StagedFile)
		if err != nil || digest != artifact.SHA256 || length != artifact.ByteLength {
			return errors.Join(fmt.Errorf("staged capture manifest digest differs from its descriptor: %w", ErrInvalidIndex), err)
		}
	}
	if descriptor := manifestStore(manifest, "records"); descriptor != nil {
		if stagedRecords == nil {
			return fmt.Errorf("manifest records store is missing from staged set: %w", ErrInvalidIndex)
		}
		epoch, count, err := readRecordRevisionMetadata(ctx, root, operationRoot+"/"+stagedRecords.StagedFile, timeout)
		if err != nil || epoch != manifest.RecordEpoch || count != manifest.RecordVersionCount {
			return errors.Join(fmt.Errorf("staged records revision metadata differs from its manifest: %w", ErrInvalidIndex), err)
		}
	} else if stagedRecords != nil || manifest.RecordEpoch != 0 || manifest.RecordVersionCount != 0 {
		return fmt.Errorf("index-only backup has an inconsistent records store set: %w", ErrInvalidIndex)
	}
	return nil
}

func decodeWorktreeBackupManifest(raw []byte) (WorktreeBackupManifest, error) {
	var manifest WorktreeBackupManifest
	if len(raw) == 0 || len(raw) > maxWorktreeBackupManifestBytes {
		return manifest, fmt.Errorf("backup manifest exceeds its bounded size: %w", ErrInvalidIndex)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return manifest, fmt.Errorf("decode worktree backup manifest: %w", err)
	}
	canonical, err := encodeWorktreeBackupManifest(manifest)
	if err != nil {
		return manifest, err
	}
	if !bytes.Equal(canonical, raw) {
		return WorktreeBackupManifest{}, fmt.Errorf("backup manifest has unknown, duplicate, or noncanonical fields: %w", ErrInvalidIndex)
	}
	return manifest, nil
}

func readOwnedFileBounded(root *mceruntime.Root, relative string, limit int64) ([]byte, error) {
	file, err := root.OpenRegularFile(relative)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("owned JSON file exceeds its size limit: %w", ErrInvalidIndex)
	}
	return raw, nil
}

func hasBackupStore(stores []restoreSourceStore, kind string) bool {
	for _, store := range stores {
		if store.descriptor.Kind == kind {
			return true
		}
	}
	return false
}

func manifestStore(manifest WorktreeBackupManifest, kind string) *WorktreeBackupStore {
	for index := range manifest.Stores {
		if manifest.Stores[index].Kind == kind {
			return &manifest.Stores[index]
		}
	}
	return nil
}

func estimateRestorePeaks(root *mceruntime.Root, worktreeID model.UUID, sourceStores []restoreSourceStore, currentStores []verifiedSQLiteStore, manifest WorktreeBackupManifest) (int64, int64, error) {
	var cachePeak, volumePeak int64
	add := func(total *int64, value int64) bool {
		next, ok := addNonnegative(*total, value)
		if ok {
			*total = next
		}
		return ok
	}
	for _, source := range sourceStores {
		if !add(&cachePeak, source.descriptor.ByteLength) || !add(&volumePeak, source.descriptor.ByteLength) {
			return 0, 0, ErrAdmissionReservationOverflow
		}
	}
	for _, current := range currentStores {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			length, exists, err := optionalOwnedFileLength(root, current.relativePath+suffix)
			if err != nil || (suffix == "" && !exists) {
				return 0, 0, errors.Join(fmt.Errorf("measure current %s store for rollback admission: %w", current.kind, ErrInvalidStorePath), err)
			}
			if exists && !add(&volumePeak, length) {
				return 0, 0, ErrAdmissionReservationOverflow
			}
			if exists && !add(&volumePeak, length) {
				return 0, 0, ErrAdmissionReservationOverflow
			}
		}
	}
	for _, artifact := range manifest.Artifacts {
		if !add(&cachePeak, artifact.ByteLength) || !add(&volumePeak, artifact.ByteLength) {
			return 0, 0, ErrAdmissionReservationOverflow
		}
	}
	if !add(&cachePeak, 64<<10) || !add(&volumePeak, cachePeak) {
		return 0, 0, ErrAdmissionReservationOverflow
	}
	manifestBytes, err := encodeWorktreeBackupManifest(manifest)
	if err != nil || !add(&volumePeak, int64(len(manifestBytes))) || !add(&volumePeak, 4<<20) {
		return 0, 0, errors.Join(ErrAdmissionReservationOverflow, err)
	}
	journalPeak := int64(1 << 20)
	for range manifest.Stores {
		if !add(&journalPeak, 4<<10) {
			return 0, 0, ErrAdmissionReservationOverflow
		}
	}
	for _, artifact := range manifest.Artifacts {
		if !add(&journalPeak, int64(len(artifact.SourcePath))+512) {
			return 0, 0, ErrAdmissionReservationOverflow
		}
	}
	if journalPeak > maxWorktreeRestoreJournalBytes || !add(&volumePeak, journalPeak) {
		return 0, 0, fmt.Errorf("restore journal exceeds its admitted bound: %w", ErrAdmissionResourceLimit)
	}
	if cachePeak > model.MaxSafeInteger || volumePeak > model.MaxSafeInteger {
		return 0, 0, ErrAdmissionReservationOverflow
	}
	if err := validateWorktreeIdentity(worktreeID); err != nil {
		return 0, 0, err
	}
	return cachePeak, volumePeak, nil
}

func distinctIncarnation(excluded ...model.UUID) (model.UUID, error) {
	for attempt := 0; attempt < 3; attempt++ {
		value, err := newUUID()
		if err != nil {
			return "", err
		}
		match := false
		for _, previous := range excluded {
			if value == previous {
				match = true
				break
			}
		}
		if !match {
			return value, nil
		}
	}
	return "", fmt.Errorf("could not create a new store incarnation: %w", ErrInvalidIndex)
}

func rotateStoreIncarnation(ctx context.Context, root *mceruntime.Root, relative, kind string, worktreeID, incarnation model.UUID, timeout int64) error {
	if ctx == nil || (kind != "index" && kind != "records") || validateWorktreeIdentity(worktreeID) != nil || validateWorktreeIdentity(incarnation) != nil {
		return fmt.Errorf("invalid store incarnation rotation: %w", ErrInvalidIndex)
	}
	rootPath, err := root.FinalPath()
	if err != nil {
		return err
	}
	db, err := openPool(ctx, comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(relative))), "rw", timeout, false, 1)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	table := "store_identity"
	if kind == "records" {
		table = "records_meta"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.ExecContext(ctx, "UPDATE "+table+" SET incarnation=? WHERE singleton=1 AND worktree_id=?", string(incarnation), string(worktreeID))
	if err != nil {
		return fmt.Errorf("rotate %s incarnation: %w", kind, err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.Join(fmt.Errorf("rotate %s incarnation affected %d rows: %w", kind, rows, ErrInvalidIndex), err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s incarnation rotation: %w", kind, err)
	}
	rollback = false
	return db.Close()
}

func verifySQLiteFileIntegrity(ctx context.Context, root *mceruntime.Root, relative string, timeout int64) error {
	rootPath, err := root.FinalPath()
	if err != nil {
		return err
	}
	db, err := openPool(ctx, comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(relative))), "ro", timeout, true, 1)
	if err != nil {
		return err
	}
	integrityRows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return errors.Join(err, db.Close())
	}
	count := 0
	for integrityRows.Next() {
		var result string
		if err := integrityRows.Scan(&result); err != nil || result != "ok" {
			_ = integrityRows.Close()
			_ = db.Close()
			return errors.Join(fmt.Errorf("SQLite integrity_check did not return ok: %w", ErrInvalidIndex), err)
		}
		count++
	}
	rowsErr, closeRowsErr := integrityRows.Err(), integrityRows.Close()
	if rowsErr != nil || closeRowsErr != nil || count != 1 {
		return errors.Join(fmt.Errorf("SQLite integrity_check returned %d rows: %w", count, ErrInvalidIndex), rowsErr, closeRowsErr, db.Close())
	}
	fkRows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return errors.Join(err, db.Close())
	}
	if fkRows.Next() {
		var table string
		var rowID, parent, constraint any
		_ = fkRows.Scan(&table, &rowID, &parent, &constraint)
		_ = fkRows.Close()
		_ = db.Close()
		return fmt.Errorf("SQLite foreign_key_check found a violation in %s: %w", table, ErrInvalidIndex)
	}
	rowsErr, closeRowsErr = fkRows.Err(), fkRows.Close()
	return errors.Join(rowsErr, closeRowsErr, db.Close())
}

func backupSQLiteFileFromRoots(ctx context.Context, sourceRoot *mceruntime.Root, sourceRelative string, destinationRoot *mceruntime.Root, destinationRelative string, timeout int64) (model.Hash, int64, error) {
	var empty model.Hash
	if ctx == nil || sourceRoot == nil || destinationRoot == nil || timeout < 1 || timeout > math.MaxInt32 {
		return empty, 0, fmt.Errorf("invalid cross-root SQLite backup: %w", ErrInvalidIndex)
	}
	sourceFile, err := sourceRoot.OpenRegularFile(sourceRelative)
	if err != nil {
		return empty, 0, err
	}
	sourceInfo, statErr := sourceFile.Stat()
	closeErr := sourceFile.Close()
	if statErr != nil || closeErr != nil || sourceInfo.Size() < 1 || !sourceInfo.Mode().IsRegular() {
		return empty, 0, errors.Join(fmt.Errorf("SQLite backup source is not a nonempty regular file: %w", ErrInvalidStorePath), statErr, closeErr)
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(destinationRelative)))
	if parent != "." {
		if err := destinationRoot.EnsurePrivateDirs(parent); err != nil {
			return empty, 0, err
		}
	}
	if _, err := destinationRoot.PublishAtomicFile(destinationRelative, nil, 0o600); err != nil {
		return empty, 0, err
	}
	sourceRootPath, err := sourceRoot.FinalPath()
	if err != nil {
		return empty, 0, err
	}
	destinationRootPath, err := destinationRoot.FinalPath()
	if err != nil {
		return empty, 0, err
	}
	sourceDB, err := openPool(ctx, comparableOwnedPath(filepath.Join(sourceRootPath, filepath.FromSlash(sourceRelative))), "ro", timeout, true, 1)
	if err != nil {
		return empty, 0, err
	}
	destinationPath := comparableOwnedPath(filepath.Join(destinationRootPath, filepath.FromSlash(destinationRelative)))
	destinationURI, err := makeDSN(destinationPath, "rw", timeout, false)
	if err != nil {
		return empty, 0, errors.Join(err, sourceDB.Close())
	}
	backupErr := runOnlineSQLiteBackup(ctx, sourceDB, destinationURI)
	closeErr = sourceDB.Close()
	if backupErr != nil || closeErr != nil {
		return empty, 0, errors.Join(backupErr, closeErr)
	}
	return hashOwnedRegularFile(destinationRoot, destinationRelative)
}

func copyVerifiedFile(sourceRoot *mceruntime.Root, sourceRelative string, destinationRoot *mceruntime.Root, destinationRelative string, expectedLength int64, expectedHash model.Hash) (model.Hash, int64, error) {
	var empty model.Hash
	if sourceRoot == nil || destinationRoot == nil || expectedLength < 1 || expectedLength > model.MaxSafeInteger {
		return empty, 0, fmt.Errorf("invalid verified file copy: %w", ErrInvalidIndex)
	}
	digest, length, err := hashOwnedRegularFile(sourceRoot, sourceRelative)
	if err != nil || digest != expectedHash || length != expectedLength {
		return empty, 0, errors.Join(fmt.Errorf("verified copy source checksum mismatch: %w", ErrInvalidIndex), err)
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(destinationRelative)))
	if parent != "." {
		if err := destinationRoot.EnsurePrivateDirs(parent); err != nil {
			return empty, 0, err
		}
	}
	if _, err := destinationRoot.PublishAtomicFile(destinationRelative, nil, 0o600); err != nil {
		return empty, 0, err
	}
	destinationRootPath, err := destinationRoot.FinalPath()
	if err != nil {
		return empty, 0, err
	}
	destinationPath := comparableOwnedPath(filepath.Join(destinationRootPath, filepath.FromSlash(destinationRelative)))
	output, err := os.OpenFile(destinationPath, os.O_WRONLY, 0)
	if err != nil {
		return empty, 0, err
	}
	outputInfo, statErr := output.Stat()
	pathInfo, lstatErr := os.Lstat(destinationPath)
	verified, secureErr := destinationRoot.OpenRegularFile(destinationRelative)
	var secureInfo os.FileInfo
	if secureErr == nil {
		secureInfo, secureErr = verified.Stat()
	}
	if verified != nil {
		secureErr = errors.Join(secureErr, verified.Close())
	}
	if statErr != nil || lstatErr != nil || secureErr != nil || outputInfo == nil || pathInfo == nil || secureInfo == nil ||
		!pathInfo.Mode().IsRegular() || !os.SameFile(outputInfo, pathInfo) || !os.SameFile(outputInfo, secureInfo) || outputInfo.Size() != 0 {
		return empty, 0, errors.Join(fmt.Errorf("new verified copy destination is not a secure empty file: %w", ErrInvalidStorePath), statErr, lstatErr, secureErr, output.Close())
	}
	source, err := sourceRoot.OpenRegularFile(sourceRelative)
	if err != nil {
		return empty, 0, errors.Join(err, output.Close())
	}
	hasher := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(output, hasher), io.LimitReader(source, expectedLength+1))
	sourceCloseErr := source.Close()
	flushErr := output.Sync()
	outputCloseErr := output.Close()
	actualHash := model.Hash(hex.EncodeToString(hasher.Sum(nil)))
	if copyErr != nil || sourceCloseErr != nil || flushErr != nil || outputCloseErr != nil || copied != expectedLength || actualHash != expectedHash {
		return empty, 0, errors.Join(fmt.Errorf("verified file copy failed length or checksum validation: %w", ErrInvalidIndex), copyErr, sourceCloseErr, flushErr, outputCloseErr)
	}
	actualHash, actualLength, err := hashOwnedRegularFile(destinationRoot, destinationRelative)
	if err != nil || actualHash != expectedHash || actualLength != expectedLength {
		return empty, 0, errors.Join(fmt.Errorf("verify copied owned file: %w", ErrInvalidIndex), err)
	}
	return actualHash, actualLength, nil
}

func estimateRecoveryPeaks(root *mceruntime.Root, journalPath string, journal worktreeRestoreJournal) (int64, int64, error) {
	var cachePeak, volumePeak int64
	add := func(total *int64, value int64) bool {
		next, ok := addNonnegative(*total, value)
		if ok {
			*total = next
		}
		return ok
	}
	for _, store := range journal.Stores {
		replacementLength := store.StagedByteLength
		if store.RollbackReadyLength > replacementLength {
			replacementLength = store.RollbackReadyLength
		}
		if !add(&cachePeak, replacementLength) || !add(&volumePeak, replacementLength) {
			return 0, 0, ErrAdmissionReservationOverflow
		}
	}
	for _, artifact := range journal.Artifacts {
		if !add(&cachePeak, artifact.ByteLength) {
			return 0, 0, ErrAdmissionReservationOverflow
		}
	}
	if !add(&cachePeak, 64<<10) || !add(&volumePeak, cachePeak) || !add(&volumePeak, 2<<20) ||
		cachePeak > model.MaxSafeInteger || volumePeak > model.MaxSafeInteger {
		return 0, 0, ErrAdmissionReservationOverflow
	}
	_ = root
	_ = journalPath
	return cachePeak, volumePeak, nil
}

func restoreOperationRoot(journalPath string) string {
	return filepath.ToSlash(filepath.Dir(filepath.FromSlash(journalPath)))
}

func journalStoreKinds(stores []restoreJournalStore) []string {
	kinds := make([]string, len(stores))
	for index, store := range stores {
		kinds[index] = store.Kind
	}
	return kinds
}

func encodeWorktreeRestoreJournal(journal worktreeRestoreJournal) ([]byte, error) {
	if err := validateWorktreeRestoreJournal(journal); err != nil {
		return nil, err
	}
	stores := make([]any, 0, len(journal.Stores))
	for _, store := range journal.Stores {
		stores = append(stores, map[string]any{
			"kind": store.Kind, "worktree_id": store.WorktreeID,
			"old_incarnation": store.OldIncarnation, "new_incarnation": store.NewIncarnation,
			"rollback_incarnation": store.RollbackIncarnation, "schema_version": store.SchemaVersion,
			"rollback_schema_version": store.RollbackSchemaVersion,
			"staged_file":             store.StagedFile, "staged_byte_length": store.StagedByteLength, "staged_sha256": store.StagedSHA256,
			"rollback_file": store.RollbackFile, "rollback_byte_length": store.RollbackByteLength, "rollback_sha256": store.RollbackSHA256,
			"rollback_ready_file": store.RollbackReadyFile, "rollback_ready_byte_length": store.RollbackReadyLength,
			"rollback_ready_sha256": store.RollbackReadySHA256,
		})
	}
	artifacts := make([]any, 0, len(journal.Artifacts))
	for _, artifact := range journal.Artifacts {
		artifacts = append(artifacts, map[string]any{
			"source_path": artifact.SourcePath, "staged_file": artifact.StagedFile,
			"byte_length": artifact.ByteLength, "sha256": artifact.SHA256,
		})
	}
	completed := make([]any, len(journal.Completed))
	for index, step := range journal.Completed {
		completed[index] = step
	}
	raw, err := model.CanonicalJSON1(map[string]any{
		"artifacts": artifacts, "completed_steps": completed, "format_version": journal.FormatVersion,
		"manifest_sha256": journal.ManifestSHA, "operation_id": journal.OperationID, "outcome": journal.Outcome,
		"phase": journal.Phase, "stores": stores, "updated_at": journal.UpdatedAt, "worktree_id": journal.WorktreeID,
	})
	if err != nil {
		return nil, err
	}
	if len(raw) > maxWorktreeRestoreJournalBytes {
		return nil, fmt.Errorf("worktree restore journal exceeds its bounded size: %w", ErrAdmissionResourceLimit)
	}
	return raw, nil
}

func validateWorktreeRestoreJournal(journal worktreeRestoreJournal) error {
	if journal.FormatVersion != 1 || validateWorktreeIdentity(journal.OperationID) != nil || validateWorktreeIdentity(journal.WorktreeID) != nil ||
		(journal.Phase != "PREPARED" && journal.Phase != "REPLACING" && journal.Phase != "VERIFYING" && journal.Phase != "VERIFIED" && journal.Phase != "NEEDS_RECOVERY") ||
		(journal.Outcome != "pending" && journal.Outcome != "restored" && journal.Outcome != "rolled_back") || len(journal.Stores) < 1 || len(journal.Stores) > 2 || len(journal.Artifacts) > maxBackupManifestReferences ||
		len(journal.Completed) > len(journal.Stores)*4+6 {
		return fmt.Errorf("invalid worktree restore journal header or bounds: %w", ErrInvalidIndex)
	}
	manifestHash, err := model.ParseHash(string(journal.ManifestSHA))
	if err != nil || manifestHash != journal.ManifestSHA {
		return fmt.Errorf("restore journal manifest digest is invalid: %w", ErrInvalidIndex)
	}
	if _, err := time.Parse(time.RFC3339Nano, journal.UpdatedAt); err != nil {
		return fmt.Errorf("restore journal timestamp is invalid: %w", ErrInvalidIndex)
	}
	seenStores := make(map[string]struct{}, len(journal.Stores))
	for _, store := range journal.Stores {
		if (store.Kind != "index" && store.Kind != "records") || store.WorktreeID != journal.WorktreeID ||
			store.SchemaVersion < 1 || store.StagedFile != "stage-"+store.Kind+".sqlite" || store.RollbackFile != "rollback-"+store.Kind+".sqlite" ||
			store.RollbackReadyFile != "rollback-ready-"+store.Kind+".sqlite" || store.StagedByteLength < 1 || store.RollbackByteLength < 1 || store.RollbackReadyLength < 1 ||
			store.RollbackSchemaVersion < 1 ||
			validateWorktreeIdentity(store.OldIncarnation) != nil || validateWorktreeIdentity(store.NewIncarnation) != nil || validateWorktreeIdentity(store.RollbackIncarnation) != nil ||
			store.OldIncarnation == store.NewIncarnation || store.OldIncarnation == store.RollbackIncarnation || store.NewIncarnation == store.RollbackIncarnation {
			return fmt.Errorf("restore journal contains an invalid store descriptor: %w", ErrInvalidIndex)
		}
		for _, digest := range []model.Hash{store.StagedSHA256, store.RollbackSHA256, store.RollbackReadySHA256} {
			parsed, parseErr := model.ParseHash(string(digest))
			if parseErr != nil || parsed != digest {
				return fmt.Errorf("restore journal contains an invalid store digest: %w", ErrInvalidIndex)
			}
		}
		if _, duplicate := seenStores[store.Kind]; duplicate {
			return fmt.Errorf("restore journal repeats a store kind: %w", ErrInvalidIndex)
		}
		seenStores[store.Kind] = struct{}{}
	}
	if _, exists := seenStores["index"]; !exists {
		return fmt.Errorf("restore journal is missing the index store: %w", ErrInvalidIndex)
	}
	seenArtifacts := make(map[string]struct{}, len(journal.Artifacts))
	for _, artifact := range journal.Artifacts {
		if (model.AdmissionOwnedPath(artifact.SourcePath)).Validate() != nil || artifact.StagedFile != "restore-artifacts/"+artifact.SourcePath || artifact.ByteLength < 1 {
			return fmt.Errorf("restore journal contains an invalid artifact path: %w", ErrInvalidIndex)
		}
		digest, parseErr := model.ParseHash(string(artifact.SHA256))
		if parseErr != nil || digest != artifact.SHA256 {
			return fmt.Errorf("restore journal contains an invalid artifact digest: %w", ErrInvalidIndex)
		}
		if _, duplicate := seenArtifacts[artifact.SourcePath]; duplicate {
			return fmt.Errorf("restore journal repeats an artifact path: %w", ErrInvalidIndex)
		}
		seenArtifacts[artifact.SourcePath] = struct{}{}
	}
	for _, step := range journal.Completed {
		if !allowedRestoreJournalStep(step) &&
			!strings.HasPrefix(step, "stage:") && !strings.HasPrefix(step, "rollback:") && !strings.HasPrefix(step, "rollback-stage:") &&
			!strings.HasPrefix(step, "replace:") && !strings.HasPrefix(step, "restore:") {
			return fmt.Errorf("restore journal contains an unknown completed step: %w", ErrInvalidIndex)
		}
	}
	return nil
}

func encodeRestoreActiveMarker(marker restoreActiveMarker) ([]byte, error) {
	if marker.FormatVersion != 1 || validateWorktreeIdentity(marker.OperationID) != nil || validateWorktreeIdentity(marker.WorktreeID) != nil ||
		marker.JournalPath != "worktrees/"+string(marker.WorktreeID)+"/maintenance/"+string(marker.OperationID)+"/restore-journal.json" {
		return nil, fmt.Errorf("invalid restore active marker: %w", ErrInvalidIndex)
	}
	return model.CanonicalJSON1(map[string]any{
		"format_version": marker.FormatVersion, "journal_path": marker.JournalPath,
		"operation_id": marker.OperationID, "worktree_id": marker.WorktreeID,
	})
}

func decodeRestoreActiveMarker(raw []byte, worktreeID model.UUID) (restoreActiveMarker, error) {
	var marker restoreActiveMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return marker, fmt.Errorf("decode restore recovery marker: %w", err)
	}
	canonical, err := encodeRestoreActiveMarker(marker)
	if err != nil || marker.WorktreeID != worktreeID || !bytes.Equal(canonical, raw) {
		return restoreActiveMarker{}, errors.Join(fmt.Errorf("restore recovery marker is invalid: %w", ErrInvalidIndex), err)
	}
	return marker, nil
}

func readWorktreeRestoreJournal(root *mceruntime.Root, relative string, operationID, worktreeID model.UUID) (worktreeRestoreJournal, error) {
	var journal worktreeRestoreJournal
	expected := "worktrees/" + string(worktreeID) + "/maintenance/" + string(operationID) + "/restore-journal.json"
	if relative != expected {
		return journal, fmt.Errorf("restore journal path differs from its target identity: %w", ErrInvalidIndex)
	}
	raw, err := readOwnedFileBounded(root, relative, maxWorktreeRestoreJournalBytes)
	if err != nil {
		return journal, err
	}
	if err := json.Unmarshal(raw, &journal); err != nil {
		return journal, fmt.Errorf("decode worktree restore journal: %w", err)
	}
	canonical, err := encodeWorktreeRestoreJournal(journal)
	if err != nil || journal.OperationID != operationID || journal.WorktreeID != worktreeID || !bytes.Equal(canonical, raw) {
		return worktreeRestoreJournal{}, errors.Join(fmt.Errorf("worktree restore journal is invalid or noncanonical: %w", ErrInvalidIndex), err)
	}
	return journal, nil
}

func writeRestoreJournal(root *mceruntime.Root, path string, journal *worktreeRestoreJournal, phase string) error {
	if root == nil || journal == nil {
		return ErrInvalidIndex
	}
	journal.Phase = phase
	journal.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := encodeWorktreeRestoreJournal(*journal)
	if err != nil {
		return err
	}
	return root.WriteAtomicFile(path, raw, 0o600)
}

func validateRestoreEvidence(ctx context.Context, root *mceruntime.Root, journalPath string, journal *worktreeRestoreJournal, timeout int64) error {
	operationRoot := restoreOperationRoot(journalPath)
	manifestRaw, err := readOwnedFileBounded(root, operationRoot+"/restore-source-manifest.json", maxWorktreeBackupManifestBytes)
	if err != nil {
		return err
	}
	if model.SHA256(manifestRaw) != journal.ManifestSHA {
		return fmt.Errorf("preserved restore manifest digest changed: %w", ErrInvalidIndex)
	}
	manifest, err := decodeWorktreeBackupManifest(manifestRaw)
	if err != nil || manifest.WorktreeID != journal.WorktreeID {
		return errors.Join(fmt.Errorf("preserved restore manifest identity is invalid: %w", ErrInvalidIndex), err)
	}
	if err := validateJournalMatchesManifest(*journal, manifest); err != nil {
		return err
	}
	validStoreSet := func(rollback bool) error {
		for _, store := range journal.Stores {
			file, incarnation, digest, length, schema := store.StagedFile, store.NewIncarnation, store.StagedSHA256, store.StagedByteLength, store.SchemaVersion
			if rollback {
				file, incarnation, digest, length, schema = store.RollbackReadyFile, store.RollbackIncarnation, store.RollbackReadySHA256, store.RollbackReadyLength, store.RollbackSchemaVersion
			}
			relative := operationRoot + "/" + file
			actualHash, actualLength, hashErr := hashOwnedRegularFile(root, relative)
			verified, inspectErr := inspectSQLiteBackup(ctx, root, relative, store.Kind, timeout, journal.WorktreeID)
			if hashErr != nil || actualHash != digest || actualLength != length || inspectErr != nil || verified.incarnation != incarnation || verified.schemaVersion != schema {
				return errors.Join(fmt.Errorf("restore recovery evidence failed validation for %s: %w", file, ErrInvalidIndex), hashErr, inspectErr)
			}
			if err := verifySQLiteFileIntegrity(ctx, root, relative, timeout); err != nil {
				return fmt.Errorf("restore recovery evidence integrity check failed for %s: %w", file, err)
			}
		}
		return nil
	}
	forwardErr := validStoreSet(false)
	if forwardErr == nil {
		for _, artifact := range journal.Artifacts {
			digest, length, artifactErr := hashOwnedRegularFile(root, operationRoot+"/"+artifact.StagedFile)
			if artifactErr != nil || digest != artifact.SHA256 || length != artifact.ByteLength {
				forwardErr = errors.Join(fmt.Errorf("restore artifact evidence failed validation: %w", ErrInvalidIndex), artifactErr)
				break
			}
		}
	}
	if forwardErr == nil {
		journal.Outcome = "restored"
		return nil
	}
	rollbackErr := validStoreSet(true)
	if rollbackErr != nil {
		return errors.Join(forwardErr, rollbackErr, ErrStoreRecoveryRequired)
	}
	journal.Outcome = "rolled_back"
	if !containsStep(journal.Completed, "rollback_verified") {
		journal.Completed = append(journal.Completed, "rollback_verified")
	}
	return nil
}

func validateJournalMatchesManifest(journal worktreeRestoreJournal, manifest WorktreeBackupManifest) error {
	if len(journal.Stores) != len(manifest.Stores) || len(journal.Artifacts) != len(manifest.Artifacts) {
		return fmt.Errorf("restore journal store or artifact count differs from manifest: %w", ErrInvalidIndex)
	}
	for _, descriptor := range manifest.Stores {
		found := false
		for _, store := range journal.Stores {
			if store.Kind == descriptor.Kind && store.SchemaVersion == descriptor.SchemaVersion && store.WorktreeID == descriptor.WorktreeID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("restore journal omits a manifest store: %w", ErrInvalidIndex)
		}
	}
	for _, descriptor := range manifest.Artifacts {
		found := false
		for _, artifact := range journal.Artifacts {
			if artifact.SourcePath == descriptor.SourcePath && artifact.ByteLength == descriptor.ByteLength && artifact.SHA256 == descriptor.SHA256 {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("restore journal omits a manifest artifact: %w", ErrInvalidIndex)
		}
	}
	return nil
}

func runForwardRestore(ctx context.Context, root *mceruntime.Root, journal *worktreeRestoreJournal, timeout int64) error {
	if journal.Phase == "VERIFIED" {
		return verifyRestoredWorktree(ctx, root, *journal, timeout, journal.Outcome != "rolled_back")
	}
	if journal.Outcome == "rolled_back" {
		return runRollbackRestore(ctx, root, journal, timeout)
	}
	if journal.Outcome == "pending" || journal.Outcome == "restored" {
		journal.Outcome = "restored"
		if err := writeRestoreJournal(root, restoreJournalPath(*journal), journal, "REPLACING"); err != nil {
			return err
		}
		for _, store := range journal.Stores {
			if err := ctx.Err(); err != nil {
				return err
			}
			destination := "worktrees/" + string(journal.WorktreeID) + "/" + store.Kind + ".sqlite"
			source := restoreOperationRoot(restoreJournalPath(*journal)) + "/" + store.StagedFile
			if err := overwriteSQLiteFile(ctx, root, source, destination, store.Kind, journal.WorktreeID,
				store.StagedSHA256, store.StagedByteLength, store.NewIncarnation, store.SchemaVersion, timeout); err != nil {
				return fmt.Errorf("replace worktree %s store from verified stage: %w", store.Kind, err)
			}
			if !containsStep(journal.Completed, "replace:"+store.Kind) {
				journal.Completed = append(journal.Completed, "replace:"+store.Kind)
			}
			if err := writeRestoreJournal(root, restoreJournalPath(*journal), journal, "REPLACING"); err != nil {
				return err
			}
		}
		if err := installRestoreArtifacts(root, *journal); err != nil {
			return err
		}
		if !containsStep(journal.Completed, "artifacts_installed") {
			journal.Completed = append(journal.Completed, "artifacts_installed")
		}
		if err := writeRestoreJournal(root, restoreJournalPath(*journal), journal, "VERIFYING"); err != nil {
			return err
		}
		if err := verifyRestoredWorktree(ctx, root, *journal, timeout, true); err != nil {
			return err
		}
		if !containsStep(journal.Completed, "verified") {
			journal.Completed = append(journal.Completed, "verified")
		}
		return writeRestoreJournal(root, restoreJournalPath(*journal), journal, "VERIFIED")
	}
	return fmt.Errorf("unknown restore recovery outcome: %w", ErrInvalidIndex)
}

func runRollbackRestore(ctx context.Context, root *mceruntime.Root, journal *worktreeRestoreJournal, timeout int64) error {
	journal.Outcome = "rolled_back"
	if err := writeRestoreJournal(root, restoreJournalPath(*journal), journal, "REPLACING"); err != nil {
		return err
	}
	operationRoot := restoreOperationRoot(restoreJournalPath(*journal))
	for _, store := range journal.Stores {
		if err := ctx.Err(); err != nil {
			return err
		}
		destination := "worktrees/" + string(journal.WorktreeID) + "/" + store.Kind + ".sqlite"
		source := operationRoot + "/" + store.RollbackReadyFile
		if err := overwriteSQLiteFile(ctx, root, source, destination, store.Kind, journal.WorktreeID,
			store.RollbackReadySHA256, store.RollbackReadyLength, store.RollbackIncarnation, store.RollbackSchemaVersion, timeout); err != nil {
			return fmt.Errorf("restore previous %s store from verified rollback copy: %w", store.Kind, err)
		}
		if !containsStep(journal.Completed, "rollback:"+store.Kind) {
			journal.Completed = append(journal.Completed, "rollback:"+store.Kind)
		}
		if err := writeRestoreJournal(root, restoreJournalPath(*journal), journal, "REPLACING"); err != nil {
			return err
		}
	}
	if err := verifyRestoredWorktree(ctx, root, *journal, timeout, false); err != nil {
		return err
	}
	if !containsStep(journal.Completed, "verified") {
		journal.Completed = append(journal.Completed, "verified")
	}
	return writeRestoreJournal(root, restoreJournalPath(*journal), journal, "VERIFIED")
}

func restoreJournalPath(journal worktreeRestoreJournal) string {
	return "worktrees/" + string(journal.WorktreeID) + "/maintenance/" + string(journal.OperationID) + "/restore-journal.json"
}

func containsStep(steps []string, expected string) bool {
	for _, step := range steps {
		if step == expected {
			return true
		}
	}
	return false
}

func allowedRestoreJournalStep(step string) bool {
	switch step {
	case "source_manifest_preserved", "artifacts_staged", "artifacts_installed", "verified", "rollback_verified":
		return true
	default:
		return false
	}
}

func overwriteSQLiteFile(ctx context.Context, root *mceruntime.Root, sourceRelative, destinationRelative, kind string, worktreeID model.UUID,
	expectedHash model.Hash, expectedLength int64, expectedIncarnation model.UUID, expectedSchemaVersion, timeout int64) error {
	published, copyErr := root.CopyAtomicFile(sourceRelative, destinationRelative, 0o600)
	if copyErr != nil && !(published.Published && errors.Is(copyErr, mceruntime.ErrDirectoryFlushUnsupported)) {
		return fmt.Errorf("atomically replace SQLite store file: %w", copyErr)
	}
	digest, length, err := hashOwnedRegularFile(root, destinationRelative)
	if err != nil || digest != expectedHash || length != expectedLength {
		return errors.Join(fmt.Errorf("replaced SQLite store differs from its verified staging file: %w", ErrInvalidIndex), err, copyErr)
	}
	verified, err := inspectSQLiteBackup(ctx, root, destinationRelative, kind, timeout, worktreeID)
	if err != nil || verified.incarnation != expectedIncarnation || verified.schemaVersion != expectedSchemaVersion {
		return errors.Join(fmt.Errorf("replaced SQLite store identity or schema differs from its verified staging file: %w", ErrInvalidIndex), err)
	}
	if err := verifySQLiteFileIntegrity(ctx, root, destinationRelative, timeout); err != nil {
		return err
	}
	return nil
}

func installRestoreArtifacts(root *mceruntime.Root, journal worktreeRestoreJournal) error {
	operationRoot := restoreOperationRoot(restoreJournalPath(journal))
	for _, artifact := range journal.Artifacts {
		destination := "worktrees/" + string(journal.WorktreeID) + "/" + artifact.SourcePath
		currentHash, currentLength, err := hashOwnedRegularFile(root, destination)
		if err == nil {
			if currentHash != artifact.SHA256 || currentLength != artifact.ByteLength {
				return fmt.Errorf("existing capture manifest differs from verified restore artifact: %w", ErrInvalidIndex)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, _, err := copyVerifiedFile(root, operationRoot+"/"+artifact.StagedFile, root, destination, artifact.ByteLength, artifact.SHA256); err != nil {
			return fmt.Errorf("install restored capture manifest %s: %w", artifact.SourcePath, err)
		}
	}
	return nil
}

func verifyRestoredWorktree(ctx context.Context, root *mceruntime.Root, journal worktreeRestoreJournal, timeout int64, forward bool) error {
	for _, store := range journal.Stores {
		destination := "worktrees/" + string(journal.WorktreeID) + "/" + store.Kind + ".sqlite"
		wantHash, wantLength := store.StagedSHA256, store.StagedByteLength
		wantIncarnation, wantSchema := store.NewIncarnation, store.SchemaVersion
		if !forward {
			wantHash, wantLength = store.RollbackReadySHA256, store.RollbackReadyLength
			wantIncarnation, wantSchema = store.RollbackIncarnation, store.RollbackSchemaVersion
		}
		digest, length, err := hashOwnedRegularFile(root, destination)
		if err != nil || digest != wantHash || length != wantLength {
			return errors.Join(fmt.Errorf("restored %s store checksum mismatch: %w", store.Kind, ErrInvalidIndex), err)
		}
		verified, err := inspectSQLiteBackup(ctx, root, destination, store.Kind, timeout, journal.WorktreeID)
		if err != nil || verified.incarnation != wantIncarnation || verified.schemaVersion != wantSchema {
			return errors.Join(fmt.Errorf("restored %s store identity or schema mismatch: %w", store.Kind, ErrInvalidIndex), err)
		}
		if err := verifySQLiteFileIntegrity(ctx, root, destination, timeout); err != nil {
			return err
		}
	}
	if forward {
		if err := installRestoreArtifacts(root, journal); err != nil {
			return err
		}
		if err := verifyRestoredManifestState(ctx, root, journal, timeout); err != nil {
			return err
		}
	}
	return nil
}

func verifyRestoredManifestState(ctx context.Context, root *mceruntime.Root, journal worktreeRestoreJournal, timeout int64) error {
	operationRoot := restoreOperationRoot(restoreJournalPath(journal))
	raw, err := readOwnedFileBounded(root, operationRoot+"/restore-source-manifest.json", maxWorktreeBackupManifestBytes)
	if err != nil {
		return err
	}
	manifest, err := decodeWorktreeBackupManifest(raw)
	if err != nil || manifest.WorktreeID != journal.WorktreeID {
		return errors.Join(fmt.Errorf("restored manifest identity is invalid: %w", ErrInvalidIndex), err)
	}
	rootPath, err := root.FinalPath()
	if err != nil {
		return err
	}
	indexPath := "worktrees/" + string(journal.WorktreeID) + "/index.sqlite"
	db, err := openPool(ctx, comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(indexPath))), "ro", timeout, true, 1)
	if err != nil {
		return err
	}
	refs, refsErr := readCaptureManifestReferences(ctx, db)
	policies, policiesErr := readPolicyRevisions(ctx, db)
	dbCloseErr := db.Close()
	bindings, bindingsErr := readBindingRevisionCount(ctx, root, indexPath, timeout)
	if refsErr != nil || policiesErr != nil || dbCloseErr != nil || bindingsErr != nil {
		return errors.Join(refsErr, policiesErr, dbCloseErr, bindingsErr)
	}
	if !reflect.DeepEqual(policies, manifest.PolicyRevisions) || bindings != manifest.BindingRevisionCount || len(refs) != len(manifest.Artifacts) {
		return fmt.Errorf("restored index revisions differ from backup manifest: %w", ErrInvalidIndex)
	}
	artifactByPath := make(map[string]WorktreeBackupArtifact, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		artifactByPath[artifact.SourcePath] = artifact
	}
	for _, ref := range refs {
		artifact, ok := artifactByPath[ref.Path]
		if !ok || artifact.SHA256 != ref.ArtifactSHA256 || artifact.ByteLength != ref.ByteLength {
			return fmt.Errorf("restored index references a missing or mismatched capture manifest: %w", ErrInvalidIndex)
		}
		digest, length, err := hashOwnedRegularFile(root, "worktrees/"+string(journal.WorktreeID)+"/"+ref.Path)
		if err != nil || digest != ref.ArtifactSHA256 || length != ref.ByteLength {
			return errors.Join(fmt.Errorf("restored capture manifest failed raw integrity verification: %w", ErrInvalidIndex), err)
		}
	}
	if descriptor := manifestStore(manifest, "records"); descriptor != nil {
		epoch, versions, err := readRecordRevisionMetadata(ctx, root, "worktrees/"+string(journal.WorktreeID)+"/records.sqlite", timeout)
		if err != nil || epoch != manifest.RecordEpoch || versions != manifest.RecordVersionCount {
			return errors.Join(fmt.Errorf("restored record revisions differ from backup manifest: %w", ErrInvalidIndex), err)
		}
	}
	return nil
}
