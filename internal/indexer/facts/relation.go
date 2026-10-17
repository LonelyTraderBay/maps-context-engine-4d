// Package facts contains normalized index facts shared by analyzers and indexers.
package facts

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var ErrInvalidRelation = errors.New("invalid relation fact")

type RelationKind string

const (
	RelationContains       RelationKind = "CONTAINS"
	RelationImports        RelationKind = "IMPORTS"
	RelationRefersTo       RelationKind = "REFERS_TO"
	RelationReads          RelationKind = "READS"
	RelationWrites         RelationKind = "WRITES"
	RelationCallsDirect    RelationKind = "CALLS_DIRECT"
	RelationCallsInterface RelationKind = "CALLS_INTERFACE"
	RelationMayDispatchTo  RelationKind = "MAY_DISPATCH_TO"
	RelationUnresolvedRef  RelationKind = "UNRESOLVED_REF"
	RelationUnresolvedCall RelationKind = "UNRESOLVED_CALL"
	RelationTestRef        RelationKind = "TEST_REF"
	RelationDocLink        RelationKind = "DOC_LINK"
)

type EvidenceKind string

const (
	EvidenceSyntaxExact  EvidenceKind = "syntax_exact"
	EvidenceTypeResolved EvidenceKind = "type_resolved"
	EvidenceTypePossible EvidenceKind = "type_possible"
	EvidenceExplicitDoc  EvidenceKind = "explicit_document"
	EvidenceTextMatch    EvidenceKind = "text_match"
	EvidenceUnresolved   EvidenceKind = "unresolved"
)

// RelationFact follows the closed RelationFact contract in SPEC S43.
type RelationFact struct {
	RelationID      model.Hash      `json:"relation_id"`
	SourceSymbolID  model.Hash      `json:"source_symbol_id"`
	TargetSymbolID  *model.Hash     `json:"target_symbol_id"`
	Kind            RelationKind    `json:"kind"`
	EvidenceKind    EvidenceKind    `json:"evidence_kind"`
	SitePath        string          `json:"site_path"`
	Range           model.ByteRange `json:"range"`
	Reason          string          `json:"reason"`
	UnresolvedLabel *string         `json:"unresolved_label"`
}

type RelationInput struct {
	SourceSymbolID  model.Hash
	TargetSymbolID  *model.Hash
	Kind            RelationKind
	EvidenceKind    EvidenceKind
	SitePath        string
	Range           model.ByteRange
	Reason          string
	UnresolvedLabel *string
}

// NewRelationFact validates the site against captured bytes before assigning a
// deterministic ID. Target and site are part of the ID, so parallel relations
// at the same callsite remain distinct.
func NewRelationFact(input RelationInput, source []byte) (RelationFact, error) {
	relation := RelationFact{
		SourceSymbolID:  input.SourceSymbolID,
		TargetSymbolID:  cloneHash(input.TargetSymbolID),
		Kind:            input.Kind,
		EvidenceKind:    input.EvidenceKind,
		SitePath:        input.SitePath,
		Range:           input.Range,
		Reason:          input.Reason,
		UnresolvedLabel: cloneString(input.UnresolvedLabel),
	}
	if err := relation.validateFields(source); err != nil {
		return RelationFact{}, err
	}
	id, err := relationID(relation)
	if err != nil {
		return RelationFact{}, err
	}
	relation.RelationID = id
	return relation, nil
}

// Validate checks both the closed contract and the deterministic relation ID.
func (relation RelationFact) Validate(source []byte) error {
	if err := relation.validateFields(source); err != nil {
		return err
	}
	expected, err := relationID(relation)
	if err != nil {
		return err
	}
	if relation.RelationID != expected {
		return fmt.Errorf("%w: relation ID does not match its fields", ErrInvalidRelation)
	}
	return nil
}

