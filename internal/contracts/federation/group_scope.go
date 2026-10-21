package federation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrGroupScopeChanged = errors.New("group changed while resolving its scope")
	ErrInvalidMemberView = errors.New("group member resolver returned an invalid view")
)

type GroupReader interface {
	GetGroup(context.Context, model.UUID) (catalog.GroupSnapshot, error)
}

// MemberResolution is one trusted observation for an explicitly declared
// member. A resolved result must contain a real generation. An unavailable
// result carries no generation or snapshot digest.
type MemberResolution struct {
	Status         string
	Member         *model.ScopeMember
	ReasonCode     model.Identifier
	RevealIdentity bool
}

type MemberReader interface {
	ResolvePinnedMember(context.Context, catalog.GroupMemberConfig) (MemberResolution, error)
}

type GroupScopeResolver struct {
	groups  GroupReader
	members MemberReader
}

func NewGroupScopeResolver(groups GroupReader, members MemberReader) (*GroupScopeResolver, error) {
	if groups == nil || members == nil {
		return nil, fmt.Errorf("group reader and member reader are required")
	}
	return &GroupScopeResolver{groups: groups, members: members}, nil
}

// Resolve reads one group revision, resolves only its declared members, then
// verifies that the group revision and catalog incarnation did not change.
// MemberReader is responsible for returning an observed generation from the
// exact worktree/profile/service tuple; it must not follow an active/latest
// pointer when that observation is missing or stale.
func (resolver *GroupScopeResolver) Resolve(ctx context.Context, groupID model.UUID) (model.ScopeVector, error) {
	if ctx == nil || resolver == nil {
		return model.ScopeVector{}, fmt.Errorf("group resolver and context are required")
	}
	before, err := resolver.groups.GetGroup(ctx, groupID)
	if err != nil {
		return model.ScopeVector{}, err
	}
	if before.Group.Status != "active" {
		return model.ScopeVector{}, fmt.Errorf("group is not active: %w", ErrGroupScopeChanged)
	}
	if before.Group.Config.GroupID != groupID || catalog.ValidateGroupConfig(before.Group.Config) != nil || before.Group.Revision < 1 || before.CatalogIncarnation == "" {
		return model.ScopeVector{}, fmt.Errorf("invalid pinned group snapshot: %w", ErrInvalidMemberView)
	}

	expected := append([]catalog.GroupMemberConfig(nil), before.Group.Config.Members...)
	sort.Slice(expected, func(i, j int) bool {
		left, right := expected[i], expected[j]
		if left.ServiceID != right.ServiceID {
			return left.ServiceID < right.ServiceID
		}
		if left.WorktreeID != right.WorktreeID {
			return left.WorktreeID < right.WorktreeID
		}
		return left.ProfileID < right.ProfileID
	})
	vector := model.ScopeVector{
		Kind: "federated", Members: []model.ScopeMember{}, UnavailableMembers: []model.UnavailableMember{},
		CatalogIncarnation: before.CatalogIncarnation,
	}
	groupIDCopy, revision := before.Group.Config.GroupID, before.Group.Revision
	vector.GroupID, vector.GroupRevision = &groupIDCopy, &revision
	for _, declaration := range expected {
		resolution, err := resolver.members.ResolvePinnedMember(ctx, declaration)
		if err != nil {
			return model.ScopeVector{}, fmt.Errorf("resolve declared group member: %w", err)
		}
		switch resolution.Status {
		case "resolved":
			if resolution.Member == nil || resolution.ReasonCode != "" ||
				resolution.Member.WorktreeID != declaration.WorktreeID ||
				resolution.Member.ProfileID != declaration.ProfileID ||
				resolution.Member.ServiceID == nil || *resolution.Member.ServiceID != declaration.ServiceID {
				return model.ScopeVector{}, fmt.Errorf("resolved member does not match its declaration: %w", ErrInvalidMemberView)
			}
			vector.Members = append(vector.Members, *resolution.Member)
		case "index_missing", "unavailable", "profile_missing", "access_denied":
			if resolution.Member != nil {
				return model.ScopeVector{}, fmt.Errorf("unavailable member contains a generation: %w", ErrInvalidMemberView)
			}
			reason := resolution.ReasonCode
			if reason == "" {
				reason = model.Identifier(resolution.Status)
			}
			entry := model.UnavailableMember{Status: resolution.Status, ReasonCode: reason}
			if resolution.RevealIdentity && resolution.Status != "access_denied" {
				serviceID, worktreeID, profileID := declaration.ServiceID, declaration.WorktreeID, declaration.ProfileID
				entry.ServiceID, entry.WorktreeID, entry.ProfileID = &serviceID, &worktreeID, &profileID
			}
			vector.UnavailableMembers = append(vector.UnavailableMembers, entry)
		default:
			return model.ScopeVector{}, fmt.Errorf("member resolver returned an unsupported status: %w", ErrInvalidMemberView)
		}
	}

	after, err := resolver.groups.GetGroup(ctx, groupID)
	if err != nil || after.CatalogIncarnation != before.CatalogIncarnation ||
		after.Group.Config.GroupID != before.Group.Config.GroupID ||
		after.Group.Revision != before.Group.Revision || after.Group.Status != before.Group.Status ||
		!reflect.DeepEqual(after.Group.Config, before.Group.Config) {
		return model.ScopeVector{}, errors.Join(ErrGroupScopeChanged, err)
	}
	digest, err := vector.ComputeDigest()
	if err != nil {
		return model.ScopeVector{}, err
	}
	vector.VectorDigest = digest
	if err := vector.Validate(); err != nil {
		return model.ScopeVector{}, fmt.Errorf("resolved group vector is invalid: %w", err)
	}
	return vector, nil
}
