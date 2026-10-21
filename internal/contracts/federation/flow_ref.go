package federation

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type GraphRefKind string

const (
	GraphRefSymbol       GraphRefKind = "symbol"
	GraphRefEntity       GraphRefKind = "entity"
	GraphRefModule       GraphRefKind = "module"
	GraphRefSourceAnchor GraphRefKind = "source_anchor"
	GraphRefFile         GraphRefKind = "file"
)

// GraphRef and RefSelector are federation-local planner ports. Their closed
// JSON shape follows the canonical graph-ref/selector variants while keeping
// the federation package from depending on its parent package.
type GraphRef struct {
	Kind         GraphRefKind     `json:"kind"`
	WorktreeID   model.UUID       `json:"worktree_id"`
	ProfileID    model.Hash       `json:"profile_id"`
	GenerationID model.UUID       `json:"generation_id"`
	SymbolID     model.Identifier `json:"symbol_id,omitempty"`
	EntityID     model.Identifier `json:"entity_id,omitempty"`
	ModuleID     model.Identifier `json:"module_id,omitempty"`
	Path         string           `json:"path,omitempty"`
	ContentHash  *model.Hash      `json:"content_hash,omitempty"`
	BuildMapID   model.Hash       `json:"build_map_id,omitempty"`
	AnchorID     model.Hash       `json:"anchor_id,omitempty"`
	BindingID    model.Hash       `json:"binding_id,omitempty"`
}

func (ref GraphRef) Validate() error {
	if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
		return fmt.Errorf("%w: ref worktree_id: %v", ErrInvalidBridge, err)
	}
	if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
		return fmt.Errorf("%w: ref profile_id: %v", ErrInvalidBridge, err)
	}
	if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
		return fmt.Errorf("%w: ref generation_id: %v", ErrInvalidBridge, err)
	}
	validID := func(value model.Identifier) bool { return validFederationLabel(string(value)) }
	switch ref.Kind {
	case GraphRefSymbol:
		if !validID(ref.SymbolID) || ref.EntityID != "" || ref.ModuleID != "" || ref.Path != "" || ref.ContentHash != nil || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: malformed symbol ref", ErrInvalidBridge)
		}
	case GraphRefEntity:
		if !validID(ref.EntityID) || ref.SymbolID != "" || ref.ModuleID != "" || ref.Path != "" || ref.ContentHash != nil || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: malformed entity ref", ErrInvalidBridge)
		}
	case GraphRefModule:
		if !validID(ref.ModuleID) || ref.SymbolID != "" || ref.EntityID != "" || ref.Path != "" || ref.ContentHash != nil || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: malformed module ref", ErrInvalidBridge)
		}
	case GraphRefSourceAnchor:
		for _, hash := range []model.Hash{ref.BuildMapID, ref.AnchorID, ref.BindingID} {
			if _, err := model.ParseHash(string(hash)); err != nil {
				return fmt.Errorf("%w: malformed source-anchor ref: %v", ErrInvalidBridge, err)
			}
		}
		if ref.SymbolID != "" || ref.EntityID != "" || ref.ModuleID != "" || ref.Path != "" || ref.ContentHash != nil {
			return fmt.Errorf("%w: source-anchor ref contains unrelated fields", ErrInvalidBridge)
		}
	case GraphRefFile:
		if err := model.ValidateRelativePath(ref.Path); err != nil {
			return fmt.Errorf("%w: ref path: %v", ErrInvalidBridge, err)
		}
		if ref.ContentHash != nil {
			if _, err := model.ParseHash(string(*ref.ContentHash)); err != nil {
				return fmt.Errorf("%w: ref content_hash: %v", ErrInvalidBridge, err)
			}
		}
		if ref.SymbolID != "" || ref.EntityID != "" || ref.ModuleID != "" || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: file ref contains unrelated fields", ErrInvalidBridge)
		}
	default:
		return fmt.Errorf("%w: unsupported graph-ref kind", ErrInvalidBridge)
	}
	return nil
}

type RefSelector struct {
	Kind      string            `json:"kind"`
	Ref       GraphRef          `json:"ref"`
	ServiceID *model.Identifier `json:"service_id,omitempty"`
}

func (selector RefSelector) Validate() error {
	if selector.Kind != "ref" {
		return fmt.Errorf("%w: selector kind must be ref", ErrInvalidBridge)
	}
	if err := selector.Ref.Validate(); err != nil {
		return err
	}
	if selector.ServiceID != nil && !validFederationLabel(string(*selector.ServiceID)) {
		return fmt.Errorf("%w: selector service_id is invalid", ErrInvalidBridge)
	}
	return nil
}