func (relation RelationFact) validateFields(source []byte) error {
	if _, err := model.ParseHash(string(relation.SourceSymbolID)); err != nil {
		return fmt.Errorf("%w: invalid source symbol ID", ErrInvalidRelation)
	}
	if relation.TargetSymbolID != nil {
		if _, err := model.ParseHash(string(*relation.TargetSymbolID)); err != nil {
			return fmt.Errorf("%w: invalid target symbol ID", ErrInvalidRelation)
		}
	}
	if err := model.ValidateRelativePath(relation.SitePath); err != nil {
		return fmt.Errorf("%w: invalid site path", ErrInvalidRelation)
	}
	if err := relation.Range.ValidateAgainst(source); err != nil {
		return fmt.Errorf("%w: invalid source range: %v", ErrInvalidRelation, err)
	}
	if !validRelationKind(relation.Kind) || !validEvidenceKind(relation.EvidenceKind) {
		return fmt.Errorf("%w: unknown relation or evidence kind", ErrInvalidRelation)
	}
	if !utf8.ValidString(relation.Reason) || utf8.RuneCountInString(relation.Reason) > 1024 {
		return fmt.Errorf("%w: reason must be valid UTF-8 and at most 1024 characters", ErrInvalidRelation)
	}
	if relation.UnresolvedLabel != nil && (!utf8.ValidString(*relation.UnresolvedLabel) ||
		utf8.RuneCountInString(*relation.UnresolvedLabel) < 1 || utf8.RuneCountInString(*relation.UnresolvedLabel) > 512) {
		return fmt.Errorf("%w: unresolved label must be valid UTF-8 and 1 to 512 characters", ErrInvalidRelation)
	}
	switch relation.Kind {
	case RelationUnresolvedRef, RelationUnresolvedCall:
		if relation.TargetSymbolID != nil || relation.EvidenceKind != EvidenceUnresolved ||
			relation.UnresolvedLabel == nil || relation.Reason == "" {
			return fmt.Errorf("%w: unresolved relation requires null target, reason, label, and unresolved evidence", ErrInvalidRelation)
		}
	case RelationCallsDirect:
		if relation.TargetSymbolID == nil || (relation.EvidenceKind != EvidenceTypeResolved && relation.EvidenceKind != EvidenceSyntaxExact) {
			return fmt.Errorf("%w: direct call requires a target and resolved or exact evidence", ErrInvalidRelation)
		}
	case RelationCallsInterface:
		if relation.TargetSymbolID == nil || relation.EvidenceKind != EvidenceTypeResolved {
			return fmt.Errorf("%w: interface call requires a type-resolved target", ErrInvalidRelation)
		}
	case RelationMayDispatchTo:
		if relation.TargetSymbolID == nil || relation.EvidenceKind != EvidenceTypePossible {
			return fmt.Errorf("%w: candidate dispatch requires a type-possible target", ErrInvalidRelation)
		}
	}
	return nil
}

func relationID(relation RelationFact) (model.Hash, error) {
	return model.HashValue("relation-fact-v1", map[string]any{
		"source_symbol_id": relation.SourceSymbolID,
		"target_symbol_id": optionalHash(relation.TargetSymbolID),
		"kind":             string(relation.Kind),
		"evidence_kind":    string(relation.EvidenceKind),
		"site_path":        relation.SitePath,
		"start_byte":       relation.Range.StartByte,
		"end_byte":         relation.Range.EndByte,
		"reason":           relation.Reason,
		"unresolved_label": optionalString(relation.UnresolvedLabel),
	})
}

func validRelationKind(value RelationKind) bool {
	switch value {
	case RelationContains, RelationImports, RelationRefersTo, RelationReads, RelationWrites,
		RelationCallsDirect, RelationCallsInterface, RelationMayDispatchTo,
		RelationUnresolvedRef, RelationUnresolvedCall, RelationTestRef, RelationDocLink:
		return true
	default:
		return false
	}
}

func validEvidenceKind(value EvidenceKind) bool {
	switch value {
	case EvidenceSyntaxExact, EvidenceTypeResolved, EvidenceTypePossible,
		EvidenceExplicitDoc, EvidenceTextMatch, EvidenceUnresolved:
		return true
	default:
		return false
	}
}

func cloneHash(value *model.Hash) *model.Hash {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func optionalString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalHash(value *model.Hash) any {
	if value == nil {
		return nil
	}
	return *value
}
