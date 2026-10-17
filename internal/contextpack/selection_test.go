package contextpack

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestSameNameCandidatesRequirePositionOrReview(t *testing.T) {
	scope, identity := testScope(t)
	pathA, pathB := "pkg/a/file.go", "pkg/b/file.go"
	hashA := model.SHA256([]byte("package a\nfunc Target() {}"))
	hashB := model.SHA256([]byte("package b\nfunc Target() {}"))
	a := testNode(identity, "symbol-a", "Target", "pkg/a.Target", pathA, hashA, 20, 30)
	b := testNode(identity, "symbol-b", "Target", "pkg/b.Target", pathB, hashB, 20, 30)
	graph := Graph{Nodes: []Node{a, b}, Complete: true}

	ambiguous, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: testRequest(
		Selector{Kind: SelectorQuery, Query: "Target", Match: MatchExact}, TaskRename,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.Status != StatusAmbiguous || ambiguous.Target != nil || len(ambiguous.Candidates) != 2 {
		t.Fatalf("same-name candidates were silently selected: %#v", ambiguous)
	}

	resolved, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: testRequest(
		Selector{Kind: SelectorPosition, Path: pathA, ContentHash: hashA, ByteOffset: 24}, TaskRename,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != StatusResolved || resolved.Target == nil || resolved.Target.ID != "symbol-a" {
		t.Fatalf("exact source position did not resolve the expected symbol: %#v", resolved)
	}

	stale, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: testRequest(
		Selector{Kind: SelectorPosition, Path: pathA, ContentHash: model.SHA256([]byte("stale")), ByteOffset: 24}, TaskRename,
	)})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Status != StatusNotFound || stale.Target != nil || stale.RequiredPending == nil || *stale.RequiredPending != 0 {
		t.Fatalf("stale content hash fell back to another selector: %#v", stale)
	}
}

func TestEqualPositionRangesStayAmbiguous(t *testing.T) {
	scope, identity := testScope(t)
	path := "pkg/a/file.go"
	hash := model.SHA256([]byte("package a\nfunc Target() {}"))
	a := testNode(identity, "symbol-a", "Target", "pkg/a.Target", path, hash, 20, 30)
	b := testNode(identity, "symbol-b", "Target", "pkg/a.Target2", path, hash, 20, 30)
	result, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{b, a}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorPosition, Path: path, ContentHash: hash, ByteOffset: 24}, TaskRename),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAmbiguous || result.Target != nil || len(result.Candidates) != 2 {
		t.Fatalf("equal source ranges must remain ambiguous: %#v", result)
	}
}

func TestCandidateCapRequiresContinuation(t *testing.T) {
	scope, identity := testScope(t)
	a := testNode(identity, "a", "Target", "pkg/a.Target", "", "", 0, 0)
	b := testNode(identity, "b", "Target", "pkg/b.Target", "", "", 0, 0)
	request := testRequest(Selector{Kind: SelectorQuery, Query: "Target", Match: MatchExact}, TaskRename)
	request.Limits.MaxCandidates = 1
	result, err := Build(context.Background(), Input{Scope: scope, Graph: Graph{Nodes: []Node{a, b}, Complete: true}, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusAmbiguous || result.Target != nil || len(result.Candidates) != 1 ||
		!result.ContinuationRequired || result.EnumerationComplete || result.RequiredPending != nil {
		t.Fatalf("candidate limit did not return a bounded ambiguous result: %#v", result)
	}
}

func TestDirectCallsRemainDistinctFromFunctionValueReferences(t *testing.T) {
	scope, identity := testScope(t)
	caller := testNode(identity, "caller", "Call", "pkg/a.Call", "", "", 0, 0)
	callee := testNode(identity, "callee", "Run", "pkg/b.Run", "", "", 0, 0)
	callback := testNode(identity, "callback", "OnDone", "pkg/c.OnDone", "", "", 0, 0)
	direct := testRelation("direct", caller.Ref, &callee.Ref, "CALLS_DIRECT", "syntax_exact", "exact_in_supported_scope")
	functionValue := testRelation("function-value", caller.Ref, &callback.Ref, "REFERS_TO", "type_resolved", "exact_in_supported_scope")
	result, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{caller, callee, callback}, Relations: []Relation{functionValue, direct}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(caller.Ref)}, TaskExplore),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.EnumerationComplete || result.RequiredPending == nil || *result.RequiredPending != 0 {
		t.Fatalf("complete supplied graph has an incorrect completion result: %#v", result)
	}
	kinds := make(map[string]int)
	for _, item := range result.Items {
		if item.RelationID != nil {
			kinds[item.RelationKind]++
		}
	}
	if kinds["CALLS_DIRECT"] != 1 || kinds["REFERS_TO"] != 1 {
		t.Fatalf("direct call and function-value reference were conflated: %#v", kinds)
	}

	candidateDirect := testRelation("candidate-direct", caller.Ref, &callee.Ref, "CALLS_DIRECT", "syntax_exact", "candidate")
	_, err = Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{caller, callee}, Relations: []Relation{candidateDirect}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(caller.Ref)}, TaskExplore),
	})
	if !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("candidate direct call accepted as an exact edge: %v", err)
	}
}

