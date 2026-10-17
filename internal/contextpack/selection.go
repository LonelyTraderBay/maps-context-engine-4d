// Package contextpack selects deterministic, source-backed context from one
// pinned graph snapshot. It does not read source files or choose a live scope.
package contextpack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrInvalidRequest = errors.New("invalid context selection request")
	ErrInvalidGraph   = errors.New("invalid context graph")
)

const (
	maxEvidenceRefs = 16
	maxGraphGaps    = 100000
)

type RefKind string

const (
	RefSymbol RefKind = "symbol"
	RefEntity RefKind = "entity"
	RefModule RefKind = "module"
	RefAnchor RefKind = "anchor"
	RefFile   RefKind = "file"
)

type ScopeIdentity struct {
	WorktreeID   model.UUID
	ProfileID    model.Hash
	GenerationID model.UUID
	ServiceID    *model.Identifier
}

type Ref struct {
	Kind        RefKind
	Scope       ScopeIdentity
	ID          string
	Path        string
	ContentHash *model.Hash
}

type SelectorKind string

const (
	SelectorSymbol   SelectorKind = "symbol"
	SelectorRef      SelectorKind = "ref"
	SelectorPosition SelectorKind = "source_position"
	SelectorQuery    SelectorKind = "query"
)

type MatchMode string

const (
	MatchExact  MatchMode = "exact"
	MatchPrefix MatchMode = "prefix"
)

type Selector struct {
	Kind        SelectorKind
	SymbolID    string
	Ref         *Ref
	Path        string
	ContentHash model.Hash
	ByteOffset  int64
	Query       string
	Match       MatchMode
}

type TaskKind string

const (
	TaskExplore    TaskKind = "explore"
	TaskPlan       TaskKind = "plan"
	TaskRename     TaskKind = "rename"
	TaskTypeChange TaskKind = "type_change"
	TaskBehavior   TaskKind = "behavior_change"
	TaskContract   TaskKind = "contract_change"
	TaskDebug      TaskKind = "debug"
)

type Node struct {
	Ref           Ref
	Kind          string
	Name          string
	Label         string
	QualifiedName string
	Plane         string
	SourceRefs    []model.SourceRef
}

type Relation struct {
	ID               model.Hash
	From             Ref
	To               *Ref
	Kind             string
	EvidenceKind     string
	ResolutionStatus string
	SourceRefs       []model.SourceRef
	Reason           string
	UnresolvedLabel  *string
}

type Gap struct {
	Code       string
	Reason     string
	Count      *int
	SourceRefs []model.SourceRef
}

type Graph struct {
	Nodes     []Node
	Relations []Relation
	Gaps      []Gap
	// Complete covers the supplied supported graph view, not runtime behavior.
	Complete bool
}

type Limits struct {
	MaxGraphNodes     int
	MaxGraphRelations int
	MaxCandidates     int
	MaxNodes          int
	MaxRelations      int
	MaxDepth          int
}

type Request struct {
	Selector Selector
	TaskKind TaskKind
	Limits   Limits
}

type Input struct {
	Scope   model.ScopeVector
	Graph   Graph
	Request Request
}

type SelectionStatus string

const (
	StatusResolved  SelectionStatus = "resolved"
	StatusAmbiguous SelectionStatus = "ambiguous"
	StatusNotFound  SelectionStatus = "not_found"
	StatusUnknown   SelectionStatus = "unknown"
)

type Item struct {
	ID                model.Hash
	Kind              string
	Ref               *Ref
	RelationID        *model.Hash
	RelationFrom      *Ref
	RelationTo        *Ref
	RelationKind      string
	Depth             int
	RequiredForReview bool
	WhyIncluded       string
	SourceRefs        []model.SourceRef
}

type Boundary struct {
	RelationID   model.Hash
	RelationFrom Ref
	RelationTo   *Ref
	Code         string
	Depth        int
	SourceRefs   []model.SourceRef
}

type Result struct {
	Status               SelectionStatus
	Target               *Ref
	Candidates           []Ref
	Items                []Item
	Boundaries           []Boundary
	Gaps                 []Gap
	EnumerationComplete  bool
	ContinuationRequired bool
	// RequiredPending is nil when the bounded input cannot determine the count.
	RequiredPending *int
}

type indexedGraph struct {
	nodes    map[string]Node
	outgoing map[string][]Relation
	incoming map[string][]Relation
	scope    model.ScopeVector
}

type visit struct {
	key   string
	depth int
}

