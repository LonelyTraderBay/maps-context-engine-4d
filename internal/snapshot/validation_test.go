package snapshot

import (
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestCompareValidationRequiresFullContentAndReportsCurrent(t *testing.T) {
	manifest, vector, files := validationFixture(t)
	result, err := CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		ObservedFiles: files, ObservedComplete: true, ObservedScope: validationScope(manifest, vector),
		SourcePreconditions: []SourcePrecondition{{Path: "main.go", ContentHash: *manifest.Files[0].ContentHash}},
		CheckedAt:           time.Date(2026, 10, 17, 16, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != FreshnessObservedCurrent || !result.EditPreconditionValid || result.Freshness.ChangedCount == nil || *result.Freshness.ChangedCount != 0 {
		t.Fatalf("unexpected current result: %+v", result)
	}
	if result.Freshness.BufferScope != "disk_only" || result.Freshness.Consistency != FreshnessCapturedBytesWithObservedStability {
		t.Fatalf("freshness omitted the required observation limits: %+v", result.Freshness)
	}
}

func TestCompareValidationDetectsSameSizeSamePathByteChanges(t *testing.T) {
	manifest, vector, files := validationFixture(t)
	files[0].ContentHash = model.SHA256([]byte("package demo\n// new\n"))
	result, err := CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		ObservedFiles: files, ObservedComplete: true, ObservedScope: validationScope(manifest, vector),
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != FreshnessChanged || result.EditPreconditionValid || result.MembershipChanged || len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "main.go" {
		t.Fatalf("same-size byte change was not detected: %+v", result)
	}
}

func TestCompareValidationDetectsAddedUntrackedMembership(t *testing.T) {
	manifest, vector, files := validationFixture(t)
	newContent := []byte("package demo\nfunc Added() {}\n")
	files = append(files, WorkspaceFile{Path: "new.go", ContentHash: model.SHA256(newContent), ByteLength: int64(len(newContent))})
	result, err := CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		ObservedFiles: files, ObservedComplete: true, ObservedScope: validationScope(manifest, vector),
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != FreshnessChanged || !result.MembershipChanged || len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "new.go" {
		t.Fatalf("added path was not detected: %+v", result)
	}
}

func TestCompareValidationKeepsIncompleteCoverageUnknown(t *testing.T) {
	manifest, vector, files := validationFixture(t)
	result, err := CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		ObservedFiles: files, ObservedComplete: false, ObservedScope: validationScope(manifest, vector),
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != FreshnessUnknown || result.EditPreconditionValid || result.Freshness.ChangedCount != nil || result.ReasonCode != "WORKSPACE_COVERAGE_INCOMPLETE" {
		t.Fatalf("incomplete scan must remain unknown: %+v", result)
	}
}

func TestCompareValidationInvalidatesPinnedRecordAndGroupRevisions(t *testing.T) {
	manifest, vector, files := validationFixture(t)
	changedRecordVector := vector
	changedRecordVector.Members = append([]model.ScopeMember(nil), vector.Members...)
	changedRecordVector.Members[0].RecordEpoch++
	setVectorDigest(t, &changedRecordVector)
	result, err := CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		ObservedFiles: files, ObservedComplete: true, ObservedScope: validationScope(manifest, changedRecordVector),
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != FreshnessChanged || !result.RecordSetChanged || result.ConfigurationChanged || result.MembershipChanged {
		t.Fatalf("record epoch change was not categorized: %+v", result)
	}

	groupID, _ := model.ParseUUID("33333333-3333-4333-8333-333333333333")
	expectedGroupRevision := int64(2)
	groupVector := vector
	groupVector.Kind = "federated"
	groupVector.Members = append([]model.ScopeMember(nil), vector.Members...)
	groupVector.GroupID = &groupID
	groupVector.GroupRevision = &expectedGroupRevision
	setVectorDigest(t, &groupVector)
	observedGroupRevision := int64(3)
	changedGroupVector := groupVector
	changedGroupVector.GroupRevision = &observedGroupRevision
	setVectorDigest(t, &changedGroupVector)
	result, err = CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: groupVector,
		ObservedFiles: files, ObservedComplete: true, ObservedScope: validationScope(manifest, changedGroupVector),
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != FreshnessChanged || !result.MembershipChanged || result.RecordSetChanged {
		t.Fatalf("group revision change was not categorized: %+v", result)
	}
}

func TestCompareValidationInvalidatesProviderAndDependencyVersions(t *testing.T) {
	manifest, vector, files := validationFixture(t)
	observed := validationScope(manifest, vector)
	observed.ProviderDigest = model.SHA256([]byte("new-provider"))
	observed.DependencyInputDigests = []model.Hash{model.SHA256([]byte("new-dependency"))}
	result, err := CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		ObservedFiles: files, ObservedComplete: true, ObservedScope: observed,
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != FreshnessChanged || !result.ConfigurationChanged || result.EditPreconditionValid {
		t.Fatalf("provider/dependency version drift was not invalidated: %+v", result)
	}
}

func TestCompareValidationRejectsSourcePreconditionOutsideCapture(t *testing.T) {
	manifest, vector, files := validationFixture(t)
	_, err := CompareValidation(ValidationInput{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		ObservedFiles: files, ObservedComplete: true, ObservedScope: validationScope(manifest, vector),
		SourcePreconditions: []SourcePrecondition{{Path: "main.go", ContentHash: model.SHA256([]byte("different"))}},
		CheckedAt:           time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("mismatched source precondition was accepted")
	}
}

func validationFixture(t *testing.T) (model.CaptureManifest, model.ScopeVector, []WorkspaceFile) {
	t.Helper()
	worktreeID, _ := model.ParseUUID("11111111-1111-4111-8111-111111111111")
	profileID := model.SHA256([]byte("profile"))
	policyDigest := model.SHA256([]byte("policy"))
	providerDigest := model.SHA256([]byte("provider"))
	content := []byte("package demo\n// old\n")
	captured, err := Build(Input{
		SchemaVersion: 1, WorktreeID: worktreeID, ProfileID: profileID,
		PolicyDigest: policyDigest, ProviderDigest: providerDigest,
		DependencyInputDigests: []model.Hash{},
		Files:                  []FileInput{{Path: "main.go", Status: FileCaptured, Content: content}},
		Git:                    GitState{HeadState: HeadUnborn}, Scope: ScopeComplete,
	})
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ := model.ParseUUID("22222222-2222-4222-8222-222222222222")
	indexIncarnation, _ := model.ParseUUID("44444444-4444-4444-8444-444444444444")
	recordsIncarnation, _ := model.ParseUUID("55555555-5555-4555-8555-555555555555")
	vector := model.ScopeVector{
		Kind: "single", Members: []model.ScopeMember{{
			WorktreeID: worktreeID, ProfileID: profileID, GenerationID: generationID,
			SnapshotDigest: captured.Manifest.SnapshotDigest, PolicyDigest: policyDigest,
			PolicyEpoch: 1, BindingRevision: 1, RecordEpoch: 0,
			IndexIncarnation: indexIncarnation, RecordsIncarnation: recordsIncarnation,
		}}, GroupID: nil, GroupRevision: nil, CatalogIncarnation: indexIncarnation,
		UnavailableMembers: []model.UnavailableMember{},
	}
	setVectorDigest(t, &vector)
	return captured.Manifest, vector, []WorkspaceFile{{Path: "main.go", ContentHash: model.SHA256(content), ByteLength: int64(len(content))}}
}

func setVectorDigest(t *testing.T, vector *model.ScopeVector) {
	t.Helper()
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	vector.VectorDigest = digest
}

func validationScope(manifest model.CaptureManifest, vector model.ScopeVector) ScopeObservation {
	return ScopeObservation{
		Vector: vector, ProviderDigest: manifest.ProviderDigest,
		DependencyInputDigests: []model.Hash{}, Complete: true,
	}
}
