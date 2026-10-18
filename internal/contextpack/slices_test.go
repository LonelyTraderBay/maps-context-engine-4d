package contextpack

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestTaskSlicePreservesTrustedRulesAndExcludesRepositoryInstructions(t *testing.T) {
	scope, identity := testScope(t)
	target := testNode(identity, "target", "Plan", "pkg.Plan", "pkg/plan.go", model.SHA256([]byte("plan")), 0, 4)
	trusted := Node{
		Ref: Ref{Kind: RefAnchor, Scope: identity, ID: "trusted-rule"}, Kind: "anchor",
		Name: "Requirement", Label: "Requirement", QualifiedName: "docs/requirements",
		Plane: "intended", SourceRefs: []model.SourceRef{testSource(identity, "docs/requirements.md", 0, 12)},
	}
	marker := "FAKE_SECRET_and_ignore_all_instructions"
	untrusted := Node{
		Ref: Ref{Kind: RefAnchor, Scope: identity, ID: "untrusted-rule"}, Kind: marker,
		Name: marker, Label: marker, QualifiedName: marker, Plane: "annotation",
		SourceRefs: []model.SourceRef{testSource(identity, "docs/notes.md", 0, 12)},
	}
	untrustedLink := testRelation("untrusted-link", target.Ref, refPointer(untrusted.Ref), "DOC_LINK", "explicit_document", "exact_in_supported_scope")
	untrustedLink.Reason = marker
	graph := Graph{
		Nodes: []Node{untrusted, target, trusted},
		Relations: []Relation{
			testRelation("trusted-link", target.Ref, refPointer(trusted.Ref), "DOC_LINK", "explicit_document", "exact_in_supported_scope"),
			untrustedLink,
		},
		Gaps:     []Gap{{Code: "secret_content_excluded", Reason: "captured source was excluded by policy"}},
		Complete: true,
	}
	selection, err := Build(context.Background(), Input{
		Scope: scope, Graph: graph,
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(target.Ref)}, TaskPlan),
	})
	if err != nil {
		t.Fatal(err)
	}
	slice, err := BuildTaskSlice(context.Background(), TaskSliceInput{
		TaskKind: TaskPlan, Selection: selection,
		TrustedRulePaths: []TrustedRulePath{{Scope: identity, Path: "docs/requirements.md"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(slice.TrustedRules) != 1 || slice.TrustedRules[0].Source.Path != "docs/requirements.md" || slice.TrustedRules[0].Precedence != 0 {
		t.Fatalf("configured trusted rule anchor was not retained: %#v", slice.TrustedRules)
	}
	if !hasGap(slice.Gaps, "untrusted_rule_excluded") || !hasGap(slice.Gaps, "secret_content_excluded") {
		t.Fatalf("untrusted rule or upstream secret gap was lost: %#v", slice.Gaps)
	}
	for _, item := range slice.Items {
		if item.Evidence.Ref != nil && item.Evidence.Ref.ID == "untrusted-rule" && item.Category != SliceDocumentation {
			t.Fatalf("repository rule-like content was not kept as ordinary data: %#v", item)
		}
	}
	encoded, err := json.Marshal(slice)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), marker) {
		t.Fatalf("repository text or secret marker escaped into the task slice: %s", encoded)
	}
}

func TestTaskSliceCarriesRenameSerializationRiskAndExplicitConstraints(t *testing.T) {
	scope, identity := testScope(t)
	hash := model.SHA256([]byte("type User struct { AccountID string }"))
	field := testNode(identity, "field-account-id", "AccountID", "api.User.AccountID", "internal/api/user.go", hash, 12, 21)
	field.Kind = "field"
	selection, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{field}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(field.Ref)}, TaskRename),
	})
	if err != nil {
		t.Fatal(err)
	}
	slice, err := BuildTaskSlice(context.Background(), TaskSliceInput{
		TaskKind: TaskRename, Selection: selection,
		ChangeConstraints: []string{"preserve_json_keys", "no_public_api_change"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(slice.Constraints) != 2 || slice.Constraints[0].Value != "no_public_api_change" || slice.Constraints[1].Value != "preserve_json_keys" {
		t.Fatalf("explicit constraints were lost or unstable: %#v", slice.Constraints)
	}
	if !slice.Items[0].Evidence.PotentialImplicitSerializationNameChange {
		t.Fatal("exported Go field serialization-key risk was not surfaced")
	}
	if !hasValidationStep(slice.Validation, "verify_implicit_serialization_key", "needs_context") ||
		!hasValidationStep(slice.Validation, "validate_explicit_constraint", "needs_context") {
		t.Fatalf("serialization risk or preserve_json_keys constraint is missing from validation plan: %#v", slice.Validation)
	}
	if !hasGap(slice.Gaps, "serialization_contract_unknown") || slice.RequiredPending != nil {
		t.Fatalf("unknown serialization key behavior was not kept as an explicit gap: %#v", slice.Gaps)
	}
	if slice.Validation.Status != "not_run" {
		t.Fatalf("context preparation must not claim validation ran: %#v", slice.Validation)
	}
}

func TestTaskSliceDoesNotPromoteTextMatchesOrUnknownBoundaries(t *testing.T) {
	scope, identity := testScope(t)
	target := testNode(identity, "target", "Client", "pkg.Client", "pkg/client.go", model.SHA256([]byte("client")), 0, 6)
	lexical := testNode(identity, "lexical", "ClientConfig", "pkg.ClientConfig", "pkg/config.ts", model.SHA256([]byte("config")), 0, 6)
	textMatch := testRelation("text-match", target.Ref, refPointer(lexical.Ref), "REFERS_TO", "text_match", "candidate")
	external := testRelation("external", target.Ref, nil, "UNRESOLVED_REF", "unresolved", "unresolved")
	external.Reason = "external target is not enumerated"
	externalLabel := "external consumer"
	external.UnresolvedLabel = &externalLabel
	selection, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{target, lexical}, Relations: []Relation{textMatch, external}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(target.Ref)}, TaskContract),
	})
	if err != nil {
		t.Fatal(err)
	}
	slice, err := BuildTaskSlice(context.Background(), TaskSliceInput{TaskKind: TaskContract, Selection: selection})
	if err != nil {
		t.Fatal(err)
	}
	foundLexical, foundBoundary := false, false
	for _, item := range slice.Items {
		if item.Evidence.RelationID == nil {
			continue
		}
		if *item.Evidence.RelationID == textMatch.ID {
			foundLexical = item.Category == SliceLexicalCandidate && item.EvidenceClass == "lexical_only"
		}
		if item.Evidence.BoundaryCode == "unresolved_relation" {
			foundBoundary = item.Category == SliceSafetyBoundary
		}
	}
	if !foundLexical || !foundBoundary || !hasValidationStep(slice.Validation, "verify_lexical_candidate", "needs_context") ||
		!hasValidationStep(slice.Validation, "review_unresolved_boundary", "needs_context") {
		t.Fatalf("lexical or unknown external evidence was overstated or omitted: %#v", slice)
	}
}

