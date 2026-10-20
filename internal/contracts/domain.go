// Package contracts contains closed internal value contracts for domain and
// cross-service declarations. It does not parse or execute provider formats.
package contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	wirecontracts "github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var ErrInvalidContract = errors.New("invalid domain contract")

type Plane string

const (
	PlaneIntended       Plane = "intended"
	PlaneObservedStatic Plane = "observed_static"
)

// SourceReference binds a domain fact to captured source bytes or an approved,
// versioned record. Exactly one form must be present.
type SourceReference struct {
	Source *model.SourceRef `json:"source_ref,omitempty"`
	Record *RecordRef       `json:"record_ref,omitempty"`
}

type RecordRef struct {
	RecordID model.UUID `json:"record_id"`
	Version  int64      `json:"version"`
}

func (ref RecordRef) Validate() error {
	if _, err := model.ParseUUID(string(ref.RecordID)); err != nil {
		return contractError("record_id", err)
	}
	if ref.Version < 1 || ref.Version > model.MaxSafeInteger {
		return fmt.Errorf("%w: record version must be a positive safe integer", ErrInvalidContract)
	}
	return nil
}

func (ref SourceReference) Validate() error {
	if (ref.Source == nil) == (ref.Record == nil) {
		return fmt.Errorf("%w: source reference needs exactly one source or approved record", ErrInvalidContract)
	}
	if ref.Source != nil {
		return validateSourceRef(*ref.Source)
	}
	return ref.Record.Validate()
}

// ValidateAgainst checks the captured bytes for source-backed evidence. The
// ordinary Validate method checks only the reference identity and range shape.
func (ref SourceReference) ValidateAgainst(source []byte) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.Source == nil {
		return nil
	}
	if err := ref.Source.ValidateAgainst(source); err != nil {
		return contractError("source content", err)
	}
	return nil
}

type DomainEntity struct {
	GenerationID model.UUID        `json:"generation_id"`
	EntityID     model.Identifier  `json:"entity_id"`
	Kind         model.Identifier  `json:"kind"`
	QualifiedKey string            `json:"qualified_key"`
	Plane        Plane             `json:"plane"`
	Payload      json.RawMessage   `json:"payload"`
	SourceRefs   []SourceReference `json:"source_refs"`
}

func (entity DomainEntity) Validate() error {
	if _, err := model.ParseUUID(string(entity.GenerationID)); err != nil {
		return contractError("generation_id", err)
	}
	if err := validateIdentifier("entity_id", entity.EntityID); err != nil {
		return err
	}
	if err := validateIdentifier("kind", entity.Kind); err != nil {
		return err
	}
	if !validText(entity.QualifiedKey) {
		return fmt.Errorf("%w: qualified_key must be nonempty UTF-8", ErrInvalidContract)
	}
	if entity.Plane != PlaneIntended && entity.Plane != PlaneObservedStatic {
		return fmt.Errorf("%w: unknown entity plane", ErrInvalidContract)
	}
	if err := validateJSON(entity.Payload); err != nil {
		return fmt.Errorf("%w: payload must be strict JSON: %v", ErrInvalidContract, err)
	}
	if entity.SourceRefs == nil || len(entity.SourceRefs) == 0 {
		return fmt.Errorf("%w: entity needs source or approved-record evidence", ErrInvalidContract)
	}
	for _, ref := range entity.SourceRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type FieldDirection string

const (
	DirectionInput         FieldDirection = "input"
	DirectionOutput        FieldDirection = "output"
	DirectionBidirectional FieldDirection = "bidirectional"
	DirectionStored        FieldDirection = "stored"
)

// ContractField keeps the declaration payload intact so provider-specific
// units, precision, versions, and nullability are not guessed or discarded.
type ContractField struct {
	GenerationID model.UUID       `json:"generation_id"`
	EntityID     model.Identifier `json:"entity_id"`
	FieldPath    string           `json:"field_path"`
	Direction    FieldDirection   `json:"direction"`
	Declaration  json.RawMessage  `json:"declaration"`
}

func (field ContractField) Validate() error {
	if _, err := model.ParseUUID(string(field.GenerationID)); err != nil {
		return contractError("generation_id", err)
	}
	if err := validateIdentifier("entity_id", field.EntityID); err != nil {
		return err
	}
	if !validText(field.FieldPath) {
		return fmt.Errorf("%w: field_path must be nonempty UTF-8", ErrInvalidContract)
	}
	switch field.Direction {
	case DirectionInput, DirectionOutput, DirectionBidirectional, DirectionStored:
	default:
		return fmt.Errorf("%w: unknown contract-field direction", ErrInvalidContract)
	}
	if err := validateJSON(field.Declaration); err != nil {
		return fmt.Errorf("%w: field declaration must be strict JSON: %v", ErrInvalidContract, err)
	}
	return nil
}

type Endpoint struct {
	SymbolID *model.Identifier `json:"symbol_id,omitempty"`
	EntityID *model.Identifier `json:"entity_id,omitempty"`
}

func (endpoint Endpoint) Validate() error {
	if (endpoint.SymbolID == nil) == (endpoint.EntityID == nil) {
		return fmt.Errorf("%w: link endpoint must contain exactly one symbol or entity ID", ErrInvalidContract)
	}
	if endpoint.SymbolID != nil {
		return validateIdentifier("symbol_id", *endpoint.SymbolID)
	}
	return validateIdentifier("entity_id", *endpoint.EntityID)
}

type ResolutionStatus string

const (
	ResolutionExact      ResolutionStatus = "exact_in_supported_scope"
	ResolutionCandidate  ResolutionStatus = "candidate"
	ResolutionUnresolved ResolutionStatus = "unresolved"
)

type EvidenceKind string

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
)

