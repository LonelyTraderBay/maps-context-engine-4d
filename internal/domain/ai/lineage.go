// Package ai reviews source-bound AI/ML/RAG metadata without loading or
// executing datasets, embeddings, models, checkpoints, notebooks, or providers.
package ai

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	MaxComponents            = 512
	MaxLinks                 = 4096
	MaxGaps                  = 4096
	MaxAttributesPerSnapshot = 4096
	MaxSourceRefsPerSnapshot = 4096
	MaxAttributesPerNode     = 128
	MaxSourceRefsPerRecord   = 128
	MaxMetadataValueBytes    = 4096
	MaxSourcePathBytes       = 4096
)

const (
	FieldModelID                       = "model_id"
	FieldProviderID                    = "provider_id"
	FieldRevision                      = "revision"
	FieldPromptTemplateHash            = "prompt_template_hash"
	FieldInputSchemaRef                = "input_schema_ref"
	FieldOutputSchemaRef               = "output_schema_ref"
	FieldEmbeddingDimension            = "embedding_dimension"
	FieldDistanceMetric                = "distance_metric"
	FieldTokenizerVersion              = "tokenizer_version"
	FieldChunkingConfigHash            = "chunking_config_hash"
	FieldDatasetDigest                 = "dataset_digest"
	FieldDatasetSchemaRef              = "dataset_schema_ref"
	FieldSplitRole                     = "split_role"
	FieldPreprocessingConfigHash       = "preprocessing_config_hash"
	FieldFeatureOrderHash              = "feature_order_hash"
	FieldTrainingConfigHash            = "training_config_hash"
	FieldModelArtifactRef              = "model_artifact_ref"
	FieldInferenceConfigHash           = "inference_config_hash"
	FieldEvaluationMetric              = "evaluation_metric"
	FieldEvaluationMetricDefinitionRef = "evaluation_metric_definition_ref"
	FieldEvaluationThreshold           = "evaluation_threshold"
	FieldServingConfigRef              = "serving_config_ref"
	FieldRetryPolicyRef                = "retry_policy_ref"
	FieldTimeout                       = "timeout"
	FieldApprovedToolScopeRef          = "approved_tool_scope_ref"
	FieldFallbackPolicyRef             = "fallback_policy_ref"
	FieldIndexSchemaRef                = "index_schema_ref"
)

var ErrInvalidLineage = errors.New("invalid AI metadata lineage")

type Stage string

const (
	StageDataset          Stage = "dataset"
	StageSchema           Stage = "schema"
	StageSplit            Stage = "split"
	StagePreprocessing    Stage = "preprocessing"
	StageFeatures         Stage = "features"
	StageTrainingConfig   Stage = "training_config"
	StageModelArtifact    Stage = "model_artifact"
	StageInference        Stage = "inference"
	StagePostprocessing   Stage = "postprocessing"
	StageEvaluation       Stage = "evaluation"
	StageServing          Stage = "serving"
	StagePromptTemplate   Stage = "prompt_template"
	StageTokenizer        Stage = "tokenizer"
	StageChunker          Stage = "chunker"
	StageEmbedding        Stage = "embedding"
	StageRetrievalIndex   Stage = "retrieval_index"
	StageReranker         Stage = "reranker"
	StageModelInvocation  Stage = "model_invocation"
	StageOutputValidation Stage = "output_validation"
	StageFallback         Stage = "fallback"
)

type State string

const (
	StateImplemented   State = "implemented"
	StatePlanned       State = "planned"
	StateObservedInRun State = "observed_in_run"
	StateUnsupported   State = "unsupported"
	StateNotSeen       State = "not_seen"
)

type RevisionState string

const (
	RevisionPinned        RevisionState = "pinned"
	RevisionObserved      RevisionState = "observed"
	RevisionUnversioned   RevisionState = "unversioned"
	RevisionUnknown       RevisionState = "unknown"
	RevisionNotApplicable RevisionState = "not_applicable"
)

type Coverage string

const (
	CoverageComplete Coverage = "complete_for_declared_scope"
	CoveragePartial  Coverage = "partial"
	CoverageUnknown  Coverage = "unknown"
)

type Snapshot struct {
	// Complete means the supplied metadata covers only the caller-declared
	// scope. Its coverage manifest must have been verified by the caller before
	// being supplied here; this package checks only SourceRef structure. It
	// never asserts that a pipeline ran or that runtime was observed.
	Coverage        Coverage
	CoverageSources []model.SourceRef
	Components      []Component
	Links           []Link
	Gaps            []Gap
}

// Component is one declared, observed, or explicitly missing pipeline stage.
// ID is a producer-supplied stable key; names and similar values are never
// used to match components across snapshots.
type Component struct {
	ID            string
	Stage         Stage
	State         State
	RevisionState RevisionState
	Sources       []model.SourceRef
	Attributes    []Attribute
}