func TestTaskSliceOrderingIgnoresInputPermutation(t *testing.T) {
	scope, identity := testScope(t)
	target := testNode(identity, "target", "Plan", "pkg.Plan", "pkg/plan.go", model.SHA256([]byte("plan")), 0, 4)
	neighbor := testNode(identity, "neighbor", "Use", "pkg.Use", "pkg/use.go", model.SHA256([]byte("use")), 0, 3)
	anchor := Node{
		Ref: Ref{Kind: RefAnchor, Scope: identity, ID: "rule"}, Kind: "anchor",
		Name: "Rule", Label: "Rule", QualifiedName: "docs/rule", Plane: "intended",
		SourceRefs: []model.SourceRef{testSource(identity, "docs/rule.md", 0, 4)},
	}
	selection, err := Build(context.Background(), Input{
		Scope: scope,
		Graph: Graph{
			Nodes: []Node{target, neighbor, anchor},
			Relations: []Relation{
				testRelation("uses", target.Ref, refPointer(neighbor.Ref), "REFERS_TO", "type_resolved", "exact_in_supported_scope"),
				testRelation("documents", target.Ref, refPointer(anchor.Ref), "DOC_LINK", "explicit_document", "exact_in_supported_scope"),
			}, Complete: true,
		},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(target.Ref)}, TaskPlan),
	})
	if err != nil {
		t.Fatal(err)
	}
	trusted := []TrustedRulePath{{Scope: identity, Path: "docs/rule.md"}}
	first, err := BuildTaskSlice(context.Background(), TaskSliceInput{
		TaskKind: TaskPlan, Selection: selection,
		ChangeConstraints: []string{"preserve_json_keys", "no_source_mutation"}, TrustedRulePaths: trusted,
	})
	if err != nil {
		t.Fatal(err)
	}
	permuted := selection
	permuted.Items = append([]Item(nil), selection.Items...)
	for left, right := 0, len(permuted.Items)-1; left < right; left, right = left+1, right-1 {
		permuted.Items[left], permuted.Items[right] = permuted.Items[right], permuted.Items[left]
	}
	second, err := BuildTaskSlice(context.Background(), TaskSliceInput{
		TaskKind: TaskPlan, Selection: permuted,
		ChangeConstraints: []string{"no_source_mutation", "preserve_json_keys"}, TrustedRulePaths: trusted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("permuted evidence or request constraints changed task slice:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestSpecOnlyPlanningKeepsIntendedSeparateFromUnavailableObservedPlanes(t *testing.T) {
	scope, identity := testScope(t)
	requirement := Node{
		Ref: Ref{Kind: RefAnchor, Scope: identity, ID: "planned-component"}, Kind: "component",
		Name: "Payments", Label: "Payments", QualifiedName: "payments", Plane: "intended",
		SourceRefs: []model.SourceRef{testSource(identity, "docs/architecture.md", 0, 8)},
	}
	selection, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{requirement}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(requirement.Ref)}, TaskPlan),
	})
	if err != nil {
		t.Fatal(err)
	}
	slice, err := BuildTaskSlice(context.Background(), TaskSliceInput{TaskKind: TaskPlan, Selection: selection})
	if err != nil {
		t.Fatal(err)
	}
	if slice.ScopeVectorDigest != scope.VectorDigest || slice.FreshnessStatus != "not_checked" ||
		slice.Readiness != "needs_context" || slice.EditPreconditionValidated {
		t.Fatalf("planning snapshot was marked fresh or edit-ready: %#v", slice)
	}
	if planeSummary(slice.PlaneSummaries, "intended").Status != "present" ||
		planeSummary(slice.PlaneSummaries, "observed_static").Status != "not_seen" ||
		planeSummary(slice.PlaneSummaries, "observed_runtime").Status != "not_seen" {
		t.Fatalf("spec-only facts were conflated with observed implementation/runtime: %#v", slice.PlaneSummaries)
	}
	group := obligationGroup(slice.ObligationGroups, SliceTarget, "intended")
	if group == nil || group.EvidenceCount != 1 || group.TotalEvidenceCount == nil || *group.TotalEvidenceCount != 1 ||
		len(group.EvidenceIDs) != 1 || group.EvidenceIDs[0] != slice.Items[0].Evidence.ID {
		t.Fatalf("complete task evidence group lost its exact reference/count: %#v", slice.ObligationGroups)
	}
}

