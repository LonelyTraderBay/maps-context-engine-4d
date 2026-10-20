package contracts

import (
	"errors"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	testGroupID            = model.UUID("33333333-3333-4333-8333-333333333333")
	testCatalogIncarnation = model.UUID("44444444-4444-4444-8444-444444444444")
	testVectorGeneration   = model.UUID("55555555-5555-4555-8555-555555555555")
	testVectorDigest       = model.Hash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	testPolicyDigest       = model.Hash("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	testIndexIncarnation   = model.UUID("66666666-6666-4666-8666-666666666666")
	testRecordsIncarnation = model.UUID("77777777-7777-4777-8777-777777777777")
)

func TestSelectPinnedUsesServiceAndExactVectorMember(t *testing.T) {
	vector := federatedVector(t,
		scopeMember("service-a"),
		scopeMember("service-b"),
	)
	serviceID := model.Identifier("service-c")
	worktreeID := model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	profileID := model.Hash("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	vector.UnavailableMembers = []model.UnavailableMember{{
		ServiceID:  &serviceID,
		WorktreeID: &worktreeID,
		ProfileID:  &profileID,
		Status:     "unavailable",
		ReasonCode: "service_offline",
	}}
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest after adding unavailable member: %v", err)
	}
	vector.VectorDigest = digest
	selector := symbolSelector("service-b")
	selected, err := SelectPinned(vector, vector.VectorDigest, selector)
	if err != nil {
		t.Fatalf("SelectPinned: %v", err)
	}
	if selected.Vector.VectorDigest != vector.VectorDigest || selected.Member.ServiceID == nil || *selected.Member.ServiceID != "service-b" {
		t.Fatalf("selection lost pinned identity: %#v", selected)
	}
	if len(selected.Vector.UnavailableMembers) != 1 || selected.Vector.UnavailableMembers[0].ServiceID == nil || *selected.Vector.UnavailableMembers[0].ServiceID != "service-c" {
		t.Fatal("pinned scope selection dropped the expected unavailable member")
	}

	unqualified := symbolSelector("")
	if _, err := SelectPinned(vector, vector.VectorDigest, unqualified); !errors.Is(err, ErrAmbiguousMember) {
		t.Fatalf("unqualified group selection error = %v, want ambiguous member", err)
	}

	*selector.ServiceID = "service-a"
	if *selected.Selector.ServiceID != "service-b" || *selected.Member.ServiceID != "service-b" {
		t.Fatal("selection aliases mutable selector or vector service identity")
	}
	*vector.Members[1].ServiceID = "service-mutated"
	if *selected.Vector.Members[1].ServiceID != "service-b" {
		t.Fatal("pinned scope vector aliases the caller's mutable vector")
	}
	*vector.UnavailableMembers[0].ServiceID = "service-mutated"
	if *selected.Vector.UnavailableMembers[0].ServiceID != "service-c" {
		t.Fatal("pinned unavailable-member ledger aliases the caller's mutable vector")
	}
}

func TestSelectPinnedDoesNotUpgradeOrResolveOutsideVector(t *testing.T) {
	vector := federatedVector(t, scopeMember("service-a"))
	selector := symbolSelector("service-a")
	selector.Ref.GenerationID = "88888888-8888-4888-8888-888888888888"
	if _, err := SelectPinned(vector, vector.VectorDigest, selector); !errors.Is(err, ErrMemberNotFound) {
		t.Fatalf("changed generation error = %v, want not found in pinned vector", err)
	}
	selector.Ref.GenerationID = testVectorGeneration
	selector.ServiceID = ptr(model.Identifier("service-other"))
	if _, err := SelectPinned(vector, vector.VectorDigest, selector); !errors.Is(err, ErrMemberNotFound) {
		t.Fatalf("unknown service error = %v, want not found in pinned vector", err)
	}
}

