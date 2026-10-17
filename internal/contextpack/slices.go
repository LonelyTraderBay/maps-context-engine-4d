package contextpack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	maxTaskConstraints  = 16
	maxTrustedRulePaths = 128
	maxTaskSliceItems   = 500001 // One node per selected node plus relation and boundary items.
)

type SliceCategory string

const (
	SliceSafetyBoundary   SliceCategory = "safety_boundary"
	SliceTarget           SliceCategory = "target"
	SliceIntended         SliceCategory = "intended"
	SliceObserved         SliceCategory = "observed"
	SliceDeclaration      SliceCategory = "declaration"
	SliceSource           SliceCategory = "source"
	SliceReference        SliceCategory = "reference"
	SliceDataflow         SliceCategory = "dataflow"
	SliceDependency       SliceCategory = "dependency"
	SliceContract         SliceCategory = "contract"
	SliceTest             SliceCategory = "test"
	SliceDocumentation    SliceCategory = "documentation"
	SliceModule           SliceCategory = "module"
	SliceLexicalCandidate SliceCategory = "lexical_candidate"
	SliceNeighbor         SliceCategory = "neighbor"
)

type TrustedRulePath struct {
	Scope ScopeIdentity
	Path  string
}

type TaskSliceInput struct {
	TaskKind          TaskKind
	Intent            string // Caller text only; never a trust or execution policy.
	Selection         Result
	ChangeConstraints []string
	TrustedRulePaths  []TrustedRulePath
}

type TaskSliceItem struct {
	Evidence      Item
	Category      SliceCategory
	EvidenceClass string
}

type TrustedRuleAnchor struct {
	ID         model.Hash
	Scope      ScopeIdentity
	Source     model.SourceRef
	Precedence int
}

type ConstraintDisposition struct {
	Value     string
	Supported bool
	Status    string
	Reason    string
}

type ValidationStep struct {
	Code        string
	Subject     string
	Status      string
	Required    bool
	EvidenceIDs []model.Hash
	Reason      string
}

type ValidationPlan struct {
	Status string
	Steps  []ValidationStep
}

// TaskSlice contains evidence descriptors only. It never carries source bytes,
// executes repository text, or treats repository content as trusted policy.
type TaskSlice struct {
	TaskKind             TaskKind
	Intent               string
	Focus                []string
	Items                []TaskSliceItem
	TrustedRules         []TrustedRuleAnchor
	Constraints          []ConstraintDisposition
	Validation           ValidationPlan
	Gaps                 []Gap
	EnumerationComplete  bool
	ContinuationRequired bool
	RequiredPending      *int
}