// Build validates a supplied, pinned graph view, resolves its selector, then
// walks only task-relevant relations with explicit finite limits.
func Build(ctx context.Context, input Input) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: nil context", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := input.Scope.Validate(); err != nil {
		return Result{}, fmt.Errorf("%w: scope vector: %v", ErrInvalidRequest, err)
	}
	if err := validateRequest(input.Request); err != nil {
		return Result{}, err
	}
	graph, err := indexGraph(ctx, input.Scope, input.Graph, input.Request.Limits)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Status: StatusNotFound, Candidates: make([]Ref, 0), Items: make([]Item, 0),
		Boundaries: make([]Boundary, 0), Gaps: cloneGaps(input.Graph.Gaps),
		EnumerationComplete: input.Graph.Complete && len(input.Scope.UnavailableMembers) == 0 && len(input.Graph.Gaps) == 0,
	}
	if len(input.Scope.UnavailableMembers) > 0 {
		result.Gaps = append(result.Gaps, Gap{
			Code: "scope_members_unavailable", Reason: "one or more pinned scope members are unavailable",
			Count: intPointer(len(input.Scope.UnavailableMembers)),
		})
		result.EnumerationComplete = false
	}
	if !input.Graph.Complete {
		result.Gaps = append(result.Gaps, Gap{
			Code: "graph_enumeration_incomplete", Reason: "the supplied graph view is not fully enumerated",
		})
		result.EnumerationComplete = false
	}
	if result.EnumerationComplete {
		result.RequiredPending = intPointer(0)
	}

	candidates, truncated, err := resolveSelector(ctx, graph, input.Request.Selector, input.Request.Limits.MaxCandidates)
	if err != nil {
		return Result{}, err
	}
	result.Candidates = refsFromNodes(candidates)
	if truncated {
		result.ContinuationRequired = true
		result.EnumerationComplete = false
		result.RequiredPending = nil
		result.Gaps = append(result.Gaps, Gap{
			Code: "target_candidates_truncated", Reason: "selector candidates exceed the bounded result; continue enumeration",
		})
	}
	if len(candidates) == 0 {
		if !result.EnumerationComplete {
			result.Status = StatusUnknown
		}
		result.Gaps = normalizeGaps(result.Gaps)
		return result, nil
	}
	if len(candidates) != 1 || truncated {
		result.Status = StatusAmbiguous
		result.Gaps = normalizeGaps(result.Gaps)
		return result, nil
	}

	target := cloneRef(candidates[0].Ref)
	result.Target = &target
	result.Status = StatusResolved
	if err := walkGraph(ctx, graph, input.Request, candidates[0], &result); err != nil {
		return Result{}, err
	}
	result.Gaps = normalizeGaps(result.Gaps)
	sort.Slice(result.Boundaries, func(i, j int) bool {
		left, right := result.Boundaries[i], result.Boundaries[j]
		if left.Depth != right.Depth {
			return left.Depth < right.Depth
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if leftFrom, rightFrom := refKey(left.RelationFrom), refKey(right.RelationFrom); leftFrom != rightFrom {
			return leftFrom < rightFrom
		}
		return left.RelationID < right.RelationID
	})
	sort.Slice(result.Items, func(i, j int) bool {
		left, right := result.Items[i], result.Items[j]
		if left.Depth != right.Depth {
			return left.Depth < right.Depth
		}
		if left.RequiredForReview != right.RequiredForReview {
			return left.RequiredForReview
		}
		leftKey, rightKey := itemSortKey(left), itemSortKey(right)
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		return left.ID < right.ID
	})
	if len(result.Gaps) > 0 || !input.Graph.Complete || len(input.Scope.UnavailableMembers) > 0 {
		result.EnumerationComplete = false
	}
	if result.EnumerationComplete && !result.ContinuationRequired {
		result.RequiredPending = intPointer(0)
	}
	return result, nil
}

func validateRequest(request Request) error {
	if !validTaskKind(request.TaskKind) {
		return fmt.Errorf("%w: unsupported task kind %q", ErrInvalidRequest, request.TaskKind)
	}
	if err := validateSelector(request.Selector); err != nil {
		return err
	}
	limits := request.Limits
	if limits.MaxGraphNodes < 1 || limits.MaxGraphNodes > 100000 ||
		limits.MaxGraphRelations < 1 || limits.MaxGraphRelations > 200000 ||
		limits.MaxCandidates < 1 || limits.MaxCandidates > limits.MaxGraphNodes ||
		limits.MaxNodes < 1 || limits.MaxNodes > limits.MaxGraphNodes ||
		limits.MaxRelations < 1 || limits.MaxRelations > limits.MaxGraphRelations ||
		limits.MaxDepth < 1 || limits.MaxDepth > 64 {
		return fmt.Errorf("%w: graph and traversal limits must be finite and internally consistent", ErrInvalidRequest)
	}
	return nil
}

