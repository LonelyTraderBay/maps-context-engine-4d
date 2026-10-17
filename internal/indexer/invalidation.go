package indexer

import (
	"fmt"
	"path"
	"sort"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/inventory"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

type InventorySnapshotInput struct {
	Manifest snapshot.Input
	Observed inventory.Snapshot
	Captures map[string]snapshot.FileInput
}

// BuildSnapshotFromInventory binds all currently permitted inventory paths to
// their capture result. Unknown paths and incomplete captures remain visible
// as an error or partial scope, never as a false no-op.
func BuildSnapshotFromInventory(input InventorySnapshotInput) (snapshot.Snapshot, error) {
	if input.Captures == nil {
		input.Captures = map[string]snapshot.FileInput{}
	}
	if input.Observed.Coverage.State != inventory.CoverageComplete && input.Observed.Coverage.State != inventory.CoveragePartial {
		return snapshot.Snapshot{}, fmt.Errorf("unknown inventory coverage state %q: %w", input.Observed.Coverage.State, model.ErrInvalidValue)
	}
	manifestInput := input.Manifest
	manifestInput.Files = make([]snapshot.FileInput, 0, len(input.Observed.Files))
	manifestInput.Git = snapshot.GitState{
		Head: input.Observed.Head, HeadState: snapshot.HeadState(input.Observed.HeadState), Branch: input.Observed.Branch,
	}
	manifestInput.Scope = snapshot.ScopeComplete
	if input.Observed.Coverage.State != inventory.CoverageComplete || !input.Observed.Coverage.CaseSemanticsKnown || len(input.Observed.Boundaries) != 0 {
		manifestInput.Scope = snapshot.ScopePartial
	}

	consumed := make(map[string]struct{}, len(input.Captures))
	for _, file := range input.Observed.Files {
		if err := model.ValidateRelativePath(file.Path); err != nil {
			return snapshot.Snapshot{}, fmt.Errorf("inventory path %q: %w", file.Path, err)
		}
		capture, captured := input.Captures[file.Path]
		if captured {
			if capture.Path != file.Path {
				return snapshot.Snapshot{}, fmt.Errorf("capture key/path mismatch for %q: %w", file.Path, model.ErrInvalidValue)
			}
			consumed[file.Path] = struct{}{}
		}
		switch file.State {
		case inventory.FilePresent:
			if !captured {
				reason := model.Identifier("CAPTURE_NOT_PROVIDED")
				capture = snapshot.FileInput{Path: file.Path, Status: snapshot.FileMetadataOnly, ReasonCode: &reason}
			} else if capture.Status == snapshot.FileMissing {
				return snapshot.Snapshot{}, fmt.Errorf("present inventory path %q was captured as missing: %w", file.Path, model.ErrInvalidValue)
			}
		case inventory.FileMissing:
			if captured {
				return snapshot.Snapshot{}, fmt.Errorf("missing inventory path %q has captured bytes: %w", file.Path, model.ErrInvalidValue)
			}
			reason := model.Identifier("MISSING")
			capture = snapshot.FileInput{Path: file.Path, Status: snapshot.FileMissing, ReasonCode: &reason}
		case inventory.FileUnmerged:
			if captured {
				return snapshot.Snapshot{}, fmt.Errorf("unmerged inventory path %q has captured bytes: %w", file.Path, model.ErrInvalidValue)
			}
			reason := model.Identifier("UNMERGED")
			capture = snapshot.FileInput{Path: file.Path, Status: snapshot.FileMetadataOnly, ReasonCode: &reason}
		case inventory.FileSparseOmitted:
			if captured {
				return snapshot.Snapshot{}, fmt.Errorf("sparse-omitted path %q has captured bytes: %w", file.Path, model.ErrInvalidValue)
			}
			reason := model.Identifier("SPARSE_OMITTED")
			capture = snapshot.FileInput{Path: file.Path, Status: snapshot.FileMetadataOnly, ReasonCode: &reason}
		case inventory.FileInaccessible:
			if captured {
				return snapshot.Snapshot{}, fmt.Errorf("inaccessible inventory path %q has captured bytes: %w", file.Path, model.ErrInvalidValue)
			}
			reason := model.Identifier("INACCESSIBLE")
			capture = snapshot.FileInput{Path: file.Path, Status: snapshot.FileReadError, ReasonCode: &reason}
		default:
			return snapshot.Snapshot{}, fmt.Errorf("unsupported inventory state %q for %q: %w", file.State, file.Path, model.ErrInvalidValue)
		}
		if capture.Status != snapshot.FileCaptured {
			manifestInput.Scope = snapshot.ScopePartial
		}
		manifestInput.Files = append(manifestInput.Files, capture)
	}
	for path := range input.Captures {
		if _, exists := consumed[path]; !exists {
			return snapshot.Snapshot{}, fmt.Errorf("capture path %q is absent from observed inventory: %w", path, model.ErrInvalidValue)
		}
	}
	return snapshot.Build(manifestInput)
}

type InvalidationMode string

const (
	InvalidationNoop        InvalidationMode = "no_op"
	InvalidationFullRebuild InvalidationMode = "full_rebuild"
)

type InvalidationReason string

const (
	ReasonInitialSnapshot    InvalidationReason = "initial_snapshot"
	ReasonFileAdded          InvalidationReason = "file_added"
	ReasonFileRemoved        InvalidationReason = "file_removed"
	ReasonFileChanged        InvalidationReason = "file_changed"
	ReasonManifestChanged    InvalidationReason = "manifest_changed"
	ReasonDependencyChanged  InvalidationReason = "dependency_changed"
	ReasonProfileChanged     InvalidationReason = "profile_changed"
	ReasonPolicyChanged      InvalidationReason = "policy_changed"
	ReasonProviderChanged    InvalidationReason = "provider_changed"
	ReasonWorktreeChanged    InvalidationReason = "worktree_changed"
	ReasonRepositoryState    InvalidationReason = "repository_state_changed"
	ReasonScopeIncomplete    InvalidationReason = "scope_incomplete"
	ReasonModuleConfigChange InvalidationReason = "module_config_changed"
)

// InvalidationPlan is deliberately conservative: semantic facts are reused
// only when the complete content manifest and Git provenance are unchanged.
type InvalidationPlan struct {
	Mode               InvalidationMode
	AddedPaths         []string
	RemovedPaths       []string
	ChangedPaths       []string
	Reasons            []InvalidationReason
	InputScopeComplete bool
}

func CompareSnapshots(previous, current *snapshot.Snapshot) (InvalidationPlan, error) {
	if current == nil {
		return InvalidationPlan{}, fmt.Errorf("current snapshot is required: %w", model.ErrInvalidValue)
	}
	if err := current.Validate(); err != nil {
		return InvalidationPlan{}, fmt.Errorf("validate current snapshot: %w", err)
	}
	plan := InvalidationPlan{
		Mode:       InvalidationFullRebuild,
		AddedPaths: make([]string, 0), RemovedPaths: make([]string, 0), ChangedPaths: make([]string, 0),
		Reasons: make([]InvalidationReason, 0), InputScopeComplete: current.Scope == snapshot.ScopeComplete,
	}
	if !plan.InputScopeComplete {
		plan.addReason(ReasonScopeIncomplete)
	}
	if previous == nil {
		plan.addReason(ReasonInitialSnapshot)
		return plan, nil
	}
	if err := previous.Validate(); err != nil {
		return InvalidationPlan{}, fmt.Errorf("validate previous snapshot: %w", err)
	}
	if previous.Scope != snapshot.ScopeComplete {
		plan.addReason(ReasonScopeIncomplete)
	}

	oldManifest, newManifest := previous.Manifest, current.Manifest
	if oldManifest.WorktreeID != newManifest.WorktreeID {
		plan.addReason(ReasonWorktreeChanged)
	}
	if oldManifest.ProfileID != newManifest.ProfileID {
		plan.addReason(ReasonProfileChanged)
	}
	if oldManifest.PolicyDigest != newManifest.PolicyDigest {
		plan.addReason(ReasonPolicyChanged)
	}
	if oldManifest.ProviderDigest != newManifest.ProviderDigest {
		plan.addReason(ReasonProviderChanged)
	}
	if !equalHashes(oldManifest.DependencyInputDigests, newManifest.DependencyInputDigests) {
		plan.addReason(ReasonDependencyChanged)
	}

	compareManifestFiles(oldManifest.Files, newManifest.Files, &plan)
	if !equalGitState(previous.Git, current.Git) {
		plan.addReason(ReasonRepositoryState)
	}
	if oldManifest.SnapshotDigest != newManifest.SnapshotDigest && len(plan.Reasons) == 0 {
		plan.addReason(ReasonManifestChanged)
	}
	if len(plan.AddedPaths)+len(plan.RemovedPaths)+len(plan.ChangedPaths) > 0 {
		if hasModuleConfigPath(plan.AddedPaths, plan.RemovedPaths, plan.ChangedPaths) {
			plan.addReason(ReasonModuleConfigChange)
		}
	}
	if len(plan.Reasons) == 0 {
		plan.Mode = InvalidationNoop
	}
	return plan, nil
}

func compareManifestFiles(before, after []model.ManifestFile, plan *InvalidationPlan) {
	oldFiles := make(map[string]model.ManifestFile, len(before))
	newFiles := make(map[string]model.ManifestFile, len(after))
	for _, file := range before {
		oldFiles[file.Path] = file
	}
	for _, file := range after {
		newFiles[file.Path] = file
	}
	for path, current := range newFiles {
		previous, exists := oldFiles[path]
		if !exists {
			plan.AddedPaths = append(plan.AddedPaths, path)
			plan.addReason(ReasonFileAdded)
		} else if !equalManifestFile(previous, current) {
			plan.ChangedPaths = append(plan.ChangedPaths, path)
			plan.addReason(ReasonFileChanged)
		}
	}
	for path := range oldFiles {
		if _, exists := newFiles[path]; !exists {
			plan.RemovedPaths = append(plan.RemovedPaths, path)
			plan.addReason(ReasonFileRemoved)
		}
	}
	sort.Strings(plan.AddedPaths)
	sort.Strings(plan.RemovedPaths)
	sort.Strings(plan.ChangedPaths)
}

func equalManifestFile(left, right model.ManifestFile) bool {
	return left.Path == right.Path && left.Status == right.Status &&
		equalOptionalHash(left.ContentHash, right.ContentHash) &&
		equalOptionalInt(left.ByteLength, right.ByteLength) &&
		equalOptionalIdentifier(left.ReasonCode, right.ReasonCode)
}

func equalOptionalHash(left, right *model.Hash) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalOptionalInt(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalOptionalIdentifier(left, right *model.Identifier) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalHashes(left, right []model.Hash) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]model.Hash(nil), left...)
	rightCopy := append([]model.Hash(nil), right...)
	sort.Slice(leftCopy, func(i, j int) bool { return leftCopy[i] < leftCopy[j] })
	sort.Slice(rightCopy, func(i, j int) bool { return rightCopy[i] < rightCopy[j] })
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func equalGitState(left, right snapshot.GitState) bool {
	return left.Head == right.Head && left.HeadState == right.HeadState && left.Branch == right.Branch
}

func hasModuleConfigPath(groups ...[]string) bool {
	for _, paths := range groups {
		for _, filePath := range paths {
			switch path.Base(filePath) {
			case "go.mod", "go.sum", "go.work", "go.work.sum":
				return true
			}
		}
	}
	return false
}

func (plan *InvalidationPlan) addReason(reason InvalidationReason) {
	for _, existing := range plan.Reasons {
		if existing == reason {
			return
		}
	}
	plan.Reasons = append(plan.Reasons, reason)
	sort.Slice(plan.Reasons, func(i, j int) bool { return plan.Reasons[i] < plan.Reasons[j] })
}