// Attribute stores one bounded scalar metadata value, never a payload.
// Prompt/template material must be represented by a hash or opaque reference.
type Attribute struct {
	Key        string
	Value      string
	SourceRefs []model.SourceRef
}

// Link is an explicit dependency assertion. It is used only when its source
// references are structurally valid and both endpoints are source-bound.
type Link struct {
	From       string
	To         string
	Relation   string
	SourceRefs []model.SourceRef
}

type Gap struct {
	Code        string
	ComponentID string
	Field       string
	Reason      string
}

// ReviewedSnapshot contains only source-bound components, attributes and
// links. SourceRef hashes and ranges are syntax-checked here; their bytes are
// not opened or verified by this metadata-only package.
type ReviewedSnapshot struct {
	Coverage        Coverage
	CoverageSources []model.SourceRef
	Components      []Component
	Links           []Link
	Gaps            []Gap
}

type ChangeKind string

const (
	ChangeAdded         ChangeKind = "added"
	ChangeRemoved       ChangeKind = "removed"
	ChangeChanged       ChangeKind = "changed"
	ChangeSourceChanged ChangeKind = "source_changed"
)

type Change struct {
	Kind          ChangeKind
	ComponentID   string
	Field         string
	Before        string
	After         string
	BeforeSources []model.SourceRef
	AfterSources  []model.SourceRef
}

type LinkChange struct {
	Kind          ChangeKind
	From          string
	To            string
	Relation      string
	BeforeSources []model.SourceRef
	AfterSources  []model.SourceRef
}

type Report struct {
	CoverageBefore Coverage
	CoverageAfter  Coverage
	Changes        []Change
	LinkChanges    []LinkChange
	Affected       []string
	Gaps           []Gap
	// SourceContentVerification remains structural_only because this package
	// intentionally does not open source files or artifact references.
	SourceContentVerification string
}

const SourceContentStructuralOnly = "structural_only"