func selectPinned(vector model.ScopeVector, expectedDigest model.Hash, selector RefSelector) (PinnedSelection, error) {
	if err := vector.Validate(); err != nil {
		return PinnedSelection{}, fmt.Errorf("%w: invalid pinned scope vector: %v", ErrInvalidBridge, err)
	}
	if _, err := model.ParseHash(string(expectedDigest)); err != nil {
		return PinnedSelection{}, fmt.Errorf("%w: expected vector digest: %v", ErrInvalidBridge, err)
	}
	if vector.VectorDigest != expectedDigest {
		return PinnedSelection{}, ErrPinnedScopeChanged
	}
	if err := selector.Validate(); err != nil {
		return PinnedSelection{}, err
	}
	if vector.Kind == "federated" && selector.ServiceID == nil {
		return PinnedSelection{}, ErrAmbiguousMember
	}

	ref := selector.Ref
	matchIndex := -1
	for index := range vector.Members {
		member := &vector.Members[index]
		if member.WorktreeID != ref.WorktreeID || member.ProfileID != ref.ProfileID || member.GenerationID != ref.GenerationID {
			continue
		}
		if selector.ServiceID != nil && !sameServiceID(member.ServiceID, selector.ServiceID) {
			continue
		}
		if matchIndex >= 0 {
			return PinnedSelection{}, ErrAmbiguousMember
		}
		matchIndex = index
	}
	if matchIndex >= 0 {
		pinnedVector := cloneFederatedVector(vector)
		return PinnedSelection{
			Vector: pinnedVector, Selector: cloneFlowSelector(selector),
			Member: cloneScopeMember(pinnedVector.Members[matchIndex]),
		}, nil
	}
	for _, unavailable := range vector.UnavailableMembers {
		if matchesUnavailableMember(unavailable, selector) {
			return PinnedSelection{}, ErrMemberUnavailable
		}
	}
	return PinnedSelection{}, ErrMemberNotFound
}

func sameServiceID(left, right *model.Identifier) bool {
	return left != nil && right != nil && *left == *right
}

func matchesUnavailableMember(member model.UnavailableMember, selector RefSelector) bool {
	matchedIdentity := false
	if member.ServiceID != nil {
		if selector.ServiceID != nil && *member.ServiceID != *selector.ServiceID {
			return false
		}
		matchedIdentity = true
	}
	if member.WorktreeID != nil {
		if *member.WorktreeID != selector.Ref.WorktreeID {
			return false
		}
		matchedIdentity = true
	}
	if member.ProfileID != nil {
		if *member.ProfileID != selector.Ref.ProfileID {
			return false
		}
		matchedIdentity = true
	}
	return matchedIdentity
}

type PinnedSelection struct {
	Vector   model.ScopeVector
	Selector RefSelector
	Member   model.ScopeMember
}

type EvidenceKind string
type EvidenceOrigin string

const (
	EvidenceSyntaxExact      EvidenceKind = "syntax_exact"
	EvidenceTypeResolved     EvidenceKind = "type_resolved"
	EvidenceTypePossible     EvidenceKind = "type_possible"
	EvidenceExplicitDocument EvidenceKind = "explicit_document"
	EvidenceTextMatch        EvidenceKind = "text_match"
	EvidenceUnresolved       EvidenceKind = "unresolved"
	EvidenceBuildAnchored    EvidenceKind = "build_anchored"
	EvidenceRuntimeObserved  EvidenceKind = "runtime_observed"
	EvidenceManualDeclared   EvidenceKind = "manual_declared"
	EvidenceProposed         EvidenceKind = "proposed"

	EvidenceOriginStaticAnalyzer       EvidenceOrigin = "static_analyzer"
	EvidenceOriginBuildInstrumentation EvidenceOrigin = "build_instrumentation"
	EvidenceOriginRuntimeObservation   EvidenceOrigin = "runtime_observation"
	EvidenceOriginManualApproved       EvidenceOrigin = "manual_approved"
	EvidenceOriginAIProposal           EvidenceOrigin = "ai_proposal"
)

type BridgeRecordRef struct {
	RecordID model.UUID `json:"record_id"`
	Version  int64      `json:"version"`
}

type BridgeEvidence struct {
	EvidenceID model.Identifier  `json:"evidence_id"`
	Origin     EvidenceOrigin    `json:"origin"`
	Kind       EvidenceKind      `json:"kind"`
	SourceRefs []model.SourceRef `json:"source_refs"`
	RecordRef  *BridgeRecordRef  `json:"record_ref,omitempty"`
	ArtifactID *model.Hash       `json:"artifact_id,omitempty"`
}

