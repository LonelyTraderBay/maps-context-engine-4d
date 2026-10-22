package architecture

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestEvaluateAllowedDependencyPassesOnlyForCompleteDeclaredScope(t *testing.T) {
	intended, observed := architectureFixture()
	report, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictPass || report.Coverage != CoverageComplete || len(report.Violations) != 0 || len(report.Gaps) != 0 {
		t.Fatalf("allowed complete architecture = %+v", report)
	}
	if report.SourceContentVerification != "structural_only" {
		t.Fatalf("source content verification = %q", report.SourceContentVerification)
	}
	if len(report.ComponentStatuses) != 2 || !report.ComponentStatuses[0].ObservedMapped {
		t.Fatalf("explicit component mappings were not retained: %+v", report.ComponentStatuses)
	}
}

func TestEvaluateForbiddenDependencyReturnsSourceBoundWitness(t *testing.T) {
	intended, observed := architectureFixture()
	observed.Dependencies = []Dependency{{
		ProviderScope: "repo", From: "app", To: "database", WitnessPath: "internal/app/store.go",
		SourceRefs: []model.SourceRef{testSource("internal/app/store.go", "app imports database")},
	}}
	report, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictDrift || report.Coverage != CoverageComplete || len(report.Violations) != 1 {
		t.Fatalf("forbidden dependency report = %+v", report)
	}
	finding := report.Violations[0]
	if finding.RuleID != "app-dependencies" || finding.ProviderScope != "repo" || finding.From != "app" || finding.To != "database" || finding.WitnessPath != "internal/app/store.go" || finding.Severity != SeverityError || finding.Rationale == "" {
		t.Fatalf("finding lost its policy or exact witness: %+v", finding)
	}
	if len(finding.SourceRefs) != 1 || finding.SourceRefs[0].Path != finding.WitnessPath {
		t.Fatalf("finding source does not bind the exact witness path: %+v", finding.SourceRefs)
	}
}