func TestIncompletePlanningCountsStayUnknownAndCursorPagesAdvancePinnedEvidence(t *testing.T) {
	scope, identity := testScope(t)
	target := testNode(identity, "target", "Plan", "pkg.Plan", "pkg/plan.go", model.SHA256([]byte("plan")), 0, 4)
	a := testNode(identity, "a", "A", "pkg.A", "pkg/a.go", model.SHA256([]byte("a")), 0, 1)
	b := testNode(identity, "b", "B", "pkg.B", "pkg/b.go", model.SHA256([]byte("b")), 0, 1)
	c := testNode(identity, "c", "C", "pkg.C", "pkg/c.go", model.SHA256([]byte("c")), 0, 1)
	graph := Graph{
		Nodes: []Node{c, target, a, b},
		Relations: []Relation{
			testRelation("target-a", target.Ref, refPointer(a.Ref), "REFERS_TO", "type_resolved", "exact_in_supported_scope"),
			testRelation("target-b", target.Ref, refPointer(b.Ref), "REFERS_TO", "type_resolved", "exact_in_supported_scope"),
			testRelation("target-c", target.Ref, refPointer(c.Ref), "REFERS_TO", "type_resolved", "exact_in_supported_scope"),
		},
		Complete: false,
	}
	selection, err := Build(context.Background(), Input{
		Scope: scope, Graph: graph,
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(target.Ref)}, TaskExplore),
	})
	if err != nil {
		t.Fatal(err)
	}
	slice, err := BuildTaskSlice(context.Background(), TaskSliceInput{TaskKind: TaskExplore, Selection: selection})
	if err != nil {
		t.Fatal(err)
	}
	if slice.EnumerationComplete || !slice.ContinuationRequired ||
		planeSummary(slice.PlaneSummaries, "observed_runtime").Status != "unknown" ||
		planeSummary(slice.PlaneSummaries, "observed_runtime").TotalEvidenceCount != nil {
		t.Fatalf("incomplete graph produced a false zero or complete count: %#v", slice)
	}
	for _, group := range slice.ObligationGroups {
		if group.TotalEvidenceCount != nil {
			t.Fatalf("incomplete evidence group published an exact whole-graph total: %#v", group)
		}
	}

	session, err := NewPlanningSession(context.Background(), slice)
	if err != nil {
		t.Fatal(err)
	}
	var cursor *PlanningCursor
	seen := make([]model.Hash, 0, len(slice.Items))
	var last PlanningPage
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, pageErr := session.Page(context.Background(), cursor, 2)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		if page.StartIndex != len(seen) || page.SelectedItemCount != len(slice.Items) ||
			page.RemainingSelected != len(slice.Items)-len(seen)-len(page.Items) ||
			page.SelectionComplete || !page.SelectionContinuationRequired {
			t.Fatalf("page count/progress did not preserve the incomplete pinned selection: %#v", page)
		}
		for _, item := range page.Items {
			seen = append(seen, item.Evidence.ID)
		}
		last = page
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	want := make([]model.Hash, len(slice.Items))
	for index, item := range slice.Items {
		want[index] = item.Evidence.ID
	}
	if !reflect.DeepEqual(seen, want) || !last.PageComplete || last.EnumerationComplete || last.RemainingSelected != 0 {
		t.Fatalf("page continuation skipped/repeated evidence or overstated completion: got=%v want=%v last=%#v", seen, want, last)
	}
	if cursor == nil {
		t.Fatal("multi-page selection did not produce a continuation cursor")
	}

	otherScope := scope
	otherScope.Members = append([]model.ScopeMember(nil), scope.Members...)
	otherScope.Members[0].SnapshotDigest = model.SHA256([]byte("new immutable snapshot"))
	otherScope.VectorDigest, err = otherScope.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	otherSelection, err := Build(context.Background(), Input{
		Scope: otherScope, Graph: graph,
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(target.Ref)}, TaskExplore),
	})
	if err != nil {
		t.Fatal(err)
	}
	otherSlice, err := BuildTaskSlice(context.Background(), TaskSliceInput{TaskKind: TaskExplore, Selection: otherSelection})
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := NewPlanningSession(context.Background(), otherSlice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherSession.Page(context.Background(), cursor, 2); !errors.Is(err, ErrPlanningCursorMismatch) {
		t.Fatalf("cursor was accepted for another pinned scope: %v", err)
	}
	otherIntent := slice
	otherIntent.Intent = "a different planning request"
	otherIntentSession, err := NewPlanningSession(context.Background(), otherIntent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherIntentSession.Page(context.Background(), cursor, 2); !errors.Is(err, ErrPlanningCursorMismatch) {
		t.Fatalf("cursor was accepted for a different selection request: %v", err)
	}
	amendedCursor := *cursor
	amendedCursor.afterItemKey = "tampered"
	if _, err := session.Page(context.Background(), &amendedCursor, 2); !errors.Is(err, ErrPlanningCursorMismatch) {
		t.Fatalf("tampered cursor was accepted: %v", err)
	}
}

func planeSummary(summaries []PlanningPlaneSummary, plane string) PlanningPlaneSummary {
	for _, summary := range summaries {
		if summary.Plane == plane {
			return summary
		}
	}
	return PlanningPlaneSummary{}
}

func obligationGroup(groups []TaskObligationGroup, category SliceCategory, plane string) *TaskObligationGroup {
	for index := range groups {
		if groups[index].Category == category && groups[index].Plane == plane {
			return &groups[index]
		}
	}
	return nil
}

func TestTaskSliceRejectsMismatchedTrustAndDuplicateConstraints(t *testing.T) {
	scope, identity := testScope(t)
	node := testNode(identity, "target", "Run", "pkg.Run", "pkg/run.go", model.SHA256([]byte("run")), 0, 3)
	selection, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{node}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(node.Ref)}, TaskExplore),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildTaskSlice(context.Background(), TaskSliceInput{
		TaskKind: TaskExplore, Selection: selection,
		ChangeConstraints: []string{"preserve_json_keys", "preserve_json_keys"},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("duplicate constraints were silently accepted: %v", err)
	}
	_, err = BuildTaskSlice(context.Background(), TaskSliceInput{
		TaskKind: TaskExplore, Selection: selection,
		TrustedRulePaths: []TrustedRulePath{{Scope: identity, Path: "../AGENTS.md"}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid trusted path escaped validation: %v", err)
	}
	unsupported, err := BuildTaskSlice(context.Background(), TaskSliceInput{
		TaskKind: TaskExplore, Selection: selection,
		ChangeConstraints: []string{"preserve_custom_contract"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unsupported.Constraints) != 1 || unsupported.Constraints[0].Supported ||
		unsupported.Constraints[0].Value != "preserve_custom_contract" || !hasGap(unsupported.Gaps, "unsupported_change_constraint") {
		t.Fatalf("unsupported explicit constraint was dropped or silently accepted: %#v", unsupported)
	}
}

func TestTaskSliceAcceptsAllSealedTaskKinds(t *testing.T) {
	scope, identity := testScope(t)
	node := testNode(identity, "target", "Run", "pkg.Run", "pkg/run.go", model.SHA256([]byte("run")), 0, 3)
	tasks := []TaskKind{TaskExplore, TaskPlan, TaskRename, TaskTypeChange, TaskBehavior, TaskContract, TaskDebug}
	focusSets := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		t.Run(string(task), func(t *testing.T) {
			selection, err := Build(context.Background(), Input{
				Scope: scope, Graph: Graph{Nodes: []Node{node}, Complete: true},
				Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(node.Ref)}, task),
			})
			if err != nil {
				t.Fatal(err)
			}
			intent := ""
			if task == TaskDebug {
				intent = "Request-owned symptom: timeout after save"
			}
			slice, err := BuildTaskSlice(context.Background(), TaskSliceInput{TaskKind: task, Intent: intent, Selection: selection})
			if err != nil {
				t.Fatal(err)
			}
			if slice.TaskKind != task || len(slice.Focus) == 0 || len(slice.Items) != 1 || slice.Items[0].Category != SliceTarget {
				t.Fatalf("task kind did not produce its pinned target slice: %#v", slice)
			}
			if slice.Intent != intent {
				t.Fatalf("request intent was changed while building a task slice: %q", slice.Intent)
			}
			focusSets[strings.Join(slice.Focus, ",")] = struct{}{}
		})
	}
	if len(focusSets) != len(tasks) {
		t.Fatalf("sealed task kinds do not have distinct stable focus mappings: %#v", focusSets)
	}
}

func TestDebugTaskRequiresUserSymptomInsteadOfInferringOne(t *testing.T) {
	scope, identity := testScope(t)
	node := testNode(identity, "target", "Run", "pkg.Run", "pkg/run.go", model.SHA256([]byte("run")), 0, 3)
	selection, err := Build(context.Background(), Input{
		Scope: scope, Graph: Graph{Nodes: []Node{node}, Complete: true},
		Request: testRequest(Selector{Kind: SelectorRef, Ref: refPointer(node.Ref)}, TaskDebug),
	})
	if err != nil {
		t.Fatal(err)
	}
	slice, err := BuildTaskSlice(context.Background(), TaskSliceInput{TaskKind: TaskDebug, Selection: selection})
	if err != nil {
		t.Fatal(err)
	}
	if !hasGap(slice.Gaps, "debug_symptom_missing") || !hasValidationStep(slice.Validation, "supply_debug_symptom", "needs_context") ||
		slice.RequiredPending != nil {
		t.Fatalf("missing debug symptom was inferred from repository evidence: %#v", slice)
	}
}

func hasValidationStep(plan ValidationPlan, code, status string) bool {
	for _, step := range plan.Steps {
		if step.Code == code && step.Status == status {
			return true
		}
	}
	return false
}
