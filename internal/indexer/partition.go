package indexer

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var ErrWorkerMemoryBudget = errors.New("worker memory budget cannot admit one partition")

type PartitionInvalidationMode string

const (
	PartitionNoop        PartitionInvalidationMode = "no_op"
	PartitionFullRebuild PartitionInvalidationMode = "full_rebuild"
)

// BuildPartitionIdentity names one logical build unit, independent of a
// generation. Worktree/profile/module/package/variant changes create a new ID.
type BuildPartitionIdentity struct {
	WorktreeID model.UUID
	ProfileID  model.Hash
	ModuleKey  string
	ImportPath string
	Variant    string
}

func (identity BuildPartitionIdentity) ID() (model.Hash, error) {
	if _, err := model.ParseUUID(string(identity.WorktreeID)); err != nil {
		return "", fmt.Errorf("partition worktree: %w", err)
	}
	if _, err := model.ParseHash(string(identity.ProfileID)); err != nil {
		return "", fmt.Errorf("partition profile: %w", err)
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "module key", value: identity.ModuleKey},
		{name: "import path", value: identity.ImportPath},
		{name: "variant", value: identity.Variant},
	} {
		if err := validateCacheIdentifier(model.Identifier(item.value), item.name); err != nil {
			return "", err
		}
	}
	if identity.Variant != "production" && identity.Variant != "internal_test" && identity.Variant != "external_test" {
		return "", fmt.Errorf("unsupported package variant %q: %w", identity.Variant, model.ErrInvalidValue)
	}
	return model.HashValue("build-partition-v1", map[string]any{
		"worktree_id": identity.WorktreeID, "profile_id": identity.ProfileID,
		"module_key": identity.ModuleKey, "import_path": identity.ImportPath,
		"variant": identity.Variant,
	})
}

// PackageMembershipInput captures the complete package file set and the
// configuration/policy/provider inputs that can turn a previous negative
// lookup into a newly resolvable package.
type PackageMembershipInput struct {
	PartitionID         model.Hash
	ConfigurationDigest model.Hash
	PolicyDigest        model.Hash
	ProviderDigest      model.Hash
	Files               []string
	Complete            bool
}

func PackageMembershipDigest(input PackageMembershipInput) (model.Hash, error) {
	if !input.Complete {
		return "", fmt.Errorf("package membership is incomplete: %w", model.ErrInvalidValue)
	}
	for _, item := range []struct {
		name  string
		value model.Hash
	}{
		{name: "partition ID", value: input.PartitionID},
		{name: "configuration digest", value: input.ConfigurationDigest},
		{name: "policy digest", value: input.PolicyDigest},
		{name: "provider digest", value: input.ProviderDigest},
	} {
		if err := validatePartitionDigest(item.name, item.value); err != nil {
			return "", err
		}
	}
	files := append([]string(nil), input.Files...)
	sort.Strings(files)
	fileValues := make([]any, len(files))
	for index, file := range files {
		if err := model.ValidateRelativePath(file); err != nil {
			return "", fmt.Errorf("membership file %q: %w", file, err)
		}
		if index > 0 && files[index-1] == file {
			return "", fmt.Errorf("duplicate membership file %q: %w", file, model.ErrInvalidValue)
		}
		fileValues[index] = file
	}
	return model.HashValue("package-membership-v1", map[string]any{
		"partition_id": input.PartitionID, "configuration_digest": input.ConfigurationDigest,
		"policy_digest": input.PolicyDigest, "provider_digest": input.ProviderDigest,
		"files": fileValues, "complete": true,
	})
}

// BuildPartition contains one package/module-variant unit and its directed
// dependency edges. ExportDigest represents the externally visible facts
// consumers resolve; incomplete units never authorize selective reuse.
type BuildPartition struct {
	ID                        model.Hash
	InputDigest               model.Hash
	ConfigurationDigest       model.Hash
	ExportDigest              model.Hash
	DependencyIDs             []model.Hash
	DependencyClosureComplete bool
}

// PartitionSnapshot makes set-level coverage explicit, including a genuinely
// empty but complete package set versus an incomplete inventory with no units.
type PartitionSnapshot struct {
	Partitions []BuildPartition
	Complete   bool
}

type PartitionInvalidationPlan struct {
	Mode                PartitionInvalidationMode
	ChangedPartitionIDs []model.Hash
	// ReverseClosurePartitionIDs is the deterministic candidate scope from the
	// union of dependency graphs; it never authorizes semantic fact reuse.
	ReverseClosurePartitionIDs []model.Hash
	// AffectedPartitionIDs is the actual rebuild scope. With current evidence,
	// any input change rebuilds the complete partition set.
	AffectedPartitionIDs    []model.Hash
	UnresolvedDependencyIDs []model.Hash
	Reasons                 []string
	FullFallback            bool
	SemanticReuseAllowed    bool
}

