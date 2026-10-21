package federation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestPlanFlowQueryUsesOnlyExplicitServiceScopedBridges(t *testing.T) {
	vector := flowVector(t, false)
	root := flowSelector("svc-a", "shared-symbol")
	bridge := flowBridge("bridge-ab", "svc-a", "shared-symbol", "svc-b", "shared-symbol")
	unreachable := flowBridge("bridge-unused", "svc-b", "other-symbol", "svc-a", "other-symbol")

	plan, err := PlanFlowQuery(vector, vector.VectorDigest, root, []ContractBridge{unreachable, bridge})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Queries) != 2 || len(plan.Bridges) != 1 || plan.Bridges[0].BridgeID != "bridge-ab" {
		t.Fatalf("query plan included an implicit or unreachable link: queries=%d bridges=%#v", len(plan.Queries), plan.Bridges)
	}
	if *plan.Queries[0].Selector.ServiceID != "svc-a" || *plan.Queries[1].Selector.ServiceID != "svc-b" {
		t.Fatalf("same symbol across services lost service qualification: %#v", plan.Queries)
	}
	if plan.Bridges[0].SourceContract.Family != ContractHTTP || plan.Bridges[0].TargetContract.Family != ContractSQL ||
		plan.Bridges[0].SourceContract.Namespace == plan.Bridges[0].TargetContract.Namespace {
		t.Fatalf("bridge plan dropped cross-family namespace identity: %#v", plan.Bridges[0])
	}

	reversed, err := PlanFlowQuery(vector, vector.VectorDigest, root, []ContractBridge{bridge, unreachable})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Queries, reversed.Queries) || !reflect.DeepEqual(plan.Bridges, reversed.Bridges) {
		t.Fatal("bridge input order changed the deterministic query plan")
	}
	bridge.Evidence[0].ArtifactID = nil
	if plan.Bridges[0].Evidence[0].ArtifactID == nil {
		t.Fatal("the plan retained a mutable alias to caller-owned bridge evidence")
	}

	unqualified := flowSelector("", "shared-symbol")
	if _, err := PlanFlowQuery(vector, vector.VectorDigest, unqualified, nil); !errors.Is(err, ErrAmbiguousMember) {
		t.Fatalf("unqualified duplicate symbol error = %v, want ambiguous member", err)
	}
}

