package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const scxmlStart = `<scxml xmlns="` + SCXMLNamespace + `" version="1.0" initial="root">`

func sourceRefFor(source []byte, path string) model.SourceRef {
	return model.SourceRef{
		WorktreeID:   model.UUID("11111111-1111-4111-8111-111111111111"),
		ProfileID:    model.Hash(strings.Repeat("a", 64)),
		GenerationID: model.UUID("22222222-2222-4222-8222-222222222222"),
		Path:         path,
		ContentHash:  model.SHA256(source),
		Range:        model.ByteRange{StartByte: 0, EndByte: int64(len(source))},
	}
}

func parse(t *testing.T, source string) (Machine, []byte, model.SourceRef) {
	t.Helper()
	data := []byte(source)
	ref := sourceRefFor(data, "workflows/checkout.scxml")
	machine, err := ParseSCXML(ref, data)
	if err != nil {
		t.Fatalf("ParseSCXML() error = %v", err)
	}
	return machine, data, ref
}

func TestParseSCXMLBuildsSourceBoundHierarchicalFactsWithoutExecution(t *testing.T) {
	source := scxmlStart + `<state id="root" initial="pending">
  <onentry><log label="must not be returned">private action body</log></onentry>
  <state id="pending"><transition event="finish" target="done" cond="private_guard()"/></state>
  <final id="done"><onexit><raise event="finished"/></onexit></final>
</state></scxml>`
	machine, data, sourceRef := parse(t, source)
	if !machine.Complete || machine.Reachability != AnalysisUnknown {
		t.Fatalf("machine completeness/reachability = %v/%q", machine.Complete, machine.Reachability)
	}
	if machine.ScopeID == "" || machine.Source != sourceRef || machine.InitialStateID != "root" {
		t.Fatalf("machine identity/source/initial = %+v", machine)
	}
	if len(machine.States) != 3 || len(machine.Transitions) != 1 {
		t.Fatalf("facts = %d states, %d transitions", len(machine.States), len(machine.Transitions))
	}
	if machine.States[0].ParentStateID != "" || machine.States[1].ParentStateID != "root" || machine.States[2].Kind != "final" {
		t.Fatalf("hierarchy/final facts = %+v", machine.States)
	}
	if machine.States[0].InitialStateID != "pending" || machine.Transitions[0].TargetStateIDs[0] != "done" {
		t.Fatalf("initial/target facts = %+v / %+v", machine.States[0], machine.Transitions[0])
	}
	transition := machine.Transitions[0]
	if transition.IdentityKind != "source_offset" || transition.ID == "" || transition.SourceStateID != "pending" || transition.Event != "finish" {
		t.Fatalf("transition identity = %+v", transition)
	}
	if transition.Guard == nil || transition.Guard.AnalysisStatus != AnalysisUnknown || transition.Guard.Source != transition.Source {
		t.Fatalf("guard reference = %+v", transition.Guard)
	}
	if len(machine.States[0].Actions) != 1 || machine.States[0].Actions[0].Name != "log" || machine.States[0].Actions[0].Kind != "onentry" {
		t.Fatalf("entry action references = %+v", machine.States[0].Actions)
	}
	if len(machine.States[2].Actions) != 1 || machine.States[2].Actions[0].Name != "raise" || machine.States[2].Actions[0].Kind != "onexit" {
		t.Fatalf("exit action references = %+v", machine.States[2].Actions)
	}
	for _, ref := range []model.SourceRef{machine.States[0].Source, transition.Source, *&transition.Guard.Source, machine.States[0].Actions[0].Source} {
		if err := ref.ValidateAgainst(data); err != nil {
			t.Fatalf("source reference does not validate: %+v: %v", ref, err)
		}
	}
	encoded, err := json.Marshal(machine)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"private_guard()", "private action body", "must not be returned"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("parser output leaked source expression/content %q", raw)
		}
	}
}

func TestMachineScopeIsBoundToWorktreeProfileGenerationAndPath(t *testing.T) {
	data := []byte(scxmlStart + `<state id="root"/></scxml>`)
	ref := sourceRefFor(data, "a.scxml")
	first, err := ParseSCXML(ref, data)
	if err != nil {
		t.Fatal(err)
	}
	ref.Path = "b.scxml"
	second, err := ParseSCXML(ref, data)
	if err != nil {
		t.Fatal(err)
	}
	if first.ScopeID == second.ScopeID {
		t.Fatal("different source paths shared a machine scope")
	}
}

