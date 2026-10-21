package federation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrInvalidBridge       = errors.New("invalid federated contract bridge")
	ErrBridgeScopeMismatch = errors.New("bridge identity does not match its pinned member")
	ErrNoAvailableMembers  = errors.New("pinned federated scope has no available members")
	ErrPinnedScopeChanged  = errors.New("pinned scope vector changed")
	ErrAmbiguousMember     = errors.New("federated reference is ambiguous")
	ErrMemberUnavailable   = errors.New("pinned scope member is unavailable")
	ErrMemberNotFound      = errors.New("reference does not belong to the pinned scope")
)

// BridgeEndpoint joins one explicitly selected graph reference to its source
// contract identity. RefSelector supplies the immutable service/worktree/
// profile/generation binding.
type BridgeEndpoint struct {
	Selector RefSelector      `json:"selector"`
	Contract ContractIdentity `json:"contract"`
}

type ContractBridge struct {
	BridgeID model.Identifier `json:"bridge_id"`
	Kind     model.Identifier `json:"kind"`
	Source   BridgeEndpoint   `json:"source"`
	Target   BridgeEndpoint   `json:"target"`
	Evidence []BridgeEvidence `json:"evidence"`
}

type PinnedBridge struct {
	BridgeID       model.Identifier
	Kind           model.Identifier
	Source         PinnedSelection
	SourceContract ContractIdentity
	Target         PinnedSelection
	TargetContract ContractIdentity
	Evidence       []BridgeEvidence
}

type FlowQueryPlan struct {
	Vector       model.ScopeVector
	VectorDigest model.Hash
	Root         PinnedSelection
	Queries      []PinnedSelection
	Bridges      []PinnedBridge
	Gaps         []model.UnavailableMember
}

const ConsistencyIndependentMemberReads = "independent_member_reads"

type FederatedFlowResult struct {
	Vector       model.ScopeVector         `json:"vector"`
	VectorDigest model.Hash                `json:"vector_digest"`
	Pages        []MemberFlowPage          `json:"pages"`
	Gaps         []model.UnavailableMember `json:"gaps"`
	Consistency  string                    `json:"consistency"`
}

// PinnedScopeVerifier rechecks group revision, catalog/store incarnations,
// policy and member epochs using short independent reads. It must return
// ErrPinnedScopeChanged when any pinned identity no longer matches.
type PinnedScopeVerifier interface {
	VerifyPinnedScope(context.Context, model.ScopeVector) error
}

// MemberFlowReader authorizes and reads one exact member selection. It must
// recheck the selected generation and current member policy, must not follow
// an active/latest pointer, and must not consult another member's database.
type MemberFlowReader interface {
	ReadMemberFlow(context.Context, PinnedSelection) (MemberFlowPage, error)
}

