package snapshot

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type FreshnessStatus string

const (
	FreshnessObservedCurrent FreshnessStatus = "observed_current"
	FreshnessChanged         FreshnessStatus = "changed"
	FreshnessUnknown         FreshnessStatus = "unknown"
	FreshnessNotChecked      FreshnessStatus = "not_checked"
)

type FreshnessScope string

const (
	FreshnessWorkspace     FreshnessScope = "workspace"
	FreshnessReturnedFiles FreshnessScope = "returned_files"
	FreshnessNone          FreshnessScope = "none"
)

type FreshnessConsistency string

const FreshnessCapturedBytesWithObservedStability FreshnessConsistency = "captured_bytes_with_observed_stability"

type FreshnessSourceView string

const FreshnessIndexedSnapshot FreshnessSourceView = "indexed_snapshot"

const maxValidationDependencies = 1_000_000

type Freshness struct {
	Status       FreshnessStatus      `json:"status"`
	Scope        FreshnessScope       `json:"scope"`
	CheckedAt    *time.Time           `json:"checked_at"`
	BufferScope  string               `json:"buffer_scope"`
	Consistency  FreshnessConsistency `json:"consistency"`
	ChangedCount *int64               `json:"changed_count"`
	SourceView   FreshnessSourceView  `json:"source_view"`
}

type WorkspaceFile struct {
	Path        string
	ContentHash model.Hash
	ByteLength  int64
}

// SourcePrecondition is the byte identity returned with a pack or rename
// result. It intentionally does not contain editor-buffer state.
type SourcePrecondition struct {
	Path        string
	ContentHash model.Hash
}

type ScopeObservation struct {
	Vector                 model.ScopeVector
	ProviderDigest         model.Hash
	DependencyInputDigests []model.Hash
	Complete               bool
}

func (observation ScopeObservation) Validate() error {
	if !observation.Complete || observation.DependencyInputDigests == nil || len(observation.DependencyInputDigests) > maxValidationDependencies {
		return fmt.Errorf("scope/configuration observation is incomplete: %w", ErrInvalidValidationInput)
	}
	if err := observation.Vector.Validate(); err != nil {
		return fmt.Errorf("scope vector: %w: %v", ErrInvalidValidationInput, err)
	}
	if _, err := model.ParseHash(string(observation.ProviderDigest)); err != nil {
		return fmt.Errorf("provider digest: %w: %v", ErrInvalidValidationInput, err)
	}
	for _, digest := range observation.DependencyInputDigests {
		if _, err := model.ParseHash(string(digest)); err != nil {
			return fmt.Errorf("dependency digest: %w: %v", ErrInvalidValidationInput, err)
		}
	}
	return nil
}

type ValidationInput struct {
	Expected            model.CaptureManifest
	ExpectedComplete    bool
	ExpectedScopeVector model.ScopeVector
	ObservedFiles       []WorkspaceFile
	ObservedComplete    bool
	ObservedScope       ScopeObservation
	SourcePreconditions []SourcePrecondition
	CheckedAt           time.Time
}

type ValidationResult struct {
	Freshness             Freshness
	EditPreconditionValid bool
	ChangedPaths          []string
	MembershipChanged     bool
	ConfigurationChanged  bool
	RecordSetChanged      bool
	ReasonCode            string
}

var ErrInvalidValidationInput = errors.New("invalid workspace validation input")

func UnknownValidation(checkedAt time.Time, reasonCode string) ValidationResult {
	checkedAt = checkedAt.UTC()
	return ValidationResult{
		Freshness: Freshness{
			Status: FreshnessUnknown, Scope: FreshnessWorkspace, CheckedAt: timePointer(checkedAt),
			BufferScope: "disk_only", Consistency: FreshnessCapturedBytesWithObservedStability,
			ChangedCount: nil, SourceView: FreshnessIndexedSnapshot,
		},
		ChangedPaths: []string{}, ReasonCode: reasonCode,
	}
}