// ReviewSnapshot validates metadata structure, keeps only source-bound facts,
// and reports missing lineage as gaps instead of guessing from names.
func ReviewSnapshot(snapshot Snapshot) (ReviewedSnapshot, error) {
	if len(snapshot.Components) > MaxComponents || len(snapshot.Links) > MaxLinks || len(snapshot.Gaps) > MaxGaps {
		return ReviewedSnapshot{}, invalid("snapshot exceeds component, link, or gap limit")
	}
	totalAttributes := 0
	totalSourceRefs := len(snapshot.CoverageSources)
	for _, component := range snapshot.Components {
		if len(component.Attributes) > MaxAttributesPerNode || totalAttributes > MaxAttributesPerSnapshot-len(component.Attributes) {
			return ReviewedSnapshot{}, invalid("snapshot exceeds attribute limit")
		}
		totalAttributes += len(component.Attributes)
		if totalSourceRefs > MaxSourceRefsPerSnapshot-len(component.Sources) {
			return ReviewedSnapshot{}, invalid("snapshot exceeds source reference limit")
		}
		totalSourceRefs += len(component.Sources)
		for _, attribute := range component.Attributes {
			if totalSourceRefs > MaxSourceRefsPerSnapshot-len(attribute.SourceRefs) {
				return ReviewedSnapshot{}, invalid("snapshot exceeds source reference limit")
			}
			totalSourceRefs += len(attribute.SourceRefs)
		}
	}
	for _, link := range snapshot.Links {
		if totalSourceRefs > MaxSourceRefsPerSnapshot-len(link.SourceRefs) {
			return ReviewedSnapshot{}, invalid("snapshot exceeds source reference limit")
		}
		totalSourceRefs += len(link.SourceRefs)
	}
	if totalSourceRefs > MaxSourceRefsPerSnapshot {
		return ReviewedSnapshot{}, invalid("snapshot exceeds source reference limit")
	}
	coverage := snapshot.Coverage
	if coverage == "" {
		coverage = CoverageUnknown
	}
	if !validCoverage(coverage) {
		return ReviewedSnapshot{}, invalid("coverage state is invalid")
	}
	if err := validateSourceRefs(snapshot.CoverageSources); err != nil {
		return ReviewedSnapshot{}, invalid("coverage source reference is invalid")
	}
	coverageSources := cloneSourceRefs(snapshot.CoverageSources)
	gaps := cloneGaps(snapshot.Gaps)
	for _, gap := range gaps {
		if !validToken(gap.Code, 128) || (gap.ComponentID != "" && !validToken(gap.ComponentID, 128)) ||
			(gap.Field != "" && !validAttributeKey(gap.Field)) || !validScalar(gap.Reason) {
			return ReviewedSnapshot{}, invalid("explicit gap identity or reason is invalid")
		}
	}
	if coverage == CoverageComplete && len(coverageSources) == 0 {
		coverage = CoverageUnknown
		gaps = append(gaps, Gap{Code: "coverage_unbound", Reason: "complete coverage is not bound to a declared metadata manifest"})
	}

	components := make(map[string]Component, len(snapshot.Components))
	trusted := make(map[string]bool, len(snapshot.Components))
	for _, component := range snapshot.Components {
		if !validToken(component.ID, 128) || !validStage(component.Stage) || !validState(component.State) || !validRevisionState(component.RevisionState) {
			return ReviewedSnapshot{}, invalid("component identity, stage, state, or revision state is invalid")
		}
		if _, exists := components[component.ID]; exists {
			return ReviewedSnapshot{}, invalid("component IDs must be unique")
		}
		if err := validateSourceRefs(component.Sources); err != nil {
			return ReviewedSnapshot{}, invalid("component source reference is invalid")
		}
		if len(component.Attributes) > MaxAttributesPerNode {
			return ReviewedSnapshot{}, invalid("component exceeds attribute limit")
		}
		if len(component.Sources) == 0 {
			gaps = append(gaps, Gap{Code: "component_unbound", ComponentID: component.ID, Reason: "component identity and stage have no source reference"})
		}
		seenKeys := make(map[string]struct{}, len(component.Attributes))
		copyComponent := Component{
			ID: component.ID, Stage: component.Stage, State: component.State,
			RevisionState: component.RevisionState,
			Sources:       cloneSourceRefs(component.Sources),
		}
		for _, attribute := range component.Attributes {
			if !validAttributeKey(attribute.Key) || !validScalar(attribute.Value) {
				return ReviewedSnapshot{}, invalid("attribute key or scalar value is invalid")
			}
			if _, exists := seenKeys[attribute.Key]; exists {
				return ReviewedSnapshot{}, invalid("attribute keys must be unique per component")
			}
			seenKeys[attribute.Key] = struct{}{}
			if err := validateAttributeValue(attribute.Key, attribute.Value); err != nil {
				return ReviewedSnapshot{}, invalid("attribute value does not match its metadata type")
			}
			if err := validateSourceRefs(attribute.SourceRefs); err != nil {
				return ReviewedSnapshot{}, invalid("attribute source reference is invalid")
			}
			if len(attribute.SourceRefs) == 0 {
				gaps = append(gaps, Gap{Code: "metadata_unbound", ComponentID: component.ID, Field: attribute.Key, Reason: "metadata value has no source reference and is excluded from comparison"})
				continue
			}
			copyComponent.Attributes = append(copyComponent.Attributes, Attribute{
				Key: attribute.Key, Value: attribute.Value, SourceRefs: cloneSourceRefs(attribute.SourceRefs),
			})
		}
		sort.Slice(copyComponent.Attributes, func(i, j int) bool { return copyComponent.Attributes[i].Key < copyComponent.Attributes[j].Key })
		components[component.ID] = copyComponent
		trusted[component.ID] = len(component.Sources) > 0
		gaps = append(gaps, componentObligationGaps(component, copyComponent.Attributes)...)
	}

	links := make([]Link, 0, len(snapshot.Links))
	seenLinks := make(map[string]struct{}, len(snapshot.Links))
	for _, link := range snapshot.Links {
		if !validToken(link.From, 128) || !validToken(link.To, 128) || !validToken(link.Relation, 128) {
			return ReviewedSnapshot{}, invalid("link identity is invalid")
		}
		key := linkKey(link)
		if _, exists := seenLinks[key]; exists {
			return ReviewedSnapshot{}, invalid("links must be unique by exact endpoints and relation")
		}
		seenLinks[key] = struct{}{}
		if err := validateSourceRefs(link.SourceRefs); err != nil {
			return ReviewedSnapshot{}, invalid("link source reference is invalid")
		}
		if _, ok := components[link.From]; !ok {
			gaps = append(gaps, Gap{Code: "lineage_endpoint_missing", ComponentID: link.From, Field: link.Relation, Reason: "link source endpoint is absent from the supplied metadata scope"})
			continue
		}
		if _, ok := components[link.To]; !ok {
			gaps = append(gaps, Gap{Code: "lineage_endpoint_missing", ComponentID: link.To, Field: link.Relation, Reason: "link target endpoint is absent from the supplied metadata scope"})
			continue
		}
		if len(link.SourceRefs) == 0 {
			gaps = append(gaps, Gap{Code: "lineage_link_unbound", ComponentID: link.From, Field: link.Relation, Reason: "link has no source reference and is excluded from dependency propagation"})
			continue
		}
		if !trusted[link.From] || !trusted[link.To] {
			gaps = append(gaps, Gap{Code: "lineage_endpoint_unbound", ComponentID: link.From, Field: link.Relation, Reason: "link endpoint is not source-bound and is excluded from dependency propagation"})
			continue
		}
		links = append(links, Link{From: link.From, To: link.To, Relation: link.Relation, SourceRefs: cloneSourceRefs(link.SourceRefs)})
	}
	sort.Slice(links, func(i, j int) bool { return linkKey(links[i]) < linkKey(links[j]) })
	orderedComponents := make([]Component, 0, len(components))
	for _, component := range components {
		if trusted[component.ID] {
			orderedComponents = append(orderedComponents, component)
		}
	}
	sort.Slice(orderedComponents, func(i, j int) bool { return orderedComponents[i].ID < orderedComponents[j].ID })
	normalizedGaps := normalizeGaps(gaps)
	if len(normalizedGaps) > MaxGaps {
		return ReviewedSnapshot{}, invalid("review generates more than the gap limit")
	}
	return ReviewedSnapshot{
		Coverage: coverage, CoverageSources: coverageSources, Components: orderedComponents,
		Links: links, Gaps: normalizedGaps,
	}, nil
}

