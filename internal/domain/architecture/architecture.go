// Package architecture compares approved architecture metadata with
// caller-supplied dependency evidence. It never scans source trees or edits
// architecture automatically.
package architecture

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	MaxComponents      = 512
	MaxRules           = 2048
	MaxDependencies    = 8192
	MaxProviderScopes  = 256
	MaxExemptions      = 2048
	MaxSourceRefs      = 128
	MaxIdentifierBytes = 128
	MaxStatementBytes  = 4096
	MaxSourcePathBytes = 4096
	MaxAllowedTargets  = 512
)

var ErrInvalidArchitecture = errors.New("invalid architecture evidence")

type ApprovalState string

const (
	ApprovalDraft       ApprovalState = "draft"
	ApprovalApproved    ApprovalState = "approved"
	ApprovalNeedsReview ApprovalState = "needs_review"
	ApprovalSuperseded  ApprovalState = "superseded"
)

type ComponentState string

const (
	StateImplemented   ComponentState = "implemented"
	StatePlanned       ComponentState = "planned"
	StateObservedInRun ComponentState = "observed_in_run"
	StateUnsupported   ComponentState = "unsupported"
	StateNotSeen       ComponentState = "not_seen"
)

type CoverageState string

const (
	CoverageComplete CoverageState = "complete_for_declared_scope"
	CoveragePartial  CoverageState = "partial"
	CoverageUnknown  CoverageState = "unknown"
)

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type ExemptionStatus string

const (
	ExemptionStatusExpired     ExemptionStatus = "expired"
	ExemptionStatusNotApproved ExemptionStatus = "not_approved"
)

type Verdict string

const (
	VerdictPass       Verdict = "pass"
	VerdictDrift      Verdict = "drift"
	VerdictIncomplete Verdict = "incomplete"
)

// IntendedArchitecture is an explicit caller-supplied decision record. The
// evaluator does not authenticate ApprovalState; the caller remains the
// authority that supplies the approved decision and its source references.
type IntendedArchitecture struct {
	ID                     string
	Version                string
	Scope                  string
	Statement              string
	Approval               ApprovalState
	InvalidationTriggers   []string
	ReplacedBy             string
	SourceRefs             []model.SourceRef
	RequiredProviderScopes []string
	Components             []Component
	Rules                  []AllowedDependencyRule
	Exemptions             []Exemption
}

// Component is an exact, producer-assigned component identity and its
// explicitly stated status. Names are never matched heuristically.
type Component struct {
	ID            string
	ProviderScope string
	State         ComponentState
	SourceRefs    []model.SourceRef
}

// AllowedDependencyRule closes the observed outgoing dependency set for one
// component in one provider scope. Targets not listed in AllowedTo are drift
// only when the rule itself and the observed edge both have source evidence.
type AllowedDependencyRule struct {
	ID            string
	ProviderScope string
	From          string
	AllowedTo     []string
	Severity      Severity
	Rationale     string
	SourceRefs    []model.SourceRef
}

// Exemption applies to one exact dependency edge and rule until the stated UTC
// calendar date. An unapproved or expired exemption never suppresses drift.
type Exemption struct {
	ID            string
	RuleID        string
	ProviderScope string
	From          string
	To            string
	ExpiresOn     string
	Approved      bool
	Rationale     string
	SourceRefs    []model.SourceRef
}

type ProviderScopeCoverage struct {
	ProviderScope string
	Coverage      CoverageState
	SourceRefs    []model.SourceRef
}

type ObservedGraph struct {
	Coverage        CoverageState
	CoverageSources []model.SourceRef
	ProviderScopes  []ProviderScopeCoverage
	Components      []Component
	Dependencies    []Dependency
}

// Dependency carries the exact witness path and the source references that
// bind the observed edge. Source bytes remain opaque to this package.
type Dependency struct {
	ProviderScope string
	From          string
	To            string
	WitnessPath   string
	SourceRefs    []model.SourceRef
}

type Report struct {
	Verdict                   Verdict
	Coverage                  CoverageState
	SourceContentVerification string
	ComponentStatuses         []ComponentStatus
	Violations                []Violation
	AppliedExemptions         []AppliedExemption
	Gaps                      []Gap
}

type ComponentStatus struct {
	ID                 string
	ProviderScope      string
	IntendedState      ComponentState
	ObservedState      ComponentState
	ObservedMapped     bool
	IntendedSourceRefs []model.SourceRef
	ObservedSourceRefs []model.SourceRef
}

