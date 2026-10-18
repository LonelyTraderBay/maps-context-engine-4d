package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/maintenance"
)

const maxBackupManifestReferences = 1_000_000

type WorktreeBackupOptions struct {
	HomePath                string
	WorktreeID              model.UUID
	OperationID             model.UUID
	BusyTimeoutMS           int64
	LockWaitMS              int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
}

type WorktreeBackupManifest struct {
	FormatVersion        int64                          `json:"format_version"`
	OperationID          model.UUID                     `json:"operation_id"`
	WorktreeID           model.UUID                     `json:"worktree_id"`
	Consistency          string                         `json:"consistency"`
	CreatedAt            string                         `json:"created_at"`
	Stores               []WorktreeBackupStore          `json:"stores"`
	Artifacts            []WorktreeBackupArtifact       `json:"artifacts"`
	PolicyRevisions      []WorktreeBackupPolicyRevision `json:"policy_revisions"`
	BindingRevisionCount int64                          `json:"binding_revision_count"`
	RecordEpoch          int64                          `json:"record_epoch"`
	RecordVersionCount   int64                          `json:"record_version_count"`
}

type WorktreeBackupStore struct {
	Kind          string     `json:"kind"`
	WorktreeID    model.UUID `json:"worktree_id"`
	Incarnation   model.UUID `json:"incarnation"`
	SchemaVersion int64      `json:"schema_version"`
	File          string     `json:"file"`
	ByteLength    int64      `json:"byte_length"`
	SHA256        model.Hash `json:"sha256"`
}

type WorktreeBackupArtifact struct {
	SourcePath string     `json:"source_path"`
	BackupPath string     `json:"backup_path"`
	ByteLength int64      `json:"byte_length"`
	SHA256     model.Hash `json:"sha256"`
}

type WorktreeBackupPolicyRevision struct {
	ProfileID       model.Hash `json:"profile_id"`
	PolicyDigest    model.Hash `json:"policy_digest"`
	PolicyEpoch     int64      `json:"policy_epoch"`
	GenerationCount int64      `json:"generation_count"`
}

type verifiedSQLiteStore struct {
	kind          string
	relativePath  string
	identity      model.UUID
	incarnation   model.UUID
	schemaVersion int64
}

