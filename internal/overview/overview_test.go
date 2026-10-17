package overview

import (
	"errors"
	"reflect"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var testScope = Scope{
	WorktreeID:   model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
	ProfileID:    model.Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
	GenerationID: model.UUID("cccccccc-cccc-4ccc-8ccc-cccccccccccc"),
}

func hashOf(t *testing.T, value any) model.Hash {
	t.Helper()
	hash, err := model.HashValue("overview-test-v1", value)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func membership(key string, file SourceFile) ModuleMembershipInput {
	return ModuleMembershipInput{ModuleKey: key, Source: SourceEvidence{
		Path: file.Path, ContentHash: file.ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: file.ByteLength},
	}}
}

func fixture() Input {
	manifest := []byte("module example.test/project\n")
	consumer := []byte("package consumer\nfunc Run() {}\n")
	shared := []byte("package shared\nfunc Read() {}\n")
	return Input{
		Scope: testScope,
		Files: []SourceFile{
			{Path: "go.mod", ContentHash: model.SHA256(manifest), ByteLength: int64(len(manifest)), Language: "go", Role: "manifest"},
			{Path: "consumer/consumer.go", ContentHash: model.SHA256(consumer), ByteLength: int64(len(consumer)), Language: "go", Role: "source"},
			{Path: "consumer/consumer_test.go", ContentHash: model.SHA256([]byte("package consumer\n")), ByteLength: int64(len("package consumer\n")), Language: "go", Role: "test"},
			{Path: "shared/shared.go", ContentHash: model.SHA256(shared), ByteLength: int64(len(shared)), Language: "go", Role: "source"},
		},
		Modules: []ModuleInput{
			{Key: "project", Label: "Project", Kind: "go_module", Plane: "observed_static", Source: SourceEvidence{Path: "go.mod", ContentHash: model.SHA256(manifest), Range: model.ByteRange{StartByte: 0, EndByte: int64(len(manifest))}}},
			{Key: "shared", Label: "Shared", Kind: "package_group", Plane: "observed_static", ParentKey: "project", Source: SourceEvidence{Path: "go.mod", ContentHash: model.SHA256(manifest), Range: model.ByteRange{StartByte: 0, EndByte: int64(len(manifest))}}},
		},
		Packages: []PackageInput{
			{Key: model.Hash("1111111111111111111111111111111111111111111111111111111111111111"), ImportPath: "example.test/project/consumer", Variant: "production", ModuleMemberships: []ModuleMembershipInput{membership("project", SourceFile{Path: "go.mod", ContentHash: model.SHA256(manifest), ByteLength: int64(len(manifest))})}, Files: []string{"consumer/consumer.go"}},
			{Key: model.Hash("2222222222222222222222222222222222222222222222222222222222222222"), ImportPath: "example.test/project/shared", Variant: "production", ModuleMemberships: []ModuleMembershipInput{membership("shared", SourceFile{Path: "go.mod", ContentHash: model.SHA256(manifest), ByteLength: int64(len(manifest))})}, Files: []string{"shared/shared.go"}},
		},
	}
}

func TestBuildGroupsReferencesWithExactExpansion(t *testing.T) {
	input := fixture()
	consumer := input.Packages[0]
	shared := input.Packages[1]
	for i := 0; i < 2048; i++ {
		input.References = append(input.References, ReferenceInput{
			ID: hashOf(t, int64(i)), SourcePackageKey: consumer.Key, TargetPackageKey: hashPointer(shared.Key),
			Kind: "CALLS_DIRECT", EvidenceKind: "type_resolved", ResolutionStatus: "exact_in_supported_scope",
			Source: SourceEvidence{Path: "consumer/consumer.go", ContentHash: input.Files[1].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: 7}},
		})
	}

	overview, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Dependencies) != 1 || overview.Dependencies[0].ReferenceCount != 2048 {
		t.Fatalf("group lost references: dependencies=%+v", overview.Dependencies)
	}
	items, err := overview.Expand(overview.Dependencies[0].ExpandRef)
	if err != nil || len(items) != 2048 {
		t.Fatalf("expand returned %d references, err=%v", len(items), err)
	}
	seen := make(map[model.Hash]bool, len(items))
	for _, item := range items {
		if seen[item.ID] {
			t.Fatalf("duplicate relation %s in expansion", item.ID)
		}
		seen[item.ID] = true
	}
	if len(seen) != len(input.References) || overview.Statistics.ReferenceCount != len(input.References) {
		t.Fatalf("reference catalog/expansion mismatch: catalog=%d expansion=%d", overview.Statistics.ReferenceCount, len(seen))
	}
	if overview.Statistics.GroupedReferenceCount != 2048 || overview.Statistics.CatalogOnlyReferenceCount != 0 {
		t.Fatalf("overview scale counts are inconsistent: %+v", overview.Statistics)
	}
	if overview.Packages[0].OutgoingReferenceCount != 2048 || overview.Packages[1].IncomingReferenceCount != 2048 {
		t.Fatalf("package fanout counts are incomplete: %+v", overview.Packages)
	}
	items[0].TargetPackageKey = nil
	items[0].SourceModuleIDs[0] = model.Hash("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	again, err := overview.Expand(overview.Dependencies[0].ExpandRef)
	if err != nil || again[0].TargetPackageKey == nil || again[0].SourceModuleIDs[0] == items[0].SourceModuleIDs[0] {
		t.Fatal("caller mutation leaked into the retained expansion")
	}
	if overview.Capabilities.D3BehaviorAndConstraints != "not_available" || overview.Capabilities.D4EvolutionAndEvidence != "partial" {
		t.Fatalf("unsupported dimensions were overstated: %+v", overview.Capabilities)
	}
}

