package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

type memoryStore struct {
	mu       sync.Mutex
	pipeline *runtimeconfig.Pipeline
	raw      []byte
	present  bool
	writes   int

	changeBeforeCAS bool
}

func (store *memoryStore) ReadCanonical(context.Context) ([]byte, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]byte(nil), store.raw...), store.present, nil
}

func (store *memoryStore) CompareAndSwapCanonical(_ context.Context, expected *model.Hash, candidate []byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.writes++
	if store.changeBeforeCAS {
		store.raw = []byte(`{"schema_version":1,"core":{"query_timeout_ms":2000}}`)
		store.present = true
	}
	current, err := store.pipeline.ParseSource(store.raw, store.present)
	if err != nil {
		return err
	}
	if !sameDigest(current.Digest(), expected) {
		return runtimeconfig.ErrConfigConflict
	}
	parsed, err := store.pipeline.ParseSource(candidate, true)
	if err != nil {
		return err
	}
	store.raw = parsed.CanonicalJSON()
	store.present = true
	return nil
}

func newConfigService(t *testing.T, store *memoryStore) *Service {
	t.Helper()
	pipeline, err := runtimeconfig.NewPipeline()
	if err != nil {
		t.Fatal(err)
	}
	store.pipeline = pipeline
	active, err := pipeline.Resolve(runtimeconfig.Source{}, nil, nil, nil, 4, runtimeconfig.ModeShow)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store, pipeline, active.ConfigDigest)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestValidatePreviewsWholeReplacementWithoutWriting(t *testing.T) {
	store := &memoryStore{raw: []byte(`{"schema_version":1,"core":{"max_indexed_files":100}}`), present: true}
	service := newConfigService(t, store)
	home, err := store.pipeline.ParseSource(store.raw, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := service.Validate(context.Background(), []byte(`{"schema_version":1,"core":{"query_timeout_ms":1000}}`), false, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if data.Config.Core.MaxIndexedFiles != 1_000_000 || data.Config.Core.QueryTimeoutMS != 1_000 {
		t.Fatalf("old home values leaked into candidate: %#v", data.Config.Core)
	}
	if data.SourceDigest == nil || *data.SourceDigest != *home.Digest() || store.writes != 0 {
		t.Fatalf("validate changed state or lost source metadata: data=%#v writes=%d", data, store.writes)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"request_id":     "123e4567-e89b-42d3-a456-426614174000",
		"status":         "ok",
		"scope":          nil,
		"freshness": map[string]any{
			"status": "not_checked", "scope": "none", "checked_at": nil,
			"buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability",
			"changed_count": nil, "source_view": "indexed_snapshot",
		},
		"coverage": map[string]any{
			"status": "supported_scope_complete", "capabilities": []any{}, "gaps": []any{},
			"scope_description": "runtime configuration",
		},
		"readiness": "needs_context", "data": data,
		"pagination": map[string]any{
			"truncated": false, "enumeration_complete": true, "returned_count": 0,
			"required_pending": 0, "optional_pending": nil, "next_cursor": nil, "limit_reason": nil,
		},
		"budget": map[string]any{
			"max_output_bytes": 4096, "serialized_bytes": 0, "token_count": nil,
			"token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "cli",
		},
		"warnings": []any{}, "error": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateResponse("CLI config", encoded); err != nil {
		t.Fatalf("runtime config response does not match the public contract: %v", err)
	}
}

func TestSetPersistsOnlyCandidateAndUsesCanonicalSourceCAS(t *testing.T) {
	store := &memoryStore{raw: []byte(`{"schema_version":1,"core":{"max_indexed_files":100}}`), present: true}
	service := newConfigService(t, store)
	home, err := store.pipeline.ParseSource(store.raw, true)
	if err != nil {
		t.Fatal(err)
	}
	expected := *home.Digest()
	data, err := service.Set(context.Background(), []byte(`{"schema_version":1,"core":{"query_timeout_ms":1000}}`), false, &expected, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if data.Config.Core.MaxIndexedFiles != 1_000_000 || data.SourceDigest == nil || store.writes != 1 {
		t.Fatalf("set result mismatch: data=%#v writes=%d", data, store.writes)
	}
	stored, err := store.pipeline.CanonicalizeCandidate(store.raw)
	if err != nil {
		t.Fatal(err)
	}
	if *stored.Digest() != *data.SourceDigest || bytes.Contains(store.raw, []byte("max_indexed_files")) {
		t.Fatalf("set persisted merged defaults or returned a different source digest: %s", store.raw)
	}
}

func TestSetRejectsStaleCASAndStorageRace(t *testing.T) {
	store := &memoryStore{raw: []byte(`{"schema_version":1}`), present: true}
	service := newConfigService(t, store)
	wrong, _ := model.ParseHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := service.Set(context.Background(), []byte(`{"schema_version":1}`), false, &wrong, nil, 4); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale source digest was not rejected: %v", err)
	}
	if store.writes != 0 {
		t.Fatal("stale preflight attempted a write")
	}
	home, _ := store.pipeline.ParseSource(store.raw, true)
	expected := *home.Digest()
	store.changeBeforeCAS = true
	if _, err := service.Set(context.Background(), []byte(`{"schema_version":1}`), false, &expected, nil, 4); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS race was not surfaced as conflict: %v", err)
	}
}

func TestInvalidExistingSourceIsNotTreatedAsAbsent(t *testing.T) {
	store := &memoryStore{raw: []byte("not json"), present: true}
	service := newConfigService(t, store)
	if _, err := service.Show(context.Background(), nil, false, nil, 4); err == nil {
		t.Fatal("malformed existing source was treated as built-in config")
	}
}

func TestAdmissionSnapshotUsesOnlyCanonicalSharedFields(t *testing.T) {
	store := &memoryStore{raw: []byte(`{"schema_version":1,"core":{"query_timeout_ms":1000}}`), present: true}
	service := newConfigService(t, store)
	first, err := service.ReadAdmissionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store.raw = []byte(`{"schema_version":1,"core":{"query_timeout_ms":2000}}`)
	second, err := service.ReadAdmissionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigDigest != second.ConfigDigest || *first.SourceDigest == *second.SourceDigest {
		t.Fatalf("nonshared runtime config altered quota authority: first=%#v second=%#v", first, second)
	}
	store.raw = []byte(`{"schema_version":1,"core":{"max_worktree_cache_bytes":999999999}}`)
	third, err := service.ReadAdmissionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigDigest == third.ConfigDigest || third.Config.MaxWorktreeCacheBytes != 999_999_999 {
		t.Fatalf("shared home setting did not change admission digest: %#v", third)
	}
}

func TestShowReportsOnlyParticipatingLayersAndNeverPersistsOverrides(t *testing.T) {
	store := &memoryStore{}
	service := newConfigService(t, store)
	data, err := service.Show(context.Background(), []byte(`{"schema_version":1,"core":{"query_timeout_ms":1500}}`), true, map[string]int64{"query_timeout_ms": 2000}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if data.Config.Core.QueryTimeoutMS != 2_000 || data.SourceKind != "built_in" || data.SourceDigest != nil {
		t.Fatalf("show source metadata or process layer is incorrect: %#v", data)
	}
	if len(data.EffectiveSources) != 3 || data.EffectiveSources[0] != "built_in" || data.EffectiveSources[1] != "approved_alternate" || data.EffectiveSources[2] != "cli_process" {
		t.Fatalf("show provenance does not match layers used: %#v", data.EffectiveSources)
	}
	if store.writes != 0 || store.present {
		t.Fatal("show persisted an alternate or process override")
	}
}

func TestConcurrentSetWithSameSourceDigestHasOneWinner(t *testing.T) {
	store := &memoryStore{raw: []byte(`{"schema_version":1}`), present: true}
	service := newConfigService(t, store)
	home, err := store.pipeline.ParseSource(store.raw, true)
	if err != nil {
		t.Fatal(err)
	}
	expected := *home.Digest()
	candidates := [][]byte{
		[]byte(`{"schema_version":1,"core":{"query_timeout_ms":1111}}`),
		[]byte(`{"schema_version":1,"core":{"query_timeout_ms":2222}}`),
	}
	var wait sync.WaitGroup
	var resultMu sync.Mutex
	wins, conflicts := 0, 0
	for _, candidate := range candidates {
		wait.Add(1)
		go func(raw []byte) {
			defer wait.Done()
			_, setErr := service.Set(context.Background(), raw, false, &expected, nil, 4)
			resultMu.Lock()
			defer resultMu.Unlock()
			if setErr == nil {
				wins++
			} else if errors.Is(setErr, ErrConflict) {
				conflicts++
			} else {
				t.Errorf("unexpected concurrent config error: %v", setErr)
			}
		}(candidate)
	}
	wait.Wait()
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected one CAS winner and one conflict, got wins=%d conflicts=%d", wins, conflicts)
	}
}