func TestApprovedExemptionAppliesUntilExpiryThenLeavesViolation(t *testing.T) {
	intended, observed := architectureFixture()
	intended.Exemptions = []Exemption{{
		ID: "temporary-db-edge", RuleID: "app-dependencies", ProviderScope: "repo", From: "app", To: "database",
		ExpiresOn: "2026-10-23", Approved: true, Rationale: "temporary migration bridge",
		SourceRefs: []model.SourceRef{testSource("architecture/exemptions.md", "approved temporary exception")},
	}}
	observed.Dependencies = []Dependency{{
		ProviderScope: "repo", From: "app", To: "database", WitnessPath: "internal/app/store.go",
		SourceRefs: []model.SourceRef{testSource("internal/app/store.go", "app imports database")},
	}}
	active, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if active.Verdict != VerdictPass || len(active.AppliedExemptions) != 1 || active.AppliedExemptions[0].ExpiresOn != "2026-10-23" || len(active.Violations) != 0 {
		t.Fatalf("active exemption was not reported or applied: %+v", active)
	}
	expired, err := Evaluate(intended, observed, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if expired.Verdict != VerdictDrift || len(expired.Violations) != 1 || len(expired.Gaps) != 0 {
		t.Fatalf("expired exemption hid a fully witnessed architecture drift: %+v", expired)
	}
	if violation := expired.Violations[0]; violation.ExemptionID != "temporary-db-edge" || violation.ExemptionExpiresOn != "2026-10-23" || violation.ExemptionStatus != ExemptionStatusExpired {
		t.Fatalf("violation lost its expired exemption reference: %+v", violation)
	}
	intended.Exemptions[0].Approved = false
	unapproved, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if unapproved.Verdict != VerdictDrift || len(unapproved.Gaps) != 0 || len(unapproved.Violations) != 1 || unapproved.Violations[0].ExemptionID != "temporary-db-edge" || unapproved.Violations[0].ExemptionStatus != ExemptionStatusNotApproved {
		t.Fatalf("unapproved exemption suppressed drift or lost its reference: %+v", unapproved)
	}
}

func TestMissingProviderScopeCannotReportSystemWidePass(t *testing.T) {
	intended, observed := architectureFixture()
	observed.ProviderScopes = nil
	report, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictIncomplete || report.Coverage != CoveragePartial || !hasGap(report.Gaps, "provider_scope_missing") {
		t.Fatalf("missing required provider scope did not make coverage incomplete: %+v", report)
	}
	observed.Coverage = CoveragePartial
	partial, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if partial.Verdict == VerdictPass || partial.Coverage != CoveragePartial {
		t.Fatalf("partial graph produced a whole-scope pass: %+v", partial)
	}
}

func TestMissingComponentMappingIsNotInferredAsNotSeen(t *testing.T) {
	intended, observed := architectureFixture()
	observed.Components = observed.Components[:1]
	report, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	var modelStatus *ComponentStatus
	for i := range report.ComponentStatuses {
		if report.ComponentStatuses[i].ID == "model" {
			modelStatus = &report.ComponentStatuses[i]
		}
	}
	if modelStatus == nil || modelStatus.ObservedMapped || modelStatus.ObservedState != "" || !hasGapForComponent(report.Gaps, "component_mapping_missing", "model") {
		t.Fatalf("missing mapping was converted into an absence claim: %+v", report)
	}
	observed.Components = append(observed.Components, Component{ID: "model", ProviderScope: "repo", State: StateNotSeen, SourceRefs: []model.SourceRef{testSource("observed/model-scope.json", "explicit not seen")}})
	explicit, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range explicit.ComponentStatuses {
		if status.ID == "model" && (!status.ObservedMapped || status.ObservedState != StateNotSeen) {
			t.Fatalf("explicit not_seen status was not preserved: %+v", status)
		}
	}
}

func TestComponentStatusValuesRemainDistinct(t *testing.T) {
	intended, observed := architectureFixture()
	states := []ComponentState{StateImplemented, StatePlanned, StateObservedInRun, StateUnsupported, StateNotSeen}
	intended.Components = nil
	intended.Rules = nil
	observed.Components = nil
	observed.Dependencies = nil
	for i, state := range states {
		id := []string{"one", "two", "three", "four", "five"}[i]
		intended.Components = append(intended.Components, Component{ID: id, ProviderScope: "repo", State: state, SourceRefs: []model.SourceRef{testSource("architecture/"+id+".md", string(state))}})
		intended.Rules = append(intended.Rules, AllowedDependencyRule{ID: id + "-rule", ProviderScope: "repo", From: id, AllowedTo: []string{}, Severity: SeverityWarning, Rationale: "explicit closed dependency set", SourceRefs: []model.SourceRef{testSource("architecture/rules.md", id+" rule")}})
		observed.Components = append(observed.Components, Component{ID: id, ProviderScope: "repo", State: state, SourceRefs: []model.SourceRef{testSource("observed/"+id+".json", string(state))}})
	}
	report, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]ComponentState, 0, len(report.ComponentStatuses))
	for _, status := range report.ComponentStatuses {
		if status.IntendedState != status.ObservedState {
			t.Fatalf("status was collapsed or changed: %+v", status)
		}
		got = append(got, status.ObservedState)
	}
	sortStates(got)
	want := append([]ComponentState(nil), states...)
	sortStates(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("distinct status values = %v, want %v", got, want)
	}
}

func TestUnboundWitnessIsAGapNotAViolation(t *testing.T) {
	intended, observed := architectureFixture()
	observed.Dependencies = []Dependency{{
		ProviderScope: "repo", From: "app", To: "database", WitnessPath: "internal/app/store.go",
		SourceRefs: []model.SourceRef{testSource("internal/app/other.go", "different source")},
	}}
	report, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictIncomplete || len(report.Violations) != 0 || !hasGap(report.Gaps, "dependency_witness_unbound") {
		t.Fatalf("unbound edge produced a policy finding or pass: %+v", report)
	}
}