type Violation struct {
	RuleID             string
	ProviderScope      string
	From               string
	To                 string
	WitnessPath        string
	Severity           Severity
	Rationale          string
	ExemptionID        string
	ExemptionExpiresOn string
	ExemptionStatus    ExemptionStatus
	SourceRefs         []model.SourceRef
}

type AppliedExemption struct {
	ID                string
	RuleID            string
	ProviderScope     string
	From              string
	To                string
	ExpiresOn         string
	WitnessPath       string
	WitnessSourceRefs []model.SourceRef
	ExemptionSources  []model.SourceRef
}

type Gap struct {
	Code          string
	ProviderScope string
	ComponentID   string
	Reason        string
}

// Evaluate compares explicit allowed-dependency rules with supplied observed
// edges. asOf is required so exemption expiry is deterministic and testable.
// PASS means only that the supplied, declared scopes were complete and matched.
func Evaluate(intended IntendedArchitecture, observed ObservedGraph, asOf time.Time) (Report, error) {
	if err := validateIntended(intended); err != nil {
		return Report{}, err
	}
	if err := validateObserved(observed); err != nil {
		return Report{}, err
	}
	if asOf.IsZero() {
		return Report{}, invalid("evaluation time is required")
	}

	report := Report{
		Verdict:                   VerdictIncomplete,
		Coverage:                  observed.Coverage,
		SourceContentVerification: "structural_only",
		ComponentStatuses:         make([]ComponentStatus, 0, len(intended.Components)),
		Violations:                []Violation{},
		AppliedExemptions:         []AppliedExemption{},
		Gaps:                      []Gap{},
	}

	switch intended.Approval {
	case ApprovalApproved:
	case ApprovalNeedsReview:
		report.Gaps = append(report.Gaps, Gap{Code: "architecture_needs_review", Reason: "the intended architecture is marked needs_review and cannot be enforced as current policy"})
	case ApprovalSuperseded:
		report.Gaps = append(report.Gaps, Gap{Code: "architecture_superseded", Reason: "the intended architecture has been superseded"})
	default:
		report.Gaps = append(report.Gaps, Gap{Code: "architecture_not_approved", Reason: "only an explicitly approved intended architecture can be evaluated as policy"})
	}

	requiredScopes := make(map[string]struct{}, len(intended.RequiredProviderScopes))
	for _, scope := range intended.RequiredProviderScopes {
		requiredScopes[scope] = struct{}{}
	}
	scopeCoverage := make(map[string]ProviderScopeCoverage, len(observed.ProviderScopes))
	for _, scope := range observed.ProviderScopes {
		scopeCoverage[scope.ProviderScope] = scope
		if _, required := requiredScopes[scope.ProviderScope]; !required {
			report.Gaps = append(report.Gaps, Gap{Code: "provider_scope_unmodeled", ProviderScope: scope.ProviderScope, Reason: "observed provider scope is not declared by the intended architecture"})
		}
	}
	coverageComplete := observed.Coverage == CoverageComplete && len(observed.CoverageSources) > 0
	if observed.Coverage == CoverageComplete && len(observed.CoverageSources) == 0 {
		report.Gaps = append(report.Gaps, Gap{Code: "coverage_unbound", Reason: "complete graph coverage has no source-bound coverage manifest"})
		coverageComplete = false
	}
	if observed.Coverage != CoverageComplete {
		report.Gaps = append(report.Gaps, Gap{Code: "graph_coverage_incomplete", Reason: "the supplied observed graph is partial or unknown"})
	}
	for _, required := range intended.RequiredProviderScopes {
		coverage, exists := scopeCoverage[required]
		if !exists {
			report.Gaps = append(report.Gaps, Gap{Code: "provider_scope_missing", ProviderScope: required, Reason: "a required provider scope has no observed coverage record"})
			coverageComplete = false
			continue
		}
		if coverage.Coverage != CoverageComplete || len(coverage.SourceRefs) == 0 {
			report.Gaps = append(report.Gaps, Gap{Code: "provider_scope_coverage_incomplete", ProviderScope: required, Reason: "required provider scope coverage is incomplete or lacks source evidence"})
			coverageComplete = false
		}
	}
	if !coverageComplete && observed.Coverage == CoverageComplete {
		report.Coverage = CoveragePartial
	}

	intendedComponents := make(map[string]Component, len(intended.Components))
	for _, component := range intended.Components {
		intendedComponents[component.ID] = component
	}
	observedComponents := make(map[string]Component, len(observed.Components))
	for _, component := range observed.Components {
		observedComponents[component.ID] = component
	}
	for _, component := range intended.Components {
		status := ComponentStatus{
			ID: component.ID, ProviderScope: component.ProviderScope,
			IntendedState: component.State, IntendedSourceRefs: cloneSourceRefs(component.SourceRefs),
		}
		observedComponent, exists := observedComponents[component.ID]
		if !exists {
			report.Gaps = append(report.Gaps, Gap{Code: "component_mapping_missing", ProviderScope: component.ProviderScope, ComponentID: component.ID, Reason: "no exact observed component mapping was supplied; this does not prove the component is absent"})
		} else {
			status.ObservedMapped = true
			status.ObservedState = observedComponent.State
			status.ObservedSourceRefs = cloneSourceRefs(observedComponent.SourceRefs)
			if observedComponent.ProviderScope != component.ProviderScope {
				report.Gaps = append(report.Gaps, Gap{Code: "component_scope_mismatch", ProviderScope: observedComponent.ProviderScope, ComponentID: component.ID, Reason: "observed component provider scope does not match its exact intended mapping"})
			}
		}
		report.ComponentStatuses = append(report.ComponentStatuses, status)
	}
	for _, component := range observed.Components {
		if _, exists := intendedComponents[component.ID]; !exists {
			report.Gaps = append(report.Gaps, Gap{Code: "observed_component_unmapped", ProviderScope: component.ProviderScope, ComponentID: component.ID, Reason: "observed component has no exact intended component identity"})
		}
	}

	rules := make(map[string]AllowedDependencyRule, len(intended.Rules))
	for _, rule := range intended.Rules {
		rules[ruleKey(rule.ProviderScope, rule.From)] = rule
	}
	for _, component := range intended.Components {
		if _, exists := rules[ruleKey(component.ProviderScope, component.ID)]; !exists {
			report.Gaps = append(report.Gaps, Gap{Code: "allowed_dependency_rule_missing", ProviderScope: component.ProviderScope, ComponentID: component.ID, Reason: "the intended component has no explicit allowed-dependency rule"})
		}
	}

	exemptions := make(map[string]Exemption, len(intended.Exemptions))
	for _, exemption := range intended.Exemptions {
		exemptions[exemptionKey(exemption.RuleID, exemption.ProviderScope, exemption.From, exemption.To)] = exemption
	}
	for _, dependency := range observed.Dependencies {
		witnessBound := false
		for _, source := range dependency.SourceRefs {
			if source.Path == dependency.WitnessPath {
				witnessBound = true
				break
			}
		}
		if !witnessBound {
			report.Gaps = append(report.Gaps, Gap{Code: "dependency_witness_unbound", ProviderScope: dependency.ProviderScope, ComponentID: dependency.From, Reason: "dependency witness path is not bound to a supplied source reference"})
			continue
		}
		if _, required := requiredScopes[dependency.ProviderScope]; !required {
			report.Gaps = append(report.Gaps, Gap{Code: "dependency_provider_scope_unmodeled", ProviderScope: dependency.ProviderScope, ComponentID: dependency.From, Reason: "dependency evidence belongs to an undeclared provider scope"})
			continue
		}
		component, exists := intendedComponents[dependency.From]
		if !exists || component.ProviderScope != dependency.ProviderScope {
			report.Gaps = append(report.Gaps, Gap{Code: "dependency_source_unmapped", ProviderScope: dependency.ProviderScope, ComponentID: dependency.From, Reason: "dependency source has no exact intended component mapping in this provider scope"})
			continue
		}
		rule, exists := rules[ruleKey(dependency.ProviderScope, dependency.From)]
		if !exists {
			continue // the missing-rule gap above prevents a PASS claim
		}
		if !hasString(rule.AllowedTo, dependency.To) {
			exemption, hasExemption := exemptions[exemptionKey(rule.ID, dependency.ProviderScope, dependency.From, dependency.To)]
			if hasExemption && exemption.Approved && asOf.UTC().Format("2006-01-02") <= exemption.ExpiresOn {
				report.AppliedExemptions = append(report.AppliedExemptions, AppliedExemption{
					ID: exemption.ID, RuleID: exemption.RuleID, ProviderScope: exemption.ProviderScope,
					From: exemption.From, To: exemption.To, ExpiresOn: exemption.ExpiresOn,
					WitnessPath: dependency.WitnessPath, WitnessSourceRefs: cloneSourceRefs(dependency.SourceRefs),
					ExemptionSources: cloneSourceRefs(exemption.SourceRefs),
				})
				continue
			}
			var exemptionStatus ExemptionStatus
			if hasExemption {
				exemptionStatus = ExemptionStatusNotApproved
				if exemption.Approved {
					exemptionStatus = ExemptionStatusExpired
				}
			}
			report.Violations = append(report.Violations, Violation{
				RuleID: rule.ID, ProviderScope: dependency.ProviderScope, From: dependency.From, To: dependency.To,
				WitnessPath: dependency.WitnessPath, Severity: rule.Severity, Rationale: rule.Rationale,
				ExemptionID: exemption.ID, ExemptionExpiresOn: exemption.ExpiresOn,
				ExemptionStatus: exemptionStatus, SourceRefs: cloneSourceRefs(dependency.SourceRefs),
			})
		}
	}

	sort.Slice(report.ComponentStatuses, func(i, j int) bool { return report.ComponentStatuses[i].ID < report.ComponentStatuses[j].ID })
	sort.Slice(report.Violations, func(i, j int) bool {
		a, b := report.Violations[i], report.Violations[j]
		return tuple(a.ProviderScope, a.From, a.To, a.WitnessPath) < tuple(b.ProviderScope, b.From, b.To, b.WitnessPath)
	})
	sort.Slice(report.AppliedExemptions, func(i, j int) bool {
		a, b := report.AppliedExemptions[i], report.AppliedExemptions[j]
		return tuple(a.ProviderScope, a.From, a.To, a.WitnessPath) < tuple(b.ProviderScope, b.From, b.To, b.WitnessPath)
	})
	sort.Slice(report.Gaps, func(i, j int) bool {
		a, b := report.Gaps[i], report.Gaps[j]
		return tuple(a.Code, a.ProviderScope, a.ComponentID, a.Reason) < tuple(b.Code, b.ProviderScope, b.ComponentID, b.Reason)
	})
	if report.Coverage != CoverageComplete || len(report.Gaps) > 0 {
		report.Verdict = VerdictIncomplete
	} else if len(report.Violations) > 0 {
		report.Verdict = VerdictDrift
	} else {
		report.Verdict = VerdictPass
	}
	return report, nil
}