func TestSharedPackageMembershipRemainsExplicitAndUncollapsed(t *testing.T) {
	input := fixture()
	manifest := input.Files[0]
	for _, key := range []string{"shared-copy"} {
		input.Modules = append(input.Modules, ModuleInput{Key: key, Label: "Shared copy", Kind: "package_group", Plane: "observed_static",
			Source: SourceEvidence{Path: "go.mod", ContentHash: manifest.ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: manifest.ByteLength}}})
	}
	input.Packages[1].ModuleMemberships = []ModuleMembershipInput{membership("shared", manifest), membership("shared-copy", manifest)}
	input.References = []ReferenceInput{{
		ID: hashOf(t, "shared-call"), SourcePackageKey: input.Packages[0].Key, TargetPackageKey: hashPointer(input.Packages[1].Key),
		Kind: "CALLS_DIRECT", EvidenceKind: "type_resolved", ResolutionStatus: "exact_in_supported_scope",
		Source: SourceEvidence{Path: "consumer/consumer.go", ContentHash: input.Files[1].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: 7}},
	}}

	result, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Dependencies) != 0 || len(result.ReferenceCatalog) != 1 || len(result.ReferenceCatalog[0].TargetModuleIDs) != 2 {
		t.Fatalf("shared package ownership was collapsed: dependencies=%+v references=%+v", result.Dependencies, result.ReferenceCatalog)
	}
	if len(result.Gaps) != 1 || result.Gaps[0].Code != "references_not_grouped_to_unique_modules" || result.Gaps[0].Count != 1 || result.Statistics.CatalogOnlyReferenceCount != 1 {
		t.Fatalf("ambiguous module mapping was not surfaced: %+v", result.Gaps)
	}
	for _, module := range result.Modules {
		if module.Key == "shared" || module.Key == "shared-copy" {
			if len(module.PackageKeys) != 1 || module.PackageKeys[0] != input.Packages[1].Key {
				t.Fatalf("shared package missing from module %q: %+v", module.Key, module.PackageKeys)
			}
		}
	}
}

