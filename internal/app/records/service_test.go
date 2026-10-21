package records

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/history"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

const (
	testWorktreeID         = model.UUID("11111111-1111-4111-8111-111111111111")
	testRecordID           = model.UUID("22222222-2222-4222-8222-222222222222")
	testIncarnation        = model.UUID("33333333-3333-4333-8333-333333333333")
	testGeneration         = model.UUID("44444444-4444-4444-8444-444444444444")
	testCatalogIncarnation = model.UUID("55555555-5555-4555-8555-555555555555")
	testProfileID          = model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	testContentHash        = model.Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

type testRecordsStore struct {
	worktree      model.UUID
	incarnation   model.UUID
	epoch         int64
	records       []history.Version
	reviewCalls   int
	lastPrincipal model.Identifier
}

func (store *testRecordsStore) WorktreeID() model.UUID  { return store.worktree }
func (store *testRecordsStore) Incarnation() model.UUID { return store.incarnation }

func (store *testRecordsStore) List(_ context.Context, filter history.ListFilter) (sqlite.RecordPage, error) {
	items := append([]history.Version(nil), store.records...)
	sort.Slice(items, func(i, j int) bool { return items[i].RecordID < items[j].RecordID })
	page := sqlite.RecordPage{Items: []history.Version{}, RecordEpoch: store.epoch}
	for _, item := range items {
		if item.RecordID <= filter.AfterRecordID || filter.Kind != "" && item.Kind != filter.Kind {
			continue
		}
		if filter.Status != "" && filter.Status != history.StatusNeedsReview && item.DeclaredStatus != filter.Status {
			continue
		}
		if filter.Status == history.StatusNeedsReview && item.DeclaredStatus != history.StatusApproved {
			continue
		}
		if int64(len(page.Items)) == filter.Limit {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, item)
	}
	if page.HasMore && len(page.Items) > 0 {
		page.NextAfterID = page.Items[len(page.Items)-1].RecordID
	}
	return page, nil
}

func (store *testRecordsStore) Read(_ context.Context, recordID model.UUID, version *int64) (history.Version, int64, error) {
	for _, item := range store.records {
		if item.RecordID != recordID || version != nil && item.Version != *version {
			continue
		}
		return item, store.epoch, nil
	}
	return history.Version{}, store.epoch, history.ErrRecordNotFound
}

func (store *testRecordsStore) RecordEpoch(context.Context) (int64, error) { return store.epoch, nil }

func (store *testRecordsStore) Propose(_ context.Context, principal model.Identifier, input history.ProposeInput) (history.MutationResult, error) {
	store.lastPrincipal = principal
	record := history.Version{RecordID: testRecordID, Version: 1, Kind: input.Kind, DeclaredStatus: history.StatusProposed,
		Payload: input.Payload, SourceRefs: input.SourceRefs, ActorID: principal, Reason: input.Reason}
	store.records = append(store.records, record)
	store.epoch++
	return history.MutationResult{Version: record}, nil
}

func (store *testRecordsStore) Review(_ context.Context, principal model.Identifier, input history.ReviewInput) (history.MutationResult, error) {
	store.reviewCalls++
	store.lastPrincipal = principal
	for index := range store.records {
		if store.records[index].RecordID != input.RecordID || store.records[index].Version != input.ExpectedVersion {
			continue
		}
		record := store.records[index]
		record.Version++
		record.ActorID = principal
		record.Reason = input.Reason
		switch input.Action {
		case history.ReviewApprove:
			record.DeclaredStatus = history.StatusApproved
		case history.ReviewRevoke:
			record.DeclaredStatus = history.StatusRevoked
		case history.ReviewSupersede:
			record.DeclaredStatus = history.StatusSuperseded
		}
		store.records = append(store.records, record)
		store.epoch++
		return history.MutationResult{Version: record}, nil
	}
	return history.MutationResult{}, history.ErrRecordConflict
}

type testPolicyReader struct {
	state  PolicyState
	states []PolicyState
	calls  int
}

func (reader *testPolicyReader) CurrentPolicy(context.Context, model.UUID) (PolicyState, error) {
	if len(reader.states) > 0 {
		index := reader.calls
		if index >= len(reader.states) {
			index = len(reader.states) - 1
		}
		reader.calls++
		return reader.states[index], nil
	}
	reader.calls++
	return reader.state, nil
}

type testPrincipalProvider struct{ value model.Identifier }

func (provider testPrincipalProvider) Principal(context.Context) (model.Identifier, error) {
	return provider.value, nil
}

func TestListDerivesNeedsReviewAndRejectsStaleCursor(t *testing.T) {
	store := newTestRecordsStore()
	store.records = []history.Version{
		recordWithSource(testRecordID, 1, history.StatusApproved),
		recordWithSource(model.UUID("66666666-6666-4666-8666-666666666666"), 1, history.StatusApproved),
	}
	service := newTestService(t, store, nil, fixedPolicyReader(t), testPrincipalProvider{value: "server:reviewer"})

	first, err := service.List(context.Background(), ListRequest{Status: history.StatusNeedsReview, Limit: 1})
	if err != nil || len(first.Records) != 1 || first.Records[0].EffectiveStatus != history.StatusNeedsReview || first.Records[0].ProvenanceStatus != history.ProvenanceSnapshotExpired {
		t.Fatalf("first records page=%+v err=%v", first, err)
	}
	if first.NextCursor == "" {
		t.Fatal("more records exist but the first page did not return a cursor")
	}

	store.epoch++
	if _, err := service.List(context.Background(), ListRequest{Status: history.StatusNeedsReview, Limit: 1, Cursor: first.NextCursor}); !errors.Is(err, ErrStalePage) {
		t.Fatalf("old record epoch cursor returned %v; want stale page", err)
	}
}

func TestReadKeepsExpiredHistoryAndApprovalRequiresCurrentSource(t *testing.T) {
	store := newTestRecordsStore()
	store.records = []history.Version{recordWithSource(testRecordID, 1, history.StatusProposed)}
	service := newTestService(t, store, nil, fixedPolicyReader(t), testPrincipalProvider{value: "server:reviewer"})

	read, err := service.Read(context.Background(), testRecordID, nil, "", "")
	if err != nil || read.Record.EffectiveStatus != history.StatusProposed || read.Record.ProvenanceStatus != history.ProvenanceSnapshotExpired {
		t.Fatalf("historical record read=%+v err=%v", read, err)
	}
	input := history.ReviewInput{RecordID: testRecordID, ExpectedVersion: 1,
		ClientMutationID: model.UUID("77777777-7777-4777-8777-777777777777"), Action: history.ReviewApprove, Reason: "Review source"}
	if _, err := service.Review(context.Background(), input); !errors.Is(err, ErrSourceNotCurrent) {
		t.Fatalf("approval with expired source returned %v", err)
	}
	if store.reviewCalls != 0 {
		t.Fatalf("expired approval reached durable writer %d times", store.reviewCalls)
	}
	input.Action = history.ReviewRevoke
	result, err := service.Review(context.Background(), input)
	if err != nil || result.Record.DeclaredStatus != history.StatusRevoked || store.lastPrincipal != "server:reviewer" {
		t.Fatalf("server-attributed revoke=%+v principal=%q err=%v", result, store.lastPrincipal, err)
	}
}

func TestListRetriesOnePolicyRevisionChangeAndRejectsSecondRace(t *testing.T) {
	store := newTestRecordsStore()
	store.records = []history.Version{{RecordID: testRecordID, Version: 1, Kind: history.KindDecision,
		DeclaredStatus: history.StatusProposed, Payload: []byte(`{"decision":"worktree level"}`), SourceRefs: []model.SourceRef{}}}
	first := testPolicyState(t, 1)
	second := testPolicyState(t, 2)
	reader := &testPolicyReader{states: []PolicyState{first, second, second, second}}
	service := newTestService(t, store, nil, reader, nil)
	result, err := service.List(context.Background(), ListRequest{Limit: 20})
	if err != nil || len(result.Records) != 1 || reader.calls != 4 {
		t.Fatalf("bounded policy retry result=%+v calls=%d err=%v", result, reader.calls, err)
	}

	reader = &testPolicyReader{states: []PolicyState{first, second, first, second}}
	service = newTestService(t, store, nil, reader, nil)
	if _, err := service.List(context.Background(), ListRequest{Limit: 20}); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("repeated policy race returned %v", err)
	}
}