func TestFederatedScopeKeepsSymbolAndRelationIdentityScoped(t *testing.T) {
	scope, identityA, identityB := testFederatedScope(t)
	sameA := testNode(identityA, "same-symbol", "F", "pkg/a.F", "", "", 0, 0)
	sameB := testNode(identityB, "same-symbol", "F", "pkg/b.F", "", "", 0, 0)
	ambiguous, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{sameA, sameB}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorSymbol, SymbolID: "same-symbol"}, TaskExplore),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.Status != StatusAmbiguous || len(ambiguous.Candidates) != 2 || ambiguous.Target != nil {
		t.Fatalf("symbol ID was not scoped by pinned member identity: %#v", ambiguous)
	}

	hub := testNode(identityA, "hub", "Hub", "pkg/a.Hub", "", "", 0, 0)
	localTarget := testNode(identityA, "target", "Target", "pkg/a.Target", "", "", 0, 0)
	remoteSource := testNode(identityB, "source", "Source", "pkg/b.Source", "", "", 0, 0)
	sharedID := model.SHA256([]byte("same content-derived relation ID"))
	outgoing := Relation{ID: sharedID, From: hub.Ref, To: refPointer(localTarget.Ref), Kind: "CALLS_DIRECT", EvidenceKind: "type_resolved", ResolutionStatus: "exact_in_supported_scope"}
	incoming := Relation{ID: sharedID, From: remoteSource.Ref, To: refPointer(hub.Ref), Kind: "CALLS_DIRECT", EvidenceKind: "type_resolved", ResolutionStatus: "exact_in_supported_scope"}
	graphA := Graph{Nodes: []Node{remoteSource, localTarget, hub}, Relations: []Relation{incoming, outgoing}, Complete: true}
	graphB := Graph{Nodes: []Node{hub, localTarget, remoteSource}, Relations: []Relation{outgoing, incoming}, Complete: true}
	request := testRequest(Selector{Kind: SelectorRef, Ref: refPointer(hub.Ref)}, TaskExplore)
	first, err := Build(context.Background(), Input{Scope: scope, Graph: graphA, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), Input{Scope: scope, Graph: graphB, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("federated duplicate relation IDs changed with input order:\nfirst: %#v\nsecond: %#v", first, second)
	}
	var relationItems []Item
	for _, item := range first.Items {
		if item.RelationID != nil {
			relationItems = append(relationItems, item)
		}
	}
	if len(relationItems) != 2 || relationItems[0].ID == relationItems[1].ID ||
		*relationItems[0].RelationID != *relationItems[1].RelationID {
		t.Fatalf("same hash across members collapsed distinct relation facts: %#v", relationItems)
	}
	if relationItems[0].RelationFrom == nil || relationItems[1].RelationFrom == nil ||
		refKey(*relationItems[0].RelationFrom) == refKey(*relationItems[1].RelationFrom) {
		t.Fatalf("relation evidence lost its source identity: %#v", relationItems)
	}
}