// BuildTaskSlice groups one resolved selection into deterministic task-aware
// evidence and a validation plan. Trusted rule anchors are admitted only when
// both their exact scope and path were supplied by the host policy.
func BuildTaskSlice(ctx context.Context, input TaskSliceInput) (TaskSlice, error) {
	if ctx == nil {
		return TaskSlice{}, fmt.Errorf("%w: nil context", ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return TaskSlice{}, err
	}
	if !validTaskKind(input.TaskKind) || input.Selection.TaskKind != input.TaskKind ||
		input.Selection.Status != StatusResolved || input.Selection.Target == nil {
		return TaskSlice{}, fmt.Errorf("%w: task slices require the matching resolved selection", ErrInvalidRequest)
	}
	if len(input.Selection.Items) > maxTaskSliceItems {
		return TaskSlice{}, fmt.Errorf("%w: selected evidence exceeds the bounded slice limit", ErrInvalidRequest)
	}
	if !utf8.ValidString(input.Intent) || utf8.RuneCountInString(input.Intent) > 2048 {
		return TaskSlice{}, fmt.Errorf("%w: task intent must be valid UTF-8 within the request limit", ErrInvalidRequest)
	}
	if err := validateSelectionForSlice(ctx, input.Selection); err != nil {
		return TaskSlice{}, err
	}
	constraints, err := validateChangeConstraints(input.ChangeConstraints)
	if err != nil {
		return TaskSlice{}, err
	}
	trustedPaths, err := validateTrustedRulePaths(input.TrustedRulePaths, input.Selection.ScopeIdentities)
	if err != nil {
		return TaskSlice{}, err
	}

	slice := TaskSlice{
		TaskKind: input.TaskKind, Intent: input.Intent, Focus: taskFocus(input.TaskKind),
		Items:        make([]TaskSliceItem, 0, len(input.Selection.Items)),
		TrustedRules: make([]TrustedRuleAnchor, 0), Constraints: constraints,
		Gaps:                 cloneGaps(input.Selection.Gaps),
		EnumerationComplete:  input.Selection.EnumerationComplete,
		ContinuationRequired: input.Selection.ContinuationRequired,
		RequiredPending:      cloneInt(input.Selection.RequiredPending),
	}
	targetKey := refKey(*input.Selection.Target)
	for _, item := range input.Selection.Items {
		if err := ctx.Err(); err != nil {
			return TaskSlice{}, err
		}
		copied := cloneItem(item)
		slice.Items = append(slice.Items, TaskSliceItem{
			Evidence:      copied,
			Category:      taskSliceCategory(input.TaskKind, targetKey, copied),
			EvidenceClass: relationEvidenceClass(copied),
		})
	}
	sort.Slice(slice.Items, func(i, j int) bool {
		return taskSliceItemLess(slice.Items[i], slice.Items[j])
	})

	taskSpecificGap := false
	if input.TaskKind == TaskDebug && input.Intent == "" {
		slice.Gaps = append(slice.Gaps, Gap{
			Code: "debug_symptom_missing", Reason: "debug context requires a user-supplied symptom; no repository text can substitute for it",
		})
		taskSpecificGap = true
	}
	var untrustedRuleAnchor bool
	slice.TrustedRules, untrustedRuleAnchor, err = collectTrustedRuleAnchors(ctx, input.Selection, trustedPaths)
	if err != nil {
		return TaskSlice{}, err
	}
	if len(trustedPaths) > 0 && len(slice.TrustedRules) == 0 {
		slice.Gaps = append(slice.Gaps, Gap{
			Code:   "trusted_rules_not_selected",
			Reason: "configured trusted rule paths have no matching anchor in the selected graph",
		})
		taskSpecificGap = true
	}
	if untrustedRuleAnchor {
		slice.Gaps = append(slice.Gaps, Gap{
			Code:   "untrusted_rule_excluded",
			Reason: "repository rule-like content outside configured trusted paths remains ordinary data, not trusted policy",
		})
		taskSpecificGap = true
	}
	unsupportedConstraints := 0
	for _, constraint := range constraints {
		if !constraint.Supported {
			unsupportedConstraints++
		}
	}
	if unsupportedConstraints > 0 {
		slice.Gaps = append(slice.Gaps, Gap{
			Code:   "unsupported_change_constraint",
			Reason: "one or more explicit request constraints are outside the supported vocabulary and remain pending",
			Count:  intPointer(unsupportedConstraints),
		})
		taskSpecificGap = true
	}
	if input.TaskKind == TaskRename && hasConstraint(constraints, "preserve_json_keys") && hasImplicitSerializationRisk(slice.Items) {
		slice.Gaps = append(slice.Gaps, Gap{
			Code:   "serialization_contract_unknown",
			Reason: "preserve_json_keys applies to a possible implicit Go field key, but tag and runtime evidence are unavailable",
			Count:  intPointer(1),
		})
		taskSpecificGap = true
	}
	if taskSpecificGap {
		slice.RequiredPending = nil
	}
	slice.Gaps = normalizeGaps(slice.Gaps)
	slice.Validation, err = buildValidationPlan(ctx, input.TaskKind, input.Intent != "", slice.Items, constraints)
	if err != nil {
		return TaskSlice{}, err
	}
	return slice, nil
}

func validateSelectionForSlice(ctx context.Context, selection Result) error {
	if err := validateRef(*selection.Target); err != nil {
		return fmt.Errorf("%w: selected target is invalid", ErrInvalidRequest)
	}
	if !containsScopeIdentity(selection.ScopeIdentities, selection.Target.Scope) {
		return fmt.Errorf("%w: selected target is outside the captured scope", ErrInvalidRequest)
	}
	for _, identity := range selection.ScopeIdentities {
		if err := validateRef(Ref{Kind: RefSymbol, Scope: identity, ID: "scope-validation"}); err != nil {
			return fmt.Errorf("%w: captured scope identity is invalid", ErrInvalidRequest)
		}
	}
	for _, item := range selection.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := model.ParseHash(string(item.ID)); err != nil {
			return fmt.Errorf("%w: selected item has an invalid identity", ErrInvalidRequest)
		}
		if item.Ref != nil {
			if err := validateRef(*item.Ref); err != nil || !containsScopeIdentity(selection.ScopeIdentities, item.Ref.Scope) {
				return fmt.Errorf("%w: selected item reference is invalid or outside the captured scope", ErrInvalidRequest)
			}
		}
		for _, ref := range []*Ref{item.RelationFrom, item.RelationTo} {
			if ref != nil {
				if err := validateRef(*ref); err != nil || !containsScopeIdentity(selection.ScopeIdentities, ref.Scope) {
					return fmt.Errorf("%w: selected relation reference is invalid or outside the captured scope", ErrInvalidRequest)
				}
			}
		}
		for _, source := range item.SourceRefs {
			if err := validateSourceRef(source); err != nil || !sourceInScopeIdentities(source, selection.ScopeIdentities) ||
				(item.Ref != nil && !sameSourceScope(source, item.Ref.Scope)) ||
				(item.Ref == nil && item.RelationFrom != nil && !sameSourceScope(source, item.RelationFrom.Scope)) {
				return fmt.Errorf("%w: selected source evidence is invalid or outside the captured scope", ErrInvalidRequest)
			}
		}
		if item.RelationID != nil {
			if _, err := model.ParseHash(string(*item.RelationID)); err != nil ||
				!validRelationKind(item.RelationKind) || !validEvidenceKind(item.EvidenceKind) ||
				!oneOf(item.ResolutionStatus, "exact_in_supported_scope", "candidate", "unresolved") {
				return fmt.Errorf("%w: selected relation evidence is invalid", ErrInvalidRequest)
			}
		}
	}
	return nil
}

