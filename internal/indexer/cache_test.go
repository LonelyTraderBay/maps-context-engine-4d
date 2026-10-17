package indexer

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/analysis/golang"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

func TestSyntaxCacheKeyBindsPathBytesProviderAndConfiguration(t *testing.T) {
	base := SyntaxCacheInput{
		Path: "pkg/source.go", ContentDigest: testIndexHash("source"), Language: "go",
		ProviderDigest: testIndexHash("provider"), CompilerConfigDigest: testIndexHash("config"),
		InterpretationVersion: 1,
	}
	want, err := SyntaxCacheKey(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name  string
		apply func(*SyntaxCacheInput)
	}{
		{name: "path", apply: func(value *SyntaxCacheInput) { value.Path = "other/source.go" }},
		{name: "content", apply: func(value *SyntaxCacheInput) { value.ContentDigest = testIndexHash("source-v2") }},
		{name: "language", apply: func(value *SyntaxCacheInput) { value.Language = "typescript" }},
		{name: "provider", apply: func(value *SyntaxCacheInput) { value.ProviderDigest = testIndexHash("provider-v2") }},
		{name: "compiler config", apply: func(value *SyntaxCacheInput) { value.CompilerConfigDigest = testIndexHash("config-v2") }},
		{name: "interpretation", apply: func(value *SyntaxCacheInput) { value.InterpretationVersion++ }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := base
			mutation.apply(&changed)
			got, err := SyntaxCacheKey(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("syntax-affecting input change reused a cache key")
			}
		})
	}
	base.Path = "../outside.go"
	if _, err := SyntaxCacheKey(base); err == nil {
		t.Fatal("syntax cache must reject paths outside the snapshot")
	}
}

func TestSemanticCacheKeyBindsProfilePolicyDependenciesAndMembership(t *testing.T) {
	base := SemanticCacheInput{
		WorktreeID: "11111111-1111-4111-8111-111111111111", GenerationID: "22222222-2222-4222-8222-222222222222",
		PartitionID: testIndexHash("partition"), InputDigest: testIndexHash("input"),
		ProfileDigest: testIndexHash("profile"), PolicyDigest: testIndexHash("policy"),
		ProviderDigest: testIndexHash("provider"), CompilerConfigDigest: testIndexHash("compiler"),
		MembershipDigest:        testIndexHash("membership"),
		DependencyExportDigests: []model.Hash{testIndexHash("dep-a"), testIndexHash("dep-b")},
		InterpretationVersion:   1,
	}
	want, err := SemanticCacheKey(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.DependencyExportDigests = []model.Hash{base.DependencyExportDigests[1], base.DependencyExportDigests[0]}
	if got, err := SemanticCacheKey(reordered); err != nil || got != want {
		t.Fatalf("dependency order changed semantic key: got=%s want=%s err=%v", got, want, err)
	}
	mutations := []struct {
		name  string
		apply func(*SemanticCacheInput)
	}{
		{name: "worktree", apply: func(value *SemanticCacheInput) { value.WorktreeID = "33333333-3333-4333-8333-333333333333" }},
		{name: "generation", apply: func(value *SemanticCacheInput) { value.GenerationID = "44444444-4444-4444-8444-444444444444" }},
		{name: "partition", apply: func(value *SemanticCacheInput) { value.PartitionID = testIndexHash("partition-v2") }},
		{name: "package input", apply: func(value *SemanticCacheInput) { value.InputDigest = testIndexHash("input-v2") }},
		{name: "profile", apply: func(value *SemanticCacheInput) { value.ProfileDigest = testIndexHash("profile-v2") }},
		{name: "policy", apply: func(value *SemanticCacheInput) { value.PolicyDigest = testIndexHash("policy-v2") }},
		{name: "provider", apply: func(value *SemanticCacheInput) { value.ProviderDigest = testIndexHash("provider-v2") }},
		{name: "compiler config", apply: func(value *SemanticCacheInput) { value.CompilerConfigDigest = testIndexHash("compiler-v2") }},
		{name: "membership negative lookup", apply: func(value *SemanticCacheInput) { value.MembershipDigest = testIndexHash("membership-with-new-file") }},
		{name: "dependency export", apply: func(value *SemanticCacheInput) { value.DependencyExportDigests[0] = testIndexHash("dep-a-v2") }},
		{name: "interpretation", apply: func(value *SemanticCacheInput) { value.InterpretationVersion++ }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := base
			changed.DependencyExportDigests = append([]model.Hash(nil), base.DependencyExportDigests...)
			mutation.apply(&changed)
			got, err := SemanticCacheKey(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("semantic input change reused a cache key")
			}
		})
	}
	duplicate := base
	duplicate.DependencyExportDigests = []model.Hash{base.DependencyExportDigests[0], base.DependencyExportDigests[0]}
	if _, err := SemanticCacheKey(duplicate); err == nil {
		t.Fatal("duplicate dependency fingerprint must be rejected")
	}
}

