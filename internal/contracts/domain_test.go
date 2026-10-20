package contracts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	testWorktreeID   = model.UUID("11111111-1111-4111-8111-111111111111")
	testGenerationID = model.UUID("22222222-2222-4222-8222-222222222222")
	testProfileID    = model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	testContentHash  = model.Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func TestDomainEntityRequiresQualifiedIdentityAndEvidence(t *testing.T) {
	entity := validDomainEntity()
	if err := entity.Validate(); err != nil {
		t.Fatalf("valid domain entity rejected: %v", err)
	}

	entity.EntityID = "same-name"
	entity.GenerationID = "33333333-3333-4333-8333-333333333333"
	if err := entity.Validate(); err != nil {
		t.Fatalf("same-name entity in another generation must remain valid: %v", err)
	}

	entity.SourceRefs = nil
	if err := entity.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("missing source evidence error = %v, want invalid contract", err)
	}
	entity = validDomainEntity()
	entity.Plane = "observed_runtime"
	if err := entity.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unknown plane error = %v, want invalid contract", err)
	}
	entity = validDomainEntity()
	entity.Payload = json.RawMessage(`{"x":1} trailing`)
	if err := entity.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("invalid payload error = %v, want invalid contract", err)
	}
	entity = validDomainEntity()
	entity.Payload = json.RawMessage(`{"x":1,"x":2}`)
	if err := entity.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("duplicate payload key error = %v, want invalid contract", err)
	}
}

func TestSourceReferenceIsExactlySourceOrApprovedRecord(t *testing.T) {
	if err := (SourceReference{Source: ptr(validSourceRef())}).Validate(); err != nil {
		t.Fatalf("valid source reference rejected: %v", err)
	}
	if err := (SourceReference{Record: ptr(RecordRef{
		RecordID: model.UUID("33333333-3333-4333-8333-333333333333"), Version: 2,
	})}).Validate(); err != nil {
		t.Fatalf("valid approved record reference rejected: %v", err)
	}
	if err := (SourceReference{}).Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("empty reference error = %v, want invalid contract", err)
	}
	if err := (SourceReference{Source: ptr(validSourceRef()), Record: ptr(RecordRef{
		RecordID: model.UUID("33333333-3333-4333-8333-333333333333"), Version: 1,
	})}).Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("dual reference error = %v, want invalid contract", err)
	}
	bad := validSourceRef()
	bad.Path = "../outside.go"
	if err := (SourceReference{Source: &bad}).Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("traversal source path error = %v, want invalid contract", err)
	}
}

func TestSourceReferenceCanVerifyCapturedBytes(t *testing.T) {
	source := []byte("captured source")
	ref := validSourceRef()
	ref.ContentHash = model.SHA256(source)
	ref.Range = model.ByteRange{StartByte: 0, EndByte: int64(len(source))}
	if err := (SourceReference{Source: &ref}).ValidateAgainst(source); err != nil {
		t.Fatalf("matching source bytes rejected: %v", err)
	}
	if err := (SourceReference{Source: &ref}).ValidateAgainst([]byte("different source")); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("mismatched source bytes error = %v, want invalid contract", err)
	}
}

func TestContractFieldKeepsDeclaredJSONAndChecksDirection(t *testing.T) {
	field := ContractField{
		GenerationID: testGenerationID,
		EntityID:     "order",
		FieldPath:    "amount",
		Direction:    DirectionInput,
		Declaration:  json.RawMessage(`{"type":"decimal","currency":"USD","precision":"0.0001"}`),
	}
	if err := field.Validate(); err != nil {
		t.Fatalf("valid field rejected: %v", err)
	}
	if !strings.Contains(string(field.Declaration), `"precision":"0.0001"`) {
		t.Fatal("field declaration precision was not retained")
	}
	field.Direction = "unknown"
	if err := field.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unknown direction error = %v, want invalid contract", err)
	}
}

func TestEntityLinkRequiresTypedEndpointsAndEvidence(t *testing.T) {
	link := validEntityLink()
	if err := link.Validate(); err != nil {
		t.Fatalf("valid entity link rejected: %v", err)
	}
	link.Source.EntityID = ptr(model.Identifier("source-entity"))
	if err := link.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("dual source endpoint error = %v, want invalid contract", err)
	}
	link = validEntityLink()
	link.Target = Endpoint{}
	if err := link.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("empty target endpoint error = %v, want invalid contract", err)
	}
	link = validEntityLink()
	link.Evidence = nil
	if err := link.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("missing evidence error = %v, want invalid contract", err)
	}
	link = validEntityLink()
	link.ResolutionStatus = "exact_without_scope"
	if err := link.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("unknown resolution error = %v, want invalid contract", err)
	}
}

func TestStateTransitionRetainsDeclarationsWithoutExecutingThem(t *testing.T) {
	transition := StateTransition{
		GenerationID:    testGenerationID,
		MachineEntityID: "machine",
		TransitionID:    "approve",
		FromEntityID:    "pending",
		ToEntityID:      "approved",
		GuardRef:        json.RawMessage(`{"kind":"source"}`),
		EffectRefs:      json.RawMessage(`[]`),
		Evidence:        []EvidenceRef{validEvidence()},
	}
	if err := transition.Validate(); err != nil {
		t.Fatalf("valid transition declaration rejected: %v", err)
	}
	transition.EffectRefs = json.RawMessage(`{`)
	if err := transition.Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("invalid effect refs error = %v, want invalid contract", err)
	}
}

func validDomainEntity() DomainEntity {
	return DomainEntity{
		GenerationID: testGenerationID,
		EntityID:     "order",
		Kind:         "business_entity",
		QualifiedKey: "sales.order",
		Plane:        PlaneIntended,
		Payload:      json.RawMessage(`{"name":"order"}`),
		SourceRefs:   []SourceReference{{Source: ptr(validSourceRef())}},
	}
}

func validEntityLink() EntityLink {
	return EntityLink{
		GenerationID:     testGenerationID,
		LinkID:           "creates-order",
		Source:           Endpoint{SymbolID: ptr(model.Identifier("CreateOrder"))},
		Target:           Endpoint{EntityID: ptr(model.Identifier("order"))},
		Kind:             "creates",
		Evidence:         []EvidenceRef{validEvidence()},
		ResolutionStatus: ResolutionCandidate,
	}
}

func validEvidence() EvidenceRef {
	return EvidenceRef{
		EvidenceID: "source-site-1",
		Origin:     OriginStaticAnalyzer,
		Kind:       EvidenceExplicitDocument,
		SourceRefs: []model.SourceRef{validSourceRef()},
	}
}

func validSourceRef() model.SourceRef {
	return model.SourceRef{
		WorktreeID:   testWorktreeID,
		ProfileID:    testProfileID,
		GenerationID: testGenerationID,
		Path:         "internal/orders/create.go",
		ContentHash:  testContentHash,
		Range:        model.ByteRange{StartByte: 4, EndByte: 16},
	}
}

func ptr[T any](value T) *T { return &value }