// Compare detects changes only by caller-supplied component IDs and source-
// bound metadata. Downstream impact follows explicit, source-bound links.
func Compare(before, after Snapshot) (Report, error) {
	left, err := ReviewSnapshot(before)
	if err != nil {
		return Report{}, fmt.Errorf("before snapshot: %w", err)
	}
	right, err := ReviewSnapshot(after)
	if err != nil {
		return Report{}, fmt.Errorf("after snapshot: %w", err)
	}
	report := Report{
		CoverageBefore: left.Coverage, CoverageAfter: right.Coverage,
		Gaps:                      normalizeGaps(append(append([]Gap(nil), left.Gaps...), right.Gaps...)),
		SourceContentVerification: SourceContentStructuralOnly,
	}
	leftByID := componentMap(left.Components)
	rightByID := componentMap(right.Components)
	leftRaw := rawComponentMap(before.Components)
	rightRaw := rawComponentMap(after.Components)
	allIDs := unionKeys(leftByID, rightByID)
	for _, id := range allIDs {
		oldComponent, hadOld := leftByID[id]
		newComponent, hasNew := rightByID[id]
		if hadOld && !hasNew {
			if _, rawPresent := rightRaw[id]; rawPresent {
				continue // Present but unbound: the review already emitted a gap.
			}
			if right.Coverage == CoverageComplete {
				report.Changes = append(report.Changes, Change{Kind: ChangeRemoved, ComponentID: id, Field: "$component", Before: componentSummary(oldComponent), BeforeSources: cloneSourceRefs(oldComponent.Sources)})
			} else {
				report.Gaps = append(report.Gaps, Gap{Code: "component_absence_unverified", ComponentID: id, Reason: "after snapshot is not complete for its declared scope"})
			}
			continue
		}
		if hasNew && !hadOld {
			if _, rawPresent := leftRaw[id]; rawPresent {
				continue
			}
			if left.Coverage == CoverageComplete {
				report.Changes = append(report.Changes, Change{Kind: ChangeAdded, ComponentID: id, Field: "$component", After: componentSummary(newComponent), AfterSources: cloneSourceRefs(newComponent.Sources)})
			} else {
				report.Gaps = append(report.Gaps, Gap{Code: "component_absence_unverified", ComponentID: id, Reason: "before snapshot is not complete for its declared scope"})
			}
			continue
		}
		if hadOld && hasNew {
			compareComponent(oldComponent, newComponent, before, after, &report)
		}
	}
	compareLinks(before, after, left, right, &report)
	report.Affected = affectedComponents(changedComponentIDs(report.Changes, report.LinkChanges), left.Links, right.Links)
	report.Gaps = normalizeGaps(report.Gaps)
	sort.Slice(report.Changes, func(i, j int) bool {
		if report.Changes[i].ComponentID != report.Changes[j].ComponentID {
			return report.Changes[i].ComponentID < report.Changes[j].ComponentID
		}
		if report.Changes[i].Field != report.Changes[j].Field {
			return report.Changes[i].Field < report.Changes[j].Field
		}
		return report.Changes[i].Kind < report.Changes[j].Kind
	})
	sort.Slice(report.LinkChanges, func(i, j int) bool {
		return linkChangeKey(report.LinkChanges[i]) < linkChangeKey(report.LinkChanges[j])
	})
	return report, nil
}