func TestDependencyGroupsKeepDirectionRelationAndEvidenceSeparate(t *testing.T) {
	input := fixture()
	consumer, shared := input.Packages[0], input.Packages[1]
	input.References = []ReferenceInput{
		{ID: hashOf(t, "call-forward"), SourcePackageKey: consumer.Key, TargetPackageKey: hashPointer(shared.Key), Kind: "CALLS_DIRECT", EvidenceKind: "type_resolved", ResolutionStatus: "exact_in_supported_scope", Source: SourceEvidence{Path: "consumer/consumer.go", ContentHash: input.Files[1].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: 7}}},
		{ID: hashOf(t, "import-forward"), SourcePackageKey: consumer.Key, TargetPackageKey: hashPointer(shared.Key), Kind: "IMPORTS", EvidenceKind: "syntax_exact", ResolutionStatus: "exact_in_supported_scope", Source: SourceEvidence{Path: "consumer/consumer.go", ContentHash: input.Files[1].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: 7}}},
		{ID: hashOf(t, "call-reverse"), SourcePackageKey: shared.Key, TargetPackageKey: hashPointer(consumer.Key), Kind: "CALLS_DIRECT", EvidenceKind: "type_resolved", ResolutionStatus: "exact_in_supported_scope", Source: SourceEvidence{Path: "shared/shared.go", ContentHash: input.Files[3].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: 7}}},
	}

	result, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Dependencies) != 3 || result.Statistics.GroupedReferenceCount != 3 {
		t.Fatalf("direction/type/evidence groups merged or dropped: %+v", result.Dependencies)
	}
	seen := make(map[model.Hash]bool, 3)
	for _, dependency := range result.Dependencies {
		items, err := result.Expand(dependency.ExpandRef)
		if err != nil || len(items) != 1 {
			t.Fatalf("group expansion did not return one exact member: refs=%+v err=%v", items, err)
		}
		item := items[0]
		if item.Kind != dependency.Kind || item.EvidenceKind != dependency.EvidenceKind || item.SourceModuleIDs[0] != dependency.FromModuleID || item.TargetModuleIDs[0] != dependency.ToModuleID {
			t.Fatalf("dependency group changed edge kind, evidence, or direction: group=%+v ref=%+v", dependency, item)
		}
		seen[item.ID] = true
	}
	if len(seen) != len(input.References) {
		t.Fatalf("group expansions lost references: got=%d want=%d", len(seen), len(input.References))
	}
}

func TestBuildOrderDoesNotChangeIDsOrSummaries(t *testing.T) {
	input := fixture()
	input.EntryPoints = []EntryPointInput{{PackageKey: input.Packages[0].Key, Kind: "function", Name: "Run",
		Source: SourceEvidence{Path: "consumer/consumer.go", ContentHash: input.Files[1].ContentHash, Range: model.ByteRange{StartByte: 22, EndByte: 25}}}}
	input.TrustedDocuments = []DocumentInput{{Title: "Project guide", TrustRef: "config:trusted-docs", Source: SourceEvidence{Path: "go.mod", ContentHash: input.Files[0].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: input.Files[0].ByteLength}}}}
	input.References = []ReferenceInput{{
		ID: hashOf(t, "reference"), SourcePackageKey: input.Packages[0].Key, TargetPackageKey: hashPointer(input.Packages[1].Key),
		Kind: "IMPORTS", EvidenceKind: "syntax_exact", ResolutionStatus: "exact_in_supported_scope",
		Source: SourceEvidence{Path: "consumer/consumer.go", ContentHash: input.Files[1].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: 7}},
	}}
	first, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	reversed := input
	reversed.Files = reverseFiles(input.Files)
	reversed.Modules = reverseModules(input.Modules)
	reversed.Packages = reversePackages(input.Packages)
	reversed.References = reverseReferences(input.References)
	second, err := Build(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("reordering equivalent inputs changed the deterministic overview")
	}
	if first.Modules[1].ParentID == nil || len(first.EntryPoints) != 1 || len(first.TrustedDocuments) != 1 || !reflect.DeepEqual(first.Languages, []string{"go"}) || !reflect.DeepEqual(first.TestRoots, []string{"consumer"}) {
		t.Fatalf("source-backed summary fields missing: %+v", first)
	}
	if first.Modules[0].ID == first.Modules[1].ID {
		t.Fatal("distinct module identities collided")
	}
}