func TestPlanIncrementalNoopAllowsOnlyExactCapturedContentReuse(t *testing.T) {
	previous := testSnapshot(t, map[string][]byte{
		"a.go": []byte("package cachetest\nfunc A() {}\n"),
		"b.go": []byte("package cachetest\nfunc B() {}\n"),
	}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)

	plan, err := PlanIncremental(&previous, &previous)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IncrementalMode != IncrementalContentOnly || plan.Invalidation.Mode != InvalidationNoop {
		t.Fatalf("identical complete snapshot plan=%+v; want content-only no-op", plan)
	}
	if len(plan.ReusableContent) != 2 || plan.ReusableContent[0].Path != "a.go" || plan.ReusableContent[1].Path != "b.go" {
		t.Fatalf("reusable content=%+v; want stable exact-path identities", plan.ReusableContent)
	}
}

func TestPlanIncrementalUsesFullFallbackAndNeverFuzzyMatchesRenames(t *testing.T) {
	previous := testSnapshot(t, map[string][]byte{
		"changed.go": []byte("package cachetest\nvar Value = 1\n"),
		"kept.go":    []byte("package cachetest\nvar Kept = 1\n"),
		"old.go":     []byte("package cachetest\nvar Moved = 1\n"),
	}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	current := testSnapshot(t, map[string][]byte{
		"changed.go": []byte("package cachetest\nvar Value = 2\n"),
		"kept.go":    []byte("package cachetest\nvar Kept = 1\n"),
		"new.go":     []byte("package cachetest\nvar Moved = 1\n"),
	}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)

	plan, err := PlanIncremental(&previous, &current)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IncrementalMode != IncrementalFull || plan.Invalidation.Mode != InvalidationFullRebuild {
		t.Fatalf("changed snapshot plan=%+v; want a full semantic fallback", plan)
	}
	if len(plan.ReusableContent) != 1 || plan.ReusableContent[0].Path != "kept.go" {
		t.Fatalf("reusable content=%+v; changed/renamed paths must not be reused", plan.ReusableContent)
	}
}

func TestPlanIncrementalInvalidatesModuleAndDependencyChanges(t *testing.T) {
	base := testSnapshot(t, map[string][]byte{
		"go.mod":  []byte("module example.test/a\n"),
		"go.work": []byte("go 1.27\nuse .\n"),
		"api.go":  []byte("package a\ntype Service interface { Read() }\n"),
		"keep.go": []byte("package a\nfunc Keep() {}\n"),
	}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)

	for _, changed := range []struct {
		name  string
		files map[string][]byte
		check func(InvalidationPlan) bool
	}{
		{
			name: "go.mod local replace",
			files: map[string][]byte{
				"go.mod":  []byte("module example.test/a\nreplace example.test/dep => ../dep\n"),
				"go.work": []byte("go 1.27\nuse .\n"),
				"api.go":  []byte("package a\ntype Service interface { Read() }\n"),
				"keep.go": []byte("package a\nfunc Keep() {}\n"),
			},
			check: func(plan InvalidationPlan) bool { return containsReason(plan, ReasonModuleConfigChange) },
		},
		{
			name: "go.work local replace",
			files: map[string][]byte{
				"go.mod":  []byte("module example.test/a\n"),
				"go.work": []byte("go 1.27\nuse .\nreplace example.test/dep => ../dep\n"),
				"api.go":  []byte("package a\ntype Service interface { Read() }\n"),
				"keep.go": []byte("package a\nfunc Keep() {}\n"),
			},
			check: func(plan InvalidationPlan) bool { return containsReason(plan, ReasonModuleConfigChange) },
		},
		{
			name: "exported interface",
			files: map[string][]byte{
				"go.mod":  []byte("module example.test/a\n"),
				"go.work": []byte("go 1.27\nuse .\n"),
				"api.go":  []byte("package a\ntype Service interface { Read(); Write() }\n"),
				"keep.go": []byte("package a\nfunc Keep() {}\n"),
			},
			check: func(plan InvalidationPlan) bool { return containsReason(plan, ReasonFileChanged) },
		},
	} {
		t.Run(changed.name, func(t *testing.T) {
			current := testSnapshot(t, changed.files, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
			plan, err := PlanIncremental(&base, &current)
			if err != nil {
				t.Fatal(err)
			}
			if plan.IncrementalMode != IncrementalFull || !changed.check(plan.Invalidation) {
				t.Fatalf("config/API change did not force the required fallback: %+v", plan)
			}
			if !hasReusablePath(plan.ReusableContent, "keep.go") {
				t.Fatalf("unchanged exact-path content was not reusable: %+v", plan.ReusableContent)
			}
		})
	}

	dependency := base
	dependency.Manifest.DependencyInputDigests = []model.Hash{model.Hash(strings.Repeat("e", 64))}
	var err error
	dependency.Manifest.SnapshotDigest, err = dependency.Manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanIncremental(&base, &dependency)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IncrementalMode != IncrementalFull || !containsReason(plan.Invalidation, ReasonDependencyChanged) {
		t.Fatalf("dependency input drift did not force full fallback: %+v", plan)
	}
}

func TestPlanIncrementalNeverNoopsOnIncompleteScope(t *testing.T) {
	complete := testSnapshot(t, map[string][]byte{"same.go": []byte("package x\n")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	partial := testSnapshot(t, map[string][]byte{"same.go": []byte("package x\n")}, snapshot.ScopePartial, "d", snapshot.HeadPresent)
	plan, err := PlanIncremental(&complete, &partial)
	if err != nil {
		t.Fatal(err)
	}
	if plan.IncrementalMode != IncrementalFull || plan.Invalidation.Mode != InvalidationFullRebuild || plan.Invalidation.InputScopeComplete {
		t.Fatalf("incomplete scope was treated as a no-op: %+v", plan)
	}
	if !hasReusablePath(plan.ReusableContent, "same.go") {
		t.Fatalf("safe blob identity was unnecessarily lost: %+v", plan.ReusableContent)
	}
}

func TestPlanIncrementalMutationChainMatchesFullFactsAndReplaysDeterministically(t *testing.T) {
	first, err := runMutationChain(1847)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runMutationChain(1847)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seeded mutation chain produced different plans or normalized facts")
	}
}

type mutationStep struct {
	Plan  IncrementalPlan
	Facts golang.PackageFacts
}

func runMutationChain(seed int64) ([]mutationStep, error) {
	rng := rand.New(rand.NewSource(seed))
	state := map[string][]byte{
		"file-a.go": []byte("package fixture\nfunc A() int { return 1 }\n"),
		"file-b.go": []byte("package fixture\nfunc B() int { return 2 }\n"),
	}
	previous, err := testSnapshotFromFiles(state, snapshot.ScopeComplete)
	if err != nil {
		return nil, err
	}
	cached := cloneSourceFiles(state)
	steps := make([]mutationStep, 0, 33)

	noOp, err := PlanIncremental(&previous, &previous)
	if err != nil {
		return nil, err
	}
	if noOp.IncrementalMode != IncrementalContentOnly {
		return nil, fmt.Errorf("initial identical snapshot mode=%q", noOp.IncrementalMode)
	}
	noOpFacts, err := analyzeFixtureSources(reuseFixtureSources(state, cached, noOp.ReusableContent))
	if err != nil {
		return nil, err
	}
	steps = append(steps, mutationStep{Plan: noOp, Facts: noOpFacts})

	for step := 0; step < 32; step++ {
		state = mutateFixtureFiles(state, step, rng)
		current, err := testSnapshotFromFiles(state, snapshot.ScopeComplete)
		if err != nil {
			return nil, err
		}
		plan, err := PlanIncremental(&previous, &current)
		if err != nil {
			return nil, err
		}
		replay, err := PlanIncremental(&previous, &current)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(plan, replay) {
			return nil, fmt.Errorf("mutation %d produced a nondeterministic plan", step)
		}
		if plan.IncrementalMode != IncrementalFull || plan.Invalidation.Mode != InvalidationFullRebuild {
			return nil, fmt.Errorf("mutation %d skipped semantic full rebuild: %+v", step, plan)
		}
		fullFacts, err := analyzeFixtureSources(state)
		if err != nil {
			return nil, fmt.Errorf("full analysis at mutation %d: %w", step, err)
		}
		reusedFacts, err := analyzeFixtureSources(reuseFixtureSources(state, cached, plan.ReusableContent))
		if err != nil {
			return nil, fmt.Errorf("content-reuse analysis at mutation %d: %w", step, err)
		}
		if !reflect.DeepEqual(fullFacts, reusedFacts) {
			return nil, fmt.Errorf("normalized full and content-reuse facts differ at mutation %d", step)
		}
		steps = append(steps, mutationStep{Plan: plan, Facts: fullFacts})
		previous = current
		cached = cloneSourceFiles(state)
	}
	return steps, nil
}

func mutateFixtureFiles(files map[string][]byte, step int, rng *rand.Rand) map[string][]byte {
	result := cloneSourceFiles(files)
	paths := make([]string, 0, len(result))
	for path := range result {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	switch rng.Intn(4) {
	case 0:
		path := paths[rng.Intn(len(paths))]
		result[path] = []byte(fmt.Sprintf("package fixture\nfunc Mutation%d() int { return %d }\n", step, step))
	case 1:
		result[fmt.Sprintf("added-%02d.go", step)] = []byte(fmt.Sprintf("package fixture\nfunc Added%d() int { return %d }\n", step, step))
	case 2:
		if len(paths) > 1 {
			delete(result, paths[rng.Intn(len(paths))])
		} else {
			result[fmt.Sprintf("added-%02d.go", step)] = []byte(fmt.Sprintf("package fixture\nfunc Added%d() int { return %d }\n", step, step))
		}
	case 3:
		path := paths[rng.Intn(len(paths))]
		content := result[path]
		delete(result, path)
		result[fmt.Sprintf("renamed-%02d-%s", step, path)] = content
	}
	return result
}

func testSnapshotFromFiles(files map[string][]byte, scope snapshot.ScopeStatus) (snapshot.Snapshot, error) {
	inputs := make([]snapshot.FileInput, 0, len(files))
	for path, content := range files {
		inputs = append(inputs, snapshot.FileInput{Path: path, Status: snapshot.FileCaptured, Content: content})
	}
	result, err := snapshot.Build(snapshot.Input{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: model.Hash(strings.Repeat("a", 64)), PolicyDigest: model.Hash(strings.Repeat("b", 64)),
		ProviderDigest: model.Hash(strings.Repeat("c", 64)), DependencyInputDigests: []model.Hash{},
		Files: inputs, Git: snapshot.GitState{Head: strings.Repeat("d", 64), HeadState: snapshot.HeadPresent, Branch: "main"},
		Scope: scope,
	})
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return result, nil
}

func analyzeFixtureSources(files map[string][]byte) (golang.PackageFacts, error) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	input := packageInputForCacheTest(paths, files)
	return golang.AnalyzePackage(input)
}

func packageInputForCacheTest(paths []string, files map[string][]byte) golang.PackageInput {
	inputs := make([]golang.CapturedFile, 0, len(paths))
	for _, path := range paths {
		inputs = append(inputs, golang.CapturedFile{Path: path, Source: files[path]})
	}
	return golang.PackageInput{
		GenerationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		ProfileID:    model.Hash(strings.Repeat("b", 64)), ImportPath: "example.test/fixture",
		ModuleKey: "example.test/module", Variant: golang.VariantProduction,
		MaxFileBytes: golang.DefaultMaxFileBytes, Files: inputs,
	}
}

func reuseFixtureSources(current, cache map[string][]byte, reusable []ReusableContent) map[string][]byte {
	result := cloneSourceFiles(current)
	for _, candidate := range reusable {
		cached, exists := cache[candidate.Path]
		if exists && int64(len(cached)) == candidate.ByteLength && model.SHA256(cached) == candidate.ContentHash {
			result[candidate.Path] = append([]byte(nil), cached...)
		}
	}
	return result
}

func cloneSourceFiles(files map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(files))
	for path, content := range files {
		result[path] = append([]byte(nil), content...)
	}
	return result
}

func hasReusablePath(files []ReusableContent, path string) bool {
	for _, file := range files {
		if file.Path == path {
			return true
		}
	}
	return false
}