func compareComponent(before, after Component, beforeSnapshot, afterSnapshot Snapshot, report *Report) {
	if before.Stage != after.Stage {
		report.Changes = append(report.Changes, Change{Kind: ChangeChanged, ComponentID: before.ID, Field: "$stage", Before: string(before.Stage), After: string(after.Stage), BeforeSources: cloneSourceRefs(before.Sources), AfterSources: cloneSourceRefs(after.Sources)})
	}
	if before.State != after.State {
		report.Changes = append(report.Changes, Change{Kind: ChangeChanged, ComponentID: before.ID, Field: "$state", Before: string(before.State), After: string(after.State), BeforeSources: cloneSourceRefs(before.Sources), AfterSources: cloneSourceRefs(after.Sources)})
	}
	if before.RevisionState != after.RevisionState {
		report.Changes = append(report.Changes, Change{Kind: ChangeChanged, ComponentID: before.ID, Field: "$revision_state", Before: string(before.RevisionState), After: string(after.RevisionState), BeforeSources: cloneSourceRefs(before.Sources), AfterSources: cloneSourceRefs(after.Sources)})
	}
	if !sameSourceRefs(before.Sources, after.Sources) && before.Stage == after.Stage && before.State == after.State && before.RevisionState == after.RevisionState {
		report.Changes = append(report.Changes, Change{Kind: ChangeSourceChanged, ComponentID: before.ID, Field: "$source", BeforeSources: cloneSourceRefs(before.Sources), AfterSources: cloneSourceRefs(after.Sources)})
	}
	oldFields, newFields := attributeMap(before.Attributes), attributeMap(after.Attributes)
	for _, key := range unionKeys(oldFields, newFields) {
		oldField, hadOld := oldFields[key]
		newField, hasNew := newFields[key]
		if hadOld && hasNew {
			if oldField.Value != newField.Value {
				report.Changes = append(report.Changes, Change{Kind: ChangeChanged, ComponentID: before.ID, Field: key, Before: oldField.Value, After: newField.Value, BeforeSources: cloneSourceRefs(oldField.SourceRefs), AfterSources: cloneSourceRefs(newField.SourceRefs)})
			} else if !sameSourceRefs(oldField.SourceRefs, newField.SourceRefs) {
				report.Changes = append(report.Changes, Change{Kind: ChangeSourceChanged, ComponentID: before.ID, Field: key, Before: oldField.Value, After: newField.Value, BeforeSources: cloneSourceRefs(oldField.SourceRefs), AfterSources: cloneSourceRefs(newField.SourceRefs)})
			}
			continue
		}
		if hadOld {
			if rawHasAttribute(afterSnapshot, before.ID, key) {
				continue
			}
			if report.CoverageAfter == CoverageComplete {
				report.Changes = append(report.Changes, Change{Kind: ChangeRemoved, ComponentID: before.ID, Field: key, Before: oldField.Value, BeforeSources: cloneSourceRefs(oldField.SourceRefs)})
			} else {
				report.Gaps = append(report.Gaps, Gap{Code: "metadata_absence_unverified", ComponentID: before.ID, Field: key, Reason: "after snapshot is not complete for its declared scope"})
			}
		} else {
			if rawHasAttribute(beforeSnapshot, before.ID, key) {
				continue
			}
			if report.CoverageBefore == CoverageComplete {
				report.Changes = append(report.Changes, Change{Kind: ChangeAdded, ComponentID: before.ID, Field: key, After: newField.Value, AfterSources: cloneSourceRefs(newField.SourceRefs)})
			} else {
				report.Gaps = append(report.Gaps, Gap{Code: "metadata_absence_unverified", ComponentID: before.ID, Field: key, Reason: "before snapshot is not complete for its declared scope"})
			}
		}
	}
}

func compareLinks(before, after Snapshot, left, right ReviewedSnapshot, report *Report) {
	oldLinks, newLinks := linkMap(left.Links), linkMap(right.Links)
	oldRaw, newRaw := rawLinkMap(before.Links), rawLinkMap(after.Links)
	for _, key := range unionKeys(oldLinks, newLinks) {
		oldLink, hadOld := oldLinks[key]
		newLink, hasNew := newLinks[key]
		if hadOld && hasNew {
			if !sameSourceRefs(oldLink.SourceRefs, newLink.SourceRefs) {
				report.LinkChanges = append(report.LinkChanges, LinkChange{Kind: ChangeSourceChanged, From: oldLink.From, To: oldLink.To, Relation: oldLink.Relation, BeforeSources: cloneSourceRefs(oldLink.SourceRefs), AfterSources: cloneSourceRefs(newLink.SourceRefs)})
			}
			continue
		}
		if hadOld {
			if _, present := newRaw[key]; present {
				continue
			}
			if right.Coverage == CoverageComplete {
				report.LinkChanges = append(report.LinkChanges, LinkChange{Kind: ChangeRemoved, From: oldLink.From, To: oldLink.To, Relation: oldLink.Relation, BeforeSources: cloneSourceRefs(oldLink.SourceRefs)})
			} else {
				report.Gaps = append(report.Gaps, Gap{Code: "lineage_absence_unverified", ComponentID: oldLink.From, Field: oldLink.Relation, Reason: "after snapshot is not complete for its declared scope"})
			}
		} else {
			if _, present := oldRaw[key]; present {
				continue
			}
			if left.Coverage == CoverageComplete {
				report.LinkChanges = append(report.LinkChanges, LinkChange{Kind: ChangeAdded, From: newLink.From, To: newLink.To, Relation: newLink.Relation, AfterSources: cloneSourceRefs(newLink.SourceRefs)})
			} else {
				report.Gaps = append(report.Gaps, Gap{Code: "lineage_absence_unverified", ComponentID: newLink.From, Field: newLink.Relation, Reason: "before snapshot is not complete for its declared scope"})
			}
		}
	}
}

