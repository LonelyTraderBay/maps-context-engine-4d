package store

import (
	"context"
	"path/filepath"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

// These result types are application-facing views of the current SQLite
// implementation. Keeping the dependency here lets delivery adapters avoid
// importing the storage engine directly.
type AdmissionConfigProvider = sqlite.AdmissionConfigProvider
type WorktreeBackupManifest = sqlite.WorktreeBackupManifest
type GenerationGCPlan = sqlite.GenerationGCPlan
type GenerationGCResult = sqlite.GenerationGCResult
type WorktreeRestoreResult = sqlite.WorktreeRestoreResult
type WorktreeRestoreRecoveryReport = sqlite.WorktreeRestoreRecoveryReport

type GenerationGCOptions struct {
	HomePath                string
	WorktreeID              model.UUID
	KeepPerProfile          int64
	Apply                   bool
	LockWaitMS              int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
}

type WorktreeBackupOptions struct {
	HomePath                string
	WorktreeID              model.UUID
	OperationID             model.UUID
	LockWaitMS              int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
}

type WorktreeRestoreOptions struct {
	HomePath                string
	ManifestPath            string
	WorktreeID              model.UUID
	Confirm                 bool
	LockWaitMS              int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
}

type WorktreeRestoreRecoveryOptions struct {
	HomePath                string
	WorktreeID              model.UUID
	Apply                   bool
	Confirm                 bool
	LockWaitMS              int64
	AdmissionConfig         model.AdmissionConfig
	AdmissionConfigProvider AdmissionConfigProvider
}

func RunGenerationGC(ctx context.Context, options GenerationGCOptions) (sqlite.GenerationGCReport, error) {
	indexPath := filepath.Join(options.HomePath, "worktrees", string(options.WorktreeID), "index.sqlite")
	return sqlite.RunGenerationGC(ctx, sqlite.GenerationGCOptions{
		Store: sqlite.Options{
			Path: indexPath, HomePath: options.HomePath, WorktreeID: options.WorktreeID,
			LockWaitMS: options.LockWaitMS, AdmissionConfig: options.AdmissionConfig,
			AdmissionConfigProvider: options.AdmissionConfigProvider,
		},
		KeepPerProfile: options.KeepPerProfile, Apply: options.Apply,
	})
}

func BackupWorktree(ctx context.Context, options WorktreeBackupOptions) (WorktreeBackupManifest, error) {
	return sqlite.BackupWorktree(ctx, sqlite.WorktreeBackupOptions{
		HomePath: options.HomePath, WorktreeID: options.WorktreeID, OperationID: options.OperationID,
		LockWaitMS: options.LockWaitMS, AdmissionConfig: options.AdmissionConfig,
		AdmissionConfigProvider: options.AdmissionConfigProvider,
	})
}

func ReadWorktreeBackupManifest(path string) (WorktreeBackupManifest, error) {
	return sqlite.ReadWorktreeBackupManifest(path)
}

func RestoreWorktree(ctx context.Context, options WorktreeRestoreOptions) (WorktreeRestoreResult, error) {
	return sqlite.RestoreWorktree(ctx, sqlite.WorktreeRestoreOptions{
		HomePath: options.HomePath, ManifestPath: options.ManifestPath, WorktreeID: options.WorktreeID,
		Confirm: options.Confirm, LockWaitMS: options.LockWaitMS, AdmissionConfig: options.AdmissionConfig,
		AdmissionConfigProvider: options.AdmissionConfigProvider,
	})
}

func RecoverWorktreeRestore(ctx context.Context, options WorktreeRestoreRecoveryOptions) (WorktreeRestoreRecoveryReport, error) {
	return sqlite.RecoverWorktreeRestore(ctx, sqlite.WorktreeRestoreRecoveryOptions{
		HomePath: options.HomePath, WorktreeID: options.WorktreeID, Apply: options.Apply,
		Confirm: options.Confirm, LockWaitMS: options.LockWaitMS, AdmissionConfig: options.AdmissionConfig,
		AdmissionConfigProvider: options.AdmissionConfigProvider,
	})
}