// PlanFlowQuery turns explicit bridge records with acceptable evidence
// references into member-local query selections. Evidence references are
// shape-checked here; their authorization and durable resolution belong to
// the trusted store adapter. No store is joined and no member is inferred
// from a matching name, path, topic, or route.
func PlanFlowQuery(vector model.ScopeVector, expectedDigest model.Hash, root RefSelector, bridges []ContractBridge) (FlowQueryPlan, error) {
	if err := vector.Validate(); err != nil {
		return FlowQueryPlan{}, fmt.Errorf("%w: invalid pinned scope vector: %v", ErrInvalidBridge, err)
	}
	if _, err := model.ParseHash(string(expectedDigest)); err != nil {
		return FlowQueryPlan{}, fmt.Errorf("%w: invalid expected scope digest: %v", ErrInvalidBridge, err)
	}
	if vector.VectorDigest != expectedDigest {
		return FlowQueryPlan{}, ErrPinnedScopeChanged
	}
	if err := root.Validate(); err != nil {
		return FlowQueryPlan{}, err
	}
	if vector.Kind != "federated" {
		return FlowQueryPlan{}, fmt.Errorf("%w: federated flow query requires a group scope", ErrInvalidBridge)
	}
	if len(vector.Members) == 0 {
		return FlowQueryPlan{}, ErrNoAvailableMembers
	}
	rootSelection, err := selectPinned(vector, expectedDigest, root)
	if err != nil {
		return FlowQueryPlan{}, err
	}
	if bridges == nil {
		return FlowQueryPlan{}, fmt.Errorf("%w: bridges must be an explicit array", ErrInvalidBridge)
	}

	ordered := append([]ContractBridge(nil), bridges...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].BridgeID < ordered[j].BridgeID })
	resolved := make([]keyedBridge, 0, len(ordered))
	seenIDs := make(map[model.Identifier]struct{}, len(ordered))
	for _, bridge := range ordered {
		if _, exists := seenIDs[bridge.BridgeID]; exists {
			return FlowQueryPlan{}, fmt.Errorf("%w: duplicate bridge_id", ErrInvalidBridge)
		}
		seenIDs[bridge.BridgeID] = struct{}{}
		if err := validateBridgeShape(bridge); err != nil {
			return FlowQueryPlan{}, err
		}
		source, err := selectBridgeEndpoint(vector, expectedDigest, bridge.Source)
		if err != nil {
			return FlowQueryPlan{}, fmt.Errorf("bridge %s source: %w", bridge.BridgeID, err)
		}
		target, err := selectBridgeEndpoint(vector, expectedDigest, bridge.Target)
		if err != nil {
			return FlowQueryPlan{}, fmt.Errorf("bridge %s target: %w", bridge.BridgeID, err)
		}
		pinnedBridge := PinnedBridge{
			BridgeID: bridge.BridgeID, Kind: bridge.Kind,
			Source: source, SourceContract: bridge.Source.Contract,
			Target: target, TargetContract: bridge.Target.Contract,
			Evidence: cloneEvidence(bridge.Evidence),
		}
		resolved = append(resolved, keyedBridge{
			bridge: pinnedBridge,
			source: selectorKey(source.Selector),
			target: selectorKey(target.Selector),
		})
	}

	plan := FlowQueryPlan{
		Vector: cloneFederatedVector(vector), VectorDigest: expectedDigest,
		Root: clonePinnedSelection(rootSelection), Queries: []PinnedSelection{clonePinnedSelection(rootSelection)},
		Bridges: []PinnedBridge{}, Gaps: cloneUnavailableMembers(vector.UnavailableMembers),
	}
	rootKey := selectorKey(rootSelection.Selector)
	queryKeys := map[string]struct{}{rootKey: {}}
	visited := map[string]struct{}{rootKey: {}}
	queue := []string{rootKey}
	outgoing := make(map[string][]keyedBridge, len(resolved))
	for _, bridge := range resolved {
		outgoing[bridge.source] = append(outgoing[bridge.source], bridge)
	}
	for position := 0; position < len(queue); position++ {
		for _, bridge := range outgoing[queue[position]] {
			plan.Bridges = append(plan.Bridges, clonePinnedBridge(bridge.bridge))
			if _, exists := queryKeys[bridge.target]; !exists {
				queryKeys[bridge.target] = struct{}{}
				plan.Queries = append(plan.Queries, clonePinnedSelection(bridge.bridge.Target))
			}
			if _, exists := visited[bridge.target]; !exists {
				visited[bridge.target] = struct{}{}
				queue = append(queue, bridge.target)
			}
		}
	}
	return plan, nil
}

// ValidateFlowPage checks that data returned by a member-local reader belongs
// to one exact planned selector and the unchanged vector/member snapshot.
func (plan FlowQueryPlan) ValidateFlowPage(page MemberFlowPage) error {
	if err := validateFlowPlan(plan); err != nil {
		return err
	}
	return plan.validateFlowPage(page)
}

