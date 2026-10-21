package history

import (
	"fmt"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

// EffectiveModelStatus keeps the history record's declared status immutable
// and derives stale approval from the current machine source and scope.
func EffectiveModelStatus(record Version, machine Machine) (Status, error) {
	if record.Kind != KindStateModel {
		return "", fmt.Errorf("state model record kind is required: %w", ErrInvalidDocument)
	}
	if !record.DeclaredStatus.ValidDeclared() {
		return "", fmt.Errorf("declared history status is invalid: %w", ErrInvalidDocument)
	}
	scopeID, err := model.HashValue("state-machine-scope-v1", map[string]any{
		"worktree_id":   machine.Source.WorktreeID,
		"profile_id":    machine.Source.ProfileID,
		"generation_id": machine.Source.GenerationID,
		"path":          machine.Source.Path,
		"content_hash":  machine.Source.ContentHash,
	})
	if err != nil || scopeID != machine.ScopeID {
		return "", fmt.Errorf("machine source scope is inconsistent: %w", ErrInvalidDocument)
	}
	if record.DeclaredStatus != StatusApproved {
		return record.DeclaredStatus, nil
	}
	if !machine.Complete {
		return EffectiveStatus(record.DeclaredStatus, ProvenanceUnverified), nil
	}
	if len(record.SourceRefs) != 1 {
		return EffectiveStatus(record.DeclaredStatus, ProvenanceUnverified), nil
	}
	if ValidateSourceRef(record.SourceRefs[0]) != nil {
		return "", fmt.Errorf("record source reference is invalid: %w", ErrInvalidDocument)
	}
	if sameSourceSnapshot(record.SourceRefs[0], machine.Source) {
		return StatusApproved, nil
	}
	return EffectiveStatus(record.DeclaredStatus, ProvenanceSourceChanged), nil
}

func sameSourceSnapshot(left, right model.SourceRef) bool {
	return left.WorktreeID == right.WorktreeID && left.ProfileID == right.ProfileID &&
		left.GenerationID == right.GenerationID && left.Path == right.Path && left.ContentHash == right.ContentHash
}

// IsEffectiveModelApproval is the single gate callers can use before exposing
// an approved state model as context. Proposed, revoked, superseded, or stale
// records do not pass this gate.
func IsEffectiveModelApproval(record Version, machine Machine) (bool, error) {
	status, err := EffectiveModelStatus(record, machine)
	if err != nil {
		return false, err
	}
	return status == StatusApproved, nil
}