func BackupWorktree(ctx context.Context, options WorktreeBackupOptions) (manifest WorktreeBackupManifest, err error) {
	if ctx == nil || !filepath.IsAbs(options.HomePath) {
		return manifest, fmt.Errorf("absolute MCE4D home and context are required: %w", ErrInvalidIndex)
	}
	worktreeID, parseErr := model.ParseUUID(string(options.WorktreeID))
	if parseErr != nil || worktreeID != options.WorktreeID {
		return manifest, fmt.Errorf("canonical worktree UUID is required: %w", ErrInvalidIndex)
	}
	if options.BusyTimeoutMS == 0 {
		options.BusyTimeoutMS = defaultBusyTimeout
	}
	if options.BusyTimeoutMS < 1 || options.BusyTimeoutMS > math.MaxInt32 {
		return manifest, fmt.Errorf("SQLite busy timeout is outside the supported range: %w", ErrInvalidIndex)
	}
	indexPath := filepath.Join(options.HomePath, "worktrees", string(worktreeID), "index.sqlite")
	owner, err := newMaintenanceOwnership(ctx, Options{
		Path: indexPath, HomePath: options.HomePath, WorktreeID: worktreeID,
		BusyTimeoutMS: options.BusyTimeoutMS, LockWaitMS: options.LockWaitMS,
		AdmissionConfig: options.AdmissionConfig, AdmissionConfigProvider: options.AdmissionConfigProvider,
	})
	if err != nil {
		return manifest, err
	}
	defer func() { err = errors.Join(err, owner.Close()) }()
	worktreeLease, err := maintenance.AcquireWorktree(ctx, owner.root, string(worktreeID), owner.lockWait)
	if err != nil {
		return manifest, err
	}
	defer func() { err = errors.Join(err, worktreeLease.Close()) }()

	indexRelative := "worktrees/" + string(worktreeID) + "/index.sqlite"
	sources, err := listWorktreeBackupStores(ctx, owner.root, indexRelative, options.BusyTimeoutMS, worktreeID)
	if err != nil {
		return manifest, err
	}
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		return manifest, err
	}
	indexDB, err := openPool(ctx, comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(indexRelative))), "ro", options.BusyTimeoutMS, true, 1)
	if err != nil {
		return manifest, fmt.Errorf("open worktree index for backup metadata: %w", err)
	}
	policyRevisions, err := readPolicyRevisions(ctx, indexDB)
	if err != nil {
		return manifest, errors.Join(err, indexDB.Close())
	}
	manifestRefs, err := readCaptureManifestReferences(ctx, indexDB)
	if err != nil {
		return manifest, errors.Join(err, indexDB.Close())
	}
	if err := indexDB.Close(); err != nil {
		return manifest, fmt.Errorf("close worktree index metadata connection: %w", err)
	}
	for _, ref := range manifestRefs {
		if _, _, err := verifyOwnedManifest(owner.root, worktreeID, ref); err != nil {
			return manifest, fmt.Errorf("verify referenced capture manifest %s before backup: %w", ref.ArtifactSHA256, err)
		}
	}

	peakBytes, err := estimateWorktreeBackupPeak(owner.root, worktreeID, sources, manifestRefs)
	if err != nil {
		return manifest, err
	}
	operationID := options.OperationID
	if operationID == "" {
		operationID, err = newOperationUUID()
		if err != nil {
			return manifest, fmt.Errorf("create worktree backup operation identity: %w", err)
		}
	} else {
		parsed, parseErr := model.ParseUUID(string(operationID))
		if parseErr != nil || parsed != operationID {
			return manifest, fmt.Errorf("backup output requires a canonical operation UUID: %w", ErrInvalidIndex)
		}
	}
	backupRelative := "worktrees/" + string(worktreeID) + "/backups/" + string(operationID)
	reservation, err := owner.admitDurableGrowth(ctx, "backup", model.AdmissionOwnedPath(backupRelative), peakBytes)
	if err != nil {
		return manifest, fmt.Errorf("admit durable worktree backup output: %w", err)
	}
	defer func() { err = errors.Join(err, reservation.Close()) }()
	if err := owner.root.EnsurePrivateDirs(backupRelative); err != nil {
		return manifest, fmt.Errorf("create private worktree backup directory: %w", err)
	}

	manifest = WorktreeBackupManifest{
		FormatVersion: 1, OperationID: operationID, WorktreeID: worktreeID,
		Consistency: "single_store", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Stores: []WorktreeBackupStore{}, Artifacts: []WorktreeBackupArtifact{},
		PolicyRevisions: policyRevisions,
	}
	if len(sources) > 1 {
		manifest.Consistency = "quiesced_worktree_set"
	}
	for _, source := range sources {
		destinationRelative := backupRelative + "/" + filepath.Base(source.relativePath)
		digest, length, err := backupSQLiteFile(ctx, owner.root, source.relativePath, destinationRelative, options.BusyTimeoutMS)
		if err != nil {
			return manifest, fmt.Errorf("backup %s store: %w", source.kind, err)
		}
		verified, err := inspectSQLiteBackup(ctx, owner.root, destinationRelative, source.kind, options.BusyTimeoutMS, worktreeID)
		if err != nil {
			return manifest, fmt.Errorf("verify %s backup store: %w", source.kind, err)
		}
		if verified.identity != source.identity || verified.schemaVersion != source.schemaVersion {
			return manifest, fmt.Errorf("%s backup identity or schema changed during backup: %w", source.kind, ErrInvalidIndex)
		}
		manifest.Stores = append(manifest.Stores, WorktreeBackupStore{
			Kind: source.kind, WorktreeID: verified.identity, Incarnation: verified.incarnation,
			SchemaVersion: verified.schemaVersion, File: filepath.Base(source.relativePath),
			ByteLength: length, SHA256: digest,
		})
		if source.kind == "records" {
			manifest.RecordEpoch, manifest.RecordVersionCount, err = readRecordRevisionMetadata(ctx, owner.root, destinationRelative, options.BusyTimeoutMS)
			if err != nil {
				return manifest, err
			}
		}
	}
	manifest.BindingRevisionCount, err = readBindingRevisionCount(ctx, owner.root, backupRelative+"/index.sqlite", options.BusyTimeoutMS)
	if err != nil {
		return manifest, err
	}
	for _, ref := range manifestRefs {
		backupPath := backupRelative + "/artifacts/" + ref.Path
		artifact, err := copyVerifiedOwnedManifest(owner.root, worktreeID, ref, backupRelative, backupPath)
		if err != nil {
			return manifest, fmt.Errorf("copy referenced capture manifest %s: %w", ref.ArtifactSHA256, err)
		}
		manifest.Artifacts = append(manifest.Artifacts, artifact)
	}
	raw, err := encodeWorktreeBackupManifest(manifest)
	if err != nil {
		return manifest, err
	}
	if int64(len(raw)) > model.MaxSafeInteger-peakBytes {
		return manifest, ErrAdmissionReservationOverflow
	}
	result, publishErr := owner.root.PublishAtomicFile(backupRelative+"/manifest.json", raw, 0o600)
	if publishErr != nil {
		return manifest, fmt.Errorf("publish canonical worktree backup manifest (published=%t): %w", result.Published, publishErr)
	}
	return manifest, nil
}