func (plan FlowQueryPlan) validateFlowPage(page MemberFlowPage) error {
	if err := plan.Vector.Validate(); err != nil || plan.VectorDigest != plan.Vector.VectorDigest {
		return ErrPinnedScopeChanged
	}
	if page.VectorDigest != plan.VectorDigest {
		return ErrPinnedScopeChanged
	}
	for _, query := range plan.Queries {
		pinned, err := selectPinned(plan.Vector, plan.VectorDigest, query.Selector)
		if err != nil || !sameScopeMember(pinned.Member, query.Member) {
			return ErrPinnedScopeChanged
		}
		if !reflect.DeepEqual(query.Selector, page.Selector) {
			continue
		}
		if !sameScopeMember(query.Member, page.Member) {
			return ErrPinnedScopeChanged
		}
		if page.Facts == nil {
			return fmt.Errorf("%w: flow facts must be an array", ErrInvalidBridge)
		}
		for _, fact := range page.Facts {
			if err := fact.Ref.Validate(); err != nil {
				return fmt.Errorf("%w: invalid flow fact ref: %v", ErrInvalidBridge, err)
			}
			if fact.Ref.WorktreeID != query.Member.WorktreeID || fact.Ref.ProfileID != query.Member.ProfileID || fact.Ref.GenerationID != query.Member.GenerationID || fact.ServiceID != *query.Member.ServiceID {
				return ErrPinnedScopeChanged
			}
			if fact.Evidence == nil || len(fact.Evidence) == 0 {
				return fmt.Errorf("%w: flow fact requires evidence", ErrInvalidBridge)
			}
			for _, evidence := range fact.Evidence {
				if err := evidence.Validate(); err != nil {
					return fmt.Errorf("%w: invalid flow evidence: %v", ErrInvalidBridge, err)
				}
			}
		}
		return nil
	}
	return ErrMemberNotFound
}

// ExecuteFlowQuery reads each planned member independently and discards every
// page if a pre/post scope check detects change. The result explicitly retains
// expected gaps and does not claim a distributed atomic snapshot.
func ExecuteFlowQuery(ctx context.Context, plan FlowQueryPlan, verifier PinnedScopeVerifier, reader MemberFlowReader) (FederatedFlowResult, error) {
	if ctx == nil || verifier == nil || reader == nil {
		return FederatedFlowResult{}, fmt.Errorf("flow context, pinned scope verifier, and member reader are required")
	}
	plan = cloneFlowQueryPlan(plan)
	if err := validateFlowPlan(plan); err != nil {
		return FederatedFlowResult{}, err
	}
	pages := make([]MemberFlowPage, 0, len(plan.Queries))
	for _, query := range plan.Queries {
		if err := ctx.Err(); err != nil {
			return FederatedFlowResult{}, err
		}
		if err := verifier.VerifyPinnedScope(ctx, cloneFederatedVector(plan.Vector)); err != nil {
			return FederatedFlowResult{}, fmt.Errorf("verify pinned scope before member read: %w", err)
		}
		page, err := reader.ReadMemberFlow(ctx, clonePinnedSelection(query))
		if err != nil {
			return FederatedFlowResult{}, fmt.Errorf("read pinned member flow: %w", err)
		}
		if err := plan.validateFlowPage(page); err != nil {
			return FederatedFlowResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return FederatedFlowResult{}, err
		}
		if err := verifier.VerifyPinnedScope(ctx, cloneFederatedVector(plan.Vector)); err != nil {
			return FederatedFlowResult{}, fmt.Errorf("verify pinned scope after member read: %w", err)
		}
		pages = append(pages, cloneFlowPage(page))
	}
	return FederatedFlowResult{
		Vector: cloneFederatedVector(plan.Vector), VectorDigest: plan.VectorDigest,
		Pages: pages, Gaps: cloneUnavailableMembers(plan.Gaps),
		Consistency: ConsistencyIndependentMemberReads,
	}, nil
}