func newTestRecordsStore() *testRecordsStore {
	return &testRecordsStore{worktree: testWorktreeID, incarnation: testIncarnation, records: []history.Version{}}
}

func newTestService(t *testing.T, store Store, index Index, policies PolicyReader, principals PrincipalProvider) *Service {
	t.Helper()
	service, err := NewService(Options{WorktreeID: testWorktreeID, Records: store, Index: index, Policies: policies, Principals: principals})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func fixedPolicyReader(t *testing.T) *testPolicyReader {
	t.Helper()
	return &testPolicyReader{state: testPolicyState(t, 1)}
}

func testPolicyState(t *testing.T, binding int64) PolicyState {
	t.Helper()
	config := catalog.PolicyConfig{SchemaVersion: 1, AllowedRoots: []string{t.TempDir()}, DeniedGlobs: []string{},
		TrustedRulePaths: []string{}, DependencyRoots: []string{}, AllowedExecutables: []catalog.AllowedExecutable{},
		MaxFileBytes: 1, MaxSourceBytes: 1, MaxOutputBytes: 4096, SecretRulesVersion: catalog.SecretRulesVersion}
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return PolicyState{WorktreeID: testWorktreeID, CatalogIncarnation: testCatalogIncarnation,
		Digest: digest, Epoch: 1, BindingRevision: binding, Status: "registered", CanonicalRoot: config.AllowedRoots[0], Config: config}
}

func recordWithSource(recordID model.UUID, version int64, status history.Status) history.Version {
	return history.Version{RecordID: recordID, Version: version, Kind: history.KindDecision, DeclaredStatus: status,
		Payload: []byte(`{"decision":"source-backed"}`), SourceRefs: []model.SourceRef{{WorktreeID: testWorktreeID,
			ProfileID: testProfileID, GenerationID: testGeneration, Path: "src/file.go", ContentHash: testContentHash,
			Range: model.ByteRange{StartByte: 0, EndByte: 3}}}}
}
