package ai

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestCompareReportsSourceBoundRAGDriftAndKnownDownstream(t *testing.T) {
	before := ragSnapshot("1536", "embedding-before")
	after := ragSnapshot("3072", "embedding-after")

	report, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceContentVerification != SourceContentStructuralOnly {
		t.Fatalf("source verification = %q", report.SourceContentVerification)
	}
	if len(report.Changes) != 1 {
		t.Fatalf("metadata changes = %+v, want only embedding dimension drift", report.Changes)
	}
	change := report.Changes[0]
	if change.Kind != ChangeChanged || change.ComponentID != "embedding" || change.Field != "embedding_dimension" || change.Before != "1536" || change.After != "3072" {
		t.Fatalf("embedding drift = %+v", change)
	}
	wantAffected := []string{"index", "invocation", "reranker", "validation"}
	if !reflect.DeepEqual(report.Affected, wantAffected) {
		t.Fatalf("affected = %v, want explicit source-bound downstream %v", report.Affected, wantAffected)
	}
	if hasGap(report.Gaps, "model_revision_unpinned", "embedding") {
		t.Fatalf("pinned embedding revision was reported unpinned: %+v", report.Gaps)
	}
}

func TestCompareReportsPromptTokenizerAndIndexDriftAsMetadata(t *testing.T) {
	before := ragSnapshot("1536", "embedding-stable")
	after := ragSnapshot("1536", "embedding-stable")
	setAttribute(after.Components, "prompt", attribute(FieldPromptTemplateHash, modelHash("prompt-v2"), "prompt-after.json"))
	setAttribute(after.Components, "tokenizer", attribute(FieldTokenizerVersion, "tok-v2", "tokenizer-after.json"))
	setAttribute(after.Components, "index", attribute(FieldIndexSchemaRef, "index-schema-v2", "index-after.json"))

	report, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]Change)
	for _, change := range report.Changes {
		got[change.ComponentID+":"+change.Field] = change
	}
	for _, want := range []struct{ id, field, before, after string }{
		{"prompt", FieldPromptTemplateHash, modelHash("prompt-v1"), modelHash("prompt-v2")},
		{"tokenizer", FieldTokenizerVersion, "tok-v1", "tok-v2"},
		{"index", FieldIndexSchemaRef, "index-schema-v1", "index-schema-v2"},
	} {
		change, ok := got[want.id+":"+want.field]
		if !ok || change.Kind != ChangeChanged || change.Before != want.before || change.After != want.after {
			t.Errorf("metadata drift %s.%s = %+v, present=%t", want.id, want.field, change, ok)
		}
	}
	if len(report.Changes) != 3 || len(report.LinkChanges) != 0 {
		t.Fatalf("drift should remain a metadata-only report: %+v", report)
	}
	if !reflect.DeepEqual(report.Affected, []string{"chunker", "embedding", "invocation", "reranker", "validation"}) {
		t.Fatalf("affected components = %v", report.Affected)
	}
	if report.SourceContentVerification != SourceContentStructuralOnly {
		t.Fatalf("source verification = %q", report.SourceContentVerification)
	}
}

func TestCompareReportsDatasetSchemaAndEvaluationDriftWithoutPayloadClaims(t *testing.T) {
	before := datasetSnapshot("dataset-v1", "schema-v1", "f1")
	after := datasetSnapshot("dataset-v2", "schema-v2", "f1.5")

	report, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]Change)
	for _, change := range report.Changes {
		got[change.ComponentID+":"+change.Field] = change
	}
	for _, want := range []struct{ id, field, before, after string }{
		{"dataset", FieldDatasetDigest, modelHash("dataset-v1"), modelHash("dataset-v2")},
		{"schema", FieldDatasetSchemaRef, "schema-v1", "schema-v2"},
		{"evaluation", FieldEvaluationMetric, "f1", "f1.5"},
	} {
		change, ok := got[want.id+":"+want.field]
		if !ok || change.Kind != ChangeChanged || change.Before != want.before || change.After != want.after {
			t.Errorf("metadata drift %s.%s = %+v, present=%t", want.id, want.field, change, ok)
		}
	}
	if len(report.Changes) != 3 || len(report.LinkChanges) != 0 {
		t.Fatalf("dataset/evaluation changes should remain metadata-only: %+v", report)
	}
	if report.SourceContentVerification != SourceContentStructuralOnly {
		t.Fatalf("source verification = %q", report.SourceContentVerification)
	}
	if !reflect.DeepEqual(report.Affected, []string{"artifact", "features", "preprocessing", "serving", "split", "training"}) {
		t.Fatalf("affected components = %v", report.Affected)
	}
}