func componentObligationGaps(original Component, bound []Attribute) []Gap {
	fields := attributeMap(bound)
	var gaps []Gap
	missing := func(key, code, reason string) {
		if _, ok := fields[key]; !ok {
			gaps = append(gaps, Gap{Code: code, ComponentID: original.ID, Field: key, Reason: reason})
		}
	}
	switch original.Stage {
	case StageDataset:
		missing(FieldDatasetDigest, "dataset_digest_missing", "dataset identity is not bound to a declared digest")
	case StageSchema:
		missing(FieldDatasetSchemaRef, "dataset_schema_missing", "dataset schema is not source-bound")
	case StageSplit:
		missing(FieldSplitRole, "split_role_missing", "train/validation/test role is not declared in source-bound metadata")
	case StagePreprocessing:
		missing(FieldPreprocessingConfigHash, "preprocessing_config_missing", "preprocessing configuration is not source-bound")
	case StageFeatures:
		missing(FieldFeatureOrderHash, "feature_order_missing", "feature names and order are not source-bound")
	case StageTrainingConfig:
		missing(FieldTrainingConfigHash, "training_config_missing", "training configuration is not source-bound")
	case StageModelArtifact:
		missing(FieldModelArtifactRef, "model_artifact_missing", "model artifact is represented only by an opaque source-bound reference")
	case StageInference:
		missing(FieldInferenceConfigHash, "inference_config_missing", "inference configuration is not source-bound")
	case StagePromptTemplate:
		missing(FieldPromptTemplateHash, "prompt_hash_missing", "prompt/template content must be represented by a source-bound hash or opaque reference")
	case StageTokenizer:
		missing(FieldTokenizerVersion, "tokenizer_version_missing", "tokenizer version is not source-bound")
	case StageChunker:
		missing(FieldChunkingConfigHash, "chunking_config_missing", "chunking configuration is not source-bound")
	case StageEmbedding:
		missing(FieldEmbeddingDimension, "embedding_dimension_missing", "embedding dimension is not declared in source-bound metadata")
		missing(FieldDistanceMetric, "distance_metric_missing", "embedding distance metric is not source-bound")
	case StageRetrievalIndex:
		missing(FieldIndexSchemaRef, "index_schema_missing", "retrieval/index schema reference is not declared")
	case StageModelInvocation:
		missing(FieldInputSchemaRef, "input_schema_missing", "model invocation input schema is not source-bound")
		missing(FieldOutputSchemaRef, "output_schema_missing", "model invocation output schema is not source-bound")
	case StageOutputValidation:
		missing(FieldOutputSchemaRef, "output_schema_missing", "validated output schema is not source-bound")
	case StageEvaluation:
		missing(FieldEvaluationMetric, "evaluation_metric_missing", "evaluation metric is not declared")
		missing(FieldEvaluationMetricDefinitionRef, "evaluation_definition_missing", "evaluation metric definition is not source-bound")
	case StageServing:
		missing(FieldServingConfigRef, "serving_config_missing", "serving configuration is not source-bound")
		missing(FieldRetryPolicyRef, "retry_policy_missing", "serving retry policy is not source-bound")
		missing(FieldTimeout, "timeout_missing", "serving timeout is not declared")
	case StageFallback:
		missing(FieldFallbackPolicyRef, "fallback_policy_missing", "fallback policy is not source-bound")
	}
	if isModelStage(original.Stage) {
		if requiresModelIdentifier(original.Stage) {
			if _, hasModel := fields[FieldModelID]; !hasModel {
				if _, hasProvider := fields[FieldProviderID]; !hasProvider {
					gaps = append(gaps, Gap{Code: "model_identifier_missing", ComponentID: original.ID, Field: FieldModelID, Reason: "model/provider identifier is not source-bound"})
				}
			}
		}
		switch original.RevisionState {
		case RevisionPinned:
			missing(FieldRevision, "model_revision_missing", "pinned model/provider revision is not source-bound")
		case RevisionObserved:
			missing(FieldRevision, "model_revision_missing", "observed model/provider revision is not source-bound")
			gaps = append(gaps, Gap{Code: "model_revision_unpinned", ComponentID: original.ID, Field: FieldRevision, Reason: "an observed revision is not a pinned model/provider revision"})
		case RevisionUnversioned, RevisionUnknown:
			gaps = append(gaps, Gap{Code: "model_revision_unpinned", ComponentID: original.ID, Field: FieldRevision, Reason: "model/provider revision is unversioned or unknown; retain only the observed/requested identifier"})
		case RevisionNotApplicable:
			gaps = append(gaps, Gap{Code: "model_revision_unpinned", ComponentID: original.ID, Field: FieldRevision, Reason: "model-bearing stage cannot claim revision is not applicable"})
		}
	}
	return gaps
}

