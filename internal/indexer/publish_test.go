package indexer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

func TestPublishOrdersStagingValidationAndAtomicPointer(t *testing.T) {
	store := &fakePublicationStore{}
	stager := &fakeGenerationStager{events: &store.events}
	result, err := Publish(context.Background(), store, stager, testSpec(), sqlite.GenerationReady)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := []string{"lease:start", "create", "stage", "verify", "publish", "lease:end"}
	if !reflect.DeepEqual(store.events, want) {
		t.Fatalf("events=%v; want %v", store.events, want)
	}
	if !result.Activated || result.Generation.State != sqlite.GenerationReady {
		t.Fatalf("result=%+v; want activated READY", result)
	}
}

func TestPublishFailureMarksOnlyCandidateFailed(t *testing.T) {
	store := &fakePublicationStore{}
	stager := &fakeGenerationStager{events: &store.events, stageErr: errors.New("batch write failed")}
	_, err := Publish(context.Background(), store, stager, testSpec(), sqlite.GenerationReady)
	if err == nil || !strings.Contains(err.Error(), "batch write failed") {
		t.Fatalf("Publish error=%v; want batch failure", err)
	}
	want := []string{"lease:start", "create", "stage", "abort:FAILED", "lease:end"}
	if !reflect.DeepEqual(store.events, want) {
		t.Fatalf("events=%v; want %v", store.events, want)
	}
	if store.publishCalls != 0 || store.state != sqlite.GenerationFailed {
		t.Fatalf("candidate state=%s publish_calls=%d; want FAILED and no publish", store.state, store.publishCalls)
	}
}

func TestPublishValidationFailurePreservesPreviouslyPublishedGeneration(t *testing.T) {
	store := &fakePublicationStore{activeID: "old-generation"}
	stager := &fakeGenerationStager{events: &store.events, verifyErr: errors.New("manifest revalidation failed")}
	_, err := Publish(context.Background(), store, stager, testSpec(), sqlite.GenerationReady)
	if err == nil || !strings.Contains(err.Error(), "manifest revalidation failed") {
		t.Fatalf("Publish error=%v; want verifier failure", err)
	}
	if store.activeID != "old-generation" {
		t.Fatalf("active pointer=%s; want old-generation", store.activeID)
	}
	if store.state != sqlite.GenerationFailed || store.publishCalls != 0 {
		t.Fatalf("state=%s publish_calls=%d; want failed candidate and no pointer update", store.state, store.publishCalls)
	}
}

func TestPublishExistingCompletedGenerationDoesNotRestage(t *testing.T) {
	spec := testSpec()
	store := &fakePublicationStore{state: sqlite.GenerationReady, activeID: string(spec.Key.GenerationID)}
	stager := &fakeGenerationStager{events: &store.events}
	result, err := Publish(context.Background(), store, stager, spec, sqlite.GenerationReady)
	if err != nil {
		t.Fatalf("idempotent Publish: %v", err)
	}
	if !reflect.DeepEqual(store.events, []string{"lease:start", "create", "resolve", "lease:end"}) {
		t.Fatalf("events=%v; want create and receipt resolution only", store.events)
	}
	if !result.AlreadyPublished || !result.Activated || stager.stageCalls != 0 {
		t.Fatalf("result=%+v stage_calls=%d; want idempotent receipt without restaging", result, stager.stageCalls)
	}
}

func TestPublishRejectsFailedGenerationReuse(t *testing.T) {
	store := &fakePublicationStore{state: sqlite.GenerationFailed}
	stager := &fakeGenerationStager{events: &store.events}
	_, err := Publish(context.Background(), store, stager, testSpec(), sqlite.GenerationReady)
	if !errors.Is(err, sqlite.ErrGenerationState) {
		t.Fatalf("Publish error=%v; want ErrGenerationState", err)
	}
	if !reflect.DeepEqual(store.events, []string{"lease:start", "create", "lease:end"}) {
		t.Fatalf("events=%v; failed generation must not be staged again", store.events)
	}
}

func testSpec() sqlite.GenerationSpec {
	snapshot := model.Hash(strings.Repeat("a", 64))
	artifact := model.Hash(strings.Repeat("b", 64))
	return sqlite.GenerationSpec{
		Key: sqlite.GenerationKey{
			GenerationID: model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
			ProfileID:    model.Hash(strings.Repeat("c", 64)),
			OwnerJobID:   model.UUID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		},
		SnapshotDigest: snapshot, PolicyDigest: model.Hash(strings.Repeat("d", 64)),
		PolicyEpoch: 1, ProviderFingerprint: model.Hash(strings.Repeat("e", 64)),
		ManifestRef: model.StoredCaptureManifestRef{
			FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + string(artifact) + ".json",
			ByteLength: 128, ArtifactSHA256: artifact, SnapshotDigest: snapshot,
		},
	}
}

type fakePublicationStore struct {
	events       []string
	state        sqlite.GenerationState
	activeID     string
	publishCalls int
	spec         sqlite.GenerationSpec
}

func (store *fakePublicationStore) WithGenerationWriter(ctx context.Context, operation func(context.Context) error) error {
	store.events = append(store.events, "lease:start")
	err := operation(ctx)
	store.events = append(store.events, "lease:end")
	return err
}

func (store *fakePublicationStore) CreateGeneration(_ context.Context, spec sqlite.GenerationSpec) (sqlite.GenerationRecord, error) {
	store.events = append(store.events, "create")
	store.spec = spec
	if store.state == "" {
		store.state = sqlite.GenerationBuilding
	}
	return recordFor(spec, store.state), nil
}

func (store *fakePublicationStore) PublishGeneration(_ context.Context, request sqlite.PublishRequest) (sqlite.PublishResult, error) {
	store.events = append(store.events, "publish")
	store.publishCalls++
	store.state = request.State
	store.activeID = string(request.Spec.Key.GenerationID)
	return sqlite.PublishResult{Generation: recordFor(request.Spec, request.State), Activated: true}, nil
}

func (store *fakePublicationStore) AbortGeneration(_ context.Context, _ sqlite.GenerationKey, state sqlite.GenerationState) error {
	store.events = append(store.events, "abort:"+string(state))
	store.state = state
	return nil
}

func (store *fakePublicationStore) ResolvePublication(_ context.Context, key sqlite.GenerationKey) (sqlite.PublicationResolution, error) {
	store.events = append(store.events, "resolve")
	return sqlite.PublicationResolution{Generation: recordFor(store.spec, store.state), Active: store.activeID == string(key.GenerationID)}, nil
}

type fakeGenerationStager struct {
	events     *[]string
	stageErr   error
	verifyErr  error
	stageCalls int
}

func (stager *fakeGenerationStager) Stage(context.Context, sqlite.GenerationRecord) error {
	*stager.events = append(*stager.events, "stage")
	stager.stageCalls++
	return stager.stageErr
}

func (stager *fakeGenerationStager) Verify(context.Context, sqlite.GenerationRecord) error {
	*stager.events = append(*stager.events, "verify")
	return stager.verifyErr
}

func recordFor(spec sqlite.GenerationSpec, state sqlite.GenerationState) sqlite.GenerationRecord {
	manifest := spec.ManifestRef
	return sqlite.GenerationRecord{
		Key: spec.Key, SnapshotDigest: spec.SnapshotDigest, State: state,
		PolicyDigest: spec.PolicyDigest, PolicyEpoch: spec.PolicyEpoch,
		ProviderFingerprint: spec.ProviderFingerprint, ManifestRef: &manifest,
		CreatedAt: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC),
	}
}