func validateSelector(selector Selector) error {
	switch selector.Kind {
	case SelectorSymbol:
		if !validID(selector.SymbolID) || selector.Ref != nil || selector.Path != "" || selector.Query != "" {
			return fmt.Errorf("%w: symbol selector requires one symbol ID", ErrInvalidRequest)
		}
	case SelectorRef:
		if selector.Ref == nil || !validID(selector.Ref.ID) && selector.Ref.Kind != RefFile ||
			selector.SymbolID != "" || selector.Path != "" || selector.Query != "" {
			return fmt.Errorf("%w: ref selector requires one typed ref", ErrInvalidRequest)
		}
		if err := validateRef(*selector.Ref); err != nil {
			return fmt.Errorf("%w: invalid ref selector: %v", ErrInvalidRequest, err)
		}
	case SelectorPosition:
		if selector.Ref != nil || selector.SymbolID != "" || selector.Query != "" ||
			model.ValidateRelativePath(selector.Path) != nil ||
			selector.ByteOffset < 0 || selector.ByteOffset > model.MaxSafeInteger {
			return fmt.Errorf("%w: source-position selector requires path, content hash, and a safe byte offset", ErrInvalidRequest)
		}
		if _, err := model.ParseHash(string(selector.ContentHash)); err != nil {
			return fmt.Errorf("%w: source-position selector has an invalid content hash", ErrInvalidRequest)
		}
	case SelectorQuery:
		if selector.Ref != nil || selector.SymbolID != "" || selector.Path != "" ||
			!validQuery(selector.Query) || (selector.Match != MatchExact && selector.Match != MatchPrefix) {
			return fmt.Errorf("%w: query selector requires a bounded exact or prefix query", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unsupported selector kind %q", ErrInvalidRequest, selector.Kind)
	}
	return nil
}

func indexGraph(ctx context.Context, scope model.ScopeVector, graph Graph, limits Limits) (indexedGraph, error) {
	if len(graph.Nodes) > limits.MaxGraphNodes || len(graph.Relations) > limits.MaxGraphRelations ||
		len(graph.Gaps) > maxGraphGaps {
		return indexedGraph{}, fmt.Errorf("%w: supplied graph exceeds the frozen input limits", ErrInvalidGraph)
	}
	for _, gap := range graph.Gaps {
		if err := validateGap(scope, gap); err != nil {
			return indexedGraph{}, err
		}
	}
	index := indexedGraph{
		nodes:    make(map[string]Node, len(graph.Nodes)),
		outgoing: make(map[string][]Relation), incoming: make(map[string][]Relation),
		scope: scope,
	}
	for _, node := range graph.Nodes {
		if err := ctx.Err(); err != nil {
			return indexedGraph{}, err
		}
		if err := validateNode(scope, node); err != nil {
			return indexedGraph{}, err
		}
		key := refKey(node.Ref)
		if _, exists := index.nodes[key]; exists {
			return indexedGraph{}, fmt.Errorf("%w: duplicate node identity", ErrInvalidGraph)
		}
		index.nodes[key] = cloneNode(node)
	}
	seenRelations := make(map[string]struct{}, len(graph.Relations))
	for _, relation := range graph.Relations {
		if err := ctx.Err(); err != nil {
			return indexedGraph{}, err
		}
		if err := validateRelation(scope, relation); err != nil {
			return indexedGraph{}, err
		}
		relationIdentity := relationKey(relation)
		if _, exists := seenRelations[relationIdentity]; exists {
			return indexedGraph{}, fmt.Errorf("%w: duplicate relation identity %s", ErrInvalidGraph, relationIdentity)
		}
		seenRelations[relationIdentity] = struct{}{}
		fromKey := refKey(relation.From)
		if _, exists := index.nodes[fromKey]; !exists {
			return indexedGraph{}, fmt.Errorf("%w: relation source is absent from the supplied graph", ErrInvalidGraph)
		}
		if relation.To != nil {
			toKey := refKey(*relation.To)
			_, targetLoaded := index.nodes[toKey]
			targetInScope := scopeContains(scope, relation.To.Scope)
			if !targetInScope && relation.ResolutionStatus == "exact_in_supported_scope" {
				return indexedGraph{}, fmt.Errorf("%w: exact relation target is outside the pinned scope", ErrInvalidGraph)
			}
			if !targetLoaded && targetInScope && relation.ResolutionStatus == "exact_in_supported_scope" {
				return indexedGraph{}, fmt.Errorf("%w: exact relation target is missing from the supplied graph", ErrInvalidGraph)
			}
			if targetLoaded {
				index.incoming[toKey] = append(index.incoming[toKey], cloneRelation(relation))
			}
		}
		index.outgoing[fromKey] = append(index.outgoing[fromKey], cloneRelation(relation))
	}
	for key := range index.outgoing {
		sort.Slice(index.outgoing[key], func(i, j int) bool {
			return relationKey(index.outgoing[key][i]) < relationKey(index.outgoing[key][j])
		})
	}
	for key := range index.incoming {
		sort.Slice(index.incoming[key], func(i, j int) bool {
			return relationKey(index.incoming[key][i]) < relationKey(index.incoming[key][j])
		})
	}
	return index, nil
}

func resolveSelector(ctx context.Context, graph indexedGraph, selector Selector, maxCandidates int) ([]Node, bool, error) {
	matches := make([]Node, 0)
	for _, node := range graph.nodes {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		match := false
		switch selector.Kind {
		case SelectorSymbol:
			match = node.Ref.Kind == RefSymbol && node.Ref.ID == selector.SymbolID
		case SelectorRef:
			match = refKey(node.Ref) == refKey(*selector.Ref)
		case SelectorPosition:
			match = node.Ref.Kind == RefSymbol && containsPosition(node, selector.Path, selector.ContentHash, selector.ByteOffset)
		case SelectorQuery:
			match = selectorMatches(node, selector)
		}
		if match {
			matches = append(matches, cloneNode(node))
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if selector.Kind == SelectorPosition {
			leftRange, leftOK := smallestMatchingRange(matches[i], selector.Path, selector.ContentHash, selector.ByteOffset)
			rightRange, rightOK := smallestMatchingRange(matches[j], selector.Path, selector.ContentHash, selector.ByteOffset)
			if leftOK && rightOK && leftRange != rightRange {
				return leftRange < rightRange
			}
		}
		return refKey(matches[i].Ref) < refKey(matches[j].Ref)
	})
	if selector.Kind == SelectorPosition && len(matches) > 1 {
		best, ok := smallestMatchingRange(matches[0], selector.Path, selector.ContentHash, selector.ByteOffset)
		if ok {
			end := 1
			for end < len(matches) {
				next, nextOK := smallestMatchingRange(matches[end], selector.Path, selector.ContentHash, selector.ByteOffset)
				if !nextOK || next != best {
					break
				}
				end++
			}
			matches = matches[:end]
		}
	}
	if len(matches) > maxCandidates {
		return matches[:maxCandidates], true, nil
	}
	return matches, false, nil
}

func walkGraph(ctx context.Context, graph indexedGraph, request Request, target Node, result *Result) error {
	targetKey := refKey(target.Ref)
	visitedNodes := map[string]int{targetKey: 0}
	visitedRelations := make(map[string]struct{})
	queue := []visit{{key: targetKey}}
	itemIndexes := make(map[string]int)
	targetItem := nodeItem(target, 0, true, "selected_target")
	itemIndexes["node:"+targetKey] = len(result.Items)
	result.Items = append(result.Items, targetItem)

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := queue[0]
		queue = queue[1:]
		edges := append([]Relation(nil), graph.outgoing[current.key]...)
		edges = append(edges, graph.incoming[current.key]...)
		sort.Slice(edges, func(i, j int) bool { return relationKey(edges[i]) < relationKey(edges[j]) })
		seenAtNode := make(map[string]struct{}, len(edges))
		for _, relation := range edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			relationIdentity := relationKey(relation)
			if _, seen := seenAtNode[relationIdentity]; seen {
				continue
			}
			seenAtNode[relationIdentity] = struct{}{}
			if _, seen := visitedRelations[relationIdentity]; seen || !relationRelevant(request.TaskKind, relation.Kind) {
				continue
			}
			if len(visitedRelations) >= request.Limits.MaxRelations {
				markLimit(result, "traversal_relation_limit", "relevant relations exceed the frozen traversal limit")
				return nil
			}
			visitedRelations[relationIdentity] = struct{}{}
			depth := current.depth + 1
			required := current.depth == 0 && relationRequired(request.TaskKind, relation.Kind)
			relationItem := relationEvidenceItem(relation, depth, required)
			result.Items = append(result.Items, relationItem)

			neighbor, boundaryCode := relationNeighbor(graph, current.key, relation)
			if boundaryCode != "" {
				result.Boundaries = append(result.Boundaries, Boundary{
					RelationID: relation.ID, RelationFrom: cloneRef(relation.From), RelationTo: cloneRefPointer(relation.To),
					Code: boundaryCode, Depth: depth, SourceRefs: cloneSourceRefs(relation.SourceRefs),
				})
				result.Items = append(result.Items, boundaryItem(relation, depth, required, boundaryCode))
				continue
			}
			neighborKey := refKey(neighbor.Ref)
			if priorDepth, seen := visitedNodes[neighborKey]; seen {
				if required && priorDepth > 0 {
					key := "node:" + neighborKey
					if index, ok := itemIndexes[key]; ok {
						result.Items[index].RequiredForReview = true
						result.Items[index].WhyIncluded = "direct_required_evidence"
					}
				}
				continue
			}
			if depth > request.Limits.MaxDepth {
				result.Boundaries = append(result.Boundaries, Boundary{
					RelationID: relation.ID, RelationFrom: cloneRef(relation.From), RelationTo: cloneRefPointer(relation.To),
					Code: "traversal_depth_limit", Depth: depth, SourceRefs: cloneSourceRefs(relation.SourceRefs),
				})
				result.Items = append(result.Items, boundaryItem(relation, depth, required, "traversal_depth_limit"))
				markLimit(result, "traversal_depth_limit", "relevant neighbors remain beyond the frozen depth limit")
				continue
			}
			if len(visitedNodes) >= request.Limits.MaxNodes {
				result.Boundaries = append(result.Boundaries, Boundary{
					RelationID: relation.ID, RelationFrom: cloneRef(relation.From), RelationTo: cloneRefPointer(relation.To),
					Code: "traversal_node_limit", Depth: depth, SourceRefs: cloneSourceRefs(relation.SourceRefs),
				})
				result.Items = append(result.Items, boundaryItem(relation, depth, required, "traversal_node_limit"))
				markLimit(result, "traversal_node_limit", "relevant neighbors exceed the frozen node limit")
				continue
			}
			visitedNodes[neighborKey] = depth
			queue = append(queue, visit{key: neighborKey, depth: depth})
			nodeItem := nodeItem(neighbor, depth, required, chooseNodeWhy(required, depth))
			itemIndexes["node:"+neighborKey] = len(result.Items)
			result.Items = append(result.Items, nodeItem)
		}
	}
	return nil
}

