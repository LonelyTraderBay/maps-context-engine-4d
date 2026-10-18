package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	appstore "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/store"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

var errStoreCapabilityUnavailable = errors.New("requested store capability is unavailable")

type worktreeBackupCLIResult struct {
	Operation string                          `json:"operation"`
	Output    string                          `json:"output"`
	Manifest  appstore.WorktreeBackupManifest `json:"manifest"`
}

type worktreeGCCLIResult struct {
	Operation string                       `json:"operation"`
	Applied   bool                         `json:"applied"`
	Plan      appstore.GenerationGCPlan    `json:"plan"`
	Result    *appstore.GenerationGCResult `json:"result,omitempty"`
}

type worktreeRestoreCLIResult struct {
	Operation string                         `json:"operation"`
	Result    appstore.WorktreeRestoreResult `json:"result"`
}

type worktreeRecoveryCLIResult struct {
	Operation string                                 `json:"operation"`
	Report    appstore.WorktreeRestoreRecoveryReport `json:"report"`
}

func runStore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeFailure(stderr, fmt.Errorf("%w: store requires backup, gc, restore, or recover", errUsage))
	}
	switch args[0] {
	case "backup":
		return runStoreBackup(ctx, args[1:], stdout, stderr)
	case "gc":
		return runStoreGC(ctx, args[1:], stdout, stderr)
	case "restore":
		return runStoreRestore(ctx, args[1:], stdout, stderr)
	case "recover":
		return runStoreRecover(ctx, args[1:], stdout, stderr)
	default:
		return writeFailure(stderr, fmt.Errorf("%w: store action %q is unavailable", errUsage, args[0]))
	}
}

func runStoreRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, map[string]optionSpec{
		"json": {}, "manifest": {value: true}, "confirm": {}, "wait-ms": {value: true},
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "json") || !has(options, "manifest") || !has(options, "confirm") || !has(options, "wait-ms") {
		return writeFailure(stderr, fmt.Errorf("%w: store restore requires --manifest, --confirm, --wait-ms, and --json", errUsage))
	}
	manifestPath, err := filepath.Abs(first(options, "manifest"))
	if err != nil || !filepath.IsAbs(first(options, "manifest")) {
		return writeFailure(stderr, fmt.Errorf("%w: --manifest must be an absolute local manifest.json path", errUsage))
	}
	manifest, err := appstore.ReadWorktreeBackupManifest(manifestPath)
	if err != nil {
		return writeFailure(stderr, err)
	}
	worktreeID, err := model.ParseUUID(string(manifest.WorktreeID))
	if err != nil || worktreeID != manifest.WorktreeID {
		return writeFailure(stderr, fmt.Errorf("%w: backup manifest has an invalid worktree identity", errUsage))
	}
	waitMS, err := parseStoreWait(first(options, "wait-ms"))
	if err != nil {
		return writeFailure(stderr, err)
	}
	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	admission, _, provider, err := cliAdmissionSource(homePath)
	if err != nil {
		return writeFailure(stderr, err)
	}
	result, err := appstore.RestoreWorktree(ctx, appstore.WorktreeRestoreOptions{
		HomePath: homePath, ManifestPath: manifestPath, WorktreeID: worktreeID, Confirm: true,
		LockWaitMS: waitMS, AdmissionConfig: admission, AdmissionConfigProvider: provider,
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	return writeJSON(stdout, stderr, worktreeRestoreCLIResult{Operation: "store_restore", Result: result})
}

func runStoreRecover(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, map[string]optionSpec{
		"json": {}, "target": {value: true}, "worktree-id": {value: true},
		"report-only": {}, "apply": {}, "confirm": {}, "wait-ms": {value: true},
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "json") || first(options, "target") != "worktree" || !has(options, "worktree-id") || has(options, "report-only") && has(options, "apply") || has(options, "confirm") && !has(options, "apply") {
		return writeFailure(stderr, fmt.Errorf("%w: store recover requires --target worktree, --worktree-id, and --json; --apply and --report-only are mutually exclusive", errUsage))
	}
	worktreeID, err := model.ParseUUID(first(options, "worktree-id"))
	if err != nil || string(worktreeID) != first(options, "worktree-id") {
		return writeFailure(stderr, fmt.Errorf("%w: --worktree-id must be a canonical UUID", errUsage))
	}
	waitMS := int64(5_000)
	if has(options, "wait-ms") {
		waitMS, err = parseStoreWait(first(options, "wait-ms"))
		if err != nil {
			return writeFailure(stderr, err)
		}
	}
	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	request := appstore.WorktreeRestoreRecoveryOptions{HomePath: homePath, WorktreeID: worktreeID, LockWaitMS: waitMS}
	if has(options, "apply") {
		if !has(options, "confirm") {
			return writeFailure(stderr, fmt.Errorf("%w: store recover --apply requires --confirm", errUsage))
		}
		admission, _, provider, admissionErr := cliAdmissionSource(homePath)
		if admissionErr != nil {
			return writeFailure(stderr, admissionErr)
		}
		request.Apply, request.Confirm = true, true
		request.AdmissionConfig, request.AdmissionConfigProvider = admission, provider
	}
	report, err := appstore.RecoverWorktreeRestore(ctx, request)
	if err != nil {
		return writeFailure(stderr, err)
	}
	return writeJSON(stdout, stderr, worktreeRecoveryCLIResult{Operation: "store_recover", Report: report})
}

func parseStoreWait(value string) (int64, error) {
	waitMS, err := strconv.ParseInt(value, 10, 64)
	if err != nil || waitMS < 1 || waitMS > 2_147_483_647 {
		return 0, fmt.Errorf("%w: --wait-ms must be within 1..2147483647", errUsage)
	}
	return waitMS, nil
}

func runStoreGC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, map[string]optionSpec{
		"json": {}, "apply": {}, "target": {value: true}, "worktree-id": {value: true},
		"keep-per-profile": {value: true}, "wait-ms": {value: true},
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "json") || !has(options, "target") || !has(options, "worktree-id") || !has(options, "keep-per-profile") {
		return writeFailure(stderr, fmt.Errorf("%w: store gc requires --json, --target, --worktree-id, and --keep-per-profile", errUsage))
	}
	if first(options, "target") != "worktree" {
		if first(options, "target") == "catalog" {
			return writeFailure(stderr, fmt.Errorf("%w: catalog SQLite storage is not available in this runtime", errStoreCapabilityUnavailable))
		}
		return writeFailure(stderr, fmt.Errorf("%w: --target must be catalog or worktree", errUsage))
	}
	worktreeID, err := model.ParseUUID(first(options, "worktree-id"))
	if err != nil || string(worktreeID) != first(options, "worktree-id") {
		return writeFailure(stderr, fmt.Errorf("%w: --worktree-id must be a canonical UUID", errUsage))
	}
	keepPerProfile, err := strconv.ParseInt(first(options, "keep-per-profile"), 10, 64)
	if err != nil || keepPerProfile < 1 || keepPerProfile > model.MaxSafeInteger {
		return writeFailure(stderr, fmt.Errorf("%w: --keep-per-profile must be within 1..%d", errUsage, model.MaxSafeInteger))
	}
	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	waitMS := int64(5_000)
	var admission model.AdmissionConfig
	var provider appstore.AdmissionConfigProvider
	if has(options, "apply") {
		admission, waitMS, provider, err = cliAdmissionSource(homePath)
		if err != nil {
			return writeFailure(stderr, err)
		}
	}
	if has(options, "wait-ms") {
		waitMS, err = strconv.ParseInt(first(options, "wait-ms"), 10, 64)
		if err != nil || waitMS < 1 || waitMS > 2_147_483_647 {
			return writeFailure(stderr, fmt.Errorf("%w: --wait-ms must be within 1..2147483647", errUsage))
		}
	}
	report, err := appstore.RunGenerationGC(ctx, appstore.GenerationGCOptions{
		HomePath: homePath, WorktreeID: worktreeID, LockWaitMS: waitMS,
		AdmissionConfig: admission, AdmissionConfigProvider: provider,
		KeepPerProfile: keepPerProfile, Apply: has(options, "apply"),
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	return writeJSON(stdout, stderr, worktreeGCCLIResult{
		Operation: "store_gc", Applied: report.Applied, Plan: report.Plan, Result: report.Result,
	})
}

func runStoreBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, map[string]optionSpec{
		"json": {}, "target": {value: true}, "worktree-id": {value: true},
		"output": {value: true}, "wait-ms": {value: true},
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "json") || !has(options, "target") || !has(options, "output") {
		return writeFailure(stderr, fmt.Errorf("%w: store backup requires --json, --target, and --output", errUsage))
	}
	if first(options, "target") != "worktree" {
		if first(options, "target") == "catalog" {
			return writeFailure(stderr, fmt.Errorf("%w: catalog SQLite storage is not available in this runtime", errStoreCapabilityUnavailable))
		}
		return writeFailure(stderr, fmt.Errorf("%w: --target must be catalog or worktree", errUsage))
	}
	if !has(options, "worktree-id") {
		return writeFailure(stderr, fmt.Errorf("%w: worktree backup requires --worktree-id", errUsage))
	}
	worktreeID, err := model.ParseUUID(first(options, "worktree-id"))
	if err != nil || string(worktreeID) != first(options, "worktree-id") {
		return writeFailure(stderr, fmt.Errorf("%w: --worktree-id must be a canonical UUID", errUsage))
	}
	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	admission, defaultWaitMS, provider, err := cliAdmissionSource(homePath)
	if err != nil {
		return writeFailure(stderr, err)
	}
	waitMS := defaultWaitMS
	if has(options, "wait-ms") {
		waitMS, err = strconv.ParseInt(first(options, "wait-ms"), 10, 64)
		if err != nil || waitMS < 1 || waitMS > 2_147_483_647 {
			return writeFailure(stderr, fmt.Errorf("%w: --wait-ms must be within 1..2147483647", errUsage))
		}
	}
	operationID, err := operationIDFromBackupOutput(homePath, worktreeID, first(options, "output"))
	if err != nil {
		return writeFailure(stderr, err)
	}
	manifest, err := appstore.BackupWorktree(ctx, appstore.WorktreeBackupOptions{
		HomePath: homePath, WorktreeID: worktreeID, OperationID: operationID,
		LockWaitMS: waitMS, AdmissionConfig: admission, AdmissionConfigProvider: provider,
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	output := filepath.Join(homePath, "worktrees", string(worktreeID), "backups", string(operationID))
	return writeJSON(stdout, stderr, worktreeBackupCLIResult{Operation: "store_backup", Output: output, Manifest: manifest})
}

func operationIDFromBackupOutput(homePath string, worktreeID model.UUID, output string) (model.UUID, error) {
	if !filepath.IsAbs(output) {
		return "", fmt.Errorf("%w: --output must be an absolute owned backup directory", errUsage)
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return "", fmt.Errorf("%w: resolve --output: %v", errUsage, err)
	}
	parent := filepath.Join(homePath, "worktrees", string(worktreeID), "backups")
	parentAbs, err := filepath.Abs(parent)
	if err != nil {
		return "", fmt.Errorf("%w: resolve owned backup root: %v", errUsage, err)
	}
	actualParent := filepath.Clean(filepath.Dir(absolute))
	if runtime.GOOS == "windows" {
		actualParent = comparableCLIPath(actualParent)
		parentAbs = comparableCLIPath(parentAbs)
		if !strings.EqualFold(actualParent, parentAbs) {
			return "", fmt.Errorf("%w: --output must be an immediate child of this worktree's backups directory", errUsage)
		}
	} else if actualParent != filepath.Clean(parentAbs) {
		return "", fmt.Errorf("%w: --output must be an immediate child of this worktree's backups directory", errUsage)
	}
	operationID, err := model.ParseUUID(filepath.Base(absolute))
	if err != nil || string(operationID) != filepath.Base(absolute) {
		return "", fmt.Errorf("%w: backup output directory name must be a canonical operation UUID", errUsage)
	}
	return operationID, nil
}

func comparableCLIPath(path string) string {
	if strings.HasPrefix(path, `\\?\UNC\`) {
		path = `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
	} else if strings.HasPrefix(path, `\\?\`) {
		path = strings.TrimPrefix(path, `\\?\`)
	}
	return filepath.Clean(path)
}

func cliAdmissionSource(homePath string) (model.AdmissionConfig, int64, appstore.AdmissionConfigProvider, error) {
	pipeline, err := runtimeconfig.NewPipeline()
	if err != nil {
		return model.AdmissionConfig{}, 0, nil, err
	}
	store, err := newHomeConfigStore(homePath, pipeline)
	if err != nil {
		return model.AdmissionConfig{}, 0, nil, err
	}
	provider := func(ctx context.Context) (model.AdmissionConfig, error) {
		raw, present, err := store.ReadCanonical(ctx)
		if err != nil {
			return model.AdmissionConfig{}, err
		}
		source, err := pipeline.ParseSource(raw, present)
		if err != nil {
			return model.AdmissionConfig{}, err
		}
		preview, err := pipeline.Resolve(source, nil, nil, nil, runtime.NumCPU(), runtimeconfig.ModeShow)
		if err != nil {
			return model.AdmissionConfig{}, err
		}
		return model.AdmissionConfig{
			SchemaVersion: 1, MaxWorktreeCacheBytes: preview.Config.Core.MaxWorktreeCacheBytes,
			VolumeReserveBytes: preview.Config.Core.VolumeReserveBytes, VolumeWarningBytes: preview.Config.Core.VolumeWarningBytes,
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initial, err := provider(ctx)
	if err != nil {
		return model.AdmissionConfig{}, 0, nil, err
	}
	raw, present, err := store.ReadCanonical(ctx)
	if err != nil {
		return model.AdmissionConfig{}, 0, nil, err
	}
	source, err := pipeline.ParseSource(raw, present)
	if err != nil {
		return model.AdmissionConfig{}, 0, nil, err
	}
	preview, err := pipeline.Resolve(source, nil, nil, nil, runtime.NumCPU(), runtimeconfig.ModeShow)
	if err != nil {
		return model.AdmissionConfig{}, 0, nil, err
	}
	return initial, preview.Config.Core.MaintenanceWaitMS, provider, nil
}
