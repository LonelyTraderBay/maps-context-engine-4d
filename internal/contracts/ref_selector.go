package contracts

import (
	"errors"
	"fmt"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrAmbiguousMember    = errors.New("federated reference is ambiguous")
	ErrMemberUnavailable  = errors.New("pinned scope member is unavailable")
	ErrMemberNotFound     = errors.New("reference does not belong to the pinned scope")
	ErrPinnedScopeChanged = errors.New("pinned scope vector changed")
)

type GraphRefKind string

const (
	GraphRefSymbol       GraphRefKind = "symbol"
	GraphRefEntity       GraphRefKind = "entity"
	GraphRefModule       GraphRefKind = "module"
	GraphRefSourceAnchor GraphRefKind = "source_anchor"
	GraphRefFile         GraphRefKind = "file"
)

// GraphRef is a closed union matching the supported GraphRef variants in the
// canonical contract. Fields for other variants must stay empty.
type GraphRef struct {
	Kind         GraphRefKind
	WorktreeID   model.UUID
	ProfileID    model.Hash
	GenerationID model.UUID
	SymbolID     model.Identifier
	EntityID     model.Identifier
	ModuleID     model.Identifier
	Path         string
	ContentHash  *model.Hash
	BuildMapID   model.Hash
	AnchorID     model.Hash
	BindingID    model.Hash
}

func (ref GraphRef) Validate() error {
	if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
		return contractError("ref.worktree_id", err)
	}
	if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
		return contractError("ref.profile_id", err)
	}
	if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
		return contractError("ref.generation_id", err)
	}
	switch ref.Kind {
	case GraphRefSymbol:
		if err := validateIdentifier("ref.symbol_id", ref.SymbolID); err != nil {
			return err
		}
		if ref.EntityID != "" || ref.ModuleID != "" || ref.Path != "" || ref.ContentHash != nil || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: symbol ref contains fields from another ref kind", ErrInvalidContract)
		}
	case GraphRefEntity:
		if err := validateIdentifier("ref.entity_id", ref.EntityID); err != nil {
			return err
		}
		if ref.SymbolID != "" || ref.ModuleID != "" || ref.Path != "" || ref.ContentHash != nil || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: entity ref contains fields from another ref kind", ErrInvalidContract)
		}
	case GraphRefModule:
		if err := validateIdentifier("ref.module_id", ref.ModuleID); err != nil {
			return err
		}
		if ref.SymbolID != "" || ref.EntityID != "" || ref.Path != "" || ref.ContentHash != nil || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: module ref contains fields from another ref kind", ErrInvalidContract)
		}
	case GraphRefSourceAnchor:
		for _, item := range []struct {
			name  string
			value model.Hash
		}{
			{name: "build_map_id", value: ref.BuildMapID},
			{name: "anchor_id", value: ref.AnchorID},
			{name: "binding_id", value: ref.BindingID},
		} {
			if _, err := model.ParseHash(string(item.value)); err != nil {
				return contractError("ref."+item.name, err)
			}
		}
		if ref.SymbolID != "" || ref.EntityID != "" || ref.ModuleID != "" || ref.Path != "" || ref.ContentHash != nil {
			return fmt.Errorf("%w: source-anchor ref contains fields from another ref kind", ErrInvalidContract)
		}
	case GraphRefFile:
		if err := model.ValidateRelativePath(ref.Path); err != nil {
			return contractError("ref.path", err)
		}
		if ref.ContentHash != nil {
			if _, err := model.ParseHash(string(*ref.ContentHash)); err != nil {
				return contractError("ref.content_hash", err)
			}
		}
		if ref.SymbolID != "" || ref.EntityID != "" || ref.ModuleID != "" || ref.BuildMapID != "" || ref.AnchorID != "" || ref.BindingID != "" {
			return fmt.Errorf("%w: file ref contains fields from another ref kind", ErrInvalidContract)
		}
	default:
		return fmt.Errorf("%w: unsupported graph ref kind", ErrInvalidContract)
	}
	return nil
}

type RefSelector struct {
	Kind      string
	Ref       GraphRef
	ServiceID *model.Identifier
}