func relationNeighbor(graph indexedGraph, currentKey string, relation Relation) (Node, string) {
	if relation.To == nil {
		return Node{}, "unresolved_relation"
	}
	fromKey, toKey := refKey(relation.From), refKey(*relation.To)
	if fromKey == currentKey {
		if !scopeContains(graph.scope, relation.To.Scope) {
			return Node{}, "out_of_scope_target"
		}
		node, ok := graph.nodes[toKey]
		if !ok {
			return Node{}, "target_not_enumerated"
		}
		return node, ""
	}
	if toKey == currentKey {
		if !scopeContains(graph.scope, relation.From.Scope) {
			return Node{}, "out_of_scope_source"
		}
		node, ok := graph.nodes[fromKey]
		if !ok {
			return Node{}, "source_not_enumerated"
		}
		return node, ""
	}
	return Node{}, "relation_not_adjacent"
}

func relationRelevant(task TaskKind, kind string) bool {
	switch task {
	case TaskExplore, TaskPlan:
		return true
	case TaskRename:
		return oneOf(kind, "CONTAINS", "REFERS_TO", "CALLS_INTERFACE", "MAY_DISPATCH_TO",
			"UNRESOLVED_REF", "UNRESOLVED_CALL", "TEST_REF", "DOC_LINK", "IMPORTS")
	case TaskTypeChange:
		return oneOf(kind, "REFERS_TO", "READS", "WRITES", "CALLS_DIRECT", "CALLS_INTERFACE",
			"MAY_DISPATCH_TO", "UNRESOLVED_REF", "UNRESOLVED_CALL", "TEST_REF")
	case TaskBehavior, TaskDebug:
		return oneOf(kind, "CALLS_DIRECT", "CALLS_INTERFACE", "MAY_DISPATCH_TO", "READS",
			"WRITES", "UNRESOLVED_CALL", "UNRESOLVED_REF", "TEST_REF", "REFERS_TO")
	case TaskContract:
		return oneOf(kind, "IMPORTS", "REFERS_TO", "UNRESOLVED_REF", "UNRESOLVED_CALL",
			"TEST_REF", "DOC_LINK")
	default:
		return false
	}
}