// CompareValidation compares a complete live workspace observation against an
// immutable capture and its pinned scope vector. It never uses Git HEAD or file
// metadata as a substitute for content hashes. The result is point-in-time
// evidence, not a source lock; the host must recheck edit hashes immediately
// before applying a patch.
func CompareValidation(input ValidationInput) (ValidationResult, error) {
	if input.CheckedAt.IsZero() {
		return ValidationResult{}, fmt.Errorf("checked_at is required: %w", ErrInvalidValidationInput)
	}
	if err := ValidateBaseline(input.Expected, input.ExpectedComplete, input.ExpectedScopeVector, input.SourcePreconditions); err != nil {
		return ValidationResult{}, err
	}
	if !input.ObservedComplete {
		return UnknownValidation(input.CheckedAt, "WORKSPACE_COVERAGE_INCOMPLETE"), nil
	}
	if !input.ObservedScope.Complete {
		return UnknownValidation(input.CheckedAt, "CONFIGURATION_UNAVAILABLE"), nil
	}
	if err := input.ObservedScope.Validate(); err != nil {
		return UnknownValidation(input.CheckedAt, "CONFIGURATION_UNAVAILABLE"), nil
	}
	observed, err := observedFileMap(input.ObservedFiles)
	if err != nil {
		return ValidationResult{}, err
	}

	expected := make(map[string]model.ManifestFile, len(input.Expected.Files))
	for _, file := range input.Expected.Files {
		expected[file.Path] = file
	}
	changed := make(map[string]struct{})
	membershipChanged := false
	for path, file := range expected {
		current, exists := observed[path]
		if !exists {
			membershipChanged = true
			changed[path] = struct{}{}
			continue
		}
		if *file.ContentHash != current.ContentHash || *file.ByteLength != current.ByteLength {
			changed[path] = struct{}{}
		}
	}
	for path := range observed {
		if _, exists := expected[path]; !exists {
			membershipChanged = true
			changed[path] = struct{}{}
		}
	}

	configurationChanged := configurationDiffers(input.Expected, input.ExpectedScopeVector, input.ObservedScope.Vector) ||
		input.Expected.ProviderDigest != input.ObservedScope.ProviderDigest ||
		!sameHashes(input.Expected.DependencyInputDigests, input.ObservedScope.DependencyInputDigests)
	recordSetChanged := recordSetDiffers(input.ExpectedScopeVector, input.ObservedScope.Vector)
	vectorMembershipChanged := scopeMembershipDiffers(input.ExpectedScopeVector, input.ObservedScope.Vector)
	membershipChanged = membershipChanged || vectorMembershipChanged
	changedPaths := make([]string, 0, len(changed))
	for path := range changed {
		changedPaths = append(changedPaths, path)
	}
	sort.Strings(changedPaths)
	status := FreshnessObservedCurrent
	if configurationChanged || recordSetChanged || membershipChanged || len(changedPaths) != 0 {
		status = FreshnessChanged
	}
	count := int64(len(changedPaths))
	checkedAt := input.CheckedAt.UTC()
	result := ValidationResult{
		Freshness: Freshness{
			Status: status, Scope: FreshnessWorkspace, CheckedAt: timePointer(checkedAt),
			BufferScope: "disk_only", Consistency: FreshnessCapturedBytesWithObservedStability,
			ChangedCount: &count, SourceView: FreshnessIndexedSnapshot,
		},
		EditPreconditionValid: status == FreshnessObservedCurrent,
		ChangedPaths:          changedPaths, MembershipChanged: membershipChanged,
		ConfigurationChanged: configurationChanged, RecordSetChanged: recordSetChanged,
	}
	return result, nil
}