func validateFlowPlan(plan FlowQueryPlan) error {
	if err := plan.Vector.Validate(); err != nil || plan.Vector.Kind != "federated" || plan.VectorDigest != plan.Vector.VectorDigest {
		return ErrPinnedScopeChanged
	}
	if len(plan.Queries) == 0 || plan.Gaps == nil || plan.Bridges == nil || !reflect.DeepEqual(plan.Gaps, plan.Vector.UnavailableMembers) {
		return fmt.Errorf("%w: flow plan arrays are required", ErrInvalidBridge)
	}
	if !reflect.DeepEqual(plan.Root, plan.Queries[0]) {
		return fmt.Errorf("%w: root selection does not match the first member query", ErrInvalidBridge)
	}
	queryKeys := make(map[string]struct{}, len(plan.Queries))
	for _, query := range plan.Queries {
		pinned, err := selectPinned(plan.Vector, plan.VectorDigest, query.Selector)
		if err != nil || !sameScopeMember(pinned.Member, query.Member) {
			return ErrPinnedScopeChanged
		}
		key := selectorKey(query.Selector)
		if _, exists := queryKeys[key]; exists {
			return fmt.Errorf("%w: duplicate member query selector", ErrInvalidBridge)
		}
		queryKeys[key] = struct{}{}
	}
	adjacency := make(map[string][]string, len(plan.Bridges))
	seenBridgeIDs := make(map[model.Identifier]struct{}, len(plan.Bridges))
	for _, bridge := range plan.Bridges {
		if !validFederationLabel(string(bridge.BridgeID)) || !validFederationLabel(string(bridge.Kind)) || !hasSelection(plan.Queries, bridge.Source) || !hasSelection(plan.Queries, bridge.Target) {
			return fmt.Errorf("%w: bridge plan is not bound to requested member queries", ErrInvalidBridge)
		}
		if _, exists := seenBridgeIDs[bridge.BridgeID]; exists {
			return fmt.Errorf("%w: duplicate bridge_id in plan", ErrInvalidBridge)
		}
		seenBridgeIDs[bridge.BridgeID] = struct{}{}
		if err := bridge.SourceContract.Validate(); err != nil || bridge.SourceContract.ServiceID != *bridge.Source.Member.ServiceID || bridge.SourceContract.ProfileID != bridge.Source.Member.ProfileID {
			return ErrBridgeScopeMismatch
		}
		if err := bridge.TargetContract.Validate(); err != nil || bridge.TargetContract.ServiceID != *bridge.Target.Member.ServiceID || bridge.TargetContract.ProfileID != bridge.Target.Member.ProfileID {
			return ErrBridgeScopeMismatch
		}
		if err := validateBridgeEvidence(bridge.Evidence); err != nil {
			return err
		}
		adjacency[selectorKey(bridge.Source.Selector)] = append(adjacency[selectorKey(bridge.Source.Selector)], selectorKey(bridge.Target.Selector))
	}
	rootKey := selectorKey(plan.Root.Selector)
	reachable := map[string]struct{}{rootKey: {}}
	queue := []string{rootKey}
	for position := 0; position < len(queue); position++ {
		for _, target := range adjacency[queue[position]] {
			if _, exists := reachable[target]; exists {
				continue
			}
			reachable[target] = struct{}{}
			queue = append(queue, target)
		}
	}
	for key := range queryKeys {
		if _, exists := reachable[key]; !exists {
			return fmt.Errorf("%w: member query is not reachable through explicit bridges", ErrInvalidBridge)
		}
	}
	return nil
}

type FlowFact struct {
	ServiceID model.Identifier `json:"service_id"`
	Ref       GraphRef         `json:"ref"`
	Evidence  []BridgeEvidence `json:"evidence"`
}

type MemberFlowPage struct {
	VectorDigest model.Hash        `json:"vector_digest"`
	Selector     RefSelector       `json:"selector"`
	Member       model.ScopeMember `json:"member"`
	Facts        []FlowFact        `json:"facts"`
}

func selectBridgeEndpoint(vector model.ScopeVector, expectedDigest model.Hash, endpoint BridgeEndpoint) (PinnedSelection, error) {
	selection, err := selectPinned(vector, expectedDigest, endpoint.Selector)
	if err != nil {
		return PinnedSelection{}, err
	}
	if err := endpoint.Contract.Validate(); err != nil {
		return PinnedSelection{}, err
	}
	if endpoint.Contract.ServiceID != *selection.Member.ServiceID || endpoint.Contract.ProfileID != selection.Member.ProfileID {
		return PinnedSelection{}, ErrBridgeScopeMismatch
	}
	return selection, nil
}