func listWorktreeBackupStores(ctx context.Context, root *mceruntime.Root, indexRelative string, timeout int64, worktreeID model.UUID) ([]verifiedSQLiteStore, error) {
	indexFile, err := root.OpenRegularFile(indexRelative)
	if err != nil {
		return nil, fmt.Errorf("worktree index store is unavailable: %w", err)
	}
	if err := indexFile.Close(); err != nil {
		return nil, fmt.Errorf("close worktree index identity handle: %w", err)
	}
	sources := []verifiedSQLiteStore{{kind: "index", relativePath: indexRelative}}
	recordsRelative := filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(indexRelative)), "records.sqlite"))
	records, err := root.OpenRegularFile(recordsRelative)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, exists, inspectErr := optionalOwnedFileLength(root, recordsRelative+suffix); inspectErr != nil {
					return nil, fmt.Errorf("inspect orphan worktree records sidecar: %w", inspectErr)
				} else if exists {
					return nil, fmt.Errorf("records sidecar exists without its database: %w", ErrInvalidStorePath)
				}
			}
			index, inspectErr := inspectSQLiteBackup(ctx, root, indexRelative, "index", timeout, worktreeID)
			if inspectErr != nil {
				return nil, inspectErr
			}
			sources[0] = index
			return sources, nil
		}
		return nil, fmt.Errorf("inspect worktree records store: %w", err)
	}
	if err := records.Close(); err != nil {
		return nil, fmt.Errorf("close worktree records store identity handle: %w", err)
	}
	sources = append(sources, verifiedSQLiteStore{kind: "records", relativePath: recordsRelative})
	for index := range sources {
		verified, err := inspectSQLiteBackup(ctx, root, sources[index].relativePath, sources[index].kind, timeout, worktreeID)
		if err != nil {
			return nil, err
		}
		sources[index] = verified
	}
	return sources, nil
}

func inspectSQLiteBackup(ctx context.Context, root *mceruntime.Root, relative, kind string, timeout int64, worktreeID model.UUID) (verifiedSQLiteStore, error) {
	result := verifiedSQLiteStore{kind: kind, relativePath: relative}
	if kind != "index" && kind != "records" {
		return result, fmt.Errorf("unknown SQLite backup store kind %q: %w", kind, ErrInvalidIndex)
	}
	rootPath, err := root.FinalPath()
	if err != nil {
		return result, err
	}
	absolute := comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(relative)))
	if kind == "index" {
		if err := preflightIndex(ctx, absolute, timeout, worktreeID); err != nil {
			return result, fmt.Errorf("validate index backup source: %w", err)
		}
	}
	db, err := openPool(ctx, absolute, "ro", timeout, true, 1)
	if err != nil {
		return result, err
	}
	defer func() { _ = db.Close() }()
	if err := verifySQLiteIntegrity(ctx, db); err != nil {
		return result, err
	}
	if err := db.QueryRowContext(ctx, `SELECT schema_version FROM schema_meta WHERE singleton=1`).Scan(&result.schemaVersion); err != nil || result.schemaVersion < 1 {
		return result, errors.Join(fmt.Errorf("read %s schema version: %w", kind, ErrInvalidIndex), err)
	}
	if kind == "index" {
		identity, err := readIdentity(ctx, db)
		if err != nil {
			return result, fmt.Errorf("read index backup identity: %w", err)
		}
		result.identity, result.incarnation = identity.worktree, identity.incarnation
	} else {
		var ownerText, incarnationText string
		if err := db.QueryRowContext(ctx, `SELECT worktree_id,incarnation FROM records_meta WHERE singleton=1`).Scan(&ownerText, &incarnationText); err != nil {
			return result, fmt.Errorf("read records backup identity: %w", err)
		}
		result.identity, err = model.ParseUUID(ownerText)
		if err != nil || string(result.identity) != ownerText {
			return result, fmt.Errorf("records backup has invalid worktree identity: %w", ErrInvalidIndex)
		}
		result.incarnation, err = model.ParseUUID(incarnationText)
		if err != nil || string(result.incarnation) != incarnationText {
			return result, fmt.Errorf("records backup has invalid incarnation: %w", ErrInvalidIndex)
		}
	}
	if result.identity != worktreeID {
		return result, fmt.Errorf("%s backup belongs to worktree %s, want %s: %w", kind, result.identity, worktreeID, ErrWorktreeMismatch)
	}
	return result, nil
}