func TestCyclesAndTraversalLimitsRemainExplicit(t *testing.T) {
	scope, identity := testScope(t)
	a := testNode(identity, "a", "A", "pkg.A", "", "", 0, 0)
	b := testNode(identity, "b", "B", "pkg.B", "", "", 0, 0)
	aToB := testRelation("a-b", a.Ref, &b.Ref, "CALLS_DIRECT", "type_resolved", "exact_in_supported_scope")
	bToA := testRelation("b-a", b.Ref, &a.Ref, "CALLS_DIRECT", "type_resolved", "exact_in_supported_scope")
	graph := Graph{Nodes: []Node{a, b}, Relations: []Relation{bToA, aToB}, Complete: true}
	request := testRequest(Selector{Kind: SelectorRef, Ref: refPointer(a.Ref)}, TaskExplore)
	complete, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if !complete.EnumerationComplete || complete.ContinuationRequired || countNodeItems(complete.Items) != 2 {
		t.Fatalf("cycle traversal did not terminate with unique nodes: %#v", complete)
	}

	relationLimitedRequest := testRequest(request.Selector, TaskTypeChange)
	relationLimitedRequest.Limits.MaxRelations = 1
	relationLimited, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: relationLimitedRequest})
	if err != nil {
		t.Fatal(err)
	}
	if relationLimited.EnumerationComplete || !relationLimited.ContinuationRequired || relationLimited.RequiredPending != nil ||
		!hasGap(relationLimited.Gaps, "traversal_relation_limit") ||
		!hasBoundary(relationLimited.Boundaries, "traversal_relation_limit") ||
		!hasItemKind(relationLimited.Items, "boundary") {
		t.Fatalf("relation cap hid unknown pending work: %#v", relationLimited)
	}

	nodeLimitedRequest := testRequest(request.Selector, TaskTypeChange)
	nodeLimitedRequest.Limits.MaxNodes = 1
	nodeLimited, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: nodeLimitedRequest})
	if err != nil {
		t.Fatal(err)
	}
	if nodeLimited.EnumerationComplete || !nodeLimited.ContinuationRequired || nodeLimited.RequiredPending != nil ||
		!hasGap(nodeLimited.Gaps, "traversal_node_limit") || !hasItemKind(nodeLimited.Items, "boundary") {
		t.Fatalf("node cap hid the skipped target boundary: %#v", nodeLimited)
	}
}

func TestDepthAndProviderGapsDoNotClaimCompleteness(t *testing.T) {
	scope, identity := testScope(t)
	a := testNode(identity, "a", "A", "pkg.A", "", "", 0, 0)
	b := testNode(identity, "b", "B", "pkg.B", "", "", 0, 0)
	c := testNode(identity, "c", "C", "pkg.C", "", "", 0, 0)
	aToB := testRelation("a-b", a.Ref, &b.Ref, "CALLS_DIRECT", "type_resolved", "exact_in_supported_scope")
	bToC := testRelation("b-c", b.Ref, &c.Ref, "CALLS_DIRECT", "type_resolved", "exact_in_supported_scope")
	request := testRequest(Selector{Kind: SelectorRef, Ref: refPointer(a.Ref)}, TaskExplore)
	request.Limits.MaxDepth = 1
	depthLimited, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{a, b, c}, Relations: []Relation{aToB, bToC}, Complete: true}, Request: request,
	})
	if err != nil {
		t.Fatal(err)
	}
	if depthLimited.EnumerationComplete || !depthLimited.ContinuationRequired || !hasGap(depthLimited.Gaps, "traversal_depth_limit") {
		t.Fatalf("depth cap was not visible to the caller: %#v", depthLimited)
	}

	providerGap, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{a}, Gaps: []Gap{{Code: "provider_gap", Reason: "source incomplete"}}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(a.Ref)}, TaskExplore),
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerGap.EnumerationComplete || !hasGap(providerGap.Gaps, "provider_gap") {
		t.Fatalf("upstream gap was dropped or marked complete: %#v", providerGap)
	}
}