func (evidence BridgeEvidence) Validate() error {
	if !validFederationLabel(string(evidence.EvidenceID)) {
		return fmt.Errorf("%w: evidence_id is invalid", ErrInvalidBridge)
	}
	switch evidence.Origin {
	case EvidenceOriginStaticAnalyzer, EvidenceOriginBuildInstrumentation, EvidenceOriginRuntimeObservation, EvidenceOriginManualApproved, EvidenceOriginAIProposal:
	default:
		return fmt.Errorf("%w: evidence origin is unsupported", ErrInvalidBridge)
	}
	switch evidence.Kind {
	case EvidenceSyntaxExact, EvidenceTypeResolved, EvidenceTypePossible, EvidenceExplicitDocument, EvidenceTextMatch,
		EvidenceUnresolved, EvidenceBuildAnchored, EvidenceRuntimeObserved, EvidenceManualDeclared, EvidenceProposed:
	default:
		return fmt.Errorf("%w: evidence kind is unsupported", ErrInvalidBridge)
	}
	if evidence.SourceRefs == nil || len(evidence.SourceRefs) > 16 {
		return fmt.Errorf("%w: evidence source_refs must be an array capped at 16", ErrInvalidBridge)
	}
	for _, ref := range evidence.SourceRefs {
		if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
			return fmt.Errorf("%w: evidence source worktree_id: %v", ErrInvalidBridge, err)
		}
		if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
			return fmt.Errorf("%w: evidence source generation_id: %v", ErrInvalidBridge, err)
		}
		if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
			return fmt.Errorf("%w: evidence source profile_id: %v", ErrInvalidBridge, err)
		}
		if _, err := model.ParseHash(string(ref.ContentHash)); err != nil {
			return fmt.Errorf("%w: evidence source content_hash: %v", ErrInvalidBridge, err)
		}
		if err := model.ValidateRelativePath(ref.Path); err != nil {
			return fmt.Errorf("%w: evidence source path: %v", ErrInvalidBridge, err)
		}
		if ref.Range.StartByte < 0 || ref.Range.EndByte < ref.Range.StartByte || ref.Range.EndByte > model.MaxSafeInteger {
			return fmt.Errorf("%w: evidence source range is invalid", ErrInvalidBridge)
		}
	}
	if evidence.RecordRef != nil {
		if _, err := model.ParseUUID(string(evidence.RecordRef.RecordID)); err != nil || evidence.RecordRef.Version < 1 || evidence.RecordRef.Version > model.MaxSafeInteger {
			return fmt.Errorf("%w: evidence record_ref is invalid", ErrInvalidBridge)
		}
	}
	if evidence.ArtifactID != nil {
		if _, err := model.ParseHash(string(*evidence.ArtifactID)); err != nil {
			return fmt.Errorf("%w: evidence artifact_id: %v", ErrInvalidBridge, err)
		}
	}
	if len(evidence.SourceRefs) == 0 && evidence.RecordRef == nil && evidence.ArtifactID == nil {
		return fmt.Errorf("%w: evidence needs a source, approved record, or artifact", ErrInvalidBridge)
	}
	return nil
}

func validFederationLabel(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 192 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validEvidenceOrigin(evidence BridgeEvidence) bool {
	switch evidence.Kind {
	case EvidenceSyntaxExact, EvidenceTypeResolved:
		return evidence.Origin == EvidenceOriginStaticAnalyzer
	case EvidenceExplicitDocument:
		return evidence.Origin == EvidenceOriginStaticAnalyzer || evidence.Origin == EvidenceOriginManualApproved
	case EvidenceBuildAnchored:
		return evidence.Origin == EvidenceOriginBuildInstrumentation
	case EvidenceRuntimeObserved:
		return evidence.Origin == EvidenceOriginRuntimeObservation
	case EvidenceManualDeclared:
		return evidence.Origin == EvidenceOriginManualApproved
	default:
		return false
	}
}

func cloneEvidence(evidence []BridgeEvidence) []BridgeEvidence {
	result := append([]BridgeEvidence(nil), evidence...)
	for index := range result {
		result[index].SourceRefs = append([]model.SourceRef(nil), result[index].SourceRefs...)
		if result[index].SourceRefs == nil {
			result[index].SourceRefs = []model.SourceRef{}
		}
		if result[index].RecordRef != nil {
			recordRef := *result[index].RecordRef
			result[index].RecordRef = &recordRef
		}
		if result[index].ArtifactID != nil {
			artifactID := *result[index].ArtifactID
			result[index].ArtifactID = &artifactID
		}
	}
	return result
}
