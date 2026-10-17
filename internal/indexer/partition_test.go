package indexer

import (
	"errors"
	"reflect"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestBuildPartitionIdentityIsStableWithinWorktreeProfileAndVariant(t *testing.T) {
	identity := BuildPartitionIdentity{
		WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID:  testIndexHash("profile-a"),
		ModuleKey:  "example.test/module", ImportPath: "example.test/module/pkg", Variant: "production",
	}
	first, err := identity.ID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := identity.ID()
	if err != nil || first != second {
		t.Fatalf("partition identity is not deterministic: first=%s second=%s err=%v", first, second, err)
	}
	for name, mutate := range map[string]func(*BuildPartitionIdentity){
		"worktree": func(value *BuildPartitionIdentity) { value.WorktreeID = "22222222-2222-4222-8222-222222222222" },
		"profile":  func(value *BuildPartitionIdentity) { value.ProfileID = testIndexHash("profile-b") },
		"module":   func(value *BuildPartitionIdentity) { value.ModuleKey = "example.test/other" },
		"package":  func(value *BuildPartitionIdentity) { value.ImportPath = "example.test/module/other" },
		"variant":  func(value *BuildPartitionIdentity) { value.Variant = "internal_test" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := identity
			mutate(&changed)
			id, err := changed.ID()
			if err != nil {
				t.Fatal(err)
			}
			if id == first {
				t.Fatal("partition scope change reused an existing identity")
			}
		})
	}
}

func TestPackageMembershipDigestInvalidatesNegativeLookups(t *testing.T) {
	base := PackageMembershipInput{
		PartitionID: testIndexHash("partition"), ConfigurationDigest: testIndexHash("config"),
		PolicyDigest: testIndexHash("policy"), ProviderDigest: testIndexHash("provider"),
		Files: []string{"pkg/a.go", "pkg/b.go"}, Complete: true,
	}
	first, err := PackageMembershipDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.Files = []string{"pkg/b.go", "pkg/a.go"}
	second, err := PackageMembershipDigest(reordered)
	if err != nil || first != second {
		t.Fatalf("membership order changed the digest: first=%s second=%s err=%v", first, second, err)
	}
	added := base
	added.Files = append(append([]string(nil), base.Files...), "pkg/new.go")
	third, err := PackageMembershipDigest(added)
	if err != nil || third == first {
		t.Fatalf("new package file did not invalidate negative lookup: first=%s third=%s err=%v", first, third, err)
	}
	changedConfig := base
	changedConfig.ConfigurationDigest = testIndexHash("config-v2")
	fourth, err := PackageMembershipDigest(changedConfig)
	if err != nil || fourth == first {
		t.Fatalf("configuration change did not invalidate membership: first=%s fourth=%s err=%v", first, fourth, err)
	}
	if _, err := PackageMembershipDigest(PackageMembershipInput{PartitionID: base.PartitionID}); err == nil {
		t.Fatal("incomplete membership must not produce a cache key")
	}
	duplicate := base
	duplicate.Files = []string{"pkg/a.go", "pkg/a.go"}
	if _, err := PackageMembershipDigest(duplicate); err == nil {
		t.Fatal("duplicate membership path must be rejected")
	}
}