func TestParseSCXMLRejectsMalformedAndStructurallyInvalidDocuments(t *testing.T) {
	invalidDocuments := []struct {
		name   string
		source string
	}{
		{"duplicate state ID", scxmlStart + `<state id="root"/><state id="root"/></scxml>`},
		{"missing root initial", `<scxml xmlns="` + SCXMLNamespace + `" version="1.0"><state id="root"/></scxml>`},
		{"dangling root initial", `<scxml xmlns="` + SCXMLNamespace + `" version="1.0" initial="missing"><state id="root"/></scxml>`},
		{"compound state missing initial", scxmlStart + `<state id="root"><state id="child"/></state></scxml>`},
		{"initial outside parent", scxmlStart + `<state id="root" initial="sibling"><state id="child"/></state><state id="sibling"/></scxml>`},
		{"missing transition target", scxmlStart + `<state id="root"><transition event="x"/></state></scxml>`},
		{"dangling transition target", scxmlStart + `<state id="root"><transition event="x" target="missing"/></state></scxml>`},
		{"duplicate transition ID", scxmlStart + `<state id="root"><transition id="same" target="root"/><transition id="same" target="root"/></state></scxml>`},
		{"malformed XML", scxmlStart + `<state id="root"></scxml>`},
	}
	for _, test := range invalidDocuments {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(test.source)
			_, err := ParseSCXML(sourceRefFor(data, "invalid.scxml"), data)
			if !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("ParseSCXML() error = %v, want ErrInvalidDocument", err)
			}
		})
	}
}

func TestParseSCXMLReturnsGapsForUnsupportedConstructs(t *testing.T) {
	unsupported := []struct {
		name   string
		source string
		gap    string
	}{
		{"wrong namespace", `<scxml xmlns="urn:unsupported" version="1.0" initial="root"><state id="root"/></scxml>`, "unsupported_root"},
		{"wrong version", `<scxml xmlns="` + SCXMLNamespace + `" version="2.0" initial="root"><state id="root"/></scxml>`, "unsupported_scxml_version"},
		{"parallel", scxmlStart + `<parallel id="p"><state id="hidden"/></parallel><state id="root"/></scxml>`, "unsupported_parallel"},
		{"history", scxmlStart + `<state id="root"><history id="h"/></state></scxml>`, "unsupported_history"},
		{"XInclude", scxmlStart + `<state id="root"><xi:include xmlns:xi="http://www.w3.org/2001/XInclude" href="file:///secret"/></state></scxml>`, "unsupported_element_namespace"},
		{"DTD", `<!DOCTYPE scxml [<!ENTITY hidden "secret">]>` + scxmlStart + `<state id="root"/></scxml>`, "xml_directive_unsupported"},
		{"nested action", scxmlStart + `<state id="root"><onentry><if cond="x"><else/></if></onentry></state></scxml>`, "nested_action_content_unsupported"},
		{"external action reference", scxmlStart + `<state id="root"><onentry><script src="file:///secret"/></onentry></state></scxml>`, "external_action_reference"},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(test.source)
			machine, err := ParseSCXML(sourceRefFor(data, "unsupported.scxml"), data)
			if err != nil {
				t.Fatalf("ParseSCXML() error = %v", err)
			}
			if machine.Complete || !hasGap(machine, test.gap) {
				t.Fatalf("machine should carry gap %q: %+v", test.gap, machine)
			}
			if test.name == "external action reference" {
				if len(machine.States) != 1 || len(machine.States[0].Actions) != 1 || machine.States[0].Actions[0].Source.ValidateAgainst(data) != nil {
					t.Fatalf("external action must remain a local source reference: %+v", machine.States)
				}
			}
		})
	}
}