func TestSelectPinnedRejectsChangedGroupRevision(t *testing.T) {
	vector := federatedVector(t, scopeMember("service-a"))
	expected := vector.VectorDigest
	newRevision := int64(8)
	vector.GroupRevision = &newRevision
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest for revised group: %v", err)
	}
	vector.VectorDigest = digest
	if err := vector.Validate(); err != nil {
		t.Fatalf("revised vector should be internally valid: %v", err)
	}
	if _, err := SelectPinned(vector, expected, symbolSelector("service-a")); !errors.Is(err, ErrPinnedScopeChanged) {
		t.Fatalf("changed vector error = %v, want pinned scope changed", err)
	}
}

func TestSelectPinnedReportsExpectedUnavailableMemberWithoutInventingGeneration(t *testing.T) {
	serviceID := model.Identifier("service-denied")
	worktreeID := model.UUID("99999999-9999-4999-8999-999999999999")
	profileID := model.Hash("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	vector := federatedVector(t)
	vector.UnavailableMembers = []model.UnavailableMember{{
		ServiceID:  &serviceID,
		WorktreeID: &worktreeID,
		ProfileID:  &profileID,
		Status:     "access_denied",
		ReasonCode: "permission_denied",
	}}
	vector.VectorDigest = ""
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest with unavailable member: %v", err)
	}
	vector.VectorDigest = digest
	selector := RefSelector{
		Kind: "ref",
		Ref: GraphRef{
			Kind:         GraphRefEntity,
			WorktreeID:   worktreeID,
			ProfileID:    profileID,
			GenerationID: testVectorGeneration,
			EntityID:     "order",
		},
		ServiceID: &serviceID,
	}
	if _, err := SelectPinned(vector, vector.VectorDigest, selector); !errors.Is(err, ErrMemberUnavailable) {
		t.Fatalf("unavailable selection error = %v, want unavailable member", err)
	}
	if vector.UnavailableMembers[0].WorktreeID == nil || vector.UnavailableMembers[0].ProfileID == nil {
		t.Fatal("fixture must retain the expected unavailable identity")
	}
}

func TestGraphRefIsClosedAndMemberQualified(t *testing.T) {
	ref := symbolSelector("service-a").Ref
	if err := ref.Validate(); err != nil {
		t.Fatalf("valid symbol ref rejected: %v", err)
	}
	ref.EntityID = "unexpected"
	if err := ref.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("mixed ref variants error = %v, want invalid contract", err)
	}
	ref = symbolSelector("service-a").Ref
	ref.GenerationID = "not-a-uuid"
	if err := ref.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("invalid generation error = %v, want invalid contract", err)
	}
}

func federatedVector(t *testing.T, members ...model.ScopeMember) model.ScopeVector {
	t.Helper()
	if members == nil {
		members = []model.ScopeMember{}
	}
	revision := int64(7)
	vector := model.ScopeVector{
		Kind:               "federated",
		Members:            members,
		GroupID:            ptr(testGroupID),
		GroupRevision:      &revision,
		CatalogIncarnation: testCatalogIncarnation,
		UnavailableMembers: []model.UnavailableMember{},
	}
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	vector.VectorDigest = digest
	if err := vector.Validate(); err != nil {
		t.Fatalf("invalid test vector: %v", err)
	}
	return vector
}

func scopeMember(service string) model.ScopeMember {
	var serviceID *model.Identifier
	if service != "" {
		serviceValue := model.Identifier(service)
		serviceID = &serviceValue
	}
	return model.ScopeMember{
		WorktreeID:         testWorktreeID,
		ProfileID:          testProfileID,
		GenerationID:       testVectorGeneration,
		SnapshotDigest:     testVectorDigest,
		PolicyDigest:       testPolicyDigest,
		PolicyEpoch:        2,
		BindingRevision:    3,
		RecordEpoch:        4,
		ServiceID:          serviceID,
		IndexIncarnation:   testIndexIncarnation,
		RecordsIncarnation: testRecordsIncarnation,
	}
}

func symbolSelector(service string) RefSelector {
	selector := RefSelector{
		Kind: "ref",
		Ref: GraphRef{
			Kind:         GraphRefSymbol,
			WorktreeID:   testWorktreeID,
			ProfileID:    testProfileID,
			GenerationID: testVectorGeneration,
			SymbolID:     "duplicate-symbol-id",
		},
	}
	if service != "" {
		selector.ServiceID = ptr(model.Identifier(service))
	}
	return selector
}
