package config

import (
	"encoding/json"
	"errors"
	"testing"
)

func testPipeline(t *testing.T) *Pipeline {
	t.Helper()
	pipeline, err := NewPipeline()
	if err != nil {
		t.Fatal(err)
	}
	return pipeline
}

func TestDefaultsResolveWorkersAndMatchContract(t *testing.T) {
	pipeline := testPipeline(t)
	raw, err := json.Marshal(DefaultRuntimeConfig())
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := pipeline.CanonicalizeCandidate(raw)
	if err != nil {
		t.Fatalf("default config rejected by contract: %v", err)
	}
	preview, err := pipeline.Resolve(Source{}, &candidate, nil, nil, 8, ModeValidate)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Config.Core.MaxIndexedFiles != 1_000_000 || preview.Config.Core.MaxParseWorkers != 4 || preview.Config.Core.MaxWorktreeCacheBytes != 412_316_860_416 {
		t.Fatalf("unexpected built-in defaults: %#v", preview.Config.Core)
	}
	if preview.Config.Explorer.GraphHardEdges != 1_500 || len(preview.EffectiveSources) != 2 || preview.EffectiveSources[1] != "candidate_input" {
		t.Fatalf("unexpected explorer defaults or provenance: %#v", preview)
	}
}

func TestValidateUsesWholeCandidateAndReportsHomeSource(t *testing.T) {
	pipeline := testPipeline(t)
	home, err := pipeline.ParseSource([]byte(`{"schema_version":1,"core":{"max_indexed_files":100}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := pipeline.CanonicalizeCandidate([]byte(`{"schema_version":1,"core":{"query_timeout_ms":1000}}`))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := pipeline.Resolve(home, &candidate, nil, nil, 4, ModeValidate)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Config.Core.MaxIndexedFiles != 1_000_000 || preview.Config.Core.QueryTimeoutMS != 1_000 {
		t.Fatalf("candidate was merged with old home values: %#v", preview.Config.Core)
	}
	if preview.SourceKind != SourceCanonicalHome || preview.SourceDigest == nil || *preview.SourceDigest != *home.Digest() {
		t.Fatalf("validation lost current home source metadata: %#v", preview)
	}
	if len(preview.EffectiveSources) != 2 || preview.EffectiveSources[1] != "candidate_input" {
		t.Fatalf("candidate provenance is wrong: %#v", preview.EffectiveSources)
	}
}

func TestCanonicalSourceDigestIgnoresObjectKeyOrder(t *testing.T) {
	pipeline := testPipeline(t)
	left, err := pipeline.ParseSource([]byte(`{"schema_version":1,"core":{"query_timeout_ms":1000,"max_indexed_files":100}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	right, err := pipeline.ParseSource([]byte(`{"core":{"max_indexed_files":100,"query_timeout_ms":1000},"schema_version":1}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if *left.Digest() != *right.Digest() {
		t.Fatalf("key reordering changed source digest: %s != %s", *left.Digest(), *right.Digest())
	}
}

func TestEffectiveDigestTracksCPUResolutionButSourceDigestDoesNot(t *testing.T) {
	pipeline := testPipeline(t)
	home, err := pipeline.ParseSource([]byte(`{"schema_version":1}`), true)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pipeline.Resolve(home, nil, nil, nil, 2, ModeShow)
	if err != nil {
		t.Fatal(err)
	}
	right, err := pipeline.Resolve(home, nil, nil, nil, 12, ModeShow)
	if err != nil {
		t.Fatal(err)
	}
	if left.Config.Core.MaxParseWorkers != 2 || right.Config.Core.MaxParseWorkers != 4 || left.ConfigDigest == right.ConfigDigest {
		t.Fatalf("CPU resolution is not reflected in effective config: %#v %#v", left.Config.Core, right.Config.Core)
	}
	if *left.SourceDigest != *right.SourceDigest {
		t.Fatal("CPU resolution changed canonical source digest")
	}
}

func TestAlternateAndCLISharedFieldsCannotOverrideHome(t *testing.T) {
	pipeline := testPipeline(t)
	home, err := pipeline.ParseSource([]byte(`{"schema_version":1}`), true)
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := pipeline.CanonicalizeCandidate([]byte(`{"schema_version":1,"core":{"volume_reserve_bytes":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Resolve(home, nil, &alternate, nil, 4, ModeShow); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("shared alternate did not conflict: %v", err)
	}
	if _, err := pipeline.Resolve(home, nil, nil, map[string]int64{"max_worktree_cache_bytes": 9}, 4, ModeShow); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("shared CLI override was accepted: %v", err)
	}
}

func TestHardCapsAndCrossFieldLimitsRejectBeforeUse(t *testing.T) {
	pipeline := testPipeline(t)
	for name, raw := range map[string]string{
		"fixed rename result cap":   `{"schema_version":1,"core":{"rename_result_max_bytes":33554433}}`,
		"rename total below result": `{"schema_version":1,"core":{"rename_result_max_bytes":10000000,"rename_results_max_bytes":1000000}}`,
		"warning below reserve":     `{"schema_version":1,"core":{"volume_reserve_bytes":100,"volume_warning_bytes":99}}`,
		"graph default above hard":  `{"schema_version":1,"explorer":{"graph_default_nodes":400,"graph_hard_nodes":300}}`,
	} {
		candidate, err := pipeline.CanonicalizeCandidate([]byte(raw))
		if err == nil {
			_, err = pipeline.Resolve(Source{}, &candidate, nil, nil, 4, ModeValidate)
		}
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestStrictSourceRejectsDuplicateAndMalformedInput(t *testing.T) {
	pipeline := testPipeline(t)
	for _, raw := range []string{
		`{"schema_version":1,"schema_version":1}`,
		`{"schema_version":1,"core":{"unknown_limit":3}}`,
		`{"schema_version":1,"core":{"query_timeout_ms":1.0}}`,
	} {
		if _, err := pipeline.CanonicalizeCandidate([]byte(raw)); err == nil {
			t.Errorf("invalid source accepted: %s", raw)
		}
	}
	if _, err := pipeline.ParseSource(nil, true); err == nil {
		t.Fatal("empty existing source was treated as valid")
	}
}