func TestInputPermutationDoesNotChangeEvidenceOrGapOrder(t *testing.T) {
	scope, identity := testScope(t)
	a := testNode(identity, "a", "A", "pkg.A", "", "", 0, 0)
	b := testNode(identity, "b", "B", "pkg.B", "", "", 0, 0)
	sourceA := testSource(identity, "pkg/a.go", 1, 2)
	sourceB := testSource(identity, "pkg/b.go", 3, 4)
	a.SourceRefs = []model.SourceRef{sourceB, sourceA}
	b.SourceRefs = []model.SourceRef{sourceA}
	forward := testRelation("a-b", a.Ref, &b.Ref, "CALLS_DIRECT", "type_resolved", "exact_in_supported_scope")
	backward := testRelation("b-a", b.Ref, &a.Ref, "REFERS_TO", "type_resolved", "exact_in_supported_scope")
	count := 1
	firstGraph := Graph{
		Nodes: []Node{a, b}, Relations: []Relation{forward, backward},
		Gaps: []Gap{
			{Code: "provider_gap", Reason: "incomplete", SourceRefs: []model.SourceRef{sourceB, sourceA}},
			{Code: "provider_gap", Reason: "incomplete", Count: &count, SourceRefs: []model.SourceRef{sourceA}},
		}, Complete: true,
	}
	secondGraph := Graph{
		Nodes: []Node{b, a}, Relations: []Relation{backward, forward},
		Gaps: []Gap{
			{Code: "provider_gap", Reason: "incomplete", Count: &count, SourceRefs: []model.SourceRef{sourceA}},
			{Code: "provider_gap", Reason: "incomplete", SourceRefs: []model.SourceRef{sourceA, sourceB}},
		}, Complete: true,
	}
	request := testRequest(Selector{Kind: SelectorRef, Ref: refPointer(a.Ref)}, TaskExplore)
	first, err := Build(context.Background(), Input{Scope: scope, Graph: firstGraph, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(context.Background(), Input{Scope: scope, Graph: secondGraph, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("input permutation changed deterministic output:\nfirst: %#v\nsecond: %#v", first, second)
	}
}

func TestEvidenceReferenceArraysAreBounded(t *testing.T) {
	scope, identity := testScope(t)
	node := testNode(identity, "node", "F", "pkg.F", "", "", 0, 0)
	source := testSource(identity, "pkg/file.go", 0, 1)
	node.SourceRefs = make([]model.SourceRef, maxEvidenceRefs+1)
	for i := range node.SourceRefs {
		node.SourceRefs[i] = source
	}
	request := testRequest(Selector{Kind: SelectorRef, Ref: refPointer(node.Ref)}, TaskExplore)
	if _, err := Build(context.Background(), Input{Scope: scope, Graph: Graph{Nodes: []Node{node}, Complete: true}, Request: request}); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("unbounded node evidence refs accepted: %v", err)
	}

	gap := Gap{Code: "too_many_sources", Reason: "bounded gap", SourceRefs: make([]model.SourceRef, maxEvidenceRefs+1)}
	for i := range gap.SourceRefs {
		gap.SourceRefs[i] = source
	}
	graph := Graph{Nodes: []Node{testNode(identity, "node", "F", "pkg.F", "", "", 0, 0)}, Gaps: []Gap{gap}, Complete: true}
	if _, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: request}); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("unbounded gap evidence refs accepted: %v", err)
	}

	node = testNode(identity, "node", "F", "pkg.F", "", "", 0, 0)
	graph = Graph{Nodes: []Node{node}, Gaps: make([]Gap, maxGraphGaps+1), Complete: true}
	for i := range graph.Gaps {
		graph.Gaps[i] = Gap{Code: "provider_gap", Reason: "bounded gap count"}
	}
	if _, err := Build(context.Background(), Input{Scope: scope, Graph: graph, Request: request}); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("unbounded graph gap count accepted: %v", err)
	}
}

func TestSourceGapCanMatchServiceScopedGeneration(t *testing.T) {
	scope, identity := testScope(t)
	serviceID := model.Identifier("service-a")
	scope.Members[0].ServiceID = &serviceID
	var err error
	scope.VectorDigest, err = scope.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Validate(); err != nil {
		t.Fatal(err)
	}
	identity.ServiceID = &serviceID
	node := testNode(identity, "node", "F", "pkg.F", "", "", 0, 0)
	gap := Gap{Code: "provider_gap", Reason: "service-scoped gap", SourceRefs: []model.SourceRef{testSource(identity, "pkg/file.go", 0, 1)}}
	result, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{node}, Gaps: []Gap{gap}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(node.Ref)}, TaskExplore),
	})
	if err != nil {
		t.Fatalf("generation-bound source ref should not require service identity: %v", err)
	}
	if result.EnumerationComplete || !hasGap(result.Gaps, "provider_gap") {
		t.Fatalf("service-scoped provider gap was not preserved: %#v", result)
	}
}

func TestInvalidScopeRangesAndExpiredContextsFailClosed(t *testing.T) {
	scope, identity := testScope(t)
	valid := testNode(identity, "symbol", "F", "pkg.F", "", "", 0, 0)
	request := testRequest(Selector{Kind: SelectorRef, Ref: refPointer(valid.Ref)}, TaskExplore)
	outside := identity
	outside.GenerationID = "99999999-9999-4999-8999-999999999999"
	badScope := testNode(outside, "outside", "G", "pkg.G", "", "", 0, 0)
	if _, err := Build(context.Background(), Input{Scope: scope, Graph: Graph{Nodes: []Node{badScope}, Complete: true}, Request: request}); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("out-of-scope node accepted: %v", err)
	}

	badRange := valid
	badRange.SourceRefs = []model.SourceRef{testSource(identity, "pkg/file.go", -1, 1)}
	if _, err := Build(context.Background(), Input{Scope: scope, Graph: Graph{Nodes: []Node{badRange}, Complete: true}, Request: request}); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("malformed source range accepted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, Input{Scope: scope, Graph: Graph{Nodes: []Node{valid}, Complete: true}, Request: request}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request continued: %v", err)
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := Build(deadline, Input{Scope: scope, Graph: Graph{Nodes: []Node{valid}, Complete: true}, Request: request}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline continued: %v", err)
	}
}