func validateIntended(intended IntendedArchitecture) error {
	if !validToken(intended.ID) || !validToken(intended.Version) || !validToken(intended.Scope) || !validText(intended.Statement, MaxStatementBytes) {
		return invalid("architecture identity, scope, version, or statement is invalid")
	}
	switch intended.Approval {
	case ApprovalDraft, ApprovalApproved, ApprovalNeedsReview:
		if intended.ReplacedBy != "" {
			return invalid("replaced_by is only valid for a superseded architecture")
		}
	case ApprovalSuperseded:
		if !validToken(intended.ReplacedBy) {
			return invalid("superseded architecture requires replaced_by")
		}
	default:
		return invalid("architecture approval state is invalid")
	}
	if len(intended.RequiredProviderScopes) == 0 || len(intended.RequiredProviderScopes) > MaxProviderScopes || len(intended.Components) == 0 || len(intended.Components) > MaxComponents || len(intended.Rules) == 0 || len(intended.Rules) > MaxRules || len(intended.Exemptions) > MaxExemptions || len(intended.InvalidationTriggers) > 64 {
		return invalid("architecture scope or record count is outside the supported limit")
	}
	if err := validateSourceRefs(intended.SourceRefs, true); err != nil {
		return err
	}
	for _, trigger := range intended.InvalidationTriggers {
		if !validText(trigger, MaxStatementBytes) {
			return invalid("invalidation trigger is invalid")
		}
	}
	scopes := make(map[string]struct{}, len(intended.RequiredProviderScopes))
	for _, scope := range intended.RequiredProviderScopes {
		if !validToken(scope) {
			return invalid("required provider scope is invalid")
		}
		if _, exists := scopes[scope]; exists {
			return invalid("required provider scopes must be unique")
		}
		scopes[scope] = struct{}{}
	}
	components := make(map[string]Component, len(intended.Components))
	for _, component := range intended.Components {
		if !validComponent(component) {
			return invalid("intended component identity, state, scope, or source evidence is invalid")
		}
		if _, exists := scopes[component.ProviderScope]; !exists {
			return invalid("intended component uses an undeclared provider scope")
		}
		if _, exists := components[component.ID]; exists {
			return invalid("intended component IDs must be unique")
		}
		components[component.ID] = component
	}
	rulesByID := make(map[string]AllowedDependencyRule, len(intended.Rules))
	ruleKeys := make(map[string]struct{}, len(intended.Rules))
	for _, rule := range intended.Rules {
		if !validToken(rule.ID) || !validToken(rule.ProviderScope) || !validToken(rule.From) || !validSeverity(rule.Severity) || !validText(rule.Rationale, MaxStatementBytes) || len(rule.AllowedTo) > MaxAllowedTargets {
			return invalid("allowed-dependency rule identity, rationale, severity, or target count is invalid")
		}
		if _, exists := scopes[rule.ProviderScope]; !exists {
			return invalid("allowed-dependency rule uses an undeclared provider scope")
		}
		component, exists := components[rule.From]
		if !exists || component.ProviderScope != rule.ProviderScope {
			return invalid("allowed-dependency rule source is not an intended component in its provider scope")
		}
		if err := validateSourceRefs(rule.SourceRefs, true); err != nil {
			return err
		}
		if _, exists := rulesByID[rule.ID]; exists {
			return invalid("allowed-dependency rule IDs must be unique")
		}
		key := ruleKey(rule.ProviderScope, rule.From)
		if _, exists := ruleKeys[key]; exists {
			return invalid("only one closed allowed-dependency rule is permitted per source component and scope")
		}
		ruleKeys[key] = struct{}{}
		rulesByID[rule.ID] = rule
		seenTargets := make(map[string]struct{}, len(rule.AllowedTo))
		for _, target := range rule.AllowedTo {
			if !validToken(target) {
				return invalid("allowed dependency target is invalid")
			}
			if _, exists := seenTargets[target]; exists {
				return invalid("allowed dependency targets must be unique")
			}
			seenTargets[target] = struct{}{}
		}
	}
	exemptionKeys := make(map[string]struct{}, len(intended.Exemptions))
	for _, exemption := range intended.Exemptions {
		if !validToken(exemption.ID) || !validToken(exemption.RuleID) || !validToken(exemption.ProviderScope) || !validToken(exemption.From) || !validToken(exemption.To) || !validText(exemption.Rationale, MaxStatementBytes) {
			return invalid("exemption identity or rationale is invalid")
		}
		rule, exists := rulesByID[exemption.RuleID]
		if !exists || rule.ProviderScope != exemption.ProviderScope || rule.From != exemption.From || hasString(rule.AllowedTo, exemption.To) {
			return invalid("exemption must reference a matching rule and a disallowed edge")
		}
		if parsed, err := time.Parse("2006-01-02", exemption.ExpiresOn); err != nil || parsed.Format("2006-01-02") != exemption.ExpiresOn {
			return invalid("exemption expiry must be a canonical UTC calendar date")
		}
		if err := validateSourceRefs(exemption.SourceRefs, true); err != nil {
			return err
		}
		key := exemptionKey(exemption.RuleID, exemption.ProviderScope, exemption.From, exemption.To)
		if _, exists := exemptionKeys[key]; exists {
			return invalid("duplicate exemption for the same rule and edge")
		}
		exemptionKeys[key] = struct{}{}
	}
	return nil
}