func TestReviewTurnsMissingAIContractsIntoGaps(t *testing.T) {
	snapshot := Snapshot{
		Coverage: CoverageUnknown,
		Components: []Component{
			component("dataset", StageDataset, RevisionUnknown),
			component("split", StageSplit, RevisionNotApplicable),
			component("prompt", StagePromptTemplate, RevisionNotApplicable),
			component("tokenizer", StageTokenizer, RevisionNotApplicable),
			component("chunker", StageChunker, RevisionNotApplicable),
			component("embedding", StageEmbedding, RevisionUnversioned,
				attribute("model_id", "remote-model-request", "embedding-model.json")),
			component("eval", StageEvaluation, RevisionNotApplicable),
		},
	}

	review, err := ReviewSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ code, id string }{
		{"dataset_digest_missing", "dataset"},
		{"split_role_missing", "split"},
		{"prompt_hash_missing", "prompt"},
		{"tokenizer_version_missing", "tokenizer"},
		{"chunking_config_missing", "chunker"},
		{"embedding_dimension_missing", "embedding"},
		{"distance_metric_missing", "embedding"},
		{"model_revision_unpinned", "embedding"},
		{"evaluation_metric_missing", "eval"},
		{"evaluation_definition_missing", "eval"},
	} {
		if !hasGap(review.Gaps, want.code, want.id) {
			t.Errorf("missing gap %s for %s in %+v", want.code, want.id, review.Gaps)
		}
	}
	modelComponent := componentByID(review.Components, "embedding")
	if modelComponent == nil || fieldValue(*modelComponent, "model_id") != "remote-model-request" || fieldValue(*modelComponent, "revision") != "" {
		t.Fatalf("unversioned remote model must retain only its bound identifier: %+v", modelComponent)
	}
}

func TestReviewReportsTrainingAndServingLineageGaps(t *testing.T) {
	snapshot := Snapshot{Coverage: CoveragePartial, Components: []Component{
		component("schema", StageSchema, RevisionNotApplicable),
		component("preprocessing", StagePreprocessing, RevisionNotApplicable),
		component("features", StageFeatures, RevisionNotApplicable),
		component("training", StageTrainingConfig, RevisionNotApplicable),
		component("artifact", StageModelArtifact, RevisionUnknown),
		component("inference", StageInference, RevisionUnknown),
		component("invocation", StageModelInvocation, RevisionUnknown),
		component("output", StageOutputValidation, RevisionNotApplicable),
		component("evaluation", StageEvaluation, RevisionNotApplicable),
		component("serving", StageServing, RevisionUnknown),
		component("fallback", StageFallback, RevisionNotApplicable),
	}}

	review, err := ReviewSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ code, id string }{
		{"dataset_schema_missing", "schema"},
		{"preprocessing_config_missing", "preprocessing"},
		{"feature_order_missing", "features"},
		{"training_config_missing", "training"},
		{"model_artifact_missing", "artifact"},
		{"inference_config_missing", "inference"},
		{"input_schema_missing", "invocation"},
		{"output_schema_missing", "invocation"},
		{"output_schema_missing", "output"},
		{"evaluation_metric_missing", "evaluation"},
		{"evaluation_definition_missing", "evaluation"},
		{"serving_config_missing", "serving"},
		{"retry_policy_missing", "serving"},
		{"timeout_missing", "serving"},
		{"fallback_policy_missing", "fallback"},
		{"model_identifier_missing", "inference"},
	} {
		if !hasGap(review.Gaps, want.code, want.id) {
			t.Errorf("missing gap %s for %s", want.code, want.id)
		}
	}
}

