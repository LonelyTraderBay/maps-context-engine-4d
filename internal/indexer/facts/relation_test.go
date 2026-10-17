package facts

import (
	"errors"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestNewRelationFactBindsSiteAndKeepsParallelTargetsDistinct(t *testing.T) {
	source := []byte("func Run() { target() }\n")
	site := model.ByteRange{StartByte: 13, EndByte: 21}
	firstTarget := model.SHA256([]byte("first"))
	secondTarget := model.SHA256([]byte("second"))
	input := RelationInput{
		SourceSymbolID: model.SHA256([]byte("caller")), TargetSymbolID: &firstTarget,
		Kind: RelationCallsDirect, EvidenceKind: EvidenceTypeResolved,
		SitePath: "pkg/run.go", Range: site, Reason: "statically resolved Go call",
	}
	first, err := NewRelationFact(input, source)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := NewRelationFact(input, source)
	if err != nil {
		t.Fatal(err)
	}
	if first.RelationID != repeated.RelationID {
		t.Fatalf("same relation has unstable identity: %s != %s", first.RelationID, repeated.RelationID)
	}
	if err := first.Validate(source); err != nil {
		t.Fatalf("created relation failed validation: %v", err)
	}
	input.TargetSymbolID = &secondTarget
	second, err := NewRelationFact(input, source)
	if err != nil {
		t.Fatal(err)
	}
	if first.RelationID == second.RelationID {
		t.Fatal("parallel targets at one callsite collapsed to one relation ID")
	}
}

func TestNewRelationFactKeepsUnresolvedCallBoundary(t *testing.T) {
	label := "callback"
	source := []byte("func Run() { callback() }\n")
	relation, err := NewRelationFact(RelationInput{
		SourceSymbolID: model.SHA256([]byte("caller")), Kind: RelationUnresolvedCall,
		EvidenceKind: EvidenceUnresolved, SitePath: "pkg/run.go",
		Range:           model.ByteRange{StartByte: 13, EndByte: 23},
		Reason:          "callee is a function value; concrete runtime target is unknown",
		UnresolvedLabel: &label,
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	if relation.TargetSymbolID != nil || relation.UnresolvedLabel == nil || *relation.UnresolvedLabel != label {
		t.Fatalf("unresolved target boundary was not retained: %+v", relation)
	}
}

func TestNewRelationFactRejectsInvalidSiteAndUnresolvedTarget(t *testing.T) {
	source := []byte("func Run() { target() }\n")
	label := "target"
	base := RelationInput{
		SourceSymbolID: model.SHA256([]byte("caller")), Kind: RelationUnresolvedCall,
		EvidenceKind: EvidenceUnresolved, SitePath: "pkg/run.go",
		Range:  model.ByteRange{StartByte: 13, EndByte: 21},
		Reason: "target is not statically known", UnresolvedLabel: &label,
	}
	if _, err := NewRelationFact(base, []byte("package only")); !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("out-of-bounds site accepted: %v", err)
	}
	target := model.SHA256([]byte("must-not-be-set"))
	base.TargetSymbolID = &target
	if _, err := NewRelationFact(base, source); !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("unresolved call with a target was accepted: %v", err)
	}
}
