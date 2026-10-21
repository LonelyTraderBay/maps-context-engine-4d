package history

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func diffSnapshot(generation string) *SemanticSnapshot {
	worktree, _ := model.ParseUUID("11111111-1111-4111-8111-111111111111")
	profile := model.SHA256([]byte("profile"))
	generationID, _ := model.ParseUUID(generation)
	return &SemanticSnapshot{
		Availability:   SnapshotRetained,
		Scope:          DiffScopeComplete,
		WorktreeID:     worktree,
		ProfileID:      profile,
		GenerationID:   generationID,
		SnapshotDigest: model.SHA256([]byte("snapshot:" + generation)),
		PolicyDigest:   model.SHA256([]byte("policy")),
		ProviderDigest: model.SHA256([]byte("provider")),
	}
}

func diffSource(snapshot *SemanticSnapshot, path, content string, start, end int64) model.SourceRef {
	return model.SourceRef{
		WorktreeID:   snapshot.WorktreeID,
		ProfileID:    snapshot.ProfileID,
		GenerationID: snapshot.GenerationID,
		Path:         path,
		ContentHash:  model.SHA256([]byte(content)),
		Range:        model.ByteRange{StartByte: start, EndByte: end},
	}
}

func diffSymbol(snapshot *SemanticSnapshot, id, name, qualified, signature, path, content string, start, end int64) DiffSymbol {
	return DiffSymbol{
		ID: id, Kind: "function", Name: name, QualifiedName: qualified, Owner: "pkg",
		Signature: signature, SemanticHash: model.SHA256([]byte("sem:" + name)),
		Source: diffSource(snapshot, path, content, start, end),
	}
}

func TestCompareSemanticSnapshotsUsesQualifiedAndSourceLineage(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	left.Symbols = []DiffSymbol{diffSymbol(left, "old-id", "Run", "pkg.Worker.Run", "func()", "worker.go", "func Run() {}", 0, 14)}
	right.Symbols = []DiffSymbol{diffSymbol(right, "new-id", "Run", "pkg.Worker.Run", "func()", "worker.go", "func Run() {}", 0, 14)}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Comparable || len(result.SymbolMappings) != 1 || result.SymbolMappings[0].Status != SymbolExact {
		t.Fatalf("symbol mapping = %+v, comparable=%v", result.SymbolMappings, result.Comparable)
	}
	if len(result.AddedSymbols)+len(result.RemovedSymbols)+len(result.ChangedSymbols) != 0 {
		t.Fatalf("unchanged declaration became a change: %+v", result)
	}
}

func TestCompareSemanticSnapshotsKeepsSameNameCandidatesUncertain(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	left.Symbols = []DiffSymbol{diffSymbol(left, "old", "Load", "pkg.old.Load", "", "old.go", "func Load(){}", 0, 13)}
	right.Symbols = []DiffSymbol{
		diffSymbol(right, "new-a", "Load", "pkg.a.Load", "", "a.go", "func Load(){}", 0, 13),
		diffSymbol(right, "new-b", "Load", "pkg.b.Load", "", "b.go", "func Load(){}", 0, 13),
	}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SymbolMappings) != 1 || result.SymbolMappings[0].Status != SymbolAmbiguous || !reflect.DeepEqual(result.SymbolMappings[0].Candidates, []string{"new-a", "new-b"}) {
		t.Fatalf("ambiguous candidates = %+v", result.SymbolMappings)
	}
	if len(result.AddedSymbols)+len(result.RemovedSymbols) != 0 {
		t.Fatalf("uncertain same-name declarations became add/remove: %+v", result)
	}
}

func TestCompareSemanticSnapshotsBoundsAmbiguousCandidatePreview(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	left.Symbols = []DiffSymbol{diffSymbol(left, "old", "Load", "pkg.old.Load", "", "old.go", "func Load(){}", 0, 13)}
	for index := 0; index < maxSymbolCandidatePreview+8; index++ {
		id := fmt.Sprintf("new-%02d", index)
		name := fmt.Sprintf("pkg.new.Load%d", index)
		right.Symbols = append(right.Symbols, diffSymbol(right, id, "Load", name, "", name+".go", "func Load(){}", 0, 13))
	}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SymbolMappings) != 1 {
		t.Fatalf("symbol mappings = %+v", result.SymbolMappings)
	}
	mapping := result.SymbolMappings[0]
	if mapping.Status != SymbolAmbiguous || len(mapping.Candidates) != maxSymbolCandidatePreview || !mapping.CandidatesTruncated {
		t.Fatalf("bounded candidate mapping = %+v", mapping)
	}
	if mapping.Candidates[0] != "new-00" || mapping.Candidates[len(mapping.Candidates)-1] != "new-31" {
		t.Fatalf("candidate preview is not stable and sorted: %+v", mapping.Candidates)
	}
	if len(result.AddedSymbols)+len(result.RemovedSymbols) != 0 {
		t.Fatalf("truncated ambiguity became a definite change: %+v", result)
	}
}