// PlanPartitionInvalidation computes the deterministic reverse dependency
// closure over the union of old and new graphs. It reports the closure for
// future differential work but rebuilds the complete set on any change until
// semantic fact reuse has a full-equivalence proof. Missing edges and
// incomplete snapshots also force that conservative fallback.
func PlanPartitionInvalidation(previous *PartitionSnapshot, current PartitionSnapshot) (PartitionInvalidationPlan, error) {
	oldUnits := map[model.Hash]BuildPartition{}
	var err error
	if previous != nil {
		oldUnits, err = indexPartitions(previous.Partitions)
		if err != nil {
			return PartitionInvalidationPlan{}, fmt.Errorf("validate previous partitions: %w", err)
		}
	}
	newUnits, err := indexPartitions(current.Partitions)
	if err != nil {
		return PartitionInvalidationPlan{}, fmt.Errorf("validate current partitions: %w", err)
	}
	plan := PartitionInvalidationPlan{
		Mode: PartitionNoop, ChangedPartitionIDs: []model.Hash{}, AffectedPartitionIDs: []model.Hash{},
		UnresolvedDependencyIDs: []model.Hash{}, Reasons: []string{}, SemanticReuseAllowed: false,
	}
	seeds := make(map[model.Hash]struct{})
	reasons := make(map[string]struct{})
	currentIDs := make(map[model.Hash]struct{}, len(newUnits))
	for id := range newUnits {
		currentIDs[id] = struct{}{}
	}
	for id, unit := range newUnits {
		old, existed := oldUnits[id]
		if !existed {
			seeds[id] = struct{}{}
			reasons["partition_added"] = struct{}{}
		} else {
			if unit.InputDigest != old.InputDigest {
				seeds[id] = struct{}{}
				reasons["input_changed"] = struct{}{}
			}
			if unit.ConfigurationDigest != old.ConfigurationDigest {
				seeds[id] = struct{}{}
				reasons["configuration_changed"] = struct{}{}
			}
			if unit.ExportDigest != old.ExportDigest {
				seeds[id] = struct{}{}
				reasons["export_changed"] = struct{}{}
			}
			if !equalPartitionIDs(unit.DependencyIDs, old.DependencyIDs) {
				seeds[id] = struct{}{}
				reasons["dependencies_changed"] = struct{}{}
			}
			if unit.DependencyClosureComplete != old.DependencyClosureComplete {
				seeds[id] = struct{}{}
				reasons["dependency_boundary_changed"] = struct{}{}
			}
		}
		if !unit.DependencyClosureComplete {
			reasons["incomplete_dependency_closure"] = struct{}{}
		}
	}
	for id, unit := range oldUnits {
		if _, exists := newUnits[id]; !exists {
			seeds[id] = struct{}{}
			reasons["partition_removed"] = struct{}{}
		}
		if !unit.DependencyClosureComplete {
			reasons["incomplete_dependency_closure"] = struct{}{}
		}
	}
	if previous == nil {
		for id := range newUnits {
			seeds[id] = struct{}{}
		}
		reasons["initial_snapshot"] = struct{}{}
	}
	if !current.Complete || (previous != nil && !previous.Complete) {
		reasons["incomplete_partition_snapshot"] = struct{}{}
	}
	if len(newUnits) == 0 && len(oldUnits) == 0 && previous == nil {
		reasons["initial_snapshot"] = struct{}{}
	}

	unresolved := make(map[model.Hash]struct{})
	reverse := make(map[model.Hash][]model.Hash)
	for _, units := range []map[model.Hash]BuildPartition{oldUnits, newUnits} {
		for owner, unit := range units {
			for _, dependency := range unit.DependencyIDs {
				if _, exists := units[dependency]; !exists {
					unresolved[dependency] = struct{}{}
					reasons["unresolved_dependency"] = struct{}{}
					continue
				}
				reverse[dependency] = append(reverse[dependency], owner)
			}
		}
	}
	for dependency, dependents := range reverse {
		slices.Sort(dependents)
		unique := dependents[:0]
		for _, dependent := range dependents {
			if len(unique) == 0 || unique[len(unique)-1] != dependent {
				unique = append(unique, dependent)
			}
		}
		reverse[dependency] = unique
	}
	plan.ChangedPartitionIDs = sortedPartitionIDs(seeds)
	plan.UnresolvedDependencyIDs = sortedPartitionIDs(unresolved)
	closure := reversePartitionClosure(seeds, reverse)
	_, incompleteBoundary := reasons["incomplete_dependency_closure"]
	_, incompleteSnapshot := reasons["incomplete_partition_snapshot"]
	plan.ReverseClosurePartitionIDs = sortedPartitionIDs(closure)
	plan.FullFallback = previous == nil || len(seeds) > 0 || len(unresolved) > 0 || incompleteBoundary || incompleteSnapshot
	if plan.FullFallback {
		// Removed units contribute invalidation seeds and old dependency edges,
		// but they are absent from the current build and must not be scheduled.
		closure = currentIDs
		plan.Mode = PartitionFullRebuild
		if len(seeds) > 0 {
			reasons["semantic_reuse_not_proven"] = struct{}{}
		}
	}
	plan.AffectedPartitionIDs = sortedPartitionIDs(closure)
	for reason := range reasons {
		plan.Reasons = append(plan.Reasons, reason)
	}
	sort.Strings(plan.Reasons)
	return plan, nil
}