func (selector RefSelector) Validate() error {
	if selector.Kind != "ref" {
		return fmt.Errorf("%w: selector kind must be ref", ErrInvalidContract)
	}
	if err := selector.Ref.Validate(); err != nil {
		return err
	}
	if selector.ServiceID != nil {
		if err := validateIdentifier("service_id", *selector.ServiceID); err != nil {
			return err
		}
	}
	return nil
}

type PinnedSelection struct {
	Vector   model.ScopeVector
	Selector RefSelector
	Member   model.ScopeMember
}

// SelectPinned resolves only against the supplied immutable scope vector. The
// expected digest comes from the earlier selection/page request and prevents
// an active-scope change from silently replacing that snapshot. It
// never follows an active-generation pointer or substitutes an unavailable
// member with a current generation.
func SelectPinned(vector model.ScopeVector, expectedDigest model.Hash, selector RefSelector) (PinnedSelection, error) {
	if err := vector.Validate(); err != nil {
		return PinnedSelection{}, fmt.Errorf("%w: invalid scope vector: %v", ErrInvalidContract, err)
	}
	if _, err := model.ParseHash(string(expectedDigest)); err != nil {
		return PinnedSelection{}, contractError("expected vector digest", err)
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
		if selector.ServiceID != nil && !sameIdentifier(member.ServiceID, selector.ServiceID) {
			continue
		}
		if matchIndex >= 0 {
			return PinnedSelection{}, ErrAmbiguousMember
		}
		matchIndex = index
	}
	if matchIndex >= 0 {
		pinnedVector := cloneScopeVector(vector)
		return PinnedSelection{
			Vector:   pinnedVector,
			Selector: cloneSelector(selector),
			Member:   cloneScopeMember(pinnedVector.Members[matchIndex]),
		}, nil
	}
	for _, unavailable := range vector.UnavailableMembers {
		if matchesUnavailable(unavailable, selector) {
			return PinnedSelection{}, ErrMemberUnavailable
		}
	}
	return PinnedSelection{}, ErrMemberNotFound
}

func matchesUnavailable(member model.UnavailableMember, selector RefSelector) bool {
	ref := selector.Ref
	matchedIdentity := false
	if member.ServiceID != nil {
		if selector.ServiceID != nil && *member.ServiceID != *selector.ServiceID {
			return false
		}
		matchedIdentity = true
	}
	if member.WorktreeID != nil {
		if *member.WorktreeID != ref.WorktreeID {
			return false
		}
		matchedIdentity = true
	}
	if member.ProfileID != nil {
		if *member.ProfileID != ref.ProfileID {
			return false
		}
		matchedIdentity = true
	}
	return matchedIdentity
}

func sameIdentifier(left, right *model.Identifier) bool {
	return left != nil && right != nil && *left == *right
}

func cloneSelector(selector RefSelector) RefSelector {
	if selector.ServiceID != nil {
		serviceID := *selector.ServiceID
		selector.ServiceID = &serviceID
	}
	if selector.Ref.ContentHash != nil {
		contentHash := *selector.Ref.ContentHash
		selector.Ref.ContentHash = &contentHash
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

func cloneScopeVector(vector model.ScopeVector) model.ScopeVector {
	vector.Members = append([]model.ScopeMember(nil), vector.Members...)
	if vector.Members == nil {
		vector.Members = []model.ScopeMember{}
	}
	for index := range vector.Members {
		vector.Members[index] = cloneScopeMember(vector.Members[index])
	}
	vector.UnavailableMembers = append([]model.UnavailableMember(nil), vector.UnavailableMembers...)
	if vector.UnavailableMembers == nil {
		vector.UnavailableMembers = []model.UnavailableMember{}
	}
	for index := range vector.UnavailableMembers {
		member := &vector.UnavailableMembers[index]
		if member.ServiceID != nil {
			serviceID := *member.ServiceID
			member.ServiceID = &serviceID
		}
		if member.WorktreeID != nil {
			worktreeID := *member.WorktreeID
			member.WorktreeID = &worktreeID
		}
		if member.ProfileID != nil {
			profileID := *member.ProfileID
			member.ProfileID = &profileID
		}
	}
	if vector.GroupID != nil {
		groupID := *vector.GroupID
		vector.GroupID = &groupID
	}
	if vector.GroupRevision != nil {
		groupRevision := *vector.GroupRevision
		vector.GroupRevision = &groupRevision
	}
	return vector
}
