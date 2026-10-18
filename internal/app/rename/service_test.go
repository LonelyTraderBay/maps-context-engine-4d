package rename

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/validate"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/lsp"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

func TestHandleInitialReturnsReadOnlyPreviewAndContinuationReusesResult(t *testing.T) {
	service, resolver, preconditions, engine, rawRequest := renameFixture(t, nil)
	service.pageBytesPerEdit = 20000
	firstRaw, err := service.Handle(context.Background(), rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var first RenameResponse
	if err := json.Unmarshal(firstRaw, &first); err != nil {
		t.Fatal(err)
	}
	if first.Data == nil || first.Data.Applied || first.Data.SourceMutationCount != 0 || !first.Data.ValidationRequired || !first.Data.ComputationComplete {
		t.Fatalf("initial response does not preserve read-only invariants: %+v", first.Data)
	}
	if first.Status != "partial" || first.Readiness != "needs_context" || first.Data.PreviewComplete || len(first.Data.Edits) != 1 || len(first.Data.Conflicts) != 1 {
		t.Fatalf("initial page or contract conflict is wrong: %+v", first)
	}
	if first.Freshness.Status != string(snapshot.FreshnessObservedCurrent) || preconditions.calls != 2 || engine.previewCalls != 1 || engine.continueCalls != 1 {
		t.Fatalf("pre/post validation or provider calls are wrong: checks=%d preview=%d continue=%d", preconditions.calls, engine.previewCalls, engine.continueCalls)
	}
	if strings.Contains(string(resolver.resolved.SelectedBytes), "Renamed") {
		t.Fatal("source bytes were changed by rename preview")
	}

	continuation, err := json.Marshal(ContinueRequest{ResultToken: first.Data.ResultToken, Cursor: "1"})
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := service.Handle(context.Background(), continuation)
	if err != nil {
		t.Fatal(err)
	}
	var second RenameResponse
	if err := json.Unmarshal(secondRaw, &second); err != nil {
		t.Fatal(err)
	}
	if second.Data == nil || second.Data.ResultID != first.Data.ResultID || len(second.Data.Edits) != 1 || !second.Data.PreviewComplete || second.Pagination.NextCursor != nil {
		t.Fatalf("continuation did not return the next immutable page: %+v", second.Data)
	}
	if second.Freshness.Status != string(snapshot.FreshnessUnknown) || second.Readiness != "needs_context" || second.Data.Applied || second.Data.SourceMutationCount != 0 {
		t.Fatalf("continuation must disclose that freshness was not rechecked: %+v", second)
	}
	if preconditions.calls != 2 || engine.previewCalls != 1 || engine.continueCalls != 2 {
		t.Fatalf("continuation reran validation or LSP computation: checks=%d preview=%d continue=%d", preconditions.calls, engine.previewCalls, engine.continueCalls)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateResponse(operation, secondRaw); err != nil {
		t.Fatalf("continuation response failed S43 validation: %v", err)
	}
}

func TestHandleBlocksBeforeProviderWhenPreconditionsAreStale(t *testing.T) {
	stale := unknownValidation("SOURCE_CHANGED")
	service, _, preconditions, engine, rawRequest := renameFixture(t, []snapshot.ValidationResult{stale})
	if _, err := service.Handle(context.Background(), rawRequest); !errors.Is(err, ErrPreconditions) {
		t.Fatalf("stale precondition error=%v, want %v", err, ErrPreconditions)
	}
	if preconditions.calls != 1 || engine.previewCalls != 0 || engine.releaseCalls != 0 {
		t.Fatalf("provider ran despite stale precondition: checks=%d preview=%d release=%d", preconditions.calls, engine.previewCalls, engine.releaseCalls)
	}
}

func TestHandleReleasesPreparedSpoolWhenPostValidationChanges(t *testing.T) {
	stale := unknownValidation("WORKSPACE_CHANGED_DURING_RENAME")
	service, _, preconditions, engine, rawRequest := renameFixture(t, []snapshot.ValidationResult{currentValidation(), stale})
	if _, err := service.Handle(context.Background(), rawRequest); !errors.Is(err, ErrPreconditions) {
		t.Fatalf("post-validation error=%v, want %v", err, ErrPreconditions)
	}
	if preconditions.calls != 2 || engine.previewCalls != 1 || engine.releaseCalls != 1 {
		t.Fatalf("unsafe result spool was not discarded: checks=%d preview=%d release=%d", preconditions.calls, engine.previewCalls, engine.releaseCalls)
	}
}

func TestHandleRejectsSelectorThatIsNotTheResolvedIdentifier(t *testing.T) {
	service, resolver, _, engine, rawRequest := renameFixture(t, nil)
	resolver.selector.ByteOffset = 0
	request := map[string]any{
		"selector": map[string]any{"kind": "source_position", "path": resolver.resolved.Rename.Source.Path, "content_hash": resolver.resolved.Rename.Source.ContentHash, "byte_offset": 0},
		"new_name": "Renamed",
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Handle(context.Background(), rawRequest); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("non-identifier selector error=%v, want %v", err, ErrInvalidRequest)
	}
	if engine.previewCalls != 0 {
		t.Fatal("provider was invoked for a selector outside an identifier")
	}
}

func TestHandleTamperedResultTokenCannotContinue(t *testing.T) {
	service, _, _, engine, _ := renameFixture(t, nil)
	request, err := json.Marshal(ContinueRequest{ResultToken: "tampered-token", Cursor: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Handle(context.Background(), request); !errors.Is(err, cursor.ErrCursorInvalid) {
		t.Fatalf("tampered result token error=%v, want invalid cursor", err)
	}
	if engine.continueCalls != 0 || engine.previewCalls != 0 {
		t.Fatal("provider was called before validating a result token")
	}
}

func TestHandleHonorsExplicitContractOptOutAndMinimumBudget(t *testing.T) {
	service, resolver, _, engine, _ := renameFixture(t, nil)
	request, err := json.Marshal(InitialRequest{
		Selector: Selector{Kind: "source_position", Path: "main.go", ContentHash: resolver.resolved.Rename.Source.ContentHash,
			ByteOffset: int64(strings.Index(string(resolver.resolved.SelectedBytes), "Exported"))},
		NewName: "Renamed", PreserveContracts: boolPointer(false), MaxOutputBytes: int64Pointer(24576),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RenameResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Data.Conflicts) != 0 || len(decoded.Warnings) != 2 || decoded.Budget.SerializedBytes != int64(len(raw)) {
		t.Fatalf("explicit contract opt-out or byte accounting is wrong: conflicts=%d warnings=%d budget=%d bytes=%d", len(decoded.Data.Conflicts), len(decoded.Warnings), decoded.Budget.SerializedBytes, len(raw))
	}
	if engine.releaseCalls != 0 {
		t.Fatal("successful result spool was unexpectedly removed")
	}

	service, _, _, engine, request = renameFixture(t, nil)
	request, err = replaceOutputBudget(request, 4096)
	if err != nil {
		t.Fatal(err)
	}
	budgetRaw, err := service.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("minimum budget should return an S43 error envelope: %v", err)
	}
	var budgetResponse RenameResponse
	if err := json.Unmarshal(budgetRaw, &budgetResponse); err != nil {
		t.Fatal(err)
	}
	if budgetResponse.Status != "error" || budgetResponse.Error == nil || budgetResponse.Error.Code != "BUDGET_TOO_SMALL" || budgetResponse.Data != nil {
		t.Fatalf("minimum budget response is not a safe S43 error: %+v", budgetResponse)
	}
	if engine.releaseCalls != 1 {
		t.Fatalf("oversized response spool release count=%d, want 1", engine.releaseCalls)
	}
}

func TestContinuationKeepsOriginalCursorLimitAcrossRepeatedBudgetResizes(t *testing.T) {
	service, _, _, engine, rawRequest := renameFixture(t, nil)
	service.pageBytesPerEdit = 256
	var edit lsp.RenameEdit
	if err := json.Unmarshal(engine.edits[0], &edit); err != nil {
		t.Fatal(err)
	}
	edit.NewText = strings.Repeat("R", 256)
	engine.edits = make([]json.RawMessage, 300)
	for index := range engine.edits {
		raw, err := json.Marshal(edit)
		if err != nil {
			t.Fatal(err)
		}
		engine.edits[index] = raw
	}
	engine.ref.EditCount = int64(len(engine.edits))
	rawRequest, err := replaceOutputBudget(rawRequest, maxOutputBytes)
	if err != nil {
		t.Fatal(err)
	}
	firstRaw, err := service.Handle(context.Background(), rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var first RenameResponse
	if err := json.Unmarshal(firstRaw, &first); err != nil {
		t.Fatal(err)
	}
	if first.Data == nil || first.Pagination.NextCursor == nil || !first.Pagination.Truncated {
		t.Fatalf("expected a paged first response: %+v", first)
	}
	continuationRaw, err := json.Marshal(ContinueRequest{
		ResultToken:    first.Data.ResultToken,
		Cursor:         *first.Pagination.NextCursor,
		MaxOutputBytes: int64Pointer(defaultOutputBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	continuedRaw, err := service.Handle(context.Background(), continuationRaw)
	if err != nil {
		t.Fatalf("repeated page reductions must keep the incoming cursor limit: %v", err)
	}
	var continued RenameResponse
	if err := json.Unmarshal(continuedRaw, &continued); err != nil {
		t.Fatal(err)
	}
	if len(engine.resizedFrom) < 2 {
		t.Fatalf("test did not exercise repeated page reductions: %v", engine.resizedFrom)
	}
	for _, previous := range engine.resizedFrom {
		if previous != engine.cursorPageSize {
			t.Fatalf("resize authenticated cursor at limit %d, want original limit %d", previous, engine.cursorPageSize)
		}
	}
	if engine.previewCalls != 1 {
		t.Fatalf("continuation recomputed the rename: previews=%d continuations=%d resizes=%d", engine.previewCalls, engine.continueCalls, len(engine.resizedFrom))
	}
}

type testResolver struct {
	identity lsp.ResultIdentity
	resolved ResolvedInitial
	selector Selector
}

func (resolver *testResolver) ResolveInitial(_ context.Context, selector Selector, _ *ScopeRequest) (ResolvedInitial, error) {
	resolver.selector = selector
	resolved := resolver.resolved
	resolved.SelectedBytes = append([]byte(nil), resolver.resolved.SelectedBytes...)
	return resolved, nil
}

func (resolver *testResolver) CurrentIdentity(context.Context) (lsp.ResultIdentity, error) {
	return resolver.identity, nil
}

type testPreconditions struct {
	results []snapshot.ValidationResult
	calls   int
}

func (validator *testPreconditions) Validate(context.Context, validate.Request) (snapshot.ValidationResult, error) {
	index := validator.calls
	validator.calls++
	if index >= len(validator.results) {
		return currentValidation(), nil
	}
	return validator.results[index], nil
}

type testEngine struct {
	identity       lsp.ResultIdentity
	ref            lsp.ResultRef
	edits          []json.RawMessage
	prepared       lsp.PreparedRename
	warnings       []string
	previewCalls   int
	continueCalls  int
	releaseCalls   int
	cursorPageSize int
	resizedFrom    []int
}

func (engine *testEngine) Preview(_ context.Context, request lsp.RenameRequest) (lsp.RenamePreview, error) {
	engine.previewCalls++
	return engine.page(request.Identity, engine.ref, "", request.PageSize, true), nil
}

func (engine *testEngine) Continue(_ context.Context, identity lsp.ResultIdentity, ref lsp.ResultRef, pageCursor string, pageSize int) (lsp.RenamePreview, error) {
	engine.continueCalls++
	if pageCursor != "" && engine.cursorPageSize == 0 {
		engine.cursorPageSize = pageSize
	}
	return engine.page(identity, ref, pageCursor, pageSize, false), nil
}

func (engine *testEngine) ContinueResized(_ context.Context, identity lsp.ResultIdentity, ref lsp.ResultRef, pageCursor string, previousPageSize, pageSize int) (lsp.RenamePreview, error) {
	engine.continueCalls++
	engine.resizedFrom = append(engine.resizedFrom, previousPageSize)
	if previousPageSize != engine.cursorPageSize {
		return lsp.RenamePreview{}, lsp.ErrResultInvalid
	}
	return engine.page(identity, ref, pageCursor, pageSize, false), nil
}

func (engine *testEngine) Release(context.Context, lsp.ResultRef) error {
	engine.releaseCalls++
	return nil
}

func (engine *testEngine) page(identity lsp.ResultIdentity, ref lsp.ResultRef, pageCursor string, pageSize int, prepared bool) lsp.RenamePreview {
	start := 0
	if pageCursor != "" {
		start, _ = strconv.Atoi(pageCursor)
	}
	if pageSize < 1 {
		pageSize = 1
	}
	end := start + pageSize
	if end > len(engine.edits) {
		end = len(engine.edits)
	}
	page := lsp.RenamePreview{
		ResultRef: ref, Edits: append([]json.RawMessage(nil), engine.edits[start:end]...), Position: int64(end),
		HasMore: end < len(engine.edits), ValidationRequired: true, Applied: false, SourceMutationCount: 0,
	}
	if page.HasMore {
		page.NextCursor = strconv.Itoa(end)
	}
	if prepared {
		page.Prepared = engine.prepared
		page.Warnings = append([]string(nil), engine.warnings...)
	}
	_ = identity
	return page
}

type testUUIDSource struct{ next int }

func (source *testUUIDSource) NewUUID() (model.UUID, error) {
	value := "66666666-6666-4666-8666-666666666666"
	if source.next > 0 {
		value = "77777777-7777-4777-8777-777777777777"
	}
	source.next++
	return model.ParseUUID(value)
}

func renameFixture(t *testing.T, results []snapshot.ValidationResult) (*Service, *testResolver, *testPreconditions, *testEngine, []byte) {
	t.Helper()
	source := []byte("package demo\ntype Record struct { Exported string }\nvar value = Record{Exported: \"x\"}\n")
	worktreeID, _ := model.ParseUUID("11111111-1111-4111-8111-111111111111")
	generationID, _ := model.ParseUUID("22222222-2222-4222-8222-222222222222")
	indexID, _ := model.ParseUUID("33333333-3333-4333-8333-333333333333")
	recordsID, _ := model.ParseUUID("44444444-4444-4444-8444-444444444444")
	profileID := model.SHA256([]byte("rename profile"))
	policyDigest := model.SHA256([]byte("rename policy"))
	providerDigest := model.SHA256([]byte("rename provider"))
	captured, err := snapshot.Build(snapshot.Input{
		SchemaVersion: 1, WorktreeID: worktreeID, ProfileID: profileID, PolicyDigest: policyDigest,
		ProviderDigest: providerDigest, DependencyInputDigests: []model.Hash{},
		Files: []snapshot.FileInput{{Path: "main.go", Status: snapshot.FileCaptured, Content: source}},
		Git:   snapshot.GitState{HeadState: snapshot.HeadUnborn}, Scope: snapshot.ScopeComplete,
	})
	if err != nil {
		t.Fatal(err)
	}
	vector := model.ScopeVector{
		Kind: "single", Members: []model.ScopeMember{{
			WorktreeID: worktreeID, ProfileID: profileID, GenerationID: generationID,
			SnapshotDigest: captured.Manifest.SnapshotDigest, PolicyDigest: policyDigest,
			PolicyEpoch: 1, BindingRevision: 1, RecordEpoch: 0,
			IndexIncarnation: indexID, RecordsIncarnation: recordsID,
		}}, CatalogIncarnation: indexID, UnavailableMembers: []model.UnavailableMember{},
	}
	vector.VectorDigest, err = vector.ComputeDigest()
	if err != nil || vector.Validate() != nil {
		t.Fatalf("build valid single scope vector: digest error=%v validate=%v", err, vector.Validate())
	}
	selectedStart := int64(strings.Index(string(source), "Exported"))
	selectedRange, _, err := lsp.IdentifierRangeAt(source, "main.go", selectedStart)
	if err != nil {
		t.Fatal(err)
	}
	contentHash := model.SHA256(source)
	identity := lsp.ResultIdentity{Scope: vector, PrincipalID: "test-principal", AllowedBindingID: "test-binding", EngineSchema: lsp.EngineSchemaVersion}
	selector := Selector{Kind: "source_position", Path: "main.go", ContentHash: contentHash, ByteOffset: selectedStart}
	root := t.TempDir()
	resolved := ResolvedInitial{
		Rename: lsp.RenameRequest{
			Identity: identity, Snapshot: lsp.WorkspaceSnapshot{Root: root, Complete: true, Scope: vector, Files: []lsp.SnapshotFile{{Path: "main.go", ContentHash: contentHash}}},
			Profile: lsp.GoProfile{ID: profileID, ProviderDigest: providerDigest},
			Source:  model.SourceRef{WorktreeID: worktreeID, ProfileID: profileID, GenerationID: generationID, Path: "main.go", ContentHash: contentHash, Range: selectedRange},
		},
		Validation: validate.Request{
			Expected: captured.Manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
			SourcePreconditions: []snapshot.SourcePrecondition{{Path: "main.go", ContentHash: contentHash}},
		},
		SelectedBytes: append([]byte(nil), source...),
	}
	resolver := &testResolver{identity: identity, resolved: resolved}
	preconditions := &testPreconditions{results: results}
	firstRange := int64(strings.Index(string(source), "Exported"))
	secondRange := int64(strings.LastIndex(string(source), "Exported"))
	edit := func(start int64) json.RawMessage {
		value, _ := json.Marshal(lsp.RenameEdit{Path: "main.go", ContentHash: contentHash, Version: 1, Range: model.ByteRange{StartByte: start, EndByte: start + int64(len("Exported"))}, NewText: "Renamed"})
		return value
	}
	refID, _ := model.ParseUUID("55555555-5555-4555-8555-555555555555")
	engine := &testEngine{
		identity: identity, ref: lsp.ResultRef{ResultID: refID, Digest: model.SHA256([]byte("result")), Expires: time.Now().Add(10 * time.Minute).Unix(), EditCount: 2},
		edits: []json.RawMessage{edit(firstRange), edit(secondRange)}, prepared: lsp.PreparedRename{Range: selectedRange, Placeholder: "Exported"},
		warnings: []string{warningRuntimeLookup, warningSerialization},
	}
	codec, err := cursor.NewCursorCodec(make([]byte, 32), cursor.DefaultCursorTTL, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(resolver, preconditions, engine, &testUUIDSource{}, codec)
	if err != nil {
		t.Fatal(err)
	}
	rawRequest, err := json.Marshal(InitialRequest{Selector: selector, NewName: "Renamed", MaxOutputBytes: int64Pointer(24576)})
	if err != nil {
		t.Fatal(err)
	}
	return service, resolver, preconditions, engine, rawRequest
}

func currentValidation() snapshot.ValidationResult {
	checkedAt := time.Now().UTC()
	count := int64(0)
	return snapshot.ValidationResult{
		EditPreconditionValid: true,
		Freshness: snapshot.Freshness{
			Status: snapshot.FreshnessObservedCurrent, Scope: snapshot.FreshnessWorkspace, CheckedAt: &checkedAt,
			BufferScope: "disk_only", Consistency: snapshot.FreshnessCapturedBytesWithObservedStability,
			ChangedCount: &count, SourceView: snapshot.FreshnessIndexedSnapshot,
		},
	}
}

func unknownValidation(reason string) snapshot.ValidationResult {
	return snapshot.UnknownValidation(time.Now().UTC(), reason)
}

func int64Pointer(value int64) *int64 { return &value }

func boolPointer(value bool) *bool { return &value }

func replaceOutputBudget(raw []byte, value int64) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	request["max_output_bytes"] = value
	return json.Marshal(request)
}