func validateChangeConstraints(values []string) ([]ConstraintDisposition, error) {
	if len(values) > maxTaskConstraints {
		return nil, fmt.Errorf("%w: too many explicit change constraints", ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(values))
	constraints := make([]ConstraintDisposition, 0, len(values))
	for _, value := range values {
		if !validBoundedData(value, 128) {
			return nil, fmt.Errorf("%w: invalid explicit change constraint", ErrInvalidRequest)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("%w: duplicate explicit change constraint", ErrInvalidRequest)
		}
		seen[value] = struct{}{}
		supported := supportedChangeConstraint(value)
		disposition := ConstraintDisposition{
			Value: value, Supported: supported, Status: "not_run",
			Reason: "explicit request constraint is retained; this planner does not claim validation was executed",
		}
		if !supported {
			disposition.Status = "needs_context"
			disposition.Reason = "constraint is not supported by the sealed request vocabulary and remains explicit for review"
		}
		constraints = append(constraints, disposition)
	}
	sort.Slice(constraints, func(i, j int) bool { return constraints[i].Value < constraints[j].Value })
	return constraints, nil
}

func supportedChangeConstraint(value string) bool {
	return oneOf(value, "preserve_json_keys", "preserve_db_schema", "no_public_api_change",
		"preserve_message_schema", "no_source_mutation")
}

// taskFocus lists the evidence obligations from S10; it does not claim that
// the supplied graph contains or satisfies them.
func taskFocus(task TaskKind) []string {
	var focus []string
	switch task {
	case TaskExplore:
		focus = []string{"identity", "definition_module_summary", "capability_freshness"}
	case TaskPlan:
		focus = []string{"intended_observed_separation", "trusted_requirement_anchors", "existing_modules", "explicit_gaps"}
	case TaskRename:
		focus = []string{"target_binding", "declarations_references", "scope_shadowing_conflicts", "interface_serialization_boundaries", "validation_plan"}
	case TaskTypeChange:
		focus = []string{"declaration_type", "readers_writers_callers_implementers", "serialization_contracts", "relevant_tests"}
	case TaskBehavior:
		focus = []string{"current_function_source", "immediate_dependencies_callers", "invariants_error_paths", "related_tests"}
	case TaskContract:
		focus = []string{"explicit_providers_consumers", "version_serialization_keys", "backward_compatibility_constraints", "external_boundaries"}
	case TaskDebug:
		focus = []string{"supplied_symptom", "candidate_call_reference_path", "current_code_tests_diagnostics"}
	}
	return focus
}

func validateTrustedRulePaths(paths []TrustedRulePath, scope []ScopeIdentity) ([]TrustedRulePath, error) {
	if len(paths) > maxTrustedRulePaths {
		return nil, fmt.Errorf("%w: too many configured trusted rule paths", ErrInvalidRequest)
	}
	seen := make(map[string]struct{}, len(paths))
	validated := make([]TrustedRulePath, len(paths))
	for index, trusted := range paths {
		if model.ValidateRelativePath(trusted.Path) != nil || !containsScopeIdentity(scope, trusted.Scope) {
			return nil, fmt.Errorf("%w: trusted rule path is invalid or outside the captured scope", ErrInvalidRequest)
		}
		key := scopeIdentityKey(trusted.Scope) + "\x00" + trusted.Path
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%w: duplicate configured trusted rule path", ErrInvalidRequest)
		}
		seen[key] = struct{}{}
		validated[index] = TrustedRulePath{Scope: cloneScopeIdentity(trusted.Scope), Path: trusted.Path}
	}
	return validated, nil
}