func TestReviewKeepsArtifactReferencesOpaque(t *testing.T) {
	artifact := component("checkpoint", StageModelArtifact, RevisionPinned,
		attribute(FieldRevision, "model-v4", "model-manifest.json"),
		attribute(FieldModelArtifactRef, "opaque://missing/checkpoint.bin", "model-manifest.json"))
	review, err := ReviewSnapshot(Snapshot{Components: []Component{artifact}})
	if err != nil {
		t.Fatalf("opaque, absent artifact reference was read or rejected: %v", err)
	}
	if got := fieldValue(review.Components[0], FieldModelArtifactRef); got != "opaque://missing/checkpoint.bin" {
		t.Fatalf("artifact reference = %q", got)
	}
}

func TestReviewEnforcesAggregateMetadataAndSourcePathLimits(t *testing.T) {
	components := make([]Component, 33)
	for index := range components {
		components[index].Attributes = make([]Attribute, MaxAttributesPerNode)
	}
	_, err := ReviewSnapshot(Snapshot{Components: components})
	if !errors.Is(err, ErrInvalidLineage) {
		t.Fatalf("aggregate attributes error = %v, want ErrInvalidLineage", err)
	}

	tooManySources := Snapshot{CoverageSources: make([]model.SourceRef, MaxSourceRefsPerSnapshot+1)}
	_, err = ReviewSnapshot(tooManySources)
	if !errors.Is(err, ErrInvalidLineage) {
		t.Fatalf("aggregate source refs error = %v, want ErrInvalidLineage", err)
	}

	unsafeLongPath := source(strings.Repeat("a", MaxSourcePathBytes+1), "long-path")
	_, err = ReviewSnapshot(Snapshot{Components: []Component{{
		ID: "schema", Stage: StageSchema, State: StateImplemented, RevisionState: RevisionNotApplicable,
		Sources: []model.SourceRef{unsafeLongPath},
	}}})
	if !errors.Is(err, ErrInvalidLineage) {
		t.Fatalf("overlong source path error = %v, want ErrInvalidLineage", err)
	}
}

func TestUnboundMetadataAndLinksAreGapsAndCannotAffectConsumers(t *testing.T) {
	before := Snapshot{
		Coverage: CoveragePartial,
		Components: []Component{
			component("source", StageSchema, RevisionNotApplicable, attribute(FieldDatasetSchemaRef, "schema-v1", "schema-before.json")),
			component("consumer", StageSchema, RevisionNotApplicable),
		},
		Links: []Link{{From: "source", To: "consumer", Relation: "consumes", SourceRefs: nil}},
	}
	after := before
	after.Components = cloneComponents(before.Components)
	after.Components[0].Attributes[0].Value = "schema-v2"
	after.Components[0].Attributes[0].SourceRefs = []model.SourceRef{source("schema-after.json", "schema-after")}
	after.Links = append([]Link(nil), before.Links...)

	report, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changes) != 1 || report.Changes[0].Field != FieldDatasetSchemaRef {
		t.Fatalf("source-bound schema change = %+v", report.Changes)
	}
	if len(report.Affected) != 0 {
		t.Fatalf("unbound link propagated impact: %v", report.Affected)
	}
	if !hasGap(report.Gaps, "lineage_link_unbound", "source") {
		t.Fatalf("unbound link gap missing: %+v", report.Gaps)
	}
}

func TestNameLikeMetadataDoesNotMatchDifferentComponentIDs(t *testing.T) {
	before := completeSnapshot(
		component("old-component", StageSchema, RevisionNotApplicable, attribute("component_name", "same-name", "old.json")),
	)
	after := completeSnapshot(
		component("new-component", StageSchema, RevisionNotApplicable, attribute("component_name", "same-name", "new.json")),
	)

	report, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ChangeKind{}
	for _, change := range report.Changes {
		if change.Field == "$component" {
			got[change.ComponentID] = change.Kind
		}
	}
	if !reflect.DeepEqual(got, map[string]ChangeKind{"new-component": ChangeAdded, "old-component": ChangeRemoved}) {
		t.Fatalf("same-name records were heuristically matched: %+v", report.Changes)
	}
}