func TestCompareSemanticSnapshotsReportsSingleWeakCandidateAsProbable(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	left.Symbols = []DiffSymbol{diffSymbol(left, "old", "Load", "pkg.old.Load", "", "old.go", "func Load(){}", 0, 13)}
	right.Symbols = []DiffSymbol{diffSymbol(right, "new", "Load", "pkg.new.Load", "", "new.go", "func Load(){}", 0, 13)}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SymbolMappings) != 1 || result.SymbolMappings[0].Status != SymbolProbable {
		t.Fatalf("mapping = %+v", result.SymbolMappings)
	}
	if len(result.AddedSymbols)+len(result.RemovedSymbols) != 0 {
		t.Fatalf("probable mapping became a definite change: %+v", result)
	}
}

func TestCompareSemanticSnapshotsSeparatesProviderAndProfileDeltas(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	left.Symbols = []DiffSymbol{diffSymbol(left, "old", "Old", "pkg.Old", "func()", "x.go", "old", 0, 3)}
	right.ProviderDigest = model.SHA256([]byte("provider-v2"))
	right.ProfileID = model.SHA256([]byte("profile-v2"))
	newSymbol := diffSymbol(right, "new", "New", "pkg.New", "func()", "x.go", "new", 0, 3)
	newSymbol.Source.ProfileID = right.ProfileID
	right.Symbols = []DiffSymbol{newSymbol}
	oldRef := diffSource(left, "rules/policy.go", "package rules\nfunc Check() {}", 13, 30)
	left.SourceEvidence = []model.SourceRef{oldRef}
	right.SourceEvidence = []model.SourceRef{diffSource(right, "rules/policy.go", "package rules\nfunc Check() {}", 13, 30)}
	record := Version{RecordID: model.UUID("44444444-4444-4444-8444-444444444444"), Version: 1, Kind: KindConstraint,
		DeclaredStatus: StatusApproved, SourceRefs: []model.SourceRef{oldRef}, PayloadHash: model.SHA256([]byte("approved"))}
	left.Constraints, right.Constraints = []Version{record}, []Version{record}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if result.Comparable || !result.Compatibility.ProviderChanged || !result.Compatibility.ProfileChanged {
		t.Fatalf("compatibility delta = %+v, comparable=%v", result.Compatibility, result.Comparable)
	}
	if len(result.AddedSymbols)+len(result.RemovedSymbols)+len(result.ChangedSymbols) != 0 {
		t.Fatalf("provider/profile delta produced false graph changes: %+v", result)
	}
	if len(result.Constraints.Changed) != 1 || result.Constraints.Changed[0].After.EffectiveStatus != StatusNeedsReview {
		t.Fatalf("source-bound approval drift was skipped: %+v", result.Constraints)
	}
}

func TestCompareSemanticSnapshotsSuppressesAddRemoveAcrossPolicyChange(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	left.Symbols = []DiffSymbol{diffSymbol(left, "old", "Old", "pkg.Old", "func()", "old.go", "func Old(){}", 0, 13)}
	right.Symbols = []DiffSymbol{diffSymbol(right, "new", "New", "pkg.New", "func()", "new.go", "func New(){}", 0, 13)}
	right.PolicyDigest = model.SHA256([]byte("policy-v2"))

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Comparable || !result.Compatibility.PolicyChanged {
		t.Fatalf("policy delta = %+v", result.Compatibility)
	}
	if len(result.AddedSymbols)+len(result.RemovedSymbols) != 0 || len(result.SymbolMappings) != 1 || result.SymbolMappings[0].Status != SymbolProbable {
		t.Fatalf("policy change caused definite add/remove: %+v", result)
	}
}

func TestCompareSemanticSnapshotsRejectsMissingAndExpiredSnapshots(t *testing.T) {
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	if _, err := CompareSemanticSnapshots(nil, right); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("missing snapshot error = %v", err)
	} else {
		var typed *SnapshotUnavailableError
		if !errors.As(err, &typed) || typed.Side != "left" || typed.Status != SnapshotMissing {
			t.Fatalf("missing snapshot error details = %#v", err)
		}
	}
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	left.Availability = SnapshotExpired
	if _, err := CompareSemanticSnapshots(left, right); !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("expired snapshot error = %v", err)
	}
}

func TestCompareSemanticSnapshotsDoesNotInferRemovalsFromPartialScope(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	left.Scope = DiffScopeUnknown
	left.Symbols = []DiffSymbol{diffSymbol(left, "old", "Keep", "pkg.Keep", "func()", "x.go", "func Keep(){}", 0, 15)}
	right.Scope = DiffScopePartial

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RemovedSymbols) != 0 || len(result.UncertainSymbols) != 1 {
		t.Fatalf("unknown scope inferred removal: %+v", result)
	}
}