func TestPlanFlowQueryPreservesRedactedExpectedUnavailableGap(t *testing.T) {
	vector := flowVector(t, true)
	plan, err := PlanFlowQuery(vector, vector.VectorDigest, flowSelector("svc-a", "root"), []ContractBridge{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Queries) != 1 || len(plan.Gaps) != 1 {
		t.Fatalf("expected one local query and one explicit gap, got queries=%d gaps=%#v", len(plan.Queries), plan.Gaps)
	}
	gap := plan.Gaps[0]
	if gap.Status != "access_denied" || gap.ServiceID != nil || gap.WorktreeID != nil || gap.ProfileID != nil {
		t.Fatalf("denied member identity was exposed or status was lost: %#v", gap)
	}
	if plan.Vector.Members[0].GenerationID == "00000000-0000-0000-0000-000000000000" {
		t.Fatal("query plan invented a generation for an unavailable member")
	}
	if err := plan.Vector.Validate(); err != nil {
		t.Fatalf("plan did not preserve a valid pinned vector: %v", err)
	}
}

func TestPlanFlowQueryBlocksWhenEveryMemberIsUnavailable(t *testing.T) {
	vector := flowVector(t, true)
	vector.Members = []model.ScopeMember{}
	vector.VectorDigest, _ = vector.ComputeDigest()
	if err := vector.Validate(); err != nil {
		t.Fatalf("all-unavailable test vector is invalid: %v", err)
	}
	if _, err := PlanFlowQuery(vector, vector.VectorDigest, flowSelector("svc-a", "root"), []ContractBridge{}); !errors.Is(err, ErrNoAvailableMembers) {
		t.Fatalf("all-unavailable plan error = %v, want explicit no-available-members", err)
	}
}

func TestPlanFlowQueryRejectsDigestIncarnationAndBridgeIdentityMismatch(t *testing.T) {
	vector := flowVector(t, false)
	root := flowSelector("svc-a", "root")
	if _, err := PlanFlowQuery(vector, model.Hash(strings.Repeat("f", 64)), root, []ContractBridge{}); !errors.Is(err, ErrPinnedScopeChanged) {
		t.Fatalf("wrong expected digest error = %v, want pinned-scope change", err)
	}

	changed := cloneFederatedVector(vector)
	changed.CatalogIncarnation = "11111111-1111-4111-8111-111111111111"
	changed.VectorDigest, _ = changed.ComputeDigest()
	if _, err := PlanFlowQuery(changed, vector.VectorDigest, root, []ContractBridge{}); !errors.Is(err, ErrPinnedScopeChanged) {
		t.Fatalf("changed catalog incarnation error = %v, want pinned-scope change", err)
	}
	changed = cloneFederatedVector(vector)
	*changed.GroupRevision++
	changed.VectorDigest, _ = changed.ComputeDigest()
	if _, err := PlanFlowQuery(changed, vector.VectorDigest, root, []ContractBridge{}); !errors.Is(err, ErrPinnedScopeChanged) {
		t.Fatalf("changed group revision error = %v, want pinned-scope change", err)
	}

	bridge := flowBridge("bridge-ab", "svc-a", "root", "svc-b", "target")
	bridge.Target.Contract.ProfileID = model.Hash(strings.Repeat("b", 64))
	if _, err := PlanFlowQuery(vector, vector.VectorDigest, root, []ContractBridge{bridge}); !errors.Is(err, ErrBridgeScopeMismatch) {
		t.Fatalf("bridge profile mismatch error = %v, want scope mismatch", err)
	}
}

func TestPlanFlowQueryRejectsUnprovenAndStaleBridges(t *testing.T) {
	vector := flowVector(t, false)
	root := flowSelector("svc-a", "root")
	weak := flowBridge("bridge-ab", "svc-a", "root", "svc-b", "target")
	weak.Evidence[0].Kind = EvidenceTextMatch
	if _, err := PlanFlowQuery(vector, vector.VectorDigest, root, []ContractBridge{weak}); !errors.Is(err, ErrInvalidBridge) {
		t.Fatalf("text-only bridge error = %v, want invalid bridge", err)
	}
	wrongOrigin := flowBridge("bridge-origin", "svc-a", "root", "svc-b", "target")
	wrongOrigin.Evidence[0].Kind = EvidenceRuntimeObserved
	if _, err := PlanFlowQuery(vector, vector.VectorDigest, root, []ContractBridge{wrongOrigin}); !errors.Is(err, ErrInvalidBridge) {
		t.Fatalf("unsupported evidence origin error = %v, want invalid bridge", err)
	}

	stale := flowBridge("bridge-ab", "svc-a", "root", "svc-b", "target")
	stale.Target.Selector.Ref.GenerationID = "77777777-7777-4777-8777-777777777777"
	if _, err := PlanFlowQuery(vector, vector.VectorDigest, root, []ContractBridge{stale}); !errors.Is(err, ErrMemberNotFound) {
		t.Fatalf("stale member-generation error = %v, want not found in pinned vector", err)
	}
}

func TestValidateFlowPageKeepsMemberEpochAndFactProvenance(t *testing.T) {
	vector := flowVector(t, false)
	plan, err := PlanFlowQuery(vector, vector.VectorDigest, flowSelector("svc-a", "root"), []ContractBridge{})
	if err != nil {
		t.Fatal(err)
	}
	query := plan.Queries[0]
	page := MemberFlowPage{
		VectorDigest: vector.VectorDigest,
		Selector:     query.Selector,
		Member:       query.Member,
		Facts: []FlowFact{{
			ServiceID: "svc-a", Ref: flowSelector("svc-a", "node").Ref,
			Evidence: []BridgeEvidence{flowEvidence()},
		}},
	}
	if err := plan.ValidateFlowPage(page); err != nil {
		t.Fatalf("valid pinned page rejected: %v", err)
	}

	page.Member.PolicyEpoch++
	if err := plan.ValidateFlowPage(page); !errors.Is(err, ErrPinnedScopeChanged) {
		t.Fatalf("changed policy epoch error = %v, want pinned-scope change", err)
	}
	page.Member = query.Member
	page.Facts[0].Ref.GenerationID = "77777777-7777-4777-8777-777777777777"
	if err := plan.ValidateFlowPage(page); !errors.Is(err, ErrPinnedScopeChanged) {
		t.Fatalf("fact from another generation error = %v, want pinned-scope change", err)
	}
}

func TestExecuteFlowQueryChecksEachPinnedMemberIndependently(t *testing.T) {
	vector := flowVector(t, false)
	plan, err := PlanFlowQuery(vector, vector.VectorDigest, flowSelector("svc-a", "root"), []ContractBridge{
		flowBridge("bridge-ab", "svc-a", "root", "svc-b", "target"),
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier := &testPinnedScopeVerifier{}
	reader := &testMemberFlowReader{}
	result, err := ExecuteFlowQuery(context.Background(), plan, verifier, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 2 || len(result.Gaps) != 0 || result.Consistency != ConsistencyIndependentMemberReads {
		t.Fatalf("unexpected federated result: %#v", result)
	}
	if verifier.calls != 4 || reader.calls != 2 {
		t.Fatalf("member reads were not guarded independently: verifier=%d reader=%d", verifier.calls, reader.calls)
	}
}

func TestExecuteFlowQueryRetainsExpectedGap(t *testing.T) {
	vector := flowVector(t, true)
	plan, err := PlanFlowQuery(vector, vector.VectorDigest, flowSelector("svc-a", "root"), []ContractBridge{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteFlowQuery(context.Background(), plan, &testPinnedScopeVerifier{}, &testMemberFlowReader{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pages) != 1 || len(result.Gaps) != 1 || result.Gaps[0].Status != "access_denied" || result.Gaps[0].ServiceID != nil {
		t.Fatalf("expected explicit redacted gap was lost: %#v", result)
	}
}

func TestExecuteFlowQueryDiscardsPagesWhenPinnedScopeChanges(t *testing.T) {
	vector := flowVector(t, false)
	plan, err := PlanFlowQuery(vector, vector.VectorDigest, flowSelector("svc-a", "root"), []ContractBridge{
		flowBridge("bridge-ab", "svc-a", "root", "svc-b", "target"),
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier := &testPinnedScopeVerifier{failAt: 2}
	reader := &testMemberFlowReader{}
	result, err := ExecuteFlowQuery(context.Background(), plan, verifier, reader)
	if !errors.Is(err, ErrPinnedScopeChanged) {
		t.Fatalf("changed scope error = %v, want pinned-scope change", err)
	}
	if len(result.Pages) != 0 || reader.calls != 1 {
		t.Fatalf("stale partial pages escaped: pages=%d reader_calls=%d", len(result.Pages), reader.calls)
	}
}

func TestContractIdentityKeyIncludesNamespaceAndService(t *testing.T) {
	first := flowContractIdentity("svc-a")
	firstKey, err := first.Key()
	if err != nil {
		t.Fatal(err)
	}
	otherService := flowContractIdentity("svc-b")
	otherServiceKey, err := otherService.Key()
	if err != nil {
		t.Fatal(err)
	}
	otherNamespace := first
	otherNamespace.Namespace = "other-database"
	otherNamespaceKey, err := otherNamespace.Key()
	if err != nil {
		t.Fatal(err)
	}
	if firstKey == otherServiceKey || firstKey == otherNamespaceKey {
		t.Fatal("contract identity key omitted service or namespace scope")
	}
	unsafeBinding := first
	unsafeBinding.ServerBinding = "https://user:secret@example.invalid"
	if err := unsafeBinding.Validate(); !errors.Is(err, ErrInvalidBridge) {
		t.Fatalf("credential-bearing server binding error = %v, want invalid bridge", err)
	}
}

func flowVector(t *testing.T, denied bool) model.ScopeVector {
	t.Helper()
	groupID := model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	revision := int64(4)
	serviceA, serviceB := model.Identifier("svc-a"), model.Identifier("svc-b")
	vector := model.ScopeVector{
		Kind: "federated", GroupID: &groupID, GroupRevision: &revision,
		CatalogIncarnation: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
		Members: []model.ScopeMember{
			*flowScopeMember(serviceA, "dddddddd-dddd-4ddd-8ddd-dddddddddddd"),
		},
		UnavailableMembers: []model.UnavailableMember{},
	}
	if denied {
		vector.UnavailableMembers = []model.UnavailableMember{{Status: "access_denied", ReasonCode: "policy_denied"}}
	} else {
		vector.Members = append(vector.Members, *flowScopeMember(serviceB, "dddddddd-dddd-4ddd-8ddd-dddddddddddd"))
	}
	vector.VectorDigest, _ = vector.ComputeDigest()
	if err := vector.Validate(); err != nil {
		t.Fatalf("invalid test scope vector: %v", err)
	}
	return vector
}

func flowScopeMember(service model.Identifier, generation model.UUID) *model.ScopeMember {
	member := federationScopeMember(service, generation)
	member.GenerationID = generation
	return member
}

func flowSelector(service, symbol string) RefSelector {
	var serviceID *model.Identifier
	if service != "" {
		parsed := model.Identifier(service)
		serviceID = &parsed
	}
	return RefSelector{
		Kind: "ref", ServiceID: serviceID,
		Ref: GraphRef{
			Kind: GraphRefSymbol, WorktreeID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			ProfileID: model.Hash(strings.Repeat("a", 64)), GenerationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
			SymbolID: model.Identifier(symbol),
		},
	}
}

func flowContractIdentity(service string) ContractIdentity {
	return ContractIdentity{
		ServiceID: model.Identifier(service), RepositoryScope: "repository-a", Environment: "prod",
		ProfileID: model.Hash(strings.Repeat("a", 64)), Family: ContractHTTP, Protocol: "openapi_3.1.0",
		Namespace: "billing-api", ServerBinding: "billing-origin-v1", ContractID: "billing", Version: "v1",
		SourceHash: model.Hash(strings.Repeat("c", 64)),
	}
}

func flowBridge(id, sourceService, sourceSymbol, targetService, targetSymbol string) ContractBridge {
	targetContract := flowContractIdentity(targetService)
	targetContract.Family = ContractSQL
	targetContract.Protocol = "mysql"
	targetContract.Namespace = "ledger-db"
	targetContract.ServerBinding = "ledger-primary"
	targetContract.ContractID = "ledger-schema"
	return ContractBridge{
		BridgeID: model.Identifier(id), Kind: "declared_mapping",
		Source:   BridgeEndpoint{Selector: flowSelector(sourceService, sourceSymbol), Contract: flowContractIdentity(sourceService)},
		Target:   BridgeEndpoint{Selector: flowSelector(targetService, targetSymbol), Contract: targetContract},
		Evidence: []BridgeEvidence{flowEvidence()},
	}
}

func flowEvidence() BridgeEvidence {
	artifact := model.Hash(strings.Repeat("d", 64))
	return BridgeEvidence{
		EvidenceID: "bridge-artifact", Origin: EvidenceOriginStaticAnalyzer,
		Kind: EvidenceExplicitDocument, SourceRefs: []model.SourceRef{}, ArtifactID: &artifact,
	}
}

type testPinnedScopeVerifier struct {
	calls  int
	failAt int
}

func (verifier *testPinnedScopeVerifier) VerifyPinnedScope(_ context.Context, vector model.ScopeVector) error {
	verifier.calls++
	if err := vector.Validate(); err != nil {
		return err
	}
	if verifier.failAt == verifier.calls {
		return ErrPinnedScopeChanged
	}
	return nil
}

type testMemberFlowReader struct{ calls int }

func (reader *testMemberFlowReader) ReadMemberFlow(_ context.Context, selection PinnedSelection) (MemberFlowPage, error) {
	reader.calls++
	return MemberFlowPage{
		VectorDigest: selection.Vector.VectorDigest,
		Selector:     selection.Selector,
		Member:       selection.Member,
		Facts: []FlowFact{{
			ServiceID: *selection.Member.ServiceID, Ref: selection.Selector.Ref,
			Evidence: []BridgeEvidence{flowEvidence()},
		}},
	}, nil
}