func TestPartialCoverageDoesNotClaimMissingComponentRemoved(t *testing.T) {
	before := Snapshot{Coverage: CoveragePartial, Components: []Component{component("schema", StageSchema, RevisionNotApplicable)}}
	after := Snapshot{Coverage: CoveragePartial}

	report, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changes) != 0 || !hasGap(report.Gaps, "component_absence_unverified", "schema") {
		t.Fatalf("partial coverage must preserve a gap instead of removal: %+v", report)
	}
}

func TestCompleteCoverageReportsRemovalOnlyInsideDeclaredScope(t *testing.T) {
	before := completeSnapshot(component("schema", StageSchema, RevisionNotApplicable))
	after := completeSnapshot()

	report, err := Compare(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changes) != 1 || report.Changes[0].Kind != ChangeRemoved || report.Changes[0].ComponentID != "schema" {
		t.Fatalf("complete declared-scope removal = %+v", report.Changes)
	}
}

func TestMissingLinkEndpointBecomesGapAndIsExcluded(t *testing.T) {
	snapshot := Snapshot{
		Coverage:   CoveragePartial,
		Components: []Component{component("schema", StageSchema, RevisionNotApplicable)},
		Links:      []Link{link("schema", "missing-consumer", "feeds", "declared-link.json")},
	}

	review, err := ReviewSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.Links) != 0 || !hasGap(review.Gaps, "lineage_endpoint_missing", "missing-consumer") {
		t.Fatalf("missing endpoint was trusted or not reported: %+v", review)
	}
}

func TestCompleteCoverageRequiresBoundManifest(t *testing.T) {
	review, err := ReviewSnapshot(Snapshot{Coverage: CoverageComplete})
	if err != nil {
		t.Fatal(err)
	}
	if review.Coverage != CoverageUnknown || !hasGap(review.Gaps, "coverage_unbound", "") {
		t.Fatalf("unbound complete-coverage claim was trusted: %+v", review)
	}
}

func TestRejectsPayloadKeysInvalidMetadataAndMalformedSourceRefs(t *testing.T) {
	tests := []struct {
		name      string
		component Component
	}{
		{name: "raw prompt", component: component("prompt", StagePromptTemplate, RevisionNotApplicable, attribute("prompt_text", "do something", "prompt.json"))},
		{name: "bad digest", component: component("dataset", StageDataset, RevisionUnknown, attribute("dataset_digest", "not-a-digest", "dataset.json"))},
		{name: "bad dimension", component: component("embedding", StageEmbedding, RevisionPinned,
			attribute("revision", "embed-v1", "model.json"), attribute("embedding_dimension", "0", "model.json"))},
		{name: "unsafe source path", component: component("schema", StageSchema, RevisionNotApplicable,
			Attribute{Key: "schema_ref", Value: "schema-v1", SourceRefs: []model.SourceRef{source("../outside.json", "bad-path")}})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ReviewSnapshot(Snapshot{Components: []Component{test.component}})
			if !errors.Is(err, ErrInvalidLineage) {
				t.Fatalf("ReviewSnapshot error = %v, want ErrInvalidLineage", err)
			}
		})
	}
}