func TestCompareSemanticSnapshotsReportsSemanticChangeAndKnownConsumers(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	leftSymbol := diffSymbol(left, "old", "Run", "pkg.Worker.Run", "func()", "worker.go", "func Run() { return }", 0, 22)
	rightSymbol := diffSymbol(right, "new", "Run", "pkg.Worker.Run", "func()", "worker.go", "func Run() { panic() }", 0, 22)
	rightSymbol.SemanticHash = model.SHA256([]byte("changed-body"))
	left.Symbols, right.Symbols = []DiffSymbol{leftSymbol}, []DiffSymbol{rightSymbol}
	key := SymbolFactKey(leftSymbol)
	left.Consumers = []DiffConsumer{{ID: "consumer-api", Name: "API handler", DependencyKeys: []string{key}}}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedSymbols) != 1 || !reflect.DeepEqual(result.ChangedSymbols[0].ChangedFields, []string{"semantic_hash"}) {
		t.Fatalf("semantic changes = %+v", result.ChangedSymbols)
	}
	if len(result.AffectedConsumers) != 1 || result.AffectedConsumers[0].ID != "consumer-api" {
		t.Fatalf("affected consumers = %+v", result.AffectedConsumers)
	}
}

func TestCompareSemanticSnapshotsDiffsRelationsAndContracts(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	leftA := diffSymbol(left, "a-old", "A", "pkg.A", "func()", "a.go", "func A(){}", 0, 11)
	leftB := diffSymbol(left, "b-old", "B", "pkg.B", "func()", "b.go", "func B(){}", 0, 11)
	rightA := diffSymbol(right, "a-new", "A", "pkg.A", "func()", "a.go", "func A(){}", 0, 11)
	rightB := diffSymbol(right, "b-new", "pkg.B", "pkg.B", "func()", "b.go", "func B(){}", 0, 11)
	left.Symbols, right.Symbols = []DiffSymbol{leftA, leftB}, []DiffSymbol{rightA, rightB}
	left.Relations = []DiffRelation{{ID: "rel-old", SourceSymbolID: "a-old", TargetSymbolID: "b-old", Kind: "calls", SemanticHash: model.SHA256([]byte("declared"))}}
	right.Relations = []DiffRelation{{ID: "rel-new", SourceSymbolID: "a-new", TargetSymbolID: "b-new", Kind: "calls", SemanticHash: model.SHA256([]byte("inferred"))}}
	left.Contracts = []SemanticFact{{ID: "api-old", Key: "http/GET/items", SemanticHash: model.SHA256([]byte("schema-v1"))}}
	right.Contracts = []SemanticFact{{ID: "api-new", Key: "http/GET/items", SemanticHash: model.SHA256([]byte("schema-v2"))}}
	left.Consumers = []DiffConsumer{{ID: "consumer", Name: "consumer", DependencyKeys: []string{RelationFactKey("calls", SymbolFactKey(leftA), SymbolFactKey(leftB)), "http/GET/items"}}}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Relations.Changed) != 1 || result.Relations.Changed[0].Before.ID != "rel-old" || result.Relations.Changed[0].After.ID != "rel-new" {
		t.Fatalf("relation delta = %+v", result.Relations)
	}
	if result.Relations.Changed[0].Before.SemanticHash != model.SHA256([]byte("declared")) || result.Relations.Changed[0].After.SemanticHash != model.SHA256([]byte("inferred")) {
		t.Fatalf("relation hashes = %+v", result.Relations.Changed[0])
	}
	if len(result.Contracts.Changed) != 1 || len(result.AffectedConsumers) != 1 {
		t.Fatalf("contract/consumer delta = %+v / %+v", result.Contracts, result.AffectedConsumers)
	}
	if result.Contracts.Changed[0].Before.SemanticHash != model.SHA256([]byte("schema-v1")) || result.Contracts.Changed[0].After.SemanticHash != model.SHA256([]byte("schema-v2")) {
		t.Fatalf("contract hashes = %+v", result.Contracts.Changed[0])
	}
}