func verifySQLiteIntegrity(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("verify SQLite backup integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity_check returned %q: %w", result, ErrInvalidIndex)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("verify SQLite backup foreign keys: %w", err)
	}
	if rows.Next() {
		var table string
		var rowID, parent, key any
		if err := rows.Scan(&table, &rowID, &parent, &key); err != nil {
			return errors.Join(fmt.Errorf("read SQLite backup foreign-key result: %w", err), rows.Close())
		}
		return errors.Join(fmt.Errorf("SQLite backup contains a foreign-key violation in %s: %w", table, ErrInvalidIndex), rows.Close())
	}
	if err := rows.Err(); err != nil {
		return errors.Join(fmt.Errorf("finish SQLite backup foreign-key check: %w", err), rows.Close())
	}
	return rows.Close()
}

func readCaptureManifestReferences(ctx context.Context, db *sql.DB) ([]model.StoredCaptureManifestRef, error) {
	rows, err := db.QueryContext(ctx, `SELECT manifest_json FROM generations WHERE manifest_json IS NOT NULL ORDER BY generation_id`)
	if err != nil {
		return nil, fmt.Errorf("list capture manifests for backup: %w", err)
	}
	byPath := make(map[string]model.StoredCaptureManifestRef)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, errors.Join(fmt.Errorf("read capture manifest reference for backup: %w", err), rows.Close())
		}
		ref, err := DecodeStoredCaptureManifestRef([]byte(raw))
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if previous, ok := byPath[ref.Path]; ok && previous != ref {
			return nil, errors.Join(fmt.Errorf("capture manifest locator has conflicting descriptors: %w", ErrInvalidIndex), rows.Close())
		}
		byPath[ref.Path] = ref
		if len(byPath) > maxBackupManifestReferences {
			return nil, errors.Join(fmt.Errorf("capture manifest backup entry limit exceeded: %w", ErrAdmissionResourceLimit), rows.Close())
		}
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		return nil, errors.Join(fmt.Errorf("finish capture manifest backup scan: %w", rowsErr), closeErr)
	}
	refs := make([]model.StoredCaptureManifestRef, 0, len(byPath))
	for _, ref := range byPath {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })
	return refs, nil
}

func readPolicyRevisions(ctx context.Context, db *sql.DB) ([]WorktreeBackupPolicyRevision, error) {
	rows, err := db.QueryContext(ctx, `SELECT profile_id,policy_digest,policy_epoch,count(*)
		FROM generations GROUP BY profile_id,policy_digest,policy_epoch ORDER BY profile_id,policy_digest,policy_epoch`)
	if err != nil {
		return nil, fmt.Errorf("list generation policy revisions for backup: %w", err)
	}
	revisions := make([]WorktreeBackupPolicyRevision, 0)
	for rows.Next() {
		var profileText, digestText string
		var revision WorktreeBackupPolicyRevision
		if err := rows.Scan(&profileText, &digestText, &revision.PolicyEpoch, &revision.GenerationCount); err != nil {
			return nil, errors.Join(fmt.Errorf("read generation policy revision for backup: %w", err), rows.Close())
		}
		revision.ProfileID, err = model.ParseHash(profileText)
		if err != nil || string(revision.ProfileID) != profileText {
			return nil, errors.Join(fmt.Errorf("backup has invalid profile revision identity: %w", ErrInvalidIndex), rows.Close())
		}
		revision.PolicyDigest, err = model.ParseHash(digestText)
		if err != nil || string(revision.PolicyDigest) != digestText || revision.PolicyEpoch < 1 || revision.GenerationCount < 1 {
			return nil, errors.Join(fmt.Errorf("backup has invalid policy revision: %w", ErrInvalidIndex), rows.Close())
		}
		revisions = append(revisions, revision)
		if len(revisions) > maxBackupManifestReferences {
			return nil, errors.Join(fmt.Errorf("policy revision backup entry limit exceeded: %w", ErrAdmissionResourceLimit), rows.Close())
		}
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		return nil, errors.Join(fmt.Errorf("finish generation policy revision scan: %w", rowsErr), closeErr)
	}
	return revisions, nil
}