func TestPlanPartitionInvalidationComputesDeterministicReverseClosure(t *testing.T) {
	old := []BuildPartition{
		testPartition("a", nil),
		testPartition("b", []string{"a"}),
		testPartition("c", []string{"b"}),
		testPartition("d", nil),
	}
	changed := clonePartitions(old)
	changed[0].ExportDigest = testIndexHash("a-export-v2")
	oldSnapshot, changedSnapshot := completePartitionSnapshot(old), completePartitionSnapshot(changed)
	first, err := PlanPartitionInvalidation(&oldSnapshot, changedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode != PartitionFullRebuild || !first.FullFallback || first.SemanticReuseAllowed {
		t.Fatalf("unexpected selective plan safety flags: %+v", first)
	}
	want := partitionIDs("a", "b", "c")
	if !reflect.DeepEqual(first.ChangedPartitionIDs, []model.Hash{partitionID("a")}) ||
		!reflect.DeepEqual(first.ReverseClosurePartitionIDs, want) ||
		!reflect.DeepEqual(first.AffectedPartitionIDs, partitionIDs("a", "b", "c", "d")) {
		t.Fatalf("changed/closure/rebuild=%+v/%+v/%+v", first.ChangedPartitionIDs, first.ReverseClosurePartitionIDs, first.AffectedPartitionIDs)
	}
	permutedOld := []BuildPartition{old[3], old[2], old[1], old[0]}
	permutedChanged := []BuildPartition{changed[2], changed[0], changed[3], changed[1]}
	permutedOldSnapshot, permutedChangedSnapshot := completePartitionSnapshot(permutedOld), completePartitionSnapshot(permutedChanged)
	second, err := PlanPartitionInvalidation(&permutedOldSnapshot, permutedChangedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("partition plan changed with input ordering:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestPlanPartitionInvalidationHandlesCyclesAndRemovedEdges(t *testing.T) {
	cycle := []BuildPartition{
		testPartition("a", []string{"b"}),
		testPartition("b", []string{"a"}),
		testPartition("c", []string{"b"}),
		testPartition("d", nil),
	}
	changed := clonePartitions(cycle)
	changed[0].InputDigest = testIndexHash("a-input-v2")
	cycleSnapshot, changedSnapshot := completePartitionSnapshot(cycle), completePartitionSnapshot(changed)
	plan, err := PlanPartitionInvalidation(&cycleSnapshot, changedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := partitionIDs("a", "b", "c")
	if !reflect.DeepEqual(plan.ReverseClosurePartitionIDs, want) || !reflect.DeepEqual(plan.AffectedPartitionIDs, partitionIDs("a", "b", "c", "d")) {
		t.Fatalf("cycle/dependent closure/rebuild=%v/%v, want %v/all", plan.ReverseClosurePartitionIDs, plan.AffectedPartitionIDs, want)
	}

	before := []BuildPartition{
		testPartition("a", nil), testPartition("b", []string{"a"}),
		testPartition("c", []string{"b"}), testPartition("d", nil),
	}
	after := clonePartitions(before)
	after[1].DependencyIDs = nil
	beforeSnapshot, afterSnapshot := completePartitionSnapshot(before), completePartitionSnapshot(after)
	removedEdgePlan, err := PlanPartitionInvalidation(&beforeSnapshot, afterSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removedEdgePlan.ChangedPartitionIDs, []model.Hash{partitionID("b")}) ||
		!reflect.DeepEqual(removedEdgePlan.ReverseClosurePartitionIDs, partitionIDs("b", "c")) ||
		!reflect.DeepEqual(removedEdgePlan.AffectedPartitionIDs, partitionIDs("a", "b", "c", "d")) {
		t.Fatalf("removed dependency edge did not invalidate downstream consumers: %+v", removedEdgePlan)
	}
}

func TestPlanPartitionInvalidationDoesNotScheduleRemovedPartitions(t *testing.T) {
	previous := []BuildPartition{
		testPartition("a", nil), testPartition("b", []string{"a"}),
		testPartition("c", []string{"b"}), testPartition("removed", nil),
	}
	current := previous[:3]
	previousSnapshot, currentSnapshot := completePartitionSnapshot(previous), completePartitionSnapshot(current)
	plan, err := PlanPartitionInvalidation(&previousSnapshot, currentSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.FullFallback || !reflect.DeepEqual(plan.ChangedPartitionIDs, []model.Hash{partitionID("removed")}) {
		t.Fatalf("removed partition did not trigger conservative invalidation: %+v", plan)
	}
	if !reflect.DeepEqual(plan.ReverseClosurePartitionIDs, []model.Hash{partitionID("removed")}) {
		t.Fatalf("unexpected reverse closure for removed leaf: %v", plan.ReverseClosurePartitionIDs)
	}
	if !reflect.DeepEqual(plan.AffectedPartitionIDs, partitionIDs("a", "b", "c")) {
		t.Fatalf("rebuild schedule includes removed or omits current partitions: %v", plan.AffectedPartitionIDs)
	}
}

func TestPlanPartitionInvalidationFallsBackForIncompleteOrUnresolvedClosure(t *testing.T) {
	complete := []BuildPartition{
		testPartition("a", nil), testPartition("b", []string{"a"}), testPartition("c", nil),
	}
	incomplete := clonePartitions(complete)
	incomplete[0].DependencyClosureComplete = false
	completeSnapshot := completePartitionSnapshot(complete)
	incompleteSnapshot := completePartitionSnapshot(incomplete)
	plan, err := PlanPartitionInvalidation(&completeSnapshot, incompleteSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.FullFallback || plan.Mode != PartitionFullRebuild || !reflect.DeepEqual(plan.AffectedPartitionIDs, partitionIDs("a", "b", "c")) {
		t.Fatalf("incomplete dependency boundary did not request full fallback: %+v", plan)
	}

	unresolved := clonePartitions(complete)
	unresolved[1].DependencyIDs = []model.Hash{partitionID("missing")}
	unresolvedSnapshot := completePartitionSnapshot(unresolved)
	plan, err = PlanPartitionInvalidation(&completeSnapshot, unresolvedSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.FullFallback || plan.Mode != PartitionFullRebuild || !reflect.DeepEqual(plan.UnresolvedDependencyIDs, []model.Hash{partitionID("missing")}) {
		t.Fatalf("unresolved dependency did not force full fallback: %+v", plan)
	}
	if !reflect.DeepEqual(plan.AffectedPartitionIDs, partitionIDs("a", "b", "c")) {
		t.Fatalf("full fallback omitted a partition: %+v", plan.AffectedPartitionIDs)
	}
}

func TestPlanPartitionInvalidationDetectsInputProfileAndMembershipIdentityChanges(t *testing.T) {
	previous := []BuildPartition{testPartition("a", nil), testPartition("b", []string{"a"})}
	current := clonePartitions(previous)
	current[0].ConfigurationDigest = testIndexHash("config-changed")
	current[1].DependencyIDs = nil
	previousSnapshot, currentSnapshot := completePartitionSnapshot(previous), completePartitionSnapshot(current)
	plan, err := PlanPartitionInvalidation(&previousSnapshot, currentSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !containsPartitionReason(plan, "configuration_changed") || !containsPartitionReason(plan, "dependencies_changed") {
		t.Fatalf("configuration or package membership edge change was not reported: %+v", plan.Reasons)
	}

	noOpSnapshot := completePartitionSnapshot(clonePartitions(previous))
	noOp, err := PlanPartitionInvalidation(&previousSnapshot, noOpSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if noOp.Mode != PartitionNoop || noOp.FullFallback || len(noOp.AffectedPartitionIDs) != 0 {
		t.Fatalf("identical complete partition set was not a no-op: %+v", noOp)
	}
}

func TestPlanPartitionInvalidationRejectsDuplicateIDsAndEdges(t *testing.T) {
	unit := testPartition("a", nil)
	duplicateSnapshot := completePartitionSnapshot([]BuildPartition{unit, unit})
	validSnapshot := completePartitionSnapshot([]BuildPartition{unit})
	if _, err := PlanPartitionInvalidation(&duplicateSnapshot, validSnapshot); err == nil {
		t.Fatal("duplicate partition ID must be rejected")
	}
	unit.DependencyIDs = []model.Hash{partitionID("b"), partitionID("b")}
	if _, err := PlanPartitionInvalidation(nil, completePartitionSnapshot([]BuildPartition{unit})); err == nil {
		t.Fatal("duplicate dependency edge must be rejected")
	}
}

func TestResolvePartitionWorkersRespectsMemoryAndConfiguredCaps(t *testing.T) {
	budget := PartitionWorkerBudget{MaxWorkers: 8, MemoryBudgetBytes: 3_000, EstimatedBytesPerWorker: 1_000}
	workers, err := ResolvePartitionWorkers(100, budget)
	if err != nil || workers != 3 {
		t.Fatalf("memory cap workers=%d err=%v, want 3", workers, err)
	}
	workers, err = ResolvePartitionWorkers(2, budget)
	if err != nil || workers != 2 {
		t.Fatalf("partition count workers=%d err=%v, want 2", workers, err)
	}
	budget.MemoryBudgetBytes = 999
	if _, err := ResolvePartitionWorkers(4, budget); !errors.Is(err, ErrWorkerMemoryBudget) {
		t.Fatalf("sub-worker memory budget err=%v, want ErrWorkerMemoryBudget", err)
	}
	if workers, err = ResolvePartitionWorkers(0, budget); err != nil || workers != 0 {
		t.Fatalf("empty partition set workers=%d err=%v, want zero workers", workers, err)
	}
	if _, err := ResolvePartitionWorkers(-1, budget); err == nil {
		t.Fatal("negative partition count must be rejected")
	}
}

func BenchmarkPlanPartitionInvalidation2048(b *testing.B) {
	previous := make([]BuildPartition, 2_048)
	for index := range previous {
		id := string(rune(index + 1))
		var dependencies []string
		if index > 0 {
			dependencies = []string{string(rune(index))}
		}
		previous[index] = testPartition(id, dependencies)
	}
	current := clonePartitions(previous)
	current[0].ExportDigest = testIndexHash("changed-export")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		oldSnapshot, currentSnapshot := completePartitionSnapshot(previous), completePartitionSnapshot(current)
		plan, err := PlanPartitionInvalidation(&oldSnapshot, currentSnapshot)
		if err != nil || len(plan.AffectedPartitionIDs) != len(current) {
			b.Fatalf("invalid benchmark plan: affected=%d err=%v", len(plan.AffectedPartitionIDs), err)
		}
	}
}

func TestPlanPartitionInvalidationDistinguishesEmptyCompleteFromUnknown(t *testing.T) {
	empty := completePartitionSnapshot(nil)
	noOp, err := PlanPartitionInvalidation(&empty, completePartitionSnapshot(nil))
	if err != nil || noOp.Mode != PartitionNoop || noOp.FullFallback {
		t.Fatalf("complete empty package set should be a no-op: plan=%+v err=%v", noOp, err)
	}
	unknown := PartitionSnapshot{Partitions: nil, Complete: false}
	full, err := PlanPartitionInvalidation(&empty, unknown)
	if err != nil || full.Mode != PartitionFullRebuild || !full.FullFallback {
		t.Fatalf("empty incomplete package set must force full fallback: plan=%+v err=%v", full, err)
	}
}

func testPartition(id string, dependencies []string) BuildPartition {
	dependencyIDs := make([]model.Hash, len(dependencies))
	for index, dependency := range dependencies {
		dependencyIDs[index] = partitionID(dependency)
	}
	return BuildPartition{
		ID: partitionID(id), InputDigest: testIndexHash(id + "-input"),
		ConfigurationDigest: testIndexHash(id + "-config"), ExportDigest: testIndexHash(id + "-export"),
		DependencyIDs: dependencyIDs, DependencyClosureComplete: true,
	}
}

func partitionID(value string) model.Hash {
	return model.SHA256([]byte("partition:" + value))
}

func partitionIDs(values ...string) []model.Hash {
	set := make(map[model.Hash]struct{}, len(values))
	for _, value := range values {
		set[partitionID(value)] = struct{}{}
	}
	return sortedPartitionIDs(set)
}

func testIndexHash(value string) model.Hash {
	return model.SHA256([]byte(value))
}

func clonePartitions(values []BuildPartition) []BuildPartition {
	cloned := append([]BuildPartition(nil), values...)
	for index := range cloned {
		cloned[index].DependencyIDs = append([]model.Hash(nil), cloned[index].DependencyIDs...)
	}
	return cloned
}

func completePartitionSnapshot(values []BuildPartition) PartitionSnapshot {
	return PartitionSnapshot{Partitions: clonePartitions(values), Complete: true}
}

func containsPartitionReason(plan PartitionInvalidationPlan, reason string) bool {
	for _, actual := range plan.Reasons {
		if actual == reason {
			return true
		}
	}
	return false
}
