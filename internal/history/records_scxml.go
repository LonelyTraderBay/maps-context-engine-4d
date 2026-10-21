// Package history parses a small, source-bound subset of declarative state
// machine documents. It never evaluates guards or executes action content.
package history

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	SCXMLNamespace       = "http://www.w3.org/2005/07/scxml"
	MaxDocumentBytes     = 1 << 20
	MaxStates            = 10_000
	MaxTransitions       = 50_000
	MaxFacts             = 100_000
	MaxXMLDepth          = 64
	MaxGaps              = 128
	maxAttributes        = 64
	maxAttributeValueLen = 4096
)

const AnalysisUnknown = "unknown"

var ErrInvalidDocument = errors.New("invalid state machine document")

type Gap struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Machine is a definition parsed from captured source. It contains no
// instance state and makes no claims about runtime reachability.
type Machine struct {
	Format         string          `json:"format"`
	ScopeID        model.Hash      `json:"scope_id"`
	Source         model.SourceRef `json:"source_ref"`
	InitialStateID string          `json:"initial_state_id,omitempty"`
	States         []State         `json:"states"`
	Transitions    []Transition    `json:"transitions"`
	Gaps           []Gap           `json:"gaps"`
	GapsTruncated  bool            `json:"gaps_truncated"`
	Complete       bool            `json:"complete"`
	Reachability   string          `json:"reachability"`
}

type State struct {
	ID             string          `json:"id"`
	Name           string          `json:"name,omitempty"`
	Kind           string          `json:"kind"`
	ParentStateID  string          `json:"parent_state_id,omitempty"`
	InitialStateID string          `json:"initial_state_id,omitempty"`
	Source         model.SourceRef `json:"source_ref"`
	Actions        []Reference     `json:"actions"`
}

type Transition struct {
	ID             string          `json:"id"`
	IdentityKind   string          `json:"identity_kind"`
	SourceStateID  string          `json:"source_state_id"`
	TargetStateIDs []string        `json:"target_state_ids"`
	Event          string          `json:"event,omitempty"`
	Type           string          `json:"type"`
	Guard          *Reference      `json:"guard,omitempty"`
	Source         model.SourceRef `json:"source_ref"`
}

// Reference points to source bytes only. Name is a declarative XML element
// name, never an instruction to invoke that content.
type Reference struct {
	Kind           string          `json:"kind"`
	Name           string          `json:"name"`
	AnalysisStatus string          `json:"analysis_status"`
	Source         model.SourceRef `json:"source_ref"`
}

type elementKind uint8

const (
	kindIgnored elementKind = iota
	kindRoot
	kindState
	kindFinal
	kindTransition
	kindOnEntry
	kindOnExit
	kindAction
)

type frame struct {
	name            xml.Name
	kind            elementKind
	start           int
	stateIndex      int
	transitionIndex int
	stateID         string
	actionKind      string
	actionName      string
	explicitID      string
}

type parser struct {
	data          []byte
	base          model.SourceRef
	machine       Machine
	stack         []frame
	stateByID     map[string]int
	children      map[string][]string
	seenIDs       map[string]struct{}
	factCount     int
	rootSeen      bool
	rootSupported bool
	incomplete    bool
}