func TestEvaluationIsDeterministicAcrossDependencyOrder(t *testing.T) {
	intended, observed := architectureFixture()
	observed.Dependencies = append(observed.Dependencies, Dependency{ProviderScope: "repo", From: "app", To: "database", WitnessPath: "internal/app/store.go", SourceRefs: []model.SourceRef{testSource("internal/app/store.go", "app imports database")}})
	first, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	observed.Dependencies[0], observed.Dependencies[1] = observed.Dependencies[1], observed.Dependencies[0]
	second, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("report depends on input order:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestInvalidSourcePathAndUnapprovedArchitectureAreNotPasses(t *testing.T) {
	intended, observed := architectureFixture()
	intended.SourceRefs[0].Path = strings.Repeat("a", MaxSourcePathBytes+1)
	if _, err := Evaluate(intended, observed, testAsOf()); !errors.Is(err, ErrInvalidArchitecture) {
		t.Fatalf("oversized source path error = %v", err)
	}
	intended, observed = architectureFixture()
	observed.Dependencies[0].WitnessPath = strings.Repeat("a", MaxSourcePathBytes+1)
	if _, err := Evaluate(intended, observed, testAsOf()); !errors.Is(err, ErrInvalidArchitecture) {
		t.Fatalf("oversized witness path error = %v", err)
	}
	intended, observed = architectureFixture()
	intended.Approval = ApprovalDraft
	report, err := Evaluate(intended, observed, testAsOf())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != VerdictIncomplete || !hasGap(report.Gaps, "architecture_not_approved") {
		t.Fatalf("draft architecture was enforced as approved: %+v", report)
	}
	observed.Dependencies[0].WitnessPath = "../escape.go"
	if _, err := Evaluate(intended, observed, testAsOf()); !errors.Is(err, ErrInvalidArchitecture) {
		t.Fatalf("traversal witness error = %v", err)
	}
}

func architectureFixture() (IntendedArchitecture, ObservedGraph) {
	intended := IntendedArchitecture{
		ID: "repo-architecture", Version: "1", Scope: "repository", Statement: "Repository dependencies follow explicit package boundaries.",
		Approval: ApprovalApproved, InvalidationTriggers: []string{"source_change"},
		SourceRefs:             []model.SourceRef{testSource("architecture/decision.md", "approved architecture decision")},
		RequiredProviderScopes: []string{"repo"},
		Components: []Component{
			{ID: "app", ProviderScope: "repo", State: StateImplemented, SourceRefs: []model.SourceRef{testSource("architecture/components.md", "app component")}},
			{ID: "model", ProviderScope: "repo", State: StateImplemented, SourceRefs: []model.SourceRef{testSource("architecture/components.md", "model component")}},
		},
		Rules: []AllowedDependencyRule{
			{ID: "app-dependencies", ProviderScope: "repo", From: "app", AllowedTo: []string{"model"}, Severity: SeverityError, Rationale: "application layer may depend on the shared domain model", SourceRefs: []model.SourceRef{testSource("architecture/rules.md", "app allowlist")}},
			{ID: "model-dependencies", ProviderScope: "repo", From: "model", AllowedTo: []string{}, Severity: SeverityWarning, Rationale: "the shared model is a leaf package", SourceRefs: []model.SourceRef{testSource("architecture/rules.md", "model leaf")}},
		},
	}
	observed := ObservedGraph{
		Coverage:        CoverageComplete,
		CoverageSources: []model.SourceRef{testSource("observed/coverage.json", "complete declared graph")},
		ProviderScopes:  []ProviderScopeCoverage{{ProviderScope: "repo", Coverage: CoverageComplete, SourceRefs: []model.SourceRef{testSource("observed/repo-scope.json", "repo scope complete")}}},
		Components: []Component{
			{ID: "app", ProviderScope: "repo", State: StateImplemented, SourceRefs: []model.SourceRef{testSource("observed/app.json", "app observed")}},
			{ID: "model", ProviderScope: "repo", State: StateImplemented, SourceRefs: []model.SourceRef{testSource("observed/model.json", "model observed")}},
		},
		Dependencies: []Dependency{{ProviderScope: "repo", From: "app", To: "model", WitnessPath: "internal/app/service.go", SourceRefs: []model.SourceRef{testSource("internal/app/service.go", "app imports model")}}},
	}
	return intended, observed
}

func testSource(path, content string) model.SourceRef {
	return model.SourceRef{
		WorktreeID:   model.UUID("00000000-0000-4000-8000-000000000001"),
		ProfileID:    model.SHA256([]byte("architecture-test-profile")),
		GenerationID: model.UUID("00000000-0000-4000-8000-000000000002"),
		Path:         path,
		ContentHash:  model.SHA256([]byte(content)),
		Range:        model.ByteRange{StartByte: 0, EndByte: int64(len(content))},
	}
}

func testAsOf() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }

func hasGap(gaps []Gap, code string) bool {
	return hasGapForComponent(gaps, code, "")
}

func hasGapForComponent(gaps []Gap, code, componentID string) bool {
	for _, gap := range gaps {
		if gap.Code == code && (componentID == "" || gap.ComponentID == componentID) {
			return true
		}
	}
	return false
}

func sortStates(states []ComponentState) {
	for i := 0; i < len(states); i++ {
		for j := i + 1; j < len(states); j++ {
			if states[j] < states[i] {
				states[i], states[j] = states[j], states[i]
			}
		}
	}
}
