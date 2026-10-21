package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/history"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestRecordsStoreCreatesDurableSchemaAndReadOnlyOpen(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "11111111-1111-4111-8111-111111111111"))
	store, err := OpenRecords(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if store.WorktreeID() != options.WorktreeID || store.Incarnation() == "" {
		t.Fatalf("records identity=%s/%s", store.WorktreeID(), store.Incarnation())
	}
	if err := verifyPool(ctx, store.writer, writerPoolSize, store.busyTimeoutMS, false); err != nil {
		t.Fatalf("records writer pool settings: %v", err)
	}
	if err := verifyPool(ctx, store.readers, readerPoolSize, store.busyTimeoutMS, true); err != nil {
		t.Fatalf("records reader pool settings: %v", err)
	}
	if epoch, err := store.RecordEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("initial record epoch=%d err=%v", epoch, err)
	}
	incarnation := store.Incarnation()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenRecordsReadOnly(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := readOnly.Close(); err != nil {
			t.Errorf("close read-only records store: %v", err)
		}
	}()
	if readOnly.Incarnation() != incarnation {
		t.Fatalf("read-only open changed records incarnation: %s != %s", readOnly.Incarnation(), incarnation)
	}
	if _, err := readOnly.Propose(ctx, "server:operator", validRecordProposal(t, "22222222-2222-4222-8222-222222222222")); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("read-only mutation returned %v; want ErrInvalidIndex", err)
	}
}

func TestRecordsProposalReplayReviewCASAndHistoricalRead(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "33333333-3333-4333-8333-333333333333"))
	store, err := OpenRecords(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close records store: %v", err)
		}
	}()

	proposal := validRecordProposal(t, "44444444-4444-4444-8444-444444444444")
	first, err := store.Propose(ctx, "server:operator", proposal)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Version.Version != 1 || first.Version.DeclaredStatus != history.StatusProposed || first.Version.ActorID != "server:operator" {
		t.Fatalf("proposal result=%+v", first)
	}
	replayed, err := store.Propose(ctx, "server:operator", proposal)
	if err != nil || !replayed.Replayed || replayed.Version.RecordID != first.Version.RecordID || replayed.Version.Version != 1 {
		t.Fatalf("proposal replay=%+v err=%v", replayed, err)
	}
	changedActor, err := store.Propose(ctx, "server:other", proposal)
	if !errors.Is(err, history.ErrMutationConflict) || changedActor.Version.RecordID != "" {
		t.Fatalf("replay with a different principal returned %+v, %v", changedActor, err)
	}

	review := history.ReviewInput{RecordID: first.Version.RecordID, ExpectedVersion: 1,
		ClientMutationID: mustRecordUUID(t, "55555555-5555-4555-8555-555555555555"), Action: history.ReviewApprove, Reason: "Reviewed against retained source"}
	approved, err := store.Review(ctx, "server:reviewer", review)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Version.Version != 2 || approved.Version.DeclaredStatus != history.StatusApproved || approved.Version.ActorID != "server:reviewer" {
		t.Fatalf("review result=%+v", approved)
	}
	approvedReplay, err := store.Review(ctx, "server:reviewer", review)
	if err != nil || !approvedReplay.Replayed || approvedReplay.Version.Version != 2 {
		t.Fatalf("review replay=%+v err=%v", approvedReplay, err)
	}
	stale := review
	stale.ClientMutationID = mustRecordUUID(t, "66666666-6666-4666-8666-666666666666")
	if _, err := store.Review(ctx, "server:reviewer", stale); !errors.Is(err, history.ErrRecordConflict) {
		t.Fatalf("stale review returned %v; want CAS conflict", err)
	}

	oldVersion := int64(1)
	old, epoch, err := store.Read(ctx, first.Version.RecordID, &oldVersion)
	if err != nil || old.Version != 1 || old.DeclaredStatus != history.StatusProposed || epoch != 2 {
		t.Fatalf("historical read=%+v epoch=%d err=%v", old, epoch, err)
	}
	current, epoch, err := store.Read(ctx, first.Version.RecordID, nil)
	if err != nil || current.Version != 2 || current.DeclaredStatus != history.StatusApproved || epoch != 2 {
		t.Fatalf("current read=%+v epoch=%d err=%v", current, epoch, err)
	}
	page, err := store.List(ctx, history.ListFilter{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].RecordID != first.Version.RecordID || page.RecordEpoch != 2 {
		t.Fatalf("records list=%+v err=%v", page, err)
	}
}

func TestRecordsMutationReplayRejectsCorruptReceiptIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		column string
		value  string
	}{
		{name: "principal", column: "principal_id", value: " "},
		{name: "request hash", column: "request_hash", value: strings.Repeat("z", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			options := testStoreOptions(t, testUUID(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
			store, err := OpenRecords(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()

			proposal := validRecordProposal(t, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
			if _, err := store.Propose(ctx, "server:operator", proposal); err != nil {
				t.Fatal(err)
			}
			if _, err := store.writer.ExecContext(ctx, `UPDATE mutation_receipts SET `+test.column+`=? WHERE client_mutation_id=?`, test.value, string(proposal.ClientMutationID)); err != nil {
				t.Fatalf("corrupt persisted receipt: %v", err)
			}
			if _, err := store.Propose(ctx, "server:operator", proposal); !errors.Is(err, history.ErrRecordStoreCorrupted) {
				t.Fatalf("replay with corrupt %s returned %v; want ErrRecordStoreCorrupted", test.column, err)
			}
		})
	}
}

func TestRecordsReviewConcurrentCASHasOneWinner(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "77777777-7777-4777-8777-777777777777"))
	first, err := OpenRecords(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := OpenRecords(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	proposal, err := first.Propose(ctx, "server:operator", validRecordProposal(t, "88888888-8888-4888-8888-888888888888"))
	if err != nil {
		t.Fatal(err)
	}
	inputs := []history.ReviewInput{
		{RecordID: proposal.Version.RecordID, ExpectedVersion: 1, ClientMutationID: mustRecordUUID(t, "99999999-9999-4999-8999-999999999999"), Action: history.ReviewApprove, Reason: "Review A"},
		{RecordID: proposal.Version.RecordID, ExpectedVersion: 1, ClientMutationID: mustRecordUUID(t, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), Action: history.ReviewRevoke, Reason: "Review B"},
	}
	stores := []*RecordsStore{first, second}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range inputs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := stores[index].Review(ctx, "server:reviewer", inputs[index])
			results <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, history.ErrRecordConflict):
			conflicts++
		default:
			t.Fatalf("concurrent review returned unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS results wins=%d conflicts=%d, want 1/1", wins, conflicts)
	}
}

func validRecordProposal(t *testing.T, mutationID string) history.ProposeInput {
	t.Helper()
	return history.ProposeInput{
		ClientMutationID: mustRecordUUID(t, mutationID),
		Kind:             history.KindDecision,
		Payload:          json.RawMessage(`{"decision":"stable identifiers"}`),
		Reason:           "Project owner requested this decision",
	}
}

func mustRecordUUID(t *testing.T, raw string) model.UUID {
	t.Helper()
	value, err := model.ParseUUID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