func validateObserved(observed ObservedGraph) error {
	if !validCoverage(observed.Coverage) || len(observed.ProviderScopes) > MaxProviderScopes || len(observed.Components) > MaxComponents || len(observed.Dependencies) > MaxDependencies {
		return invalid("observed graph coverage or record count is invalid")
	}
	if err := validateSourceRefs(observed.CoverageSources, false); err != nil {
		return err
	}
	scopes := make(map[string]struct{}, len(observed.ProviderScopes))
	for _, scope := range observed.ProviderScopes {
		if !validToken(scope.ProviderScope) || !validCoverage(scope.Coverage) {
			return invalid("provider-scope coverage identity or status is invalid")
		}
		if _, exists := scopes[scope.ProviderScope]; exists {
			return invalid("provider-scope coverage records must be unique")
		}
		scopes[scope.ProviderScope] = struct{}{}
		if err := validateSourceRefs(scope.SourceRefs, false); err != nil {
			return err
		}
	}
	components := make(map[string]struct{}, len(observed.Components))
	for _, component := range observed.Components {
		if !validComponent(component) {
			return invalid("observed component identity, state, scope, or source evidence is invalid")
		}
		if _, exists := components[component.ID]; exists {
			return invalid("observed component IDs must be unique")
		}
		components[component.ID] = struct{}{}
	}
	seenDependencies := make(map[string]struct{}, len(observed.Dependencies))
	for _, dependency := range observed.Dependencies {
		if !validToken(dependency.ProviderScope) || !validToken(dependency.From) || !validToken(dependency.To) {
			return invalid("observed dependency identity is invalid")
		}
		if err := validateEvidencePath(dependency.WitnessPath); err != nil {
			return err
		}
		if err := validateSourceRefs(dependency.SourceRefs, false); err != nil {
			return err
		}
		key := tuple(dependency.ProviderScope, dependency.From, dependency.To, dependency.WitnessPath)
		if _, exists := seenDependencies[key]; exists {
			return invalid("observed dependencies must be unique by scope, endpoints, and witness")
		}
		seenDependencies[key] = struct{}{}
	}
	return nil
}