func ValidateBaseline(manifest model.CaptureManifest, complete bool, vector model.ScopeVector, preconditions []SourcePrecondition) error {
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("expected capture manifest: %w: %v", ErrInvalidValidationInput, err)
	}
	if err := vector.Validate(); err != nil {
		return fmt.Errorf("expected scope vector: %w: %v", ErrInvalidValidationInput, err)
	}
	if !complete || !allCaptured(manifest.Files) {
		return fmt.Errorf("expected workspace capture must be complete: %w", ErrInvalidValidationInput)
	}
	member, exists := memberForManifest(manifest, vector)
	if !exists || member.SnapshotDigest != manifest.SnapshotDigest || member.PolicyDigest != manifest.PolicyDigest {
		return fmt.Errorf("expected manifest does not match its pinned scope member: %w", ErrInvalidValidationInput)
	}
	return validatePreconditions(manifest, preconditions)
}

func allCaptured(files []model.ManifestFile) bool {
	for _, file := range files {
		if file.Status != FileCaptured || file.ContentHash == nil || file.ByteLength == nil {
			return false
		}
	}
	return true
}

func validatePreconditions(manifest model.CaptureManifest, preconditions []SourcePrecondition) error {
	files := make(map[string]model.ManifestFile, len(manifest.Files))
	for _, file := range manifest.Files {
		files[file.Path] = file
	}
	seen := make(map[string]struct{}, len(preconditions))
	for _, precondition := range preconditions {
		if err := model.ValidateRelativePath(precondition.Path); err != nil {
			return fmt.Errorf("source precondition path %q: %w: %v", precondition.Path, ErrInvalidValidationInput, err)
		}
		if _, exists := seen[precondition.Path]; exists {
			return fmt.Errorf("duplicate source precondition %q: %w", precondition.Path, ErrInvalidValidationInput)
		}
		seen[precondition.Path] = struct{}{}
		if _, err := model.ParseHash(string(precondition.ContentHash)); err != nil {
			return fmt.Errorf("source precondition hash for %q: %w: %v", precondition.Path, ErrInvalidValidationInput, err)
		}
		file, exists := files[precondition.Path]
		if !exists || file.ContentHash == nil || *file.ContentHash != precondition.ContentHash {
			return fmt.Errorf("source precondition %q does not match the captured workspace: %w", precondition.Path, ErrInvalidValidationInput)
		}
	}
	return nil
}

func observedFileMap(files []WorkspaceFile) (map[string]WorkspaceFile, error) {
	observed := make(map[string]WorkspaceFile, len(files))
	for _, file := range files {
		if err := model.ValidateRelativePath(file.Path); err != nil {
			return nil, fmt.Errorf("observed path %q: %w: %v", file.Path, ErrInvalidValidationInput, err)
		}
		if _, exists := observed[file.Path]; exists {
			return nil, fmt.Errorf("duplicate observed path %q: %w", file.Path, ErrInvalidValidationInput)
		}
		if _, err := model.ParseHash(string(file.ContentHash)); err != nil {
			return nil, fmt.Errorf("observed hash for %q: %w: %v", file.Path, ErrInvalidValidationInput, err)
		}
		if file.ByteLength < 0 {
			return nil, fmt.Errorf("observed byte length for %q is negative: %w", file.Path, ErrInvalidValidationInput)
		}
		observed[file.Path] = file
	}
	return observed, nil
}

func configurationDiffers(expected model.CaptureManifest, before, after model.ScopeVector) bool {
	beforeMember, beforeExists := memberForManifest(expected, before)
	afterMember, afterExists := memberForManifest(expected, after)
	if !beforeExists || !afterExists {
		return true
	}
	if expected.WorktreeID != beforeMember.WorktreeID || expected.ProfileID != beforeMember.ProfileID || expected.PolicyDigest != beforeMember.PolicyDigest {
		return true
	}
	if before.CatalogIncarnation != after.CatalogIncarnation {
		return true
	}
	return beforeMember.GenerationID != afterMember.GenerationID ||
		beforeMember.SnapshotDigest != afterMember.SnapshotDigest ||
		beforeMember.PolicyDigest != afterMember.PolicyDigest ||
		beforeMember.PolicyEpoch != afterMember.PolicyEpoch ||
		beforeMember.BindingRevision != afterMember.BindingRevision ||
		beforeMember.IndexIncarnation != afterMember.IndexIncarnation
}

