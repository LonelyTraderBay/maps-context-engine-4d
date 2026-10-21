package federation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestResolveProducesDeterministicPinnedVectorAndHidesDeniedIdentity(t *testing.T) {
	group := federationGroup()
	reader := &federationMemberReader{resolutions: map[model.Identifier]MemberResolution{
		"svc-z": {Status: "resolved", Member: federationScopeMember("svc-z", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")},
		"svc-a": {Status: "access_denied", ReasonCode: "policy_denied", RevealIdentity: true},
	}}
	resolver, err := NewGroupScopeResolver(&federationGroupReader{snapshots: []catalog.GroupSnapshot{group, group}}, reader)
	if err != nil {
		t.Fatal(err)
	}
	vector, err := resolver.Resolve(context.Background(), group.Group.Config.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if err := vector.Validate(); err != nil {
		t.Fatalf("resolved vector does not validate: %v", err)
	}
	if len(vector.Members) != 1 || len(vector.UnavailableMembers) != 1 || vector.Members[0].ServiceID == nil || *vector.Members[0].ServiceID != "svc-z" {
		t.Fatalf("unexpected pinned member sets: %#v", vector)
	}
	unavailable := vector.UnavailableMembers[0]
	if unavailable.Status != "access_denied" || unavailable.ServiceID != nil || unavailable.WorktreeID != nil || unavailable.ProfileID != nil {
		t.Fatalf("denied member identity leaked: %#v", unavailable)
	}
	if len(reader.seen) != 2 || reader.seen[0] != "svc-a" || reader.seen[1] != "svc-z" {
		t.Fatalf("members were not resolved in stable service order: %#v", reader.seen)
	}
}

func TestResolveAllUnavailableDoesNotInventGenerations(t *testing.T) {
	group := federationGroup()
	reader := &federationMemberReader{resolutions: map[model.Identifier]MemberResolution{
		"svc-z": {Status: "index_missing", RevealIdentity: true},
		"svc-a": {Status: "unavailable", RevealIdentity: true},
	}}
	resolver, _ := NewGroupScopeResolver(&federationGroupReader{snapshots: []catalog.GroupSnapshot{group, group}}, reader)
	vector, err := resolver.Resolve(context.Background(), group.Group.Config.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vector.Members) != 0 || len(vector.UnavailableMembers) != 2 {
		t.Fatalf("missing indexes must remain explicit gaps: %#v", vector)
	}
	if err := vector.Validate(); err != nil {
		t.Fatalf("all-unavailable pinned vector should validate without fake members: %v", err)
	}
}

func TestNewLiveResolveDoesNotUpgradeAnExistingPinnedVector(t *testing.T) {
	group := federationGroup()
	memberReader := &federationMemberReader{resolutions: map[model.Identifier]MemberResolution{
		"svc-z": {Status: "resolved", Member: federationScopeMember("svc-z", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")},
		"svc-a": {Status: "unavailable", RevealIdentity: true},
	}}
	resolver, _ := NewGroupScopeResolver(&federationGroupReader{snapshots: []catalog.GroupSnapshot{group}}, memberReader)
	oldVector, err := resolver.Resolve(context.Background(), group.Group.Config.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := oldVector.VectorDigest
	newMember := federationScopeMember("svc-z", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	newMember.GenerationID = "88888888-8888-4888-8888-888888888888"
	memberReader.resolutions["svc-z"] = MemberResolution{Status: "resolved", Member: newMember}
	newVector, err := resolver.Resolve(context.Background(), group.Group.Config.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	if oldVector.Members[0].GenerationID != "dddddddd-dddd-4ddd-8ddd-dddddddddddd" || oldVector.VectorDigest != oldDigest {
		t.Fatalf("a later live resolve mutated the caller's pinned vector: %#v", oldVector)
	}
	if newVector.Members[0].GenerationID != "88888888-8888-4888-8888-888888888888" || newVector.VectorDigest == oldDigest {
		t.Fatalf("new live query did not produce its own pinned vector: %#v", newVector)
	}
}

func TestResolveRejectsGroupMutationDuringMemberReads(t *testing.T) {
	before, after := federationGroup(), federationGroup()
	after.Group.Revision++
	resolver, _ := NewGroupScopeResolver(&federationGroupReader{snapshots: []catalog.GroupSnapshot{before, after}},
		&federationMemberReader{resolutions: map[model.Identifier]MemberResolution{
			"svc-z": {Status: "unavailable", RevealIdentity: true},
			"svc-a": {Status: "unavailable", RevealIdentity: true},
		}})
	if _, err := resolver.Resolve(context.Background(), before.Group.Config.GroupID); !errors.Is(err, ErrGroupScopeChanged) {
		t.Fatalf("changed group revision was accepted: %v", err)
	}
}

func TestResolveRejectsUnversionedGroupConfigChange(t *testing.T) {
	before, after := federationGroup(), federationGroup()
	after.Group.Config.Name = "renamed-without-revision"
	resolver, _ := NewGroupScopeResolver(&federationGroupReader{snapshots: []catalog.GroupSnapshot{before, after}},
		&federationMemberReader{resolutions: map[model.Identifier]MemberResolution{
			"svc-z": {Status: "unavailable", RevealIdentity: true},
			"svc-a": {Status: "unavailable", RevealIdentity: true},
		}})
	if _, err := resolver.Resolve(context.Background(), before.Group.Config.GroupID); !errors.Is(err, ErrGroupScopeChanged) {
		t.Fatalf("same-revision group content change was accepted: %v", err)
	}
}

func federationGroup() catalog.GroupSnapshot {
	profile := model.Hash(strings.Repeat("a", 64))
	first := catalog.GroupMemberConfig{ServiceID: "svc-z", WorktreeID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ProfileID: profile,
		Namespace: catalog.ServiceNamespace{Environment: "prod", Protocol: "http"}}
	second := catalog.GroupMemberConfig{ServiceID: "svc-a", WorktreeID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", ProfileID: profile,
		Namespace: catalog.ServiceNamespace{Environment: "prod", Protocol: "http"}}
	return catalog.GroupSnapshot{
		Group:              catalog.GroupSummary{Config: catalog.GroupConfig{GroupID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "payments", Members: []catalog.GroupMemberConfig{first, second}}, Revision: 4, Status: "active"},
		CatalogIncarnation: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	}
}

func federationScopeMember(service model.Identifier, generation model.UUID) *model.ScopeMember {
	worktree := model.UUID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	profile := model.Hash(strings.Repeat("a", 64))
	serviceCopy := service
	return &model.ScopeMember{
		WorktreeID: worktree, ProfileID: profile, GenerationID: generation,
		SnapshotDigest: model.Hash(strings.Repeat("1", 64)), PolicyDigest: model.Hash(strings.Repeat("2", 64)),
		PolicyEpoch: 1, BindingRevision: 1, RecordEpoch: 0, ServiceID: &serviceCopy,
		IndexIncarnation: "ffffffff-ffff-4fff-8fff-ffffffffffff", RecordsIncarnation: "99999999-9999-4999-8999-999999999999",
	}
}

type federationGroupReader struct{ snapshots []catalog.GroupSnapshot }

func (reader *federationGroupReader) GetGroup(context.Context, model.UUID) (catalog.GroupSnapshot, error) {
	if len(reader.snapshots) == 0 {
		return catalog.GroupSnapshot{}, errors.New("no group snapshot")
	}
	snapshot := reader.snapshots[0]
	if len(reader.snapshots) > 1 {
		reader.snapshots = reader.snapshots[1:]
	}
	return snapshot, nil
}

type federationMemberReader struct {
	resolutions map[model.Identifier]MemberResolution
	seen        []model.Identifier
}

func (reader *federationMemberReader) ResolvePinnedMember(_ context.Context, expected catalog.GroupMemberConfig) (MemberResolution, error) {
	reader.seen = append(reader.seen, expected.ServiceID)
	resolution, ok := reader.resolutions[expected.ServiceID]
	if !ok {
		return MemberResolution{}, errors.New("unexpected member")
	}
	if resolution.Member != nil {
		member := *resolution.Member
		member.WorktreeID = expected.WorktreeID
		member.ProfileID = expected.ProfileID
		resolution.Member = &member
	}
	return resolution, nil
}