func TestParseSCXMLEnforcesByteDepthAndReferenceBounds(t *testing.T) {
	oversized := []byte(strings.Repeat(" ", MaxDocumentBytes+1))
	if _, err := ParseSCXML(sourceRefFor(oversized, "large.scxml"), oversized); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("oversized document error = %v", err)
	}

	var source strings.Builder
	source.WriteString(scxmlStart)
	for index := 0; index < MaxXMLDepth; index++ {
		fmt.Fprintf(&source, `<state id="s%d" initial="s%d">`, index, index+1)
	}
	for index := 0; index < MaxXMLDepth; index++ {
		source.WriteString(`</state>`)
	}
	source.WriteString(`</scxml>`)
	data := []byte(source.String())
	if _, err := ParseSCXML(sourceRefFor(data, "deep.scxml"), data); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("over-deep document error = %v", err)
	}

	valid := []byte(scxmlStart + `<state id="root"/></scxml>`)
	badRef := sourceRefFor(valid, "../escape.scxml")
	if _, err := ParseSCXML(badRef, valid); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("invalid source reference error = %v", err)
	}
}

func TestEffectiveModelStatusRequiresApprovalAndCurrentScopeBoundSource(t *testing.T) {
	source := scxmlStart + `<state id="root" name="original"/></scxml>`
	machine, _, _ := parse(t, source)
	record := Version{Kind: KindStateModel, DeclaredStatus: StatusApproved,
		SourceRefs: []model.SourceRef{machine.Source}}
	approved, err := IsEffectiveModelApproval(record, machine)
	if err != nil || !approved {
		t.Fatalf("current approved model = %v, %v", approved, err)
	}
	stateRange := machine.Source
	stateRange.Range = machine.States[0].Source.Range
	record.SourceRefs = []model.SourceRef{stateRange}
	approved, err = IsEffectiveModelApproval(record, machine)
	if err != nil || !approved {
		t.Fatalf("same source snapshot with a narrower reference = %v, %v", approved, err)
	}

	changedScopeRef := machine.Source
	changedScopeRef.GenerationID = model.UUID("33333333-3333-4333-8333-333333333333")
	changedScopeRef.Range = model.ByteRange{StartByte: 0, EndByte: int64(len(source))}
	changedScopeRef.ContentHash = model.SHA256([]byte(source))
	changedScope, err := ParseSCXML(changedScopeRef, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	status, err := EffectiveModelStatus(record, changedScope)
	if err != nil || status != StatusNeedsReview {
		t.Fatalf("changed scope status = %q, %v", status, err)
	}
	approved, err = IsEffectiveModelApproval(record, changedScope)
	if err != nil || approved {
		t.Fatalf("changed scope approval = %v, %v", approved, err)
	}

	changedSourceBytes := []byte(strings.Replace(source, `name="original"`, `name="current"`, 1))
	changedSourceRef := machine.Source
	changedSourceRef.ContentHash = model.SHA256(changedSourceBytes)
	changedSourceRef.Range = model.ByteRange{StartByte: 0, EndByte: int64(len(changedSourceBytes))}
	changedSource, err := ParseSCXML(changedSourceRef, changedSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if changedSource.ScopeID == machine.ScopeID {
		t.Fatal("a changed source hash retained the same machine scope identity")
	}
	status, err = EffectiveModelStatus(record, changedSource)
	if err != nil || status != StatusNeedsReview {
		t.Fatalf("changed source hash status = %q, %v", status, err)
	}

	multipleSources := record
	multipleSources.SourceRefs = []model.SourceRef{machine.Source, changedSourceRef}
	status, err = EffectiveModelStatus(multipleSources, machine)
	if err != nil || status != StatusNeedsReview {
		t.Fatalf("unverified additional source status = %q, %v", status, err)
	}

	proposed := record
	proposed.DeclaredStatus = StatusProposed
	proposed.SourceRefs = nil
	status, err = EffectiveModelStatus(proposed, machine)
	if err != nil || status != StatusProposed {
		t.Fatalf("proposed record status = %q, %v", status, err)
	}
	approved, err = IsEffectiveModelApproval(proposed, machine)
	if err != nil || approved {
		t.Fatalf("proposed record approval = %v, %v", approved, err)
	}

	incomplete := machine
	incomplete.Complete = false
	status, err = EffectiveModelStatus(record, incomplete)
	if err != nil || status != StatusNeedsReview {
		t.Fatalf("incomplete source status = %q, %v", status, err)
	}

	wrongKind := record
	wrongKind.Kind = KindConstraint
	if _, err := EffectiveModelStatus(wrongKind, machine); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("wrong record kind error = %v", err)
	}
}

func hasGap(machine Machine, code string) bool {
	for _, gap := range machine.Gaps {
		if gap.Code == code {
			return true
		}
	}
	return false
}