func sameHashes(left, right []model.Hash) bool {
	if len(left) != len(right) {
		return false
	}
	leftValues := append([]model.Hash(nil), left...)
	rightValues := append([]model.Hash(nil), right...)
	sort.Slice(leftValues, func(i, j int) bool { return leftValues[i] < leftValues[j] })
	sort.Slice(rightValues, func(i, j int) bool { return rightValues[i] < rightValues[j] })
	for index := range leftValues {
		if leftValues[index] != rightValues[index] {
			return false
		}
	}
	return true
}

func recordSetDiffers(before, after model.ScopeVector) bool {
	old := make(map[string]model.ScopeMember, len(before.Members))
	for _, member := range before.Members {
		old[scopeMemberKey(member)] = member
	}
	for _, member := range after.Members {
		prior, exists := old[scopeMemberKey(member)]
		if exists && (prior.RecordEpoch != member.RecordEpoch || prior.RecordsIncarnation != member.RecordsIncarnation) {
			return true
		}
	}
	return false
}

func scopeMembershipDiffers(before, after model.ScopeVector) bool {
	if !sameGroup(before.GroupID, after.GroupID) || !sameInt(before.GroupRevision, after.GroupRevision) || len(before.Members) != len(after.Members) || len(before.UnavailableMembers) != len(after.UnavailableMembers) {
		return true
	}
	oldMembers := make(map[string]struct{}, len(before.Members))
	for _, member := range before.Members {
		oldMembers[scopeMemberKey(member)] = struct{}{}
	}
	for _, member := range after.Members {
		if _, exists := oldMembers[scopeMemberKey(member)]; !exists {
			return true
		}
	}
	return !sameUnavailableMembers(before.UnavailableMembers, after.UnavailableMembers)
}

func memberForManifest(manifest model.CaptureManifest, vector model.ScopeVector) (model.ScopeMember, bool) {
	var selected model.ScopeMember
	found := false
	for _, member := range vector.Members {
		if member.WorktreeID == manifest.WorktreeID && member.ProfileID == manifest.ProfileID {
			if found && (selected.GenerationID != member.GenerationID || selected.SnapshotDigest != member.SnapshotDigest || selected.PolicyDigest != member.PolicyDigest || selected.PolicyEpoch != member.PolicyEpoch || selected.BindingRevision != member.BindingRevision || selected.IndexIncarnation != member.IndexIncarnation) {
				return model.ScopeMember{}, false
			}
			selected = member
			found = true
		}
	}
	return selected, found
}

func sameUnavailableMembers(left, right []model.UnavailableMember) bool {
	if len(left) != len(right) {
		return false
	}
	keys := make(map[string]int, len(left))
	for _, member := range left {
		keys[unavailableMemberKey(member)]++
	}
	for _, member := range right {
		key := unavailableMemberKey(member)
		if keys[key] == 0 {
			return false
		}
		keys[key]--
	}
	return true
}

func unavailableMemberKey(member model.UnavailableMember) string {
	serviceID := ""
	if member.ServiceID != nil {
		serviceID = string(*member.ServiceID)
	}
	worktreeID := ""
	if member.WorktreeID != nil {
		worktreeID = string(*member.WorktreeID)
	}
	profileID := ""
	if member.ProfileID != nil {
		profileID = string(*member.ProfileID)
	}
	return serviceID + "\x00" + worktreeID + "\x00" + profileID + "\x00" + member.Status + "\x00" + string(member.ReasonCode)
}

func scopeMemberKey(member model.ScopeMember) string {
	serviceID := ""
	if member.ServiceID != nil {
		serviceID = string(*member.ServiceID)
	}
	return string(member.WorktreeID) + "\x00" + string(member.ProfileID) + "\x00" + serviceID
}

func sameGroup(left, right *model.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameInt(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func timePointer(value time.Time) *time.Time { return &value }