func relationRequired(task TaskKind, kind string) bool {
	switch task {
	case TaskRename:
		return oneOf(kind, "REFERS_TO", "CALLS_INTERFACE", "MAY_DISPATCH_TO",
			"UNRESOLVED_REF", "UNRESOLVED_CALL", "TEST_REF")
	case TaskTypeChange:
		return oneOf(kind, "REFERS_TO", "READS", "WRITES", "CALLS_DIRECT",
			"CALLS_INTERFACE", "MAY_DISPATCH_TO", "UNRESOLVED_REF", "UNRESOLVED_CALL", "TEST_REF")
	case TaskBehavior, TaskDebug:
		return oneOf(kind, "CALLS_DIRECT", "CALLS_INTERFACE", "MAY_DISPATCH_TO",
			"READS", "WRITES", "UNRESOLVED_CALL", "TEST_REF")
	case TaskContract:
		return oneOf(kind, "IMPORTS", "REFERS_TO", "UNRESOLVED_REF", "UNRESOLVED_CALL",
			"TEST_REF", "DOC_LINK")
	default:
		return false
	}
}

func validateNode(scope model.ScopeVector, node Node) error {
	if err := validateRef(node.Ref); err != nil || !scopeContains(scope, node.Ref.Scope) {
		return fmt.Errorf("%w: node reference is invalid or outside the pinned scope", ErrInvalidGraph)
	}
	if !validID(node.Kind) || !validText(node.Label, 512) || !validText(node.QualifiedName, 1024) ||
		!oneOf(node.Plane, "intended", "observed_static", "observed_runtime", "annotation") {
		return fmt.Errorf("%w: invalid node metadata", ErrInvalidGraph)
	}
	if !validText(node.Name, 512) || len(node.SourceRefs) > maxEvidenceRefs {
		return fmt.Errorf("%w: node name or evidence references exceed the supported bounds", ErrInvalidGraph)
	}
	for _, source := range node.SourceRefs {
		if err := validateSourceRef(source); err != nil || !sameSourceScope(source, node.Ref.Scope) {
			return fmt.Errorf("%w: node source ref is invalid or outside its scope", ErrInvalidGraph)
		}
		if node.Ref.Kind == RefFile && (source.Path != node.Ref.Path ||
			(node.Ref.ContentHash != nil && source.ContentHash != *node.Ref.ContentHash)) {
			return fmt.Errorf("%w: file node source ref does not match its identity", ErrInvalidGraph)
		}
	}
	return nil
}