func readBindingRevisionCount(ctx context.Context, root *mceruntime.Root, relative string, timeout int64) (int64, error) {
	rootPath, err := root.FinalPath()
	if err != nil {
		return 0, err
	}
	db, err := openPool(ctx, comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(relative))), "ro", timeout, true, 1)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var tableCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='ui_build_bindings'`).Scan(&tableCount); err != nil {
		return 0, err
	}
	if tableCount == 0 {
		return 0, nil
	}
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ui_build_bindings`).Scan(&count); err != nil {
		return 0, fmt.Errorf("read UI binding revision count for backup: %w", err)
	}
	return count, nil
}

func readRecordRevisionMetadata(ctx context.Context, root *mceruntime.Root, relative string, timeout int64) (int64, int64, error) {
	rootPath, err := root.FinalPath()
	if err != nil {
		return 0, 0, err
	}
	db, err := openPool(ctx, comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(relative))), "ro", timeout, true, 1)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	var epoch, versions int64
	if err := db.QueryRowContext(ctx, `SELECT record_epoch FROM records_meta WHERE singleton=1`).Scan(&epoch); err != nil {
		return 0, 0, fmt.Errorf("read records epoch for backup manifest: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM record_versions`).Scan(&versions); err != nil {
		return 0, 0, fmt.Errorf("count record revisions for backup manifest: %w", err)
	}
	return epoch, versions, nil
}

func verifyOwnedManifest(root *mceruntime.Root, worktreeID model.UUID, ref model.StoredCaptureManifestRef) (model.Hash, int64, error) {
	if err := ref.Validate(); err != nil {
		return "", 0, err
	}
	return hashOwnedRegularFile(root, "worktrees/"+string(worktreeID)+"/"+ref.Path)
}