func TestBuildRejectsBrokenHierarchyAndUnboundEvidence(t *testing.T) {
	input := fixture()
	input.Modules[0].ParentKey = "shared"
	if _, err := Build(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("module cycle should be rejected, got %v", err)
	}

	input = fixture()
	input.Modules[0].Source.ContentHash = model.Hash("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if _, err := Build(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("source hash mismatch should be rejected, got %v", err)
	}

	input = fixture()
	input.Packages[0].ModuleMemberships = []ModuleMembershipInput{membership("guessed-from-folder", input.Files[0])}
	if _, err := Build(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown module mapping should be rejected, got %v", err)
	}

	input = fixture()
	input.Packages[0].ModuleMemberships[0].Source.ContentHash = model.Hash("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if _, err := Build(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unbound module membership evidence should be rejected, got %v", err)
	}

	input = fixture()
	input.Packages[0].ModuleMemberships = nil
	input.KnownGaps = []Gap{{Code: "parser_partial", Count: 2}, {Code: "parser_partial", Count: 3}}
	result, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Gaps) != 2 || result.Gaps[0].Code != "packages_without_declared_module" || result.Gaps[1].Code != "parser_partial" || result.Gaps[1].Count != 5 {
		t.Fatalf("known gaps were not preserved and normalized: %+v", result.Gaps)
	}
}

func TestModuleIDIsStableAcrossGenerationsButScopedToProfileAndWorktree(t *testing.T) {
	input := fixture()
	input.References = []ReferenceInput{{
		ID: hashOf(t, "generation-pinned-edge"), SourcePackageKey: input.Packages[0].Key, TargetPackageKey: hashPointer(input.Packages[1].Key),
		Kind: "IMPORTS", EvidenceKind: "syntax_exact", ResolutionStatus: "exact_in_supported_scope",
		Source: SourceEvidence{Path: "consumer/consumer.go", ContentHash: input.Files[1].ContentHash, Range: model.ByteRange{StartByte: 0, EndByte: 7}},
	}}
	first, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Scope.GenerationID = model.UUID("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	second, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Modules[0].ID != second.Modules[0].ID || first.Dependencies[0].ID == second.Dependencies[0].ID {
		t.Fatalf("module identity should survive generation while dependency refs stay pinned: first=%+v second=%+v", first, second)
	}
	input.Scope.ProfileID = model.Hash("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	third, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Modules[0].ID == third.Modules[0].ID || second.WorkspaceID == third.WorkspaceID {
		t.Fatal("profile changes must create distinct workspace and module identities")
	}
	input.Scope.WorktreeID = model.UUID("ffffffff-ffff-4fff-8fff-ffffffffffff")
	fourth, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if third.Modules[0].ID == fourth.Modules[0].ID || third.WorkspaceID == fourth.WorkspaceID {
		t.Fatal("worktree changes must create distinct workspace and module identities")
	}
}

func reverseFiles(values []SourceFile) []SourceFile {
	result := append([]SourceFile(nil), values...)
	reverse(result)
	return result
}
func reverseModules(values []ModuleInput) []ModuleInput {
	result := append([]ModuleInput(nil), values...)
	reverse(result)
	return result
}
func reversePackages(values []PackageInput) []PackageInput {
	result := append([]PackageInput(nil), values...)
	reverse(result)
	return result
}
func reverseReferences(values []ReferenceInput) []ReferenceInput {
	result := append([]ReferenceInput(nil), values...)
	reverse(result)
	return result
}
func reverse[T any](values []T) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}