// ParseSCXML parses only a closed structural subset of SCXML 1.0 from bytes
// already captured by the caller. It performs no file, network, process, or
// interpreter operations. A supported document with structural violations
// returns ErrInvalidDocument; unsupported constructs are returned as gaps.
func ParseSCXML(sourceRef model.SourceRef, captured []byte) (Machine, error) {
	if len(captured) == 0 || len(captured) > MaxDocumentBytes {
		return Machine{}, invalid("document must contain 1..%d bytes", MaxDocumentBytes)
	}
	if !utf8.Valid(captured) {
		return Machine{}, invalid("document is not valid UTF-8")
	}
	if err := sourceRef.ValidateAgainst(captured); err != nil {
		return Machine{}, fmt.Errorf("source binding: %w: %v", ErrInvalidDocument, err)
	}
	if sourceRef.Range.StartByte != 0 || sourceRef.Range.EndByte != int64(len(captured)) {
		return Machine{}, invalid("source reference must cover the captured document")
	}
	scopeID, err := model.HashValue("state-machine-scope-v1", map[string]any{
		"worktree_id":   sourceRef.WorktreeID,
		"profile_id":    sourceRef.ProfileID,
		"generation_id": sourceRef.GenerationID,
		"path":          sourceRef.Path,
		"content_hash":  sourceRef.ContentHash,
	})
	if err != nil {
		return Machine{}, fmt.Errorf("derive machine scope: %w: %v", ErrInvalidDocument, err)
	}
	p := &parser{
		data: captured,
		base: sourceRef,
		machine: Machine{
			Format:       "scxml-1.0-subset",
			ScopeID:      scopeID,
			Source:       sourceRef,
			States:       []State{},
			Transitions:  []Transition{},
			Gaps:         []Gap{},
			Reachability: AnalysisUnknown,
		},
		stateByID: make(map[string]int),
		children:  make(map[string][]string),
		seenIDs:   make(map[string]struct{}),
	}
	decoder := xml.NewDecoder(bytes.NewReader(captured))
	decoder.Strict = true
	for {
		before := int(decoder.InputOffset())
		token, tokenErr := decoder.Token()
		if errors.Is(tokenErr, io.EOF) {
			break
		}
		if tokenErr != nil {
			return Machine{}, fmt.Errorf("decode captured XML: %w: %v", ErrInvalidDocument, tokenErr)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if err := p.start(value, before); err != nil {
				return Machine{}, err
			}
		case xml.EndElement:
			if err := p.end(value, int(decoder.InputOffset())); err != nil {
				return Machine{}, err
			}
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" && !p.insideActionContent() && !p.insideIgnored() {
				return Machine{}, invalid("non-whitespace text is unsupported outside action source references")
			}
		case xml.Directive:
			p.addGap("xml_directive_unsupported", "XML directives and DTD declarations are not interpreted")
			p.incomplete = true
			return p.finishPartial(), nil
		case xml.ProcInst:
			if value.Target != "xml" || p.rootSeen || len(p.stack) != 0 {
				p.addGap("processing_instruction_unsupported", "processing instructions are not supported")
				p.incomplete = true
			}
		case xml.Comment:
			// Comments do not contribute machine facts.
		}
	}
	if len(p.stack) != 0 {
		return Machine{}, invalid("document ended with unclosed elements")
	}
	if !p.rootSeen {
		return Machine{}, invalid("document has no SCXML root element")
	}
	if !p.rootSupported {
		return p.finishPartial(), nil
	}
	if err := p.resolveInitialsAndTargets(); err != nil {
		return Machine{}, err
	}
	p.machine.Complete = len(p.machine.Gaps) == 0 && !p.machine.GapsTruncated
	return p.machine, nil
}