type EvidenceOrigin string

const (
	OriginStaticAnalyzer       EvidenceOrigin = "static_analyzer"
	OriginBuildInstrumentation EvidenceOrigin = "build_instrumentation"
	OriginRuntimeObservation   EvidenceOrigin = "runtime_observation"
	OriginManualApproved       EvidenceOrigin = "manual_approved"
	OriginAIProposal           EvidenceOrigin = "ai_proposal"
)

type EvidenceRef struct {
	EvidenceID model.Identifier  `json:"evidence_id"`
	Origin     EvidenceOrigin    `json:"origin"`
	Kind       EvidenceKind      `json:"kind"`
	SourceRefs []model.SourceRef `json:"source_refs"`
	RecordRef  *RecordRef        `json:"record_ref"`
	ArtifactID *model.Hash       `json:"artifact_id"`
}

func (evidence EvidenceRef) Validate() error {
	if err := validateIdentifier("evidence_id", evidence.EvidenceID); err != nil {
		return err
	}
	switch evidence.Origin {
	case OriginStaticAnalyzer, OriginBuildInstrumentation, OriginRuntimeObservation, OriginManualApproved, OriginAIProposal:
	default:
		return fmt.Errorf("%w: unknown evidence origin", ErrInvalidContract)
	}
	switch evidence.Kind {
	case EvidenceSyntaxExact, EvidenceTypeResolved, EvidenceTypePossible, EvidenceExplicitDocument,
		EvidenceTextMatch, EvidenceUnresolved, EvidenceBuildAnchored, EvidenceRuntimeObserved,
		EvidenceManualDeclared, EvidenceProposed:
	default:
		return fmt.Errorf("%w: unknown evidence kind", ErrInvalidContract)
	}
	if evidence.SourceRefs == nil || len(evidence.SourceRefs) > 16 {
		return fmt.Errorf("%w: evidence source_refs must be an array capped at 16", ErrInvalidContract)
	}
	for _, ref := range evidence.SourceRefs {
		if err := validateSourceRef(ref); err != nil {
			return err
		}
	}
	if evidence.RecordRef != nil {
		if err := evidence.RecordRef.Validate(); err != nil {
			return err
		}
	}
	if evidence.ArtifactID != nil {
		if _, err := model.ParseHash(string(*evidence.ArtifactID)); err != nil {
			return contractError("artifact_id", err)
		}
	}
	if len(evidence.SourceRefs) == 0 && evidence.RecordRef == nil && evidence.ArtifactID == nil {
		return fmt.Errorf("%w: evidence needs a source, approved record, or artifact", ErrInvalidContract)
	}
	return nil
}

type EntityLink struct {
	GenerationID     model.UUID       `json:"generation_id"`
	LinkID           model.Identifier `json:"link_id"`
	Source           Endpoint         `json:"source"`
	Target           Endpoint         `json:"target"`
	Kind             model.Identifier `json:"kind"`
	Evidence         []EvidenceRef    `json:"evidence"`
	ResolutionStatus ResolutionStatus `json:"resolution_status"`
}