func validateSourceRefs(refs []model.SourceRef) error {
	if len(refs) > MaxSourceRefsPerRecord {
		return invalid("source reference count exceeds limit")
	}
	for _, ref := range refs {
		if len(ref.Path) > MaxSourcePathBytes {
			return invalid("source reference path exceeds limit")
		}
		if _, err := model.ParseUUID(string(ref.WorktreeID)); err != nil {
			return err
		}
		if _, err := model.ParseHash(string(ref.ProfileID)); err != nil {
			return err
		}
		if _, err := model.ParseUUID(string(ref.GenerationID)); err != nil {
			return err
		}
		if _, err := model.ParseHash(string(ref.ContentHash)); err != nil {
			return err
		}
		if err := model.ValidateRelativePath(ref.Path); err != nil {
			return err
		}
		if ref.Range.StartByte < 0 || ref.Range.EndByte <= ref.Range.StartByte {
			return model.ErrInvalidRange
		}
	}
	return nil
}

func validateAttributeValue(key, value string) error {
	if strings.HasSuffix(key, "_hash") || strings.HasSuffix(key, "_digest") {
		if _, err := model.ParseHash(value); err != nil {
			return err
		}
	}
	if key == FieldEmbeddingDimension {
		dimension, err := strconv.ParseInt(value, 10, 64)
		if err != nil || dimension <= 0 || dimension > model.MaxSafeInteger {
			return errors.New("embedding dimension must be a positive safe integer")
		}
	}
	return nil
}

func validAttributeKey(value string) bool {
	if len(value) == 0 || len(value) > 96 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '.' || char == '-') {
			return false
		}
	}
	for _, forbidden := range []string{"payload", "dataset_rows", "embedding_values", "weight_bytes", "notebook_output", "pickle", "serialized"} {
		if strings.Contains(value, forbidden) {
			return false
		}
	}
	for _, contentKey := range []string{"prompt", "template", "checkpoint", "weights"} {
		if strings.Contains(value, contentKey) && !strings.HasSuffix(value, "_hash") && !strings.HasSuffix(value, "_digest") && !strings.HasSuffix(value, "_ref") {
			return false
		}
	}
	return true
}