func (p *parser) start(element xml.StartElement, start int) error {
	if start < 0 || start > len(p.data) {
		return invalid("XML token offset is outside captured source")
	}
	if len(p.stack)+1 > MaxXMLDepth {
		return invalid("XML nesting exceeds %d levels", MaxXMLDepth)
	}
	if len(element.Attr) > maxAttributes {
		return invalid("element attribute count exceeds %d", maxAttributes)
	}
	for _, attr := range element.Attr {
		if len(attr.Value) > maxAttributeValueLen {
			return invalid("attribute value exceeds %d bytes", maxAttributeValueLen)
		}
	}
	parent := frame{kind: kindIgnored, stateIndex: -1, transitionIndex: -1}
	if len(p.stack) > 0 {
		parent = p.stack[len(p.stack)-1]
	}
	if len(p.stack) > 0 && parent.kind == kindIgnored {
		p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	}
	if parent.kind == kindAction {
		p.addGap("nested_action_content_unsupported", "nested executable action structures are preserved only as an unsupported gap")
		p.incomplete = true
		p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	}
	if !p.rootSeen {
		p.rootSeen = true
		if element.Name.Local != "scxml" || element.Name.Space != SCXMLNamespace {
			p.addGap("unsupported_root", "root element is not SCXML in the supported namespace")
			p.incomplete = true
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		attrs, supported, err := p.attributes(element, "version", "initial", "name")
		if err != nil {
			return err
		}
		if !supported {
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		if attrs["version"] != "1.0" {
			p.addGap("unsupported_scxml_version", "only SCXML version 1.0 is supported")
			p.incomplete = true
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		initial := attrs["initial"]
		if !validXMLID(initial) {
			return invalid("SCXML root must declare one valid initial state ID")
		}
		p.machine.InitialStateID = initial
		p.rootSupported = true
		p.stack = append(p.stack, frame{name: element.Name, kind: kindRoot, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	}
	if element.Name.Space != SCXMLNamespace {
		p.addGap("unsupported_element_namespace", "elements outside the SCXML namespace are not resolved")
		p.incomplete = true
		p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	}
	if parent.kind == kindOnEntry || parent.kind == kindOnExit {
		return p.startAction(element, start, parent)
	}
	switch element.Name.Local {
	case "state", "final":
		if parent.kind != kindRoot && parent.kind != kindState {
			p.addGap("unsupported_state_parent", "state and final nodes must be direct children of scxml or state")
			p.incomplete = true
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		allowed := []string{"id", "name"}
		if element.Name.Local == "state" {
			allowed = append(allowed, "initial")
		}
		attrs, supported, err := p.attributes(element, allowed...)
		if err != nil {
			return err
		}
		if !supported {
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		id := attrs["id"]
		if !validXMLID(id) {
			return invalid("%s must declare one valid ID", element.Name.Local)
		}
		if err := p.claimID(id); err != nil {
			return err
		}
		parentID := ""
		if parent.kind == kindState {
			parentID = p.machine.States[parent.stateIndex].ID
		}
		kind := element.Name.Local
		state := State{ID: id, Name: attrs["name"], Kind: kind, ParentStateID: parentID,
			InitialStateID: attrs["initial"], Actions: []Reference{}}
		if state.InitialStateID != "" && !validXMLID(state.InitialStateID) {
			return invalid("state %q has an invalid initial state ID", id)
		}
		if kind == "final" && attrs["initial"] != "" {
			return invalid("final state %q cannot declare an initial state", id)
		}
		p.factCount++
		if p.factCount > MaxFacts || len(p.machine.States) >= MaxStates {
			return invalid("state fact limit exceeded")
		}
		index := len(p.machine.States)
		p.stateByID[id] = index
		p.machine.States = append(p.machine.States, state)
		p.children[parentID] = append(p.children[parentID], id)
		p.stack = append(p.stack, frame{name: element.Name, kind: kindState, start: start, stateIndex: index, transitionIndex: -1})
		if kind == "final" {
			p.stack[len(p.stack)-1].kind = kindFinal
		}
		return nil
	case "transition":
		if parent.kind != kindState {
			p.addGap("unsupported_transition_parent", "transition nodes are supported only inside state nodes")
			p.incomplete = true
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		attrs, supported, err := p.attributes(element, "id", "event", "target", "cond", "type")
		if err != nil {
			return err
		}
		if !supported {
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		explicitID := attrs["id"]
		if explicitID != "" {
			if !validXMLID(explicitID) {
				return invalid("transition has an invalid ID")
			}
			if err := p.claimID(explicitID); err != nil {
				return err
			}
		}
		targets := strings.Fields(attrs["target"])
		if len(targets) == 0 {
			return invalid("transition must declare at least one target state ID")
		}
		for _, target := range targets {
			if !validXMLID(target) {
				return invalid("transition has an invalid target state ID")
			}
		}
		typeValue := attrs["type"]
		if typeValue == "" {
			typeValue = "external"
		}
		if typeValue != "external" && typeValue != "internal" {
			return invalid("transition type must be external or internal")
		}
		transition := Transition{ID: explicitID, IdentityKind: "declared", SourceStateID: p.machine.States[parent.stateIndex].ID,
			TargetStateIDs: targets, Event: strings.TrimSpace(attrs["event"]), Type: typeValue}
		if _, hasGuard := attrs["cond"]; hasGuard {
			transition.Guard = &Reference{Kind: "guard", Name: "cond", AnalysisStatus: AnalysisUnknown}
		}
		p.factCount++
		if p.factCount > MaxFacts || len(p.machine.Transitions) >= MaxTransitions {
			return invalid("transition fact limit exceeded")
		}
		index := len(p.machine.Transitions)
		p.machine.Transitions = append(p.machine.Transitions, transition)
		p.stack = append(p.stack, frame{name: element.Name, kind: kindTransition, start: start, stateIndex: -1,
			transitionIndex: index, stateID: p.machine.States[parent.stateIndex].ID, explicitID: explicitID})
		return nil
	case "onentry", "onexit":
		if parent.kind != kindState && parent.kind != kindFinal {
			p.addGap("unsupported_action_parent", "entry and exit declarations are supported only inside state or final nodes")
			p.incomplete = true
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		_, supported, err := p.attributes(element)
		if err != nil {
			return err
		}
		if !supported {
			p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
			return nil
		}
		kind := kindOnEntry
		if element.Name.Local == "onexit" {
			kind = kindOnExit
		}
		p.stack = append(p.stack, frame{name: element.Name, kind: kind, start: start, stateIndex: parent.stateIndex, transitionIndex: -1,
			stateID: p.machine.States[parent.stateIndex].ID, actionKind: element.Name.Local})
		return nil
	case "parallel", "history", "initial":
		p.addGap("unsupported_"+element.Name.Local, element.Name.Local+" constructs are not supported")
		p.incomplete = true
		p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	default:
		p.addGap("unsupported_element", "element "+element.Name.Local+" is outside the supported structural subset")
		p.incomplete = true
		p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	}
}

func (p *parser) startAction(element xml.StartElement, start int, parent frame) error {
	if element.Name.Space != SCXMLNamespace {
		p.addGap("unsupported_action_namespace", "action references outside the SCXML namespace are unsupported")
		p.incomplete = true
		p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	}
	switch element.Name.Local {
	case "assign", "cancel", "foreach", "if", "log", "raise", "script", "send":
	default:
		p.addGap("unsupported_action", "action element "+element.Name.Local+" is outside the supported source-reference subset")
		p.incomplete = true
		p.stack = append(p.stack, frame{name: element.Name, kind: kindIgnored, start: start, stateIndex: -1, transitionIndex: -1})
		return nil
	}
	for _, attr := range element.Attr {
		if attr.Name.Space == "http://www.w3.org/XML/1998/namespace" && attr.Name.Local == "base" || attr.Name.Space == "" && (attr.Name.Local == "src" || attr.Name.Local == "href") {
			p.addGap("external_action_reference", "external action references are recorded as a gap and are never followed")
			p.incomplete = true
		}
	}
	p.stack = append(p.stack, frame{name: element.Name, kind: kindAction, start: start, stateIndex: parent.stateIndex,
		transitionIndex: -1, stateID: parent.stateID, actionKind: parent.actionKind, actionName: element.Name.Local})
	return nil
}

func (p *parser) end(element xml.EndElement, end int) error {
	if len(p.stack) == 0 {
		return invalid("closing element has no matching opener")
	}
	current := p.stack[len(p.stack)-1]
	if current.name != element.Name {
		return invalid("XML element nesting is inconsistent")
	}
	p.stack = p.stack[:len(p.stack)-1]
	if current.kind == kindIgnored {
		return nil
	}
	ref, err := p.ref(current.start, end)
	if err != nil {
		return err
	}
	switch current.kind {
	case kindState, kindFinal:
		p.machine.States[current.stateIndex].Source = ref
	case kindTransition:
		transition := &p.machine.Transitions[current.transitionIndex]
		transition.Source = ref
		if transition.ID == "" {
			transition.ID = fmt.Sprintf("transition@%d", current.start)
			transition.IdentityKind = "source_offset"
		}
		if transition.Guard != nil {
			transition.Guard.Source = ref
		}
	case kindAction:
		state := &p.machine.States[current.stateIndex]
		state.Actions = append(state.Actions, Reference{Kind: current.actionKind, Name: current.actionName,
			AnalysisStatus: AnalysisUnknown, Source: ref})
		p.factCount++
		if p.factCount > MaxFacts {
			return invalid("action reference fact limit exceeded")
		}
	}
	return nil
}

func (p *parser) attributes(element xml.StartElement, allowed ...string) (map[string]string, bool, error) {
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	attrs := make(map[string]string, len(element.Attr))
	unsupported := false
	for _, attr := range element.Attr {
		if attr.Name.Space == "xmlns" || attr.Name.Space == "" && attr.Name.Local == "xmlns" {
			continue
		}
		if attr.Name.Space != "" {
			unsupported = true
			continue
		}
		if _, ok := allow[attr.Name.Local]; !ok {
			unsupported = true
			continue
		}
		if _, exists := attrs[attr.Name.Local]; exists {
			return nil, false, invalid("duplicate %s attribute", attr.Name.Local)
		}
		attrs[attr.Name.Local] = attr.Value
	}
	if unsupported {
		p.addGap("unsupported_attribute", "element "+element.Name.Local+" has attributes outside the supported subset")
		p.incomplete = true
	}
	return attrs, !unsupported, nil
}

func (p *parser) claimID(id string) error {
	if _, exists := p.seenIDs[id]; exists {
		return invalid("duplicate ID %q within machine scope", id)
	}
	p.seenIDs[id] = struct{}{}
	return nil
}

func (p *parser) resolveInitialsAndTargets() error {
	if !p.initialTargetExists("", p.machine.InitialStateID) {
		if !p.incomplete {
			return invalid("root initial state %q is not a declared direct child state", p.machine.InitialStateID)
		}
		p.addGap("unresolved_initial_due_to_gap", "root initial target cannot be resolved in the supported subset")
	}
	for index := range p.machine.States {
		state := &p.machine.States[index]
		children := p.children[state.ID]
		if state.Kind == "final" && len(children) > 0 {
			return invalid("final state %q cannot contain nested states", state.ID)
		}
		if len(children) == 0 {
			if state.InitialStateID != "" {
				return invalid("atomic state %q cannot declare an initial child state", state.ID)
			}
			continue
		}
		if state.InitialStateID == "" {
			if !p.incomplete {
				return invalid("compound state %q must declare an initial child state", state.ID)
			}
			p.addGap("missing_nested_initial_due_to_gap", "compound state initial cannot be verified in the supported subset")
			continue
		}
		if !p.initialTargetExists(state.ID, state.InitialStateID) {
			if !p.incomplete {
				return invalid("initial state %q is not a direct child of %q", state.InitialStateID, state.ID)
			}
			p.addGap("unresolved_nested_initial_due_to_gap", "nested initial target cannot be resolved in the supported subset")
		}
	}
	for _, transition := range p.machine.Transitions {
		for _, target := range transition.TargetStateIDs {
			if _, exists := p.stateByID[target]; !exists {
				if !p.incomplete {
					return invalid("transition %q targets undeclared state %q", transition.ID, target)
				}
				p.addGap("unresolved_transition_target_due_to_gap", "transition target cannot be resolved in the supported subset")
			}
		}
		if len(transition.TargetStateIDs) > 1 {
			p.addGap("multiple_targets_unsupported", "multiple transition targets are not interpreted")
			p.incomplete = true
		}
	}
	return nil
}

func (p *parser) initialTargetExists(parentID, targetID string) bool {
	index, exists := p.stateByID[targetID]
	if !exists {
		return false
	}
	state := p.machine.States[index]
	return state.Kind == "state" && state.ParentStateID == parentID
}

func (p *parser) ref(start, end int) (model.SourceRef, error) {
	if start < 0 || end < start || end > len(p.data) {
		return model.SourceRef{}, invalid("source range is outside captured bytes")
	}
	ref := p.base
	ref.Range = model.ByteRange{StartByte: int64(start), EndByte: int64(end)}
	if err := ref.ValidateAgainst(p.data); err != nil {
		return model.SourceRef{}, fmt.Errorf("derived source reference: %w: %v", ErrInvalidDocument, err)
	}
	return ref, nil
}

func (p *parser) addGap(code, message string) {
	p.incomplete = true
	if len(p.machine.Gaps) >= MaxGaps {
		p.machine.GapsTruncated = true
		return
	}
	p.machine.Gaps = append(p.machine.Gaps, Gap{Code: code, Message: message})
}

func (p *parser) insideActionContent() bool {
	for index := len(p.stack) - 1; index >= 0; index-- {
		if p.stack[index].kind == kindAction {
			return true
		}
		if p.stack[index].kind == kindIgnored {
			return false
		}
	}
	return false
}

func (p *parser) insideIgnored() bool {
	for index := len(p.stack) - 1; index >= 0; index-- {
		if p.stack[index].kind == kindIgnored {
			return true
		}
	}
	return false
}

func (p *parser) finishPartial() Machine {
	p.machine.Complete = false
	return p.machine
}

func validXMLID(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for index, char := range value {
		if index == 0 {
			if char != '_' && !unicode.IsLetter(char) {
				return false
			}
			continue
		}
		if char == '_' || char == '-' || char == '.' || unicode.IsLetter(char) || unicode.IsDigit(char) ||
			unicode.Is(unicode.Mn, char) || unicode.Is(unicode.Mc, char) || unicode.Is(unicode.Pc, char) {
			continue
		}
		return false
	}
	return true
}

func invalid(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDocument, fmt.Sprintf(format, values...))
}