func TestCompareSemanticSnapshotsKeepsRelationsUncertainWhenEndpointsAreNotExact(t *testing.T) {
	tests := []struct {
		name  string
		right []DiffSymbol
	}{
		{
			name: "probable endpoint",
			right: func() []DiffSymbol {
				right := diffSnapshot("33333333-3333-4333-8333-333333333333")
				return []DiffSymbol{diffSymbol(right, "new", "Load", "pkg.new.Load", "", "new.go", "func Load(){}", 0, 13)}
			}(),
		},
		{
			name: "ambiguous endpoint",
			right: func() []DiffSymbol {
				right := diffSnapshot("33333333-3333-4333-8333-333333333333")
				return []DiffSymbol{
					diffSymbol(right, "new-a", "Load", "pkg.a.Load", "", "a.go", "func Load(){}", 0, 13),
					diffSymbol(right, "new-b", "Load", "pkg.b.Load", "", "b.go", "b.go", 0, 13),
				}
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left := diffSnapshot("22222222-2222-4222-8222-222222222222")
			right := diffSnapshot("33333333-3333-4333-8333-333333333333")
			left.Symbols = []DiffSymbol{diffSymbol(left, "old", "Load", "pkg.old.Load", "", "old.go", "func Load(){}", 0, 13)}
			right.Symbols = test.right
			left.Relations = []DiffRelation{{ID: "rel-old", SourceSymbolID: "old", Kind: "calls", SemanticHash: model.SHA256([]byte("edge"))}}

			result, err := CompareSemanticSnapshots(left, right)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Relations.Removed) != 0 || len(result.Relations.Added) != 0 || len(result.UncertainRelations) != 1 {
				t.Fatalf("uncertain relation became a definite change: %+v", result)
			}
		})
	}
}

func TestEvaluateDecisionStatusPreservesVersionAndRequiresExactSource(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	oldRef := diffSource(left, "rules/policy.go", "package rules\nfunc Check() {}", 13, 30)
	currentRef := diffSource(right, "rules/policy.go", "package rules\nfunc Check() {}", 13, 30)
	right.SourceEvidence = []model.SourceRef{currentRef}
	record := Version{
		RecordID: model.UUID("44444444-4444-4444-8444-444444444444"), Version: 2,
		Kind: KindConstraint, DeclaredStatus: StatusApproved, SourceRefs: []model.SourceRef{oldRef},
		PayloadHash: model.SHA256([]byte("approved")),
	}
	before := record
	result, err := EvaluateDecisionStatus(record, right)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveStatus != StatusApproved || result.ProvenanceStatus != ProvenanceRetainedSnapshot {
		t.Fatalf("stable decision = %+v", result)
	}
	if !reflect.DeepEqual(record, before) {
		t.Fatal("decision record or immutable source refs were mutated")
	}

	drifted := *right
	drifted.SourceEvidence = []model.SourceRef{diffSource(right, "rules/policy.go", "package rules\nfunc Check() { return true }", 13, 40)}
	result, err = EvaluateDecisionStatus(record, &drifted)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveStatus != StatusNeedsReview || result.ProvenanceStatus != ProvenanceSourceChanged {
		t.Fatalf("drifted decision = %+v", result)
	}
	if !reflect.DeepEqual(record, before) {
		t.Fatal("drift evaluation reanchored or mutated immutable source refs")
	}

	rangeDrift := *right
	rangeDrift.SourceEvidence = []model.SourceRef{diffSource(right, "rules/policy.go", "package rules\nfunc Check() {}", 14, 30)}
	result, err = EvaluateDecisionStatus(record, &rangeDrift)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveStatus != StatusNeedsReview || result.ProvenanceStatus != ProvenanceSourceChanged {
		t.Fatalf("range-only drift = %+v", result)
	}

	expired := *right
	expired.Availability = SnapshotExpired
	result, err = EvaluateDecisionStatus(record, &expired)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveStatus != StatusNeedsReview || result.ProvenanceStatus != ProvenanceSnapshotExpired {
		t.Fatalf("expired snapshot decision = %+v", result)
	}
}

func TestCompareSemanticSnapshotsReportsDecisionSourceDrift(t *testing.T) {
	left := diffSnapshot("22222222-2222-4222-8222-222222222222")
	right := diffSnapshot("33333333-3333-4333-8333-333333333333")
	oldRef := diffSource(left, "rules/policy.go", "package rules\nfunc Check() {}", 13, 30)
	left.SourceEvidence = []model.SourceRef{oldRef}
	right.SourceEvidence = []model.SourceRef{diffSource(right, "rules/policy.go", "package rules\nfunc Check() { return true }", 13, 40)}
	record := Version{RecordID: model.UUID("44444444-4444-4444-8444-444444444444"), Version: 2, Kind: KindConstraint,
		DeclaredStatus: StatusApproved, SourceRefs: []model.SourceRef{oldRef}, PayloadHash: model.SHA256([]byte("approved"))}
	left.Constraints, right.Constraints = []Version{record}, []Version{record}

	result, err := CompareSemanticSnapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Constraints.Changed) != 1 || result.Constraints.Changed[0].After.EffectiveStatus != StatusNeedsReview {
		t.Fatalf("constraint drift = %+v", result.Constraints)
	}
}