func validComponent(component Component) bool {
	if !validToken(component.ID) || !validToken(component.ProviderScope) || !validState(component.State) {
		return false
	}
	return validateSourceRefs(component.SourceRefs, true) == nil
}

func validateSourceRefs(refs []model.SourceRef, required bool) error {
	if (required && len(refs) == 0) || len(refs) > MaxSourceRefs {
		return invalid("source reference count is missing or exceeds the supported limit")
	}
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
			return invalid("source reference worktree ID is invalid")
		}
		if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
			return invalid("source reference profile ID is invalid")
		}
		if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
			return invalid("source reference generation ID is invalid")
		}
		if _, err := model.ParseHash(string(ref.ContentHash)); err != nil {
			return invalid("source reference content hash is invalid")
		}
		if err := validateEvidencePath(ref.Path); err != nil {
			return err
		}
		if ref.Range.StartByte < 0 || ref.Range.EndByte <= ref.Range.StartByte {
			return invalid("source reference range is invalid")
		}
		key := tuple(string(ref.WorktreeID), string(ref.ProfileID), string(ref.GenerationID), ref.Path, string(ref.ContentHash), fmt.Sprint(ref.Range.StartByte), fmt.Sprint(ref.Range.EndByte))
		if _, exists := seen[key]; exists {
			return invalid("source references must be unique")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateEvidencePath(value string) error {
	if len(value) > MaxSourcePathBytes {
		return invalid("evidence path exceeds the supported limit")
	}
	if err := model.ValidateRelativePath(value); err != nil {
		return invalid("evidence path is invalid")
	}
	return nil
}

func cloneSourceRefs(refs []model.SourceRef) []model.SourceRef {
	out := append([]model.SourceRef(nil), refs...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return tuple(a.Path, string(a.WorktreeID), string(a.ProfileID), string(a.GenerationID), string(a.ContentHash), fmt.Sprint(a.Range.StartByte), fmt.Sprint(a.Range.EndByte)) <
			tuple(b.Path, string(b.WorktreeID), string(b.ProfileID), string(b.GenerationID), string(b.ContentHash), fmt.Sprint(b.Range.StartByte), fmt.Sprint(b.Range.EndByte))
	})
	return out
}

func validToken(value string) bool {
	if len(value) == 0 || len(value) > MaxIdentifierBytes {
		return false
	}
	for i, char := range value {
		if i == 0 && !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
			return false
		}
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._:/-", char)) {
			return false
		}
	}
	return true
}

func validText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func validState(state ComponentState) bool {
	switch state {
	case StateImplemented, StatePlanned, StateObservedInRun, StateUnsupported, StateNotSeen:
		return true
	default:
		return false
	}
}

func validCoverage(coverage CoverageState) bool {
	return coverage == CoverageComplete || coverage == CoveragePartial || coverage == CoverageUnknown
}

func validSeverity(severity Severity) bool {
	return severity == SeverityInfo || severity == SeverityWarning || severity == SeverityError
}

func hasString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func ruleKey(scope, from string) string { return tuple(scope, from) }

func exemptionKey(ruleID, scope, from, to string) string { return tuple(ruleID, scope, from, to) }

func tuple(values ...string) string { return strings.Join(values, "\x00") }

func invalid(reason string) error { return fmt.Errorf("%s: %w", reason, ErrInvalidArchitecture) }