func validateRelation(scope model.ScopeVector, relation Relation) error {
	if _, err := model.ParseHash(string(relation.ID)); err != nil ||
		validateRef(relation.From) != nil || !scopeContains(scope, relation.From.Scope) ||
		!validRelationKind(relation.Kind) || !validEvidenceKind(relation.EvidenceKind) ||
		!oneOf(relation.ResolutionStatus, "exact_in_supported_scope", "candidate", "unresolved") ||
		!validText(relation.Reason, 1024) {
		return fmt.Errorf("%w: invalid relation identity or metadata", ErrInvalidGraph)
	}
	if relation.To != nil && validateRef(*relation.To) != nil {
		return fmt.Errorf("%w: invalid relation target reference", ErrInvalidGraph)
	}
	if len(relation.SourceRefs) > maxEvidenceRefs {
		return fmt.Errorf("%w: relation evidence references exceed the supported bound", ErrInvalidGraph)
	}
	for _, source := range relation.SourceRefs {
		if err := validateSourceRef(source); err != nil || !sameSourceScope(source, relation.From.Scope) {
			return fmt.Errorf("%w: relation evidence source is invalid or outside its source scope", ErrInvalidGraph)
		}
	}
	switch relation.Kind {
	case "UNRESOLVED_REF", "UNRESOLVED_CALL":
		if relation.To != nil || relation.ResolutionStatus != "unresolved" ||
			relation.EvidenceKind != "unresolved" || relation.Reason == "" ||
			relation.UnresolvedLabel == nil || !validText(*relation.UnresolvedLabel, 512) {
			return fmt.Errorf("%w: unresolved relation requires explicit boundary evidence", ErrInvalidGraph)
		}
	case "CALLS_DIRECT":
		if relation.To == nil || relation.ResolutionStatus != "exact_in_supported_scope" ||
			!oneOf(relation.EvidenceKind, "syntax_exact", "type_resolved") {
			return fmt.Errorf("%w: direct call requires an explicit resolved target in the pinned scope", ErrInvalidGraph)
		}
	case "CALLS_INTERFACE":
		if relation.To == nil || relation.EvidenceKind != "type_resolved" ||
			relation.ResolutionStatus != "exact_in_supported_scope" {
			return fmt.Errorf("%w: interface call requires a type-resolved target", ErrInvalidGraph)
		}
	case "MAY_DISPATCH_TO":
		if relation.To == nil || relation.EvidenceKind != "type_possible" ||
			relation.ResolutionStatus != "candidate" {
			return fmt.Errorf("%w: candidate dispatch must remain type_possible", ErrInvalidGraph)
		}
	}
	return nil
}

func validateGap(scope model.ScopeVector, gap Gap) error {
	if !validID(gap.Code) || !validText(gap.Reason, 1024) || utf8.RuneCountInString(gap.Reason) < 1 ||
		len(gap.SourceRefs) > maxEvidenceRefs ||
		(gap.Count != nil && (*gap.Count < 0 || int64(*gap.Count) > model.MaxSafeInteger)) {
		return fmt.Errorf("%w: invalid gap metadata or evidence references", ErrInvalidGraph)
	}
	for _, source := range gap.SourceRefs {
		if err := validateSourceRef(source); err != nil || !sourceScopeContains(scope, source) {
			return fmt.Errorf("%w: gap source ref is invalid or outside the pinned scope", ErrInvalidGraph)
		}
	}
	return nil
}

func validateRef(ref Ref) error {
	if _, err := model.ParseUUID(string(ref.Scope.WorktreeID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(ref.Scope.ProfileID)); err != nil {
		return err
	}
	if _, err := model.ParseUUID(string(ref.Scope.GenerationID)); err != nil {
		return err
	}
	if ref.Scope.ServiceID != nil && !validID(string(*ref.Scope.ServiceID)) {
		return fmt.Errorf("invalid service ID")
	}
	switch ref.Kind {
	case RefSymbol, RefEntity, RefModule, RefAnchor:
		if !validID(ref.ID) || ref.Path != "" || ref.ContentHash != nil {
			return fmt.Errorf("invalid typed reference")
		}
	case RefFile:
		if ref.ID != "" || model.ValidateRelativePath(ref.Path) != nil {
			return fmt.Errorf("invalid file reference")
		}
		if ref.ContentHash != nil {
			if _, err := model.ParseHash(string(*ref.ContentHash)); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown reference kind")
	}
	return nil
}

func validateSourceRef(ref model.SourceRef) error {
	if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
		return err
	}
	if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(ref.ContentHash)); err != nil {
		return err
	}
	if err := model.ValidateRelativePath(ref.Path); err != nil {
		return err
	}
	if ref.Range.StartByte < 0 || ref.Range.EndByte < ref.Range.StartByte ||
		ref.Range.EndByte > model.MaxSafeInteger {
		return fmt.Errorf("invalid source range")
	}
	return nil
}

func scopeContains(scope model.ScopeVector, identity ScopeIdentity) bool {
	for _, member := range scope.Members {
		if member.WorktreeID == identity.WorktreeID && member.ProfileID == identity.ProfileID &&
			member.GenerationID == identity.GenerationID && optionalIdentifierEqual(member.ServiceID, identity.ServiceID) {
			return true
		}
	}
	return false
}

