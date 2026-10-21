package history

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestCanonicalPayloadIsStableAndRejectsAmbiguousValues(t *testing.T) {
	first, firstHash, err := CanonicalPayload(json.RawMessage(`{"b":2,"a":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, secondHash, err := CanonicalPayload(json.RawMessage("{ \"a\": \"x\", \"b\": 2 }"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || firstHash != secondHash {
		t.Fatalf("equivalent payloads differ: %s/%s hash=%s/%s", first, second, firstHash, secondHash)
	}
	for _, invalid := range []string{`[]`, `{"a":1,"a":2}`, `{"fraction":1.5}`, `{"safe":9007199254740992}`, `{"a":1} {"b":2}`, `null`} {
		if _, _, err := CanonicalPayload(json.RawMessage(invalid)); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("payload %s returned %v; want ErrInvalidRecord", invalid, err)
		}
	}
}

func TestEffectiveStatusRequiresCurrentVerifiedProvenanceForApproval(t *testing.T) {
	for _, test := range []struct {
		declared   Status
		provenance ProvenanceStatus
		want       Status
	}{
		{StatusApproved, ProvenanceRetainedSnapshot, StatusApproved},
		{StatusApproved, ProvenanceSnapshotExpired, StatusNeedsReview},
		{StatusApproved, ProvenanceSourceChanged, StatusNeedsReview},
		{StatusApproved, ProvenanceAccessDenied, StatusNeedsReview},
		{StatusApproved, ProvenanceUnverified, StatusNeedsReview},
		{StatusProposed, ProvenanceSnapshotExpired, StatusProposed},
		{StatusRevoked, ProvenanceSnapshotExpired, StatusRevoked},
	} {
		if got := EffectiveStatus(test.declared, test.provenance); got != test.want {
			t.Errorf("EffectiveStatus(%s,%s)=%s, want %s", test.declared, test.provenance, got, test.want)
		}
	}
}

func TestAggregateProvenancePreservesMostActionableFailure(t *testing.T) {
	for _, test := range []struct {
		statuses []ProvenanceStatus
		want     ProvenanceStatus
	}{
		{[]ProvenanceStatus{ProvenanceRetainedSnapshot, ProvenanceRetainedSnapshot}, ProvenanceRetainedSnapshot},
		{[]ProvenanceStatus{ProvenanceRetainedSnapshot, ProvenanceUnverified}, ProvenanceUnverified},
		{[]ProvenanceStatus{ProvenanceSourceChanged, ProvenanceSnapshotExpired}, ProvenanceSnapshotExpired},
		{[]ProvenanceStatus{ProvenanceSnapshotExpired, ProvenanceAccessDenied}, ProvenanceAccessDenied},
		{[]ProvenanceStatus{ProvenanceStatus("unknown")}, ProvenanceUnverified},
		{nil, ProvenanceUnverified},
	} {
		if got := AggregateProvenance(test.statuses); got != test.want {
			t.Errorf("AggregateProvenance(%v)=%s, want %s", test.statuses, got, test.want)
		}
	}
}

func TestValidateProposeRejectsWrongWorktreeAndCallerFieldsAreNotModeled(t *testing.T) {
	worktree := recordUUID(t, "11111111-1111-4111-8111-111111111111")
	other := recordUUID(t, "22222222-2222-4222-8222-222222222222")
	input := ProposeInput{
		ClientMutationID: recordUUID(t, "33333333-3333-4333-8333-333333333333"),
		Kind:             KindDecision,
		Payload:          json.RawMessage(`{"title":"Use stable IDs"}`),
		Reason:           "Explicit project decision",
	}
	if err := ValidatePropose(worktree, "user-1", input); err != nil {
		t.Fatal(err)
	}
	input.SourceRefs = []model.SourceRef{{WorktreeID: other}}
	if err := ValidatePropose(worktree, "user-1", input); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("foreign source reference returned %v; want ErrInvalidRecord", err)
	}
}

func recordUUID(t *testing.T, raw string) model.UUID {
	t.Helper()
	value, err := model.ParseUUID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