func validateBridgeShape(bridge ContractBridge) error {
	if !validFederationLabel(string(bridge.BridgeID)) || !validFederationLabel(string(bridge.Kind)) {
		return fmt.Errorf("%w: bridge_id and kind are required", ErrInvalidBridge)
	}
	return validateBridgeEvidence(bridge.Evidence)
}

func validateBridgeEvidence(evidenceRefs []BridgeEvidence) error {
	if len(evidenceRefs) == 0 {
		return fmt.Errorf("%w: bridge evidence is required", ErrInvalidBridge)
	}
	for _, evidence := range evidenceRefs {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("%w: invalid evidence: %v", ErrInvalidBridge, err)
		}
		switch evidence.Kind {
		case EvidenceSyntaxExact, EvidenceTypeResolved, EvidenceExplicitDocument,
			EvidenceBuildAnchored, EvidenceRuntimeObserved, EvidenceManualDeclared:
		default:
			return fmt.Errorf("%w: bridge evidence is not exact or explicitly approved", ErrInvalidBridge)
		}
		if !validEvidenceOrigin(evidence) {
			return fmt.Errorf("%w: bridge evidence origin does not support its claim", ErrInvalidBridge)
		}
	}
	return nil
}

func hasSelection(queries []PinnedSelection, wanted PinnedSelection) bool {
	for _, query := range queries {
		if reflect.DeepEqual(query.Selector, wanted.Selector) && reflect.DeepEqual(query.Vector, wanted.Vector) && sameScopeMember(query.Member, wanted.Member) {
			return true
		}
	}
	return false
}

func cloneFlowPage(page MemberFlowPage) MemberFlowPage {
	page.Selector = cloneFlowSelector(page.Selector)
	page.Member = cloneScopeMember(page.Member)
	page.Facts = append([]FlowFact(nil), page.Facts...)
	if page.Facts == nil {
		page.Facts = []FlowFact{}
	}
	for index := range page.Facts {
		if page.Facts[index].Ref.ContentHash != nil {
			hash := *page.Facts[index].Ref.ContentHash
			page.Facts[index].Ref.ContentHash = &hash
		}
		page.Facts[index].Evidence = cloneEvidence(page.Facts[index].Evidence)
	}
	return page
}

func cloneFlowSelector(selector RefSelector) RefSelector {
	if selector.ServiceID != nil {
		serviceID := *selector.ServiceID
		selector.ServiceID = &serviceID
	}
	if selector.Ref.ContentHash != nil {
		hash := *selector.Ref.ContentHash
		selector.Ref.ContentHash = &hash
	}
	return selector
}

func cloneScopeMember(member model.ScopeMember) model.ScopeMember {
	if member.ServiceID != nil {
		serviceID := *member.ServiceID
		member.ServiceID = &serviceID
	}
	return member
}

func clonePinnedSelection(selection PinnedSelection) PinnedSelection {
	selection.Vector = cloneFederatedVector(selection.Vector)
	selection.Selector = cloneFlowSelector(selection.Selector)
	selection.Member = cloneScopeMember(selection.Member)
	return selection
}

func clonePinnedBridge(bridge PinnedBridge) PinnedBridge {
	bridge.Source = clonePinnedSelection(bridge.Source)
	bridge.Target = clonePinnedSelection(bridge.Target)
	bridge.Evidence = cloneEvidence(bridge.Evidence)
	return bridge
}

func cloneFlowQueryPlan(plan FlowQueryPlan) FlowQueryPlan {
	plan.Vector = cloneFederatedVector(plan.Vector)
	plan.Root = clonePinnedSelection(plan.Root)
	plan.Queries = append([]PinnedSelection(nil), plan.Queries...)
	if plan.Queries == nil {
		plan.Queries = []PinnedSelection{}
	}
	for index := range plan.Queries {
		plan.Queries[index] = clonePinnedSelection(plan.Queries[index])
	}
	plan.Bridges = append([]PinnedBridge(nil), plan.Bridges...)
	if plan.Bridges == nil {
		plan.Bridges = []PinnedBridge{}
	}
	for index := range plan.Bridges {
		plan.Bridges[index] = clonePinnedBridge(plan.Bridges[index])
	}
	plan.Gaps = cloneUnavailableMembers(plan.Gaps)
	return plan
}