func collectTrustedRuleAnchors(ctx context.Context, selection Result, paths []TrustedRulePath) ([]TrustedRuleAnchor, bool, error) {
	anchors := make([]TrustedRuleAnchor, 0)
	seen := make(map[string]struct{})
	trustedPathPrecedence := make(map[string]int, len(paths))
	for precedence, path := range paths {
		trustedPathPrecedence[trustedRulePathKey(path.Scope, path.Path)] = precedence
	}
	untrustedAnchor := false
	for _, item := range selection.Items {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if item.Ref == nil || (item.Ref.Kind != RefAnchor && item.Ref.Kind != RefFile) {
			continue
		}
		if item.Ref.Kind == RefFile && item.Ref.Path == "" {
			continue
		}
		if item.Ref.Kind == RefAnchor && len(item.SourceRefs) == 0 {
			untrustedAnchor = true
		}
		for _, source := range item.SourceRefs {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if !sameSourceScope(source, item.Ref.Scope) {
				continue
			}
			key := trustedRulePathKey(item.Ref.Scope, source.Path)
			precedence, trusted := trustedPathPrecedence[key]
			if item.Ref.Kind == RefAnchor && !trusted {
				untrustedAnchor = true
			}
			if !trusted || (item.Ref.Kind == RefFile && item.Ref.Path != source.Path) {
				continue
			}
			identity := key + "\x00" + sourceRefKey(source)
			if _, exists := seen[identity]; exists {
				continue
			}
			seen[identity] = struct{}{}
			anchors = append(anchors, TrustedRuleAnchor{
				ID:     stableItemID("trusted-rule-anchor", identity),
				Scope:  cloneScopeIdentity(item.Ref.Scope),
				Source: source, Precedence: precedence,
			})
		}
	}
	sort.Slice(anchors, func(i, j int) bool {
		if anchors[i].Precedence != anchors[j].Precedence {
			return anchors[i].Precedence < anchors[j].Precedence
		}
		return sourceRefKey(anchors[i].Source) < sourceRefKey(anchors[j].Source)
	})
	return anchors, untrustedAnchor, nil
}

func trustedRulePathKey(scope ScopeIdentity, path string) string {
	return scopeIdentityKey(scope) + "\x00" + path
}

func taskSliceCategory(task TaskKind, targetKey string, item Item) SliceCategory {
	if item.Kind == "boundary" || item.BoundaryCode != "" {
		return SliceSafetyBoundary
	}
	if item.Ref != nil && refKey(*item.Ref) == targetKey {
		return SliceTarget
	}
	if item.RelationID != nil {
		if item.EvidenceKind == "text_match" {
			return SliceLexicalCandidate
		}
		switch item.RelationKind {
		case "TEST_REF":
			return SliceTest
		case "DOC_LINK":
			return SliceDocumentation
		case "READS", "WRITES":
			return SliceDataflow
		case "IMPORTS":
			if task == TaskContract || task == TaskPlan {
				return SliceContract
			}
			return SliceDependency
		case "CALLS_DIRECT", "CALLS_INTERFACE", "MAY_DISPATCH_TO":
			return SliceDependency
		case "REFERS_TO", "UNRESOLVED_REF", "UNRESOLVED_CALL":
			return SliceReference
		default:
			return SliceNeighbor
		}
	}
	if item.Ref != nil {
		switch item.Ref.Kind {
		case RefAnchor:
			return SliceDocumentation
		case RefModule:
			return SliceModule
		}
	}
	if task == TaskPlan {
		if item.Plane == "intended" || item.Plane == "annotation" {
			return SliceIntended
		}
		if item.Plane == "observed_static" || item.Plane == "observed_runtime" {
			return SliceObserved
		}
	}
	if oneOf(item.NodeKind, "field", "function", "method", "type", "interface", "variable",
		"parameter", "receiver", "result", "constant", "closure", "interface_method", "type_parameter") {
		return SliceDeclaration
	}
	if item.Kind == "source_range" {
		return SliceSource
	}
	if item.Ref != nil && item.Ref.Kind == RefFile {
		return SliceSource
	}
	return SliceNeighbor
}