func validScalar(value string) bool {
	if value == "" || len(value) > MaxMetadataValueBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func validToken(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes {
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

func validStage(value Stage) bool {
	switch value {
	case StageDataset, StageSchema, StageSplit, StagePreprocessing, StageFeatures, StageTrainingConfig, StageModelArtifact, StageInference, StagePostprocessing, StageEvaluation, StageServing, StagePromptTemplate, StageTokenizer, StageChunker, StageEmbedding, StageRetrievalIndex, StageReranker, StageModelInvocation, StageOutputValidation, StageFallback:
		return true
	default:
		return false
	}
}

func validState(value State) bool {
	switch value {
	case StateImplemented, StatePlanned, StateObservedInRun, StateUnsupported, StateNotSeen:
		return true
	default:
		return false
	}
}

func validRevisionState(value RevisionState) bool {
	switch value {
	case RevisionPinned, RevisionObserved, RevisionUnversioned, RevisionUnknown, RevisionNotApplicable:
		return true
	default:
		return false
	}
}

func validCoverage(value Coverage) bool {
	return value == CoverageComplete || value == CoveragePartial || value == CoverageUnknown
}

func isModelStage(stage Stage) bool {
	switch stage {
	case StageModelArtifact, StageInference, StageServing, StageEmbedding, StageReranker, StageModelInvocation:
		return true
	default:
		return false
	}
}

func requiresModelIdentifier(stage Stage) bool {
	switch stage {
	case StageInference, StageServing, StageEmbedding, StageReranker, StageModelInvocation:
		return true
	default:
		return false
	}
}

func invalid(reason string) error { return fmt.Errorf("%s: %w", reason, ErrInvalidLineage) }

func componentSummary(component Component) string {
	return string(component.Stage) + ";" + string(component.State) + ";" + string(component.RevisionState)
}

func componentMap(components []Component) map[string]Component {
	result := make(map[string]Component, len(components))
	for _, component := range components {
		result[component.ID] = component
	}
	return result
}

func rawComponentMap(components []Component) map[string]Component {
	result := make(map[string]Component, len(components))
	for _, component := range components {
		result[component.ID] = component
	}
	return result
}

func attributeMap(attributes []Attribute) map[string]Attribute {
	result := make(map[string]Attribute, len(attributes))
	for _, attribute := range attributes {
		result[attribute.Key] = attribute
	}
	return result
}

func unionKeys[A any](left, right map[string]A) []string {
	set := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		set[key] = struct{}{}
	}
	for key := range right {
		set[key] = struct{}{}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func rawHasAttribute(snapshot Snapshot, componentID, key string) bool {
	for _, component := range snapshot.Components {
		if component.ID == componentID {
			for _, field := range component.Attributes {
				if field.Key == key {
					return true
				}
			}
			return false
		}
	}
	return false
}

func linkKey(link Link) string { return link.From + "\x00" + link.To + "\x00" + link.Relation }

func linkChangeKey(change LinkChange) string {
	return change.From + "\x00" + change.To + "\x00" + change.Relation + "\x00" + string(change.Kind)
}

func linkMap(links []Link) map[string]Link {
	result := make(map[string]Link, len(links))
	for _, link := range links {
		result[linkKey(link)] = link
	}
	return result
}

func rawLinkMap(links []Link) map[string]Link {
	result := make(map[string]Link, len(links))
	for _, link := range links {
		result[linkKey(link)] = link
	}
	return result
}

func changedComponentIDs(changes []Change, links []LinkChange) []string {
	set := make(map[string]struct{})
	for _, change := range changes {
		if change.ComponentID != "" {
			set[change.ComponentID] = struct{}{}
		}
	}
	for _, link := range links {
		if link.From != "" {
			set[link.From] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func affectedComponents(changed []string, linkSets ...[]Link) []string {
	adjacency := make(map[string][]string)
	for _, links := range linkSets {
		for _, link := range links {
			adjacency[link.From] = append(adjacency[link.From], link.To)
		}
	}
	visited := make(map[string]struct{})
	affected := make(map[string]struct{})
	queue := append([]string(nil), changed...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, ok := visited[current]; ok {
			continue
		}
		visited[current] = struct{}{}
		for _, next := range adjacency[current] {
			if containsString(changed, next) {
				continue
			}
			if _, seen := affected[next]; !seen {
				affected[next] = struct{}{}
				queue = append(queue, next)
			}
		}
	}
	result := make([]string, 0, len(affected))
	for id := range affected {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func sameSourceRefs(left, right []model.SourceRef) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]model.SourceRef(nil), left...)
	b := append([]model.SourceRef(nil), right...)
	sort.Slice(a, func(i, j int) bool { return sourceRefKey(a[i]) < sourceRefKey(a[j]) })
	sort.Slice(b, func(i, j int) bool { return sourceRefKey(b[i]) < sourceRefKey(b[j]) })
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sourceRefKey(ref model.SourceRef) string {
	return string(ref.WorktreeID) + "\x00" + string(ref.ProfileID) + "\x00" + string(ref.GenerationID) + "\x00" + ref.Path + "\x00" + string(ref.ContentHash) + "\x00" + strconv.FormatInt(ref.Range.StartByte, 10) + ":" + strconv.FormatInt(ref.Range.EndByte, 10)
}

func cloneSourceRefs(refs []model.SourceRef) []model.SourceRef {
	return append([]model.SourceRef(nil), refs...)
}

func cloneGaps(gaps []Gap) []Gap { return append([]Gap(nil), gaps...) }

func normalizeGaps(gaps []Gap) []Gap {
	seen := make(map[string]struct{}, len(gaps))
	result := make([]Gap, 0, len(gaps))
	for _, gap := range gaps {
		key := gap.Code + "\x00" + gap.ComponentID + "\x00" + gap.Field + "\x00" + gap.Reason
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, gap)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Code != result[j].Code {
			return result[i].Code < result[j].Code
		}
		if result[i].ComponentID != result[j].ComponentID {
			return result[i].ComponentID < result[j].ComponentID
		}
		if result[i].Field != result[j].Field {
			return result[i].Field < result[j].Field
		}
		return result[i].Reason < result[j].Reason
	})
	return result
}