type keyedBridge struct {
	bridge PinnedBridge
	source string
	target string
}

func selectorKey(selector RefSelector) string {
	var builder strings.Builder
	appendKeyPart := func(value string) {
		builder.WriteString(strconv.Itoa(len(value)))
		builder.WriteByte(':')
		builder.WriteString(value)
	}
	ref := selector.Ref
	appendKeyPart(selector.Kind)
	appendKeyPart(string(ref.Kind))
	appendKeyPart(string(ref.WorktreeID))
	appendKeyPart(string(ref.ProfileID))
	appendKeyPart(string(ref.GenerationID))
	appendKeyPart(string(ref.SymbolID))
	appendKeyPart(string(ref.EntityID))
	appendKeyPart(string(ref.ModuleID))
	appendKeyPart(ref.Path)
	appendKeyPart(optionalHashKey(ref.ContentHash))
	appendKeyPart(string(ref.BuildMapID))
	appendKeyPart(string(ref.AnchorID))
	appendKeyPart(string(ref.BindingID))
	if selector.ServiceID == nil {
		appendKeyPart("service:nil")
	} else {
		appendKeyPart("service:value")
		appendKeyPart(string(*selector.ServiceID))
	}
	return builder.String()
}

func optionalHashKey(value *model.Hash) string {
	if value == nil {
		return "content_hash:nil"
	}
	return "content_hash:" + string(*value)
}

func sameScopeMember(left, right model.ScopeMember) bool {
	return left.WorktreeID == right.WorktreeID && left.ProfileID == right.ProfileID && left.GenerationID == right.GenerationID &&
		left.SnapshotDigest == right.SnapshotDigest && left.PolicyDigest == right.PolicyDigest && left.PolicyEpoch == right.PolicyEpoch &&
		left.BindingRevision == right.BindingRevision && left.RecordEpoch == right.RecordEpoch && left.IndexIncarnation == right.IndexIncarnation &&
		left.RecordsIncarnation == right.RecordsIncarnation && equalIdentifierPointer(left.ServiceID, right.ServiceID)
}

func equalIdentifierPointer(left, right *model.Identifier) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func cloneFederatedVector(vector model.ScopeVector) model.ScopeVector {
	vector.Members = append([]model.ScopeMember(nil), vector.Members...)
	if vector.Members == nil {
		vector.Members = []model.ScopeMember{}
	}
	for index := range vector.Members {
		if vector.Members[index].ServiceID != nil {
			serviceID := *vector.Members[index].ServiceID
			vector.Members[index].ServiceID = &serviceID
		}
	}
	vector.UnavailableMembers = cloneUnavailableMembers(vector.UnavailableMembers)
	if vector.UnavailableMembers == nil {
		vector.UnavailableMembers = []model.UnavailableMember{}
	}
	if vector.GroupID != nil {
		groupID := *vector.GroupID
		vector.GroupID = &groupID
	}
	if vector.GroupRevision != nil {
		revision := *vector.GroupRevision
		vector.GroupRevision = &revision
	}
	return vector
}

func cloneUnavailableMembers(members []model.UnavailableMember) []model.UnavailableMember {
	result := append([]model.UnavailableMember(nil), members...)
	if result == nil {
		result = []model.UnavailableMember{}
	}
	for index := range result {
		if result[index].ServiceID != nil {
			serviceID := *result[index].ServiceID
			result[index].ServiceID = &serviceID
		}
		if result[index].WorktreeID != nil {
			worktreeID := *result[index].WorktreeID
			result[index].WorktreeID = &worktreeID
		}
		if result[index].ProfileID != nil {
			profileID := *result[index].ProfileID
			result[index].ProfileID = &profileID
		}
	}
	return result
}
