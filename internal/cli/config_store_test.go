package cli

import (
	"context"
	"errors"
	"sync"
	"testing"

	appconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/config"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

func testPipeline(t *testing.T) *runtimeconfig.Pipeline {
	t.Helper()
	pipeline, err := runtimeconfig.NewPipeline()
	if err != nil {
		t.Fatal(err)
	}
	return pipeline
}

func canonicalCandidate(t *testing.T, pipeline *runtimeconfig.Pipeline, raw string) []byte {
	t.Helper()
	source, err := pipeline.CanonicalizeCandidate([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return source.CanonicalJSON()
}

func TestHomeConfigStoreCASUsesCanonicalSourceDigest(t *testing.T) {
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(t.TempDir()+"/state/mce4d", pipeline)
	if err != nil {
		t.Fatal(err)
	}
	if raw, present, err := store.ReadCanonical(context.Background()); err != nil || present || len(raw) != 0 {
		t.Fatalf("initial read = (%q, %v, %v), want absent", raw, present, err)
	}
	first := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":1700}}`)
	if err := store.CompareAndSwapCanonical(context.Background(), nil, first); err != nil {
		t.Fatal(err)
	}
	before, present, err := store.ReadCanonical(context.Background())
	if err != nil || !present {
		t.Fatalf("read after initial CAS: present=%v err=%v", present, err)
	}
	parsed, err := pipeline.ParseSource(before, true)
	if err != nil {
		t.Fatal(err)
	}
	stale := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":1800}}`)
	if err := store.CompareAndSwapCanonical(context.Background(), nil, stale); !errors.Is(err, appconfig.ErrConflict) {
		t.Fatalf("stale absent-source CAS error = %v, want conflict", err)
	}
	after, present, err := store.ReadCanonical(context.Background())
	if err != nil || !present {
		t.Fatalf("read after stale CAS: present=%v err=%v", present, err)
	}
	if string(after) != string(before) {
		t.Fatalf("stale CAS changed source bytes: before=%s after=%s", before, after)
	}
	if parsed.Digest() == nil {
		t.Fatal("canonical source digest is nil")
	}
	if err := store.CompareAndSwapCanonical(context.Background(), parsed.Digest(), stale); err != nil {
		t.Fatalf("CAS with canonical source digest: %v", err)
	}
}

func TestHomeConfigStoreConcurrentAbsentCASHasOneWinner(t *testing.T) {
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(t.TempDir()+"/state/mce4d", pipeline)
	if err != nil {
		t.Fatal(err)
	}
	candidates := [][]byte{
		canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":1100}}`),
		canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":1200}}`),
	}
	var group sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for _, candidate := range candidates {
		group.Add(1)
		go func(candidate []byte) {
			defer group.Done()
			err := store.CompareAndSwapCanonical(context.Background(), nil, candidate)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, appconfig.ErrConflict):
				conflicts++
			default:
				t.Errorf("unexpected CAS result: %v", err)
			}
		}(candidate)
	}
	group.Wait()
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS outcomes: winners=%d conflicts=%d, want 1/1", wins, conflicts)
	}
}

func TestHomeConfigStoreRejectsNonCanonicalReplacement(t *testing.T) {
	store, err := newHomeConfigStore(t.TempDir()+"/state/mce4d", testPipeline(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndSwapCanonical(context.Background(), nil, []byte("{\"schema_version\":1}\n")); err == nil {
		t.Fatal("non-canonical source was written")
	}
}

func TestHomeConfigStorePropagatesCanceledContext(t *testing.T) {
	store, err := newHomeConfigStore(t.TempDir()+"/state/mce4d", testPipeline(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.ReadCanonical(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v", err)
	}
	if err := store.CompareAndSwapCanonical(ctx, nil, canonicalCandidate(t, store.pipeline, `{"schema_version":1}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write error = %v", err)
	}
}

func TestHomeConfigLockAllowsAtomicRuntimeReplacement(t *testing.T) {
	root, err := mceruntime.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteAtomicFile("runtime.json", []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lockFile, err := root.OpenLockFile("unrelated.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteAtomicFile("runtime.json", []byte(`{"schema_version":1,"core":{"query_timeout_ms":1800}}`), 0o600); err != nil {
		_ = lockFile.Close()
		t.Fatalf("atomic replacement with unrelated lockfile open: %v", err)
	}
	if err := lockFile.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := mceruntime.AcquireHomeConfig(context.Background(), root, mceruntime.LockExclusive, configLockWait())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := root.WriteAtomicFile("runtime.json", []byte(`{"schema_version":1,"core":{"query_timeout_ms":1900}}`), 0o600); err != nil {
		t.Fatalf("atomic replacement under home config lock: %v", err)
	}
}