func indexPartitions(partitions []BuildPartition) (map[model.Hash]BuildPartition, error) {
	result := make(map[model.Hash]BuildPartition, len(partitions))
	for _, partition := range partitions {
		if err := validatePartitionDigest("ID", partition.ID); err != nil {
			return nil, err
		}
		for _, item := range []struct {
			name  string
			value model.Hash
		}{
			{name: "input digest", value: partition.InputDigest},
			{name: "configuration digest", value: partition.ConfigurationDigest},
			{name: "export digest", value: partition.ExportDigest},
		} {
			if err := validatePartitionDigest(item.name, item.value); err != nil {
				return nil, err
			}
		}
		if _, exists := result[partition.ID]; exists {
			return nil, fmt.Errorf("duplicate partition ID %q: %w", partition.ID, model.ErrInvalidValue)
		}
		unit := partition
		if !slices.IsSorted(unit.DependencyIDs) {
			unit.DependencyIDs = append([]model.Hash(nil), partition.DependencyIDs...)
			slices.Sort(unit.DependencyIDs)
		}
		for index, dependency := range unit.DependencyIDs {
			if err := validatePartitionDigest("dependency", dependency); err != nil {
				return nil, err
			}
			if index > 0 && unit.DependencyIDs[index-1] == dependency {
				return nil, fmt.Errorf("duplicate partition dependency %q: %w", dependency, model.ErrInvalidValue)
			}
		}
		result[unit.ID] = unit
	}
	return result, nil
}

func equalPartitionIDs(left, right []model.Hash) bool {
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

func reversePartitionClosure(seeds map[model.Hash]struct{}, reverse map[model.Hash][]model.Hash) map[model.Hash]struct{} {
	closure := make(map[model.Hash]struct{}, len(seeds))
	queue := make([]model.Hash, 0, len(seeds))
	for seed := range seeds {
		closure[seed] = struct{}{}
		queue = append(queue, seed)
	}
	slices.Sort(queue)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, dependent := range reverse[current] {
			if _, visited := closure[dependent]; visited {
				continue
			}
			closure[dependent] = struct{}{}
			queue = append(queue, dependent)
		}
	}
	return closure
}

func validatePartitionDigest(name string, value model.Hash) error {
	if !isCanonicalPartitionDigest(string(value)) {
		_, err := model.ParseHash(string(value))
		return fmt.Errorf("partition %s: %w", name, err)
	}
	return nil
}

func isCanonicalPartitionDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func sortedPartitionIDs(values map[model.Hash]struct{}) []model.Hash {
	result := make([]model.Hash, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

type PartitionWorkerBudget struct {
	MaxWorkers              int
	MemoryBudgetBytes       int64
	EstimatedBytesPerWorker int64
}

// ResolvePartitionWorkers caps parallel work by partition count, configured
// workers, and a measured/declared per-worker memory estimate.
func ResolvePartitionWorkers(partitionCount int, budget PartitionWorkerBudget) (int, error) {
	if partitionCount < 0 || budget.MaxWorkers < 1 || budget.MemoryBudgetBytes < 1 || budget.EstimatedBytesPerWorker < 1 {
		return 0, fmt.Errorf("invalid partition worker budget: %w", model.ErrInvalidValue)
	}
	if partitionCount == 0 {
		return 0, nil
	}
	byMemory := budget.MemoryBudgetBytes / budget.EstimatedBytesPerWorker
	if byMemory < 1 {
		return 0, ErrWorkerMemoryBudget
	}
	workers := partitionCount
	if budget.MaxWorkers < workers {
		workers = budget.MaxWorkers
	}
	if byMemory < int64(workers) {
		workers = int(byMemory)
	}
	return workers, nil
}
