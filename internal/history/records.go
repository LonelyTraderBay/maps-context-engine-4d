// Package history contains validated, immutable user-record values.
package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	MaxPayloadBytes = 16 << 10
	MaxSourceRefs   = 128
	MaxReasonBytes  = 2048
)

var (
	ErrInvalidRecord        = errors.New("invalid history record")
	ErrRecordNotFound       = errors.New("history record not found")
	ErrRecordConflict       = errors.New("history record version conflict")
	ErrMutationConflict     = errors.New("history mutation identity conflict")
	ErrRecordStoreCorrupted = errors.New("history record store is corrupted")
)

type Kind string

const (
	KindDecision             Kind = "decision"
	KindConstraint           Kind = "constraint"
	KindVocabulary           Kind = "vocabulary"
	KindUIMap                Kind = "ui_mapping"
	KindIntendedArchitecture Kind = "intended_architecture"
	KindStateModel           Kind = "state_model"
)

func (kind Kind) Valid() bool {
	switch kind {
	case KindDecision, KindConstraint, KindVocabulary, KindUIMap, KindIntendedArchitecture, KindStateModel:
		return true
	default:
		return false
	}
}

type Status string

const (
	StatusProposed    Status = "proposed"
	StatusApproved    Status = "approved"
	StatusRevoked     Status = "revoked"
	StatusSuperseded  Status = "superseded"
	StatusNeedsReview Status = "needs_review"
)

func (status Status) ValidDeclared() bool {
	return status == StatusProposed || status == StatusApproved || status == StatusRevoked || status == StatusSuperseded
}

type ProvenanceStatus string

const (
	ProvenanceRetainedSnapshot ProvenanceStatus = "retained_snapshot"
	ProvenanceSnapshotExpired  ProvenanceStatus = "snapshot_expired"
	ProvenanceSourceChanged    ProvenanceStatus = "source_changed"
	ProvenanceAccessDenied     ProvenanceStatus = "access_denied"
	ProvenanceUnverified       ProvenanceStatus = "unverified"
)

func (status ProvenanceStatus) Valid() bool {
	switch status {
	case ProvenanceRetainedSnapshot, ProvenanceSnapshotExpired, ProvenanceSourceChanged, ProvenanceAccessDenied, ProvenanceUnverified:
		return true
	default:
		return false
	}
}

// EffectiveStatus preserves the declared state and only marks an approval
// stale when its immutable source provenance can no longer be verified.
func EffectiveStatus(declared Status, provenance ProvenanceStatus) Status {
	if declared == StatusApproved && provenance != ProvenanceRetainedSnapshot {
		return StatusNeedsReview
	}
	return declared
}

func AggregateProvenance(statuses []ProvenanceStatus) ProvenanceStatus {
	if len(statuses) == 0 {
		return ProvenanceUnverified
	}
	result := ProvenanceRetainedSnapshot
	for _, status := range statuses {
		if !status.Valid() {
			return ProvenanceUnverified
		}
		switch status {
		case ProvenanceAccessDenied:
			return status
		case ProvenanceSnapshotExpired:
			result = status
		case ProvenanceSourceChanged:
			if result != ProvenanceSnapshotExpired {
				result = status
			}
		case ProvenanceUnverified:
			if result == ProvenanceRetainedSnapshot {
				result = status
			}
		}
	}
	return result
}

type ProposeInput struct {
	ClientMutationID model.UUID
	Kind             Kind
	Payload          json.RawMessage
	SourceRefs       []model.SourceRef
	Reason           string
}

type ReviewAction string

const (
	ReviewApprove   ReviewAction = "approve"
	ReviewRevoke    ReviewAction = "revoke"
	ReviewSupersede ReviewAction = "supersede"
)

type ReviewInput struct {
	RecordID         model.UUID
	ExpectedVersion  int64
	ClientMutationID model.UUID
	Action           ReviewAction
	Reason           string
}

// Version is the immutable persisted form. Effective/provenance status is
// derived at read time and never written back over declared_status.
type Version struct {
	RecordID       model.UUID        `json:"record_id"`
	Version        int64             `json:"version"`
	Kind           Kind              `json:"record_kind"`
	DeclaredStatus Status            `json:"declared_status"`
	Payload        json.RawMessage   `json:"payload"`
	SourceRefs     []model.SourceRef `json:"source_refs"`
	ActorID        model.Identifier  `json:"actor_id"`
	Reason         string            `json:"reason"`
	PayloadHash    model.Hash        `json:"payload_hash"`
	CreatedAt      string            `json:"created_at"`
}

type Summary struct {
	RecordID         model.UUID        `json:"record_id"`
	Version          int64             `json:"version"`
	Kind             Kind              `json:"record_kind"`
	DeclaredStatus   Status            `json:"declared_status"`
	EffectiveStatus  Status            `json:"effective_status"`
	ProvenanceStatus ProvenanceStatus  `json:"provenance_status"`
	SourceRefs       []model.SourceRef `json:"source_refs"`
}

type MutationResult struct {
	Version  Version `json:"version"`
	Replayed bool    `json:"replayed"`
}

type ListFilter struct {
	Kind          Kind
	Status        Status
	Limit         int64
	AfterRecordID model.UUID
}