func TestReviewReturnsDefensiveCopiesAndDoesNotOpenSources(t *testing.T) {
	snapshot := completeSnapshot(component("schema", StageSchema, RevisionNotApplicable, attribute("schema_ref", "schema-v1", "schema.json")))
	review, err := ReviewSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	review.Components[0].Attributes[0].Value = "mutated"
	review.Components[0].Sources[0].Path = "mutated"
	if snapshot.Components[0].Attributes[0].Value != "schema-v1" || snapshot.Components[0].Sources[0].Path != "schema.json" {
		t.Fatal("review result mutated caller-owned metadata")
	}
	report, err := Compare(snapshot, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceContentVerification != SourceContentStructuralOnly {
		t.Fatalf("comparison claimed source-byte verification: %+v", report)
	}
}

func ragSnapshot(dimension, sourceSeed string) Snapshot {
	prompt := component("prompt", StagePromptTemplate, RevisionNotApplicable,
		attribute(FieldPromptTemplateHash, modelHash("prompt-v1"), "prompt.json"))
	tokenizer := component("tokenizer", StageTokenizer, RevisionNotApplicable,
		attribute(FieldTokenizerVersion, "tok-v1", "tokenizer.json"))
	chunker := component("chunker", StageChunker, RevisionNotApplicable,
		attribute(FieldChunkingConfigHash, modelHash("chunking-v1"), "chunker.json"))
	embedding := component("embedding", StageEmbedding, RevisionPinned,
		attribute(FieldModelID, "embedding-model", "embedding-model.json"),
		attribute(FieldRevision, "embed-v1", "embedding-model.json"),
		attribute(FieldDistanceMetric, "cosine", "embedding-model.json"),
		attribute(FieldEmbeddingDimension, dimension, sourceSeed+".json"))
	index := component("index", StageRetrievalIndex, RevisionNotApplicable,
		attribute(FieldIndexSchemaRef, "index-schema-v1", "index.json"))
	reranker := component("reranker", StageReranker, RevisionPinned,
		attribute(FieldModelID, "reranker-model", "reranker.json"),
		attribute(FieldRevision, "rerank-v1", "reranker.json"))
	invocation := component("invocation", StageModelInvocation, RevisionPinned,
		attribute(FieldModelID, "invocation-model", "invocation.json"),
		attribute(FieldInputSchemaRef, "input-schema-v1", "invocation.json"),
		attribute(FieldOutputSchemaRef, "invocation-output-schema-v1", "invocation.json"),
		attribute(FieldRevision, "model-v1", "invocation.json"))
	validation := component("validation", StageOutputValidation, RevisionNotApplicable,
		attribute(FieldOutputSchemaRef, "output-schema-v1", "validation.json"))
	links := []Link{
		link("prompt", "tokenizer", "tokenized_by", "lineage-prompt-tokenizer.json"),
		link("tokenizer", "chunker", "chunked_by", "lineage-tokenizer-chunker.json"),
		link("chunker", "embedding", "embedded_by", "lineage-chunker-embedding.json"),
		link("embedding", "index", "indexed_by", "lineage-embedding-index.json"),
		link("index", "reranker", "ranked_by", "lineage-index-reranker.json"),
		link("reranker", "invocation", "invokes", "lineage-reranker-invocation.json"),
		link("invocation", "validation", "validated_by", "lineage-invocation-validation.json"),
	}
	snapshot := completeSnapshot(prompt, tokenizer, chunker, embedding, index, reranker, invocation, validation)
	snapshot.Links = links
	return snapshot
}

func datasetSnapshot(digest, schemaRef, metric string) Snapshot {
	dataset := component("dataset", StageDataset, RevisionNotApplicable,
		attribute(FieldDatasetDigest, modelHash(digest), "dataset-manifest.json"))
	schema := component("schema", StageSchema, RevisionNotApplicable,
		attribute(FieldDatasetSchemaRef, schemaRef, "schema.json"))
	split := component("split", StageSplit, RevisionNotApplicable,
		attribute(FieldSplitRole, "test", "split-manifest.json"))
	preprocessing := component("preprocessing", StagePreprocessing, RevisionNotApplicable,
		attribute(FieldPreprocessingConfigHash, modelHash("preprocessing-v1"), "preprocessing.json"))
	features := component("features", StageFeatures, RevisionNotApplicable,
		attribute(FieldFeatureOrderHash, modelHash("features-v1"), "features.json"))
	training := component("training", StageTrainingConfig, RevisionNotApplicable,
		attribute(FieldTrainingConfigHash, modelHash("training-v1"), "training.json"))
	artifact := component("artifact", StageModelArtifact, RevisionPinned,
		attribute(FieldRevision, "model-v4", "model-manifest.json"),
		attribute(FieldModelArtifactRef, "opaque://missing/model.bin", "model-manifest.json"))
	evaluation := component("evaluation", StageEvaluation, RevisionNotApplicable,
		attribute(FieldEvaluationMetric, metric, "evaluation.json"),
		attribute(FieldEvaluationMetricDefinitionRef, "metric-definition-v1", "evaluation.json"))
	serving := component("serving", StageServing, RevisionPinned,
		attribute(FieldModelID, "service-model", "serving.json"),
		attribute(FieldRevision, "service-v4", "serving.json"),
		attribute(FieldServingConfigRef, "serving-config-v1", "serving.json"),
		attribute(FieldRetryPolicyRef, "retry-policy-v1", "serving.json"),
		attribute(FieldTimeout, "30s", "serving.json"))
	snapshot := completeSnapshot(dataset, schema, split, preprocessing, features, training, artifact, evaluation, serving)
	snapshot.Links = []Link{
		link("dataset", "schema", "described_by", "dataset-schema-link.json"),
		link("schema", "split", "split_by", "schema-split-link.json"),
		link("split", "preprocessing", "preprocessed_by", "split-preprocessing-link.json"),
		link("preprocessing", "features", "produces", "preprocessing-features-link.json"),
		link("features", "training", "used_by", "features-training-link.json"),
		link("training", "artifact", "produces", "training-artifact-link.json"),
		link("artifact", "evaluation", "evaluated_by", "artifact-evaluation-link.json"),
		link("evaluation", "serving", "qualified_for", "evaluation-serving-link.json"),
	}
	return snapshot
}

func setAttribute(components []Component, componentID string, replacement Attribute) {
	for componentIndex := range components {
		if components[componentIndex].ID != componentID {
			continue
		}
		for attributeIndex := range components[componentIndex].Attributes {
			if components[componentIndex].Attributes[attributeIndex].Key == replacement.Key {
				components[componentIndex].Attributes[attributeIndex] = replacement
				return
			}
		}
		panic("attribute not found: " + componentID + "." + replacement.Key)
	}
	panic("component not found: " + componentID)
}

func completeSnapshot(components ...Component) Snapshot {
	return Snapshot{Coverage: CoverageComplete, CoverageSources: []model.SourceRef{source("coverage-manifest.json", "coverage")}, Components: components}
}

func component(id string, stage Stage, revision RevisionState, attributes ...Attribute) Component {
	return Component{ID: id, Stage: stage, State: StateImplemented, RevisionState: revision, Sources: []model.SourceRef{source(id+".json", id)}, Attributes: attributes}
}

func attribute(key, value, path string) Attribute {
	return Attribute{Key: key, Value: value, SourceRefs: []model.SourceRef{source(path, key+":"+value)}}
}

func link(from, to, relation, path string) Link {
	return Link{From: from, To: to, Relation: relation, SourceRefs: []model.SourceRef{source(path, from+"->"+to)}}
}

func source(path, seed string) model.SourceRef {
	return model.SourceRef{
		WorktreeID: "11111111-1111-4111-8111-111111111111", ProfileID: model.SHA256([]byte("profile")),
		GenerationID: "22222222-2222-4222-8222-222222222222", Path: path,
		ContentHash: model.SHA256([]byte(seed)), Range: model.ByteRange{StartByte: 0, EndByte: 1},
	}
}

func modelHash(seed string) string { return string(model.SHA256([]byte(seed))) }

func hasGap(gaps []Gap, code, componentID string) bool {
	for _, gap := range gaps {
		if gap.Code == code && gap.ComponentID == componentID {
			return true
		}
	}
	return false
}

func componentByID(components []Component, id string) *Component {
	for index := range components {
		if components[index].ID == id {
			return &components[index]
		}
	}
	return nil
}

func fieldValue(component Component, key string) string {
	for _, field := range component.Attributes {
		if field.Key == key {
			return field.Value
		}
	}
	return ""
}

func cloneComponents(components []Component) []Component {
	result := make([]Component, len(components))
	copy(result, components)
	for index := range result {
		result[index].Sources = cloneSourceRefs(components[index].Sources)
		result[index].Attributes = append([]Attribute(nil), components[index].Attributes...)
		for attributeIndex := range result[index].Attributes {
			result[index].Attributes[attributeIndex].SourceRefs = cloneSourceRefs(components[index].Attributes[attributeIndex].SourceRefs)
		}
	}
	return result
}