func (link EntityLink) Validate() error {
	if _, err := model.ParseUUID(string(link.GenerationID)); err != nil {
		return contractError("generation_id", err)
	}
	if err := validateIdentifier("link_id", link.LinkID); err != nil {
		return err
	}
	if err := link.Source.Validate(); err != nil {
		return fmt.Errorf("%w: source: %v", ErrInvalidContract, err)
	}
	if err := link.Target.Validate(); err != nil {
		return fmt.Errorf("%w: target: %v", ErrInvalidContract, err)
	}
	if err := validateIdentifier("kind", link.Kind); err != nil {
		return err
	}
	switch link.ResolutionStatus {
	case ResolutionExact, ResolutionCandidate, ResolutionUnresolved:
	default:
		return fmt.Errorf("%w: unknown link resolution status", ErrInvalidContract)
	}
	if link.Evidence == nil || len(link.Evidence) == 0 {
		return fmt.Errorf("%w: link needs retained evidence", ErrInvalidContract)
	}
	for _, evidence := range link.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// StateTransition is a declaration only. Guard/effect JSON is retained but
// never evaluated by this package.
type StateTransition struct {
	GenerationID    model.UUID       `json:"generation_id"`
	MachineEntityID model.Identifier `json:"machine_entity_id"`
	TransitionID    model.Identifier `json:"transition_id"`
	FromEntityID    model.Identifier `json:"from_entity_id"`
	ToEntityID      model.Identifier `json:"to_entity_id"`
	GuardRef        json.RawMessage  `json:"guard_ref,omitempty"`
	EffectRefs      json.RawMessage  `json:"effect_refs"`
	Evidence        []EvidenceRef    `json:"evidence"`
}

func (transition StateTransition) Validate() error {
	if _, err := model.ParseUUID(string(transition.GenerationID)); err != nil {
		return contractError("generation_id", err)
	}
	for _, item := range []struct {
		name  string
		value model.Identifier
	}{
		{name: "machine_entity_id", value: transition.MachineEntityID},
		{name: "transition_id", value: transition.TransitionID},
		{name: "from_entity_id", value: transition.FromEntityID},
		{name: "to_entity_id", value: transition.ToEntityID},
	} {
		if err := validateIdentifier(item.name, item.value); err != nil {
			return err
		}
	}
	if len(transition.GuardRef) != 0 {
		if err := validateJSON(transition.GuardRef); err != nil {
			return fmt.Errorf("%w: guard_ref must be strict JSON when present: %v", ErrInvalidContract, err)
		}
	}
	if err := validateJSON(transition.EffectRefs); err != nil {
		return fmt.Errorf("%w: effect_refs must be strict JSON: %v", ErrInvalidContract, err)
	}
	if transition.Evidence == nil || len(transition.Evidence) == 0 {
		return fmt.Errorf("%w: transition needs retained evidence", ErrInvalidContract)
	}
	for _, evidence := range transition.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateSourceRef(ref model.SourceRef) error {
	if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
		return contractError("source worktree_id", err)
	}
	if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
		return contractError("source generation_id", err)
	}
	if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
		return contractError("source profile_id", err)
	}
	if _, err := model.ParseHash(string(ref.ContentHash)); err != nil {
		return contractError("source content_hash", err)
	}
	if err := model.ValidateRelativePath(ref.Path); err != nil {
		return contractError("source path", err)
	}
	if ref.Range.StartByte < 0 || ref.Range.EndByte < ref.Range.StartByte || ref.Range.EndByte > model.MaxSafeInteger {
		return fmt.Errorf("%w: invalid source byte range", ErrInvalidContract)
	}
	return nil
}

func validateIdentifier(name string, value model.Identifier) error {
	if len(value) < 1 || len(value) > 192 || !utf8.ValidString(string(value)) {
		return fmt.Errorf("%w: %s must be a UTF-8 identifier of 1..192 bytes", ErrInvalidContract, name)
	}
	return nil
}

func validText(value string) bool {
	return value != "" && utf8.ValidString(value)
}

func validateJSON(raw []byte) error {
	if _, err := wirecontracts.DecodeJSON(raw); err != nil {
		return err
	}
	return nil
}

func contractError(field string, cause error) error {
	return fmt.Errorf("%w: %s: %v", ErrInvalidContract, field, cause)
}