func relationEvidenceClass(item Item) string {
	if item.RelationID == nil {
		return "source_backed"
	}
	if item.EvidenceKind == "text_match" {
		return "lexical_only"
	}
	if item.ResolutionStatus == "unresolved" {
		return "unresolved"
	}
	if item.ResolutionStatus == "candidate" || item.EvidenceKind == "type_possible" {
		return "candidate"
	}
	return "evidence_backed"
}

func taskSliceItemLess(left, right TaskSliceItem) bool {
	leftRank, rightRank := sliceCategoryRank(left.Category), sliceCategoryRank(right.Category)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	if left.Evidence.RequiredForReview != right.Evidence.RequiredForReview {
		return left.Evidence.RequiredForReview
	}
	leftKind, rightKind := left.Evidence.RelationKind, right.Evidence.RelationKind
	if leftKind != rightKind {
		return leftKind < rightKind
	}
	leftKey, rightKey := itemSortKey(left.Evidence), itemSortKey(right.Evidence)
	if leftKey != rightKey {
		return leftKey < rightKey
	}
	return left.Evidence.ID < right.Evidence.ID
}

func sliceCategoryRank(category SliceCategory) int {
	switch category {
	case SliceSafetyBoundary:
		return 0
	case SliceTarget:
		return 1
	case SliceIntended, SliceObserved:
		return 2
	case SliceDeclaration, SliceSource, SliceReference, SliceDataflow, SliceDependency, SliceContract:
		return 3
	case SliceTest:
		return 4
	case SliceDocumentation, SliceModule:
		return 5
	case SliceLexicalCandidate:
		return 6
	default:
		return 7
	}
}

func buildValidationPlan(ctx context.Context, task TaskKind, intentPresent bool, items []TaskSliceItem, constraints []ConstraintDisposition) (ValidationPlan, error) {
	stepMap := make(map[string]*ValidationStep)
	implicitSerializationRisk := false
	if task == TaskDebug && !intentPresent {
		addValidationStep(stepMap, ValidationStep{
			Code: "supply_debug_symptom", Status: "needs_context", Required: true,
			Reason: "the selector and repository content do not establish the user's observed symptom",
		})
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return ValidationPlan{}, err
		}
		evidence := item.Evidence
		if evidence.PotentialImplicitSerializationNameChange {
			implicitSerializationRisk = true
		}
		if item.Category == SliceTest {
			addValidationStep(stepMap, ValidationStep{
				Code: "run_linked_tests", Status: "not_run", Required: evidence.RequiredForReview,
				EvidenceIDs: []model.Hash{evidence.ID},
				Reason:      "linked test evidence is identified; execution is outside context slice construction",
			})
		}
		if item.Category == SliceSafetyBoundary {
			addValidationStep(stepMap, ValidationStep{
				Code: "review_unresolved_boundary", Status: "needs_context", Required: evidence.RequiredForReview,
				EvidenceIDs: []model.Hash{evidence.ID},
				Reason:      "the selected graph exposes a boundary whose external behavior is not established",
			})
		}
		if item.EvidenceClass == "lexical_only" {
			addValidationStep(stepMap, ValidationStep{
				Code: "verify_lexical_candidate", Status: "needs_context", Required: false,
				EvidenceIDs: []model.Hash{evidence.ID},
				Reason:      "text match is a candidate only and does not establish a semantic relation or contract",
			})
		}
		if task == TaskRename && evidence.PotentialImplicitSerializationNameChange {
			addValidationStep(stepMap, ValidationStep{
				Code: "verify_implicit_serialization_key", Status: "needs_context", Required: true,
				EvidenceIDs: []model.Hash{evidence.ID},
				Reason:      "an exported Go field may derive its JSON key from its name; tag evidence is unavailable in this graph",
			})
		}
	}
	for _, constraint := range constraints {
		status := constraint.Status
		reason := constraint.Reason
		if task == TaskRename && constraint.Value == "preserve_json_keys" && implicitSerializationRisk {
			status = "needs_context"
			reason = "preserve_json_keys is retained, but this graph cannot establish explicit tag or runtime serialization behavior"
		}
		addValidationStep(stepMap, ValidationStep{
			Code: "validate_explicit_constraint", Subject: constraint.Value,
			Status: status, Required: true, Reason: reason,
		})
	}
	steps := make([]ValidationStep, 0, len(stepMap))
	for _, step := range stepMap {
		sort.Slice(step.EvidenceIDs, func(i, j int) bool { return step.EvidenceIDs[i] < step.EvidenceIDs[j] })
		step.EvidenceIDs = uniqueHashes(step.EvidenceIDs)
		steps = append(steps, *step)
	}
	sort.Slice(steps, func(i, j int) bool {
		left, right := validationStepKey(steps[i]), validationStepKey(steps[j])
		return left < right
	})
	return ValidationPlan{Status: "not_run", Steps: steps}, nil
}