func sameSourceScope(source model.SourceRef, identity ScopeIdentity) bool {
	return source.WorktreeID == identity.WorktreeID && source.ProfileID == identity.ProfileID &&
		source.GenerationID == identity.GenerationID
}

func sourceScopeContains(scope model.ScopeVector, source model.SourceRef) bool {
	for _, member := range scope.Members {
		if source.WorktreeID == member.WorktreeID && source.ProfileID == member.ProfileID &&
			source.GenerationID == member.GenerationID {
			return true
		}
	}
	return false
}

func containsPosition(node Node, path string, hash model.Hash, offset int64) bool {
	for _, source := range node.SourceRefs {
		if source.Path == path && source.ContentHash == hash &&
			source.Range.StartByte <= offset && offset < source.Range.EndByte {
			return true
		}
	}
	return false
}

func smallestMatchingRange(node Node, path string, hash model.Hash, offset int64) (int64, bool) {
	var smallest int64
	found := false
	for _, source := range node.SourceRefs {
		if source.Path != path || source.ContentHash != hash ||
			source.Range.StartByte > offset || offset >= source.Range.EndByte {
			continue
		}
		size := source.Range.EndByte - source.Range.StartByte
		if !found || size < smallest {
			smallest, found = size, true
		}
	}
	return smallest, found
}

func selectorMatches(node Node, selector Selector) bool {
	values := []string{node.Name, node.QualifiedName, node.Label}
	for _, value := range values {
		switch selector.Match {
		case MatchExact:
			if value == selector.Query {
				return true
			}
		case MatchPrefix:
			if strings.HasPrefix(value, selector.Query) {
				return true
			}
		}
	}
	return false
}

func nodeItem(node Node, depth int, required bool, why string) Item {
	ref := cloneRef(node.Ref)
	kind := "overview"
	if len(node.SourceRefs) > 0 {
		kind = "source_range"
	}
	return Item{
		ID: stableItemID("node", refKey(node.Ref)), Kind: kind, Ref: &ref,
		Depth: depth, RequiredForReview: required, WhyIncluded: why,
		SourceRefs: cloneSourceRefs(node.SourceRefs),
	}
}

func relationEvidenceItem(relation Relation, depth int, required bool) Item {
	id := relation.ID
	from := cloneRef(relation.From)
	return Item{
		ID: stableItemID("relation", relationKey(relation)), Kind: "reference_catalog",
		RelationID: &id, RelationFrom: &from, RelationTo: cloneRefPointer(relation.To),
		RelationKind: relation.Kind, Depth: depth,
		RequiredForReview: required, WhyIncluded: chooseRelationWhy(required, depth),
		SourceRefs: cloneSourceRefs(relation.SourceRefs),
	}
}

func boundaryItem(relation Relation, depth int, required bool, code string) Item {
	id := relation.ID
	from := cloneRef(relation.From)
	return Item{
		ID: stableItemID("boundary", relationKey(relation)+":"+code), Kind: "boundary",
		RelationID: &id, RelationFrom: &from, RelationTo: cloneRefPointer(relation.To),
		RelationKind: relation.Kind, Depth: depth,
		RequiredForReview: required, WhyIncluded: "scope_or_resolution_boundary",
		SourceRefs: cloneSourceRefs(relation.SourceRefs),
	}
}

func markLimit(result *Result, code, reason string) {
	result.EnumerationComplete = false
	result.ContinuationRequired = true
	result.RequiredPending = nil
	result.Gaps = append(result.Gaps, Gap{Code: code, Reason: reason})
}

func chooseNodeWhy(required bool, depth int) string {
	if required {
		return "direct_required_evidence"
	}
	if depth == 1 {
		return "direct_related_evidence"
	}
	return "bounded_transitive_neighbor"
}

func chooseRelationWhy(required bool, depth int) string {
	if required {
		return "direct_required_relation"
	}
	if depth == 1 {
		return "direct_related_relation"
	}
	return "bounded_transitive_relation"
}

func itemSortKey(item Item) string {
	ref := ""
	if item.Ref != nil {
		ref = refKey(*item.Ref)
	}
	relation := ""
	if item.RelationID != nil {
		relation = string(*item.RelationID)
	}
	from, to := "", ""
	if item.RelationFrom != nil {
		from = refKey(*item.RelationFrom)
	}
	if item.RelationTo != nil {
		to = refKey(*item.RelationTo)
	}
	return item.Kind + "\x00" + ref + "\x00" + item.RelationKind + "\x00" + from + "\x00" + to + "\x00" + relation
}

func stableItemID(kind, identity string) model.Hash {
	return model.SHA256([]byte("context-evidence-item-v1\x00" + kind + "\x00" + identity))
}

func refKey(ref Ref) string {
	serviceID := ""
	if ref.Scope.ServiceID != nil {
		serviceID = string(*ref.Scope.ServiceID)
	}
	hash := ""
	if ref.ContentHash != nil {
		hash = string(*ref.ContentHash)
	}
	return strings.Join([]string{
		string(ref.Kind), string(ref.Scope.WorktreeID), string(ref.Scope.ProfileID),
		string(ref.Scope.GenerationID), serviceID, ref.ID, ref.Path, hash,
	}, "\x00")
}