func ValidatePrincipal(principal model.Identifier) error {
	value := string(principal)
	if value == "" || len(value) > 192 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("principal identity is invalid: %w", ErrInvalidRecord)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f || char == '\u0085' || char == '\u2028' || char == '\u2029' {
			return fmt.Errorf("principal identity contains a control character: %w", ErrInvalidRecord)
		}
	}
	return nil
}

func CanonicalPayload(raw json.RawMessage) (json.RawMessage, model.Hash, error) {
	if len(raw) == 0 || len(raw) > MaxPayloadBytes {
		return nil, "", fmt.Errorf("payload must contain 1..%d bytes: %w", MaxPayloadBytes, ErrInvalidRecord)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder, 0)
	if err != nil {
		return nil, "", fmt.Errorf("decode record payload: %w: %v", ErrInvalidRecord, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, "", fmt.Errorf("record payload must be a JSON object: %w", ErrInvalidRecord)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("record payload has trailing JSON: %w", ErrInvalidRecord)
	}
	identityBytes, err := model.CanonicalEncode(value)
	if err != nil {
		return nil, "", fmt.Errorf("record payload is outside the canonical value contract: %w: %w", ErrInvalidRecord, err)
	}
	canonicalJSON, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("encode canonical record payload: %w", err)
	}
	return canonicalJSON, model.SHA256(identityBytes), nil
}

func ValidateSourceRefs(worktreeID model.UUID, refs []model.SourceRef) error {
	if len(refs) > MaxSourceRefs {
		return fmt.Errorf("source reference count exceeds %d: %w", MaxSourceRefs, ErrInvalidRecord)
	}
	seen := make(map[model.SourceRef]struct{}, len(refs))
	for _, ref := range refs {
		if ref.WorktreeID != worktreeID || ValidateSourceRef(ref) != nil {
			return fmt.Errorf("source reference identity is invalid or belongs to another worktree: %w", ErrInvalidRecord)
		}
		if _, exists := seen[ref]; exists {
			return fmt.Errorf("duplicate source reference: %w", ErrInvalidRecord)
		}
		seen[ref] = struct{}{}
	}
	return nil
}

func ValidateSourceRef(ref model.SourceRef) error {
	return validateSourceRefIdentity(ref)
}

func validateSourceRefIdentity(ref model.SourceRef) error {
	if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
		return err
	}
	if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
		return err
	}
	if _, err := model.ParseHash(string(ref.ContentHash)); err != nil {
		return err
	}
	if err := model.ValidateRelativePath(ref.Path); err != nil {
		return err
	}
	if ref.Range.StartByte < 0 || ref.Range.EndByte < ref.Range.StartByte || ref.Range.EndByte > model.MaxSafeInteger {
		return model.ErrInvalidRange
	}
	return nil
}

func ValidateReason(reason string) error {
	if reason == "" || len(reason) > MaxReasonBytes || !utf8.ValidString(reason) || strings.TrimSpace(reason) != reason {
		return fmt.Errorf("record reason must contain 1..%d trimmed UTF-8 bytes: %w", MaxReasonBytes, ErrInvalidRecord)
	}
	for _, char := range reason {
		if char < 0x20 && char != '\n' && char != '\t' || char == 0x7f || char == '\u0085' || char == '\u2028' || char == '\u2029' {
			return fmt.Errorf("record reason contains a control character: %w", ErrInvalidRecord)
		}
	}
	return nil
}

func ValidatePropose(worktreeID model.UUID, principal model.Identifier, input ProposeInput) error {
	if err := ValidatePrincipal(principal); err != nil {
		return err
	}
	if parsed, err := model.ParseUUID(string(input.ClientMutationID)); err != nil || parsed != input.ClientMutationID {
		return fmt.Errorf("client mutation ID is invalid: %w", ErrInvalidRecord)
	}
	if !input.Kind.Valid() {
		return fmt.Errorf("record kind is unsupported: %w", ErrInvalidRecord)
	}
	if _, _, err := CanonicalPayload(input.Payload); err != nil {
		return err
	}
	if err := ValidateSourceRefs(worktreeID, input.SourceRefs); err != nil {
		return err
	}
	return ValidateReason(input.Reason)
}

func ValidateReview(principal model.Identifier, input ReviewInput) error {
	if err := ValidatePrincipal(principal); err != nil {
		return err
	}
	if parsed, err := model.ParseUUID(string(input.RecordID)); err != nil || parsed != input.RecordID {
		return fmt.Errorf("record ID is invalid: %w", ErrInvalidRecord)
	}
	if parsed, err := model.ParseUUID(string(input.ClientMutationID)); err != nil || parsed != input.ClientMutationID {
		return fmt.Errorf("client mutation ID is invalid: %w", ErrInvalidRecord)
	}
	if input.ExpectedVersion < 1 || input.ExpectedVersion > model.MaxSafeInteger {
		return fmt.Errorf("expected version is outside the safe integer range: %w", ErrInvalidRecord)
	}
	switch input.Action {
	case ReviewApprove, ReviewRevoke, ReviewSupersede:
	default:
		return fmt.Errorf("review action is unsupported: %w", ErrInvalidRecord)
	}
	return ValidateReason(input.Reason)
}

func decodeJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("maximum payload depth exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				if _, duplicate := object[key]; duplicate {
					return nil, fmt.Errorf("duplicate object key %q", key)
				}
				child, err := decodeJSONValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				child, err := decodeJSONValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				array = append(array, child)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return array, nil
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter")
		}
	case json.Number, string, bool, nil:
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported JSON token %T", token)
	}
}