func addValidationStep(steps map[string]*ValidationStep, value ValidationStep) {
	key := strings.Join([]string{value.Code, value.Subject, value.Status, value.Reason}, "\x00")
	step, exists := steps[key]
	if !exists {
		step = &ValidationStep{
			Code: value.Code, Subject: value.Subject, Status: value.Status,
			Required: value.Required, Reason: value.Reason,
			EvidenceIDs: make([]model.Hash, 0, len(value.EvidenceIDs)),
		}
		steps[key] = step
	} else if value.Required {
		step.Required = true
	}
	step.EvidenceIDs = append(step.EvidenceIDs, value.EvidenceIDs...)
}

func uniqueHashes(values []model.Hash) []model.Hash {
	if len(values) < 2 {
		return values
	}
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

func hasConstraint(constraints []ConstraintDisposition, value string) bool {
	for _, constraint := range constraints {
		if constraint.Value == value {
			return true
		}
	}
	return false
}

func hasImplicitSerializationRisk(items []TaskSliceItem) bool {
	for _, item := range items {
		if item.Evidence.PotentialImplicitSerializationNameChange {
			return true
		}
	}
	return false
}

func validationStepKey(step ValidationStep) string {
	return strings.Join([]string{step.Code, step.Subject, step.Status, step.Reason}, "\x00")
}

func cloneItem(item Item) Item {
	item.Ref = cloneRefPointer(item.Ref)
	item.RelationFrom = cloneRefPointer(item.RelationFrom)
	item.RelationTo = cloneRefPointer(item.RelationTo)
	if item.RelationID != nil {
		value := *item.RelationID
		item.RelationID = &value
	}
	item.SourceRefs = cloneSourceRefs(item.SourceRefs)
	return item
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneScopeIdentity(identity ScopeIdentity) ScopeIdentity {
	identity.ServiceID = cloneIdentifier(identity.ServiceID)
	return identity
}

func containsScopeIdentity(scope []ScopeIdentity, identity ScopeIdentity) bool {
	key := scopeIdentityKey(identity)
	for _, candidate := range scope {
		if scopeIdentityKey(candidate) == key {
			return true
		}
	}
	return false
}

func sourceInScopeIdentities(source model.SourceRef, scope []ScopeIdentity) bool {
	for _, identity := range scope {
		if sameSourceScope(source, identity) {
			return true
		}
	}
	return false
}

func sourceRefKey(source model.SourceRef) string {
	return strings.Join([]string{
		string(source.WorktreeID), string(source.ProfileID), string(source.GenerationID), source.Path,
		string(source.ContentHash), fmt.Sprint(source.Range.StartByte), fmt.Sprint(source.Range.EndByte),
	}, "\x00")
}

func validBoundedData(value string, maxRunes int) bool {
	if utf8.RuneCountInString(value) < 1 || utf8.RuneCountInString(value) > maxRunes || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}