func testScope(t *testing.T) (model.ScopeVector, ScopeIdentity) {
	t.Helper()
	member := model.ScopeMember{
		WorktreeID: "11111111-1111-4111-8111-111111111111", ProfileID: model.SHA256([]byte("profile")),
		GenerationID: "22222222-2222-4222-8222-222222222222", SnapshotDigest: model.SHA256([]byte("snapshot")),
		PolicyDigest: model.SHA256([]byte("policy")), PolicyEpoch: 1, BindingRevision: 1,
		IndexIncarnation: "33333333-3333-4333-8333-333333333333", RecordsIncarnation: "44444444-4444-4444-8444-444444444444",
	}
	scope := model.ScopeVector{
		Kind: "single", Members: []model.ScopeMember{member}, UnavailableMembers: []model.UnavailableMember{},
		CatalogIncarnation: "55555555-5555-4555-8555-555555555555",
	}
	var err error
	scope.VectorDigest, err = scope.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Validate(); err != nil {
		t.Fatal(err)
	}
	return scope, ScopeIdentity{WorktreeID: member.WorktreeID, ProfileID: member.ProfileID, GenerationID: member.GenerationID}
}

func testFederatedScope(t *testing.T) (model.ScopeVector, ScopeIdentity, ScopeIdentity) {
	t.Helper()
	scope, identityA := testScope(t)
	memberA := scope.Members[0]
	memberB := memberA
	memberB.WorktreeID = "66666666-6666-4666-8666-666666666666"
	memberB.GenerationID = "77777777-7777-4777-8777-777777777777"
	memberB.IndexIncarnation = "88888888-8888-4888-8888-888888888888"
	memberB.RecordsIncarnation = "99999999-9999-4999-8999-999999999999"
	groupID, groupRevision := model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), int64(1)
	scope.Kind, scope.Members = "federated", []model.ScopeMember{memberA, memberB}
	scope.GroupID, scope.GroupRevision = &groupID, &groupRevision
	var err error
	scope.VectorDigest, err = scope.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Validate(); err != nil {
		t.Fatal(err)
	}
	identityB := identityA
	identityB.WorktreeID, identityB.GenerationID = memberB.WorktreeID, memberB.GenerationID
	return scope, identityA, identityB
}

func testNode(identity ScopeIdentity, id, name, qualified, path string, hash model.Hash, start, end int64) Node {
	node := Node{
		Ref: Ref{Kind: RefSymbol, Scope: identity, ID: id}, Kind: "function", Name: name,
		Label: qualified, QualifiedName: qualified, Plane: "observed_static",
	}
	if path != "" {
		node.SourceRefs = []model.SourceRef{testSource(identity, path, start, end)}
		node.SourceRefs[0].ContentHash = hash
	}
	return node
}

func testSource(identity ScopeIdentity, path string, start, end int64) model.SourceRef {
	return model.SourceRef{
		WorktreeID: identity.WorktreeID, ProfileID: identity.ProfileID, GenerationID: identity.GenerationID,
		Path: path, ContentHash: model.SHA256([]byte(path)), Range: model.ByteRange{StartByte: start, EndByte: end},
	}
}

func testRelation(id string, from Ref, to *Ref, kind, evidence, resolution string) Relation {
	return Relation{
		ID: model.SHA256([]byte(id)), From: cloneRef(from), To: cloneRefPointer(to),
		Kind: kind, EvidenceKind: evidence, ResolutionStatus: resolution,
	}
}

func testRequest(selector Selector, task TaskKind) Request {
	return Request{
		Selector: selector, TaskKind: task,
		Limits: Limits{MaxGraphNodes: 100, MaxGraphRelations: 200, MaxCandidates: 100, MaxNodes: 100, MaxRelations: 200, MaxDepth: 8},
	}
}

func refPointer(ref Ref) *Ref { return &ref }

func countNodeItems(items []Item) int {
	count := 0
	for _, item := range items {
		if item.Ref != nil {
			count++
		}
	}
	return count
}

func hasGap(gaps []Gap, code string) bool {
	for _, gap := range gaps {
		if gap.Code == code {
			return true
		}
	}
	return false
}

func hasItemKind(items []Item, kind string) bool {
	for _, item := range items {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func hasBoundary(boundaries []Boundary, code string) bool {
	for _, boundary := range boundaries {
		if boundary.Code == code {
			return true
		}
	}
	return false
}