func relationKey(relation Relation) string {
	return refKey(relation.From) + "\x00" + string(relation.ID)
}

func refsFromNodes(nodes []Node) []Ref {
	refs := make([]Ref, len(nodes))
	for i, node := range nodes {
		refs[i] = cloneRef(node.Ref)
	}
	return refs
}

func cloneNode(node Node) Node {
	node.Ref = cloneRef(node.Ref)
	node.SourceRefs = cloneSourceRefs(node.SourceRefs)
	return node
}

func cloneRelation(relation Relation) Relation {
	relation.From = cloneRef(relation.From)
	relation.To = cloneRefPointer(relation.To)
	relation.SourceRefs = cloneSourceRefs(relation.SourceRefs)
	if relation.UnresolvedLabel != nil {
		value := *relation.UnresolvedLabel
		relation.UnresolvedLabel = &value
	}
	return relation
}

func cloneRef(ref Ref) Ref {
	if ref.Scope.ServiceID != nil {
		value := *ref.Scope.ServiceID
		ref.Scope.ServiceID = &value
	}
	if ref.ContentHash != nil {
		value := *ref.ContentHash
		ref.ContentHash = &value
	}
	return ref
}

func cloneRefPointer(ref *Ref) *Ref {
	if ref == nil {
		return nil
	}
	copy := cloneRef(*ref)
	return &copy
}

func cloneSourceRefs(refs []model.SourceRef) []model.SourceRef {
	if refs == nil {
		return nil
	}
	cloned := append([]model.SourceRef(nil), refs...)
	sort.Slice(cloned, func(i, j int) bool { return sourceRefLess(cloned[i], cloned[j]) })
	return cloned
}

func cloneGaps(gaps []Gap) []Gap {
	if gaps == nil {
		return make([]Gap, 0)
	}
	result := make([]Gap, len(gaps))
	for i, gap := range gaps {
		result[i] = gap
		result[i].SourceRefs = cloneSourceRefs(gap.SourceRefs)
		if gap.Count != nil {
			count := *gap.Count
			result[i].Count = &count
		}
	}
	return result
}

func normalizeGaps(gaps []Gap) []Gap {
	for i := range gaps {
		gaps[i].SourceRefs = cloneSourceRefs(gaps[i].SourceRefs)
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].Code != gaps[j].Code {
			return gaps[i].Code < gaps[j].Code
		}
		if gaps[i].Reason != gaps[j].Reason {
			return gaps[i].Reason < gaps[j].Reason
		}
		if gaps[i].Count == nil || gaps[j].Count == nil {
			return gaps[i].Count == nil && gaps[j].Count != nil
		}
		if *gaps[i].Count != *gaps[j].Count {
			return *gaps[i].Count < *gaps[j].Count
		}
		for index := 0; index < len(gaps[i].SourceRefs) && index < len(gaps[j].SourceRefs); index++ {
			left, right := gaps[i].SourceRefs[index], gaps[j].SourceRefs[index]
			if sourceRefLess(left, right) {
				return true
			}
			if sourceRefLess(right, left) {
				return false
			}
		}
		return len(gaps[i].SourceRefs) < len(gaps[j].SourceRefs)
	})
	return gaps
}

func sourceRefLess(left, right model.SourceRef) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.Range.StartByte != right.Range.StartByte {
		return left.Range.StartByte < right.Range.StartByte
	}
	if left.Range.EndByte != right.Range.EndByte {
		return left.Range.EndByte < right.Range.EndByte
	}
	if left.ContentHash != right.ContentHash {
		return left.ContentHash < right.ContentHash
	}
	if left.WorktreeID != right.WorktreeID {
		return left.WorktreeID < right.WorktreeID
	}
	if left.ProfileID != right.ProfileID {
		return left.ProfileID < right.ProfileID
	}
	return left.GenerationID < right.GenerationID
}

func optionalIdentifierEqual(left, right *model.Identifier) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func validID(value string) bool {
	if value == "" || len(value) > 192 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validText(value string, maxRunes int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= maxRunes
}

func validQuery(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 1 &&
		utf8.RuneCountInString(value) <= 512
}

func validTaskKind(value TaskKind) bool {
	switch value {
	case TaskExplore, TaskPlan, TaskRename, TaskTypeChange, TaskBehavior, TaskContract, TaskDebug:
		return true
	default:
		return false
	}
}

func validRelationKind(value string) bool {
	return oneOf(value, "CONTAINS", "IMPORTS", "REFERS_TO", "READS", "WRITES",
		"CALLS_DIRECT", "CALLS_INTERFACE", "MAY_DISPATCH_TO", "UNRESOLVED_REF",
		"UNRESOLVED_CALL", "TEST_REF", "DOC_LINK")
}

func validEvidenceKind(value string) bool {
	return oneOf(value, "syntax_exact", "type_resolved", "type_possible",
		"explicit_document", "text_match", "unresolved")
}

func oneOf(value string, values ...string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func intPointer(value int) *int {
	return &value
}