func copyVerifiedOwnedManifest(root *mceruntime.Root, worktreeID model.UUID, ref model.StoredCaptureManifestRef, backupRoot, destination string) (WorktreeBackupArtifact, error) {
	var artifact WorktreeBackupArtifact
	source := "worktrees/" + string(worktreeID) + "/" + ref.Path
	digest, length, err := verifyOwnedManifest(root, worktreeID, ref)
	if err != nil {
		return artifact, err
	}
	if digest != ref.ArtifactSHA256 || length != ref.ByteLength {
		return artifact, fmt.Errorf("capture manifest does not match its descriptor: %w", ErrInvalidIndex)
	}
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(destination)))
	if err := root.EnsurePrivateDirs(parent); err != nil {
		return artifact, err
	}
	if _, err := root.PublishAtomicFile(destination, nil, 0o600); err != nil {
		return artifact, fmt.Errorf("create backup artifact without overwrite: %w", err)
	}
	rootPath, err := root.FinalPath()
	if err != nil {
		return artifact, err
	}
	absoluteDestination := comparableOwnedPath(filepath.Join(rootPath, filepath.FromSlash(destination)))
	output, err := os.OpenFile(absoluteDestination, os.O_WRONLY, 0)
	if err != nil {
		return artifact, fmt.Errorf("open new backup artifact for streaming: %w", err)
	}
	outputInfo, statErr := output.Stat()
	pathInfo, lstatErr := os.Lstat(absoluteDestination)
	verifiedOutput, secureErr := root.OpenRegularFile(destination)
	var secureInfo os.FileInfo
	if secureErr == nil {
		secureInfo, secureErr = verifiedOutput.Stat()
	}
	if verifiedOutput != nil {
		secureErr = errors.Join(secureErr, verifiedOutput.Close())
	}
	if statErr != nil || lstatErr != nil || secureErr != nil || outputInfo == nil || pathInfo == nil || secureInfo == nil ||
		!pathInfo.Mode().IsRegular() || !os.SameFile(outputInfo, pathInfo) || !os.SameFile(outputInfo, secureInfo) || outputInfo.Size() != 0 {
		return artifact, errors.Join(fmt.Errorf("new backup artifact is not a verified empty regular file: %w", ErrInvalidStorePath), statErr, lstatErr, secureErr, output.Close())
	}
	sourceFile, err := root.OpenRegularFile(source)
	if err != nil {
		return artifact, errors.Join(fmt.Errorf("open referenced capture manifest for backup: %w", err), output.Close())
	}
	hasher := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(output, hasher), io.LimitReader(sourceFile, ref.ByteLength+1))
	sourceCloseErr := sourceFile.Close()
	flushErr := output.Sync()
	outputCloseErr := output.Close()
	actualHash := model.Hash(hex.EncodeToString(hasher.Sum(nil)))
	if copyErr != nil || sourceCloseErr != nil || flushErr != nil || outputCloseErr != nil || copied != ref.ByteLength || actualHash != ref.ArtifactSHA256 {
		return artifact, errors.Join(fmt.Errorf("streamed backup artifact failed size or digest verification: %w", ErrInvalidIndex), copyErr, sourceCloseErr, flushErr, outputCloseErr)
	}
	actualHash, actualBytes, err := hashOwnedRegularFile(root, destination)
	if err != nil || actualHash != ref.ArtifactSHA256 || actualBytes != ref.ByteLength {
		return artifact, errors.Join(fmt.Errorf("verify published backup artifact: %w", ErrInvalidIndex), err)
	}
	return WorktreeBackupArtifact{SourcePath: ref.Path, BackupPath: strings.TrimPrefix(destination, backupRoot+"/"), ByteLength: actualBytes, SHA256: actualHash}, nil
}

func estimateWorktreeBackupPeak(root *mceruntime.Root, worktreeID model.UUID, sources []verifiedSQLiteStore, refs []model.StoredCaptureManifestRef) (int64, error) {
	var currentBytes int64
	for _, source := range sources {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			length, exists, err := optionalOwnedFileLength(root, source.relativePath+suffix)
			if err != nil {
				return 0, err
			}
			if exists {
				var ok bool
				currentBytes, ok = addNonnegative(currentBytes, length)
				if !ok {
					return 0, ErrAdmissionReservationOverflow
				}
			}
		}
	}
	peak, ok := multiplyNonnegative(currentBytes, 2)
	if !ok {
		return 0, ErrAdmissionReservationOverflow
	}
	for _, ref := range refs {
		peak, ok = addNonnegative(peak, ref.ByteLength)
		if !ok {
			return 0, ErrAdmissionReservationOverflow
		}
	}
	peak, ok = addNonnegative(peak, admissionMetadataPeak+manifestWriteOverhead)
	if !ok || peak > model.MaxSafeInteger {
		return 0, ErrAdmissionReservationOverflow
	}
	return peak, nil
}

func optionalOwnedFileLength(root *mceruntime.Root, relative string) (int64, bool, error) {
	file, err := root.OpenRegularFile(relative)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || info.Size() < 0 {
		return 0, false, errors.Join(fmt.Errorf("measure owned backup source file: %w", ErrInvalidStorePath), statErr, closeErr)
	}
	return info.Size(), true, nil
}

func encodeWorktreeBackupManifest(manifest WorktreeBackupManifest) ([]byte, error) {
	if manifest.FormatVersion != 1 || manifest.OperationID == "" || manifest.WorktreeID == "" ||
		(manifest.Consistency != "single_store" && manifest.Consistency != "quiesced_worktree_set") || len(manifest.Stores) == 0 ||
		manifest.BindingRevisionCount < 0 || manifest.RecordEpoch < 0 || manifest.RecordVersionCount < 0 {
		return nil, fmt.Errorf("invalid worktree backup manifest identity or consistency: %w", ErrInvalidIndex)
	}
	operationID, operationErr := model.ParseUUID(string(manifest.OperationID))
	worktreeID, worktreeErr := model.ParseUUID(string(manifest.WorktreeID))
	createdAt, timeErr := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
	if operationErr != nil || operationID != manifest.OperationID || worktreeErr != nil || worktreeID != manifest.WorktreeID ||
		timeErr != nil || createdAt.Location() != time.UTC || createdAt.Format(time.RFC3339Nano) != manifest.CreatedAt ||
		(len(manifest.Stores) == 1) != (manifest.Consistency == "single_store") {
		return nil, fmt.Errorf("worktree backup manifest has a noncanonical identity or timestamp: %w", ErrInvalidIndex)
	}
	stores := make([]any, 0, len(manifest.Stores))
	seenStores := make(map[string]struct{}, len(manifest.Stores))
	for _, store := range manifest.Stores {
		incarnation, incarnationErr := model.ParseUUID(string(store.Incarnation))
		digest, digestErr := model.ParseHash(string(store.SHA256))
		if (store.Kind != "index" && store.Kind != "records") || store.File != store.Kind+".sqlite" || store.WorktreeID != manifest.WorktreeID ||
			incarnationErr != nil || incarnation != store.Incarnation || digestErr != nil || digest != store.SHA256 ||
			store.SchemaVersion < 1 || store.ByteLength < 1 {
			return nil, fmt.Errorf("worktree backup manifest contains an invalid store descriptor: %w", ErrInvalidIndex)
		}
		if _, duplicate := seenStores[store.Kind]; duplicate {
			return nil, fmt.Errorf("worktree backup manifest repeats a store kind: %w", ErrInvalidIndex)
		}
		seenStores[store.Kind] = struct{}{}
		stores = append(stores, map[string]any{
			"byte_length": store.ByteLength, "file": store.File, "incarnation": store.Incarnation,
			"kind": store.Kind, "schema_version": store.SchemaVersion, "sha256": store.SHA256, "worktree_id": store.WorktreeID,
		})
	}
	artifacts := make([]any, 0, len(manifest.Artifacts))
	seenArtifacts := make(map[string]struct{}, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		digest, digestErr := model.ParseHash(string(artifact.SHA256))
		if pathErr := (model.AdmissionOwnedPath(artifact.SourcePath)).Validate(); pathErr != nil || artifact.SourcePath == "" ||
			digestErr != nil || digest != artifact.SHA256 || artifact.ByteLength < 1 || artifact.BackupPath != "artifacts/"+artifact.SourcePath {
			return nil, fmt.Errorf("worktree backup manifest contains an invalid artifact descriptor: %w", ErrInvalidIndex)
		}
		if _, duplicate := seenArtifacts[artifact.SourcePath]; duplicate {
			return nil, fmt.Errorf("worktree backup manifest repeats an artifact path: %w", ErrInvalidIndex)
		}
		seenArtifacts[artifact.SourcePath] = struct{}{}
		artifacts = append(artifacts, map[string]any{
			"backup_path": artifact.BackupPath, "byte_length": artifact.ByteLength,
			"sha256": artifact.SHA256, "source_path": artifact.SourcePath,
		})
	}
	policies := make([]any, 0, len(manifest.PolicyRevisions))
	for _, revision := range manifest.PolicyRevisions {
		profile, profileErr := model.ParseHash(string(revision.ProfileID))
		policy, policyErr := model.ParseHash(string(revision.PolicyDigest))
		if profileErr != nil || profile != revision.ProfileID || policyErr != nil || policy != revision.PolicyDigest ||
			revision.PolicyEpoch < 1 || revision.GenerationCount < 1 {
			return nil, fmt.Errorf("worktree backup manifest contains an invalid policy revision: %w", ErrInvalidIndex)
		}
		policies = append(policies, map[string]any{
			"generation_count": revision.GenerationCount, "policy_digest": revision.PolicyDigest,
			"policy_epoch": revision.PolicyEpoch, "profile_id": revision.ProfileID,
		})
	}
	value := map[string]any{
		"artifacts": artifacts, "binding_revision_count": manifest.BindingRevisionCount,
		"consistency": manifest.Consistency, "created_at": manifest.CreatedAt,
		"format_version": manifest.FormatVersion, "operation_id": manifest.OperationID,
		"policy_revisions": policies, "record_epoch": manifest.RecordEpoch,
		"record_version_count": manifest.RecordVersionCount, "stores": stores,
		"worktree_id": manifest.WorktreeID,
	}
	return model.CanonicalJSON1(value)
}
