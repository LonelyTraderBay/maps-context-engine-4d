package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestRenameResultSpoolIsImmutableAndReadableWhileSQLiteWriterIsHeld(t *testing.T) {
	ctx := context.Background()
	worktreeID := testUUID(t, "25111111-1111-4111-8111-111111111111")
	options := testStoreOptions(t, worktreeID)
	options.Create = true
	store, err := openIndex(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	resultID := testUUID(t, "25222222-2222-4222-8222-222222222222")
	raw := []byte(`{"edits":[{"uri":"file:///workspace/a.go","start":0,"end":1,"text":"b"}]}`)
	digest := model.SHA256(raw)
	if err := store.writeRenameResultSpool(ctx, resultID, raw, digest, 1<<20, 2<<20); err != nil {
		t.Fatalf("publish immutable result: %v", err)
	}
	if err := store.writeRenameResultSpool(ctx, resultID, raw, digest, 1<<20, 2<<20); err != nil {
		t.Fatalf("same immutable result was not idempotent: %v", err)
	}
	if err := store.writeRenameResultSpool(ctx, resultID, []byte("different"), model.SHA256([]byte("different")), 1<<20, 2<<20); !errors.Is(err, ErrRenameResultSpoolConflict) {
		t.Fatalf("different bytes reused an immutable result identity: %v", err)
	}

	writer, err := store.ownership.acquireWriter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadRenameResultSpool(ctx, resultID, digest)
	closeErr := writer.Close()
	if err != nil || closeErr != nil || string(got) != string(raw) {
		t.Fatalf("page read while SQLite writer was held got=%q err=%v close=%v", got, err, closeErr)
	}

	spoolPath := filepath.Join(options.HomePath, filepath.FromSlash("worktrees/"+string(worktreeID)+"/scratch/rename/"+string(resultID)+".json"))
	if err := os.WriteFile(spoolPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadRenameResultSpool(ctx, resultID, digest); !errors.Is(err, ErrRenameResultSpoolInvalid) {
		t.Fatalf("modified spool passed immutable digest check: %v", err)
	}
	if err := store.RemoveRenameResultSpool(ctx, resultID); err != nil {
		t.Fatalf("remove verified owned spool: %v", err)
	}
	if _, err := store.ReadRenameResultSpool(ctx, resultID, digest); !errors.Is(err, ErrRenameResultSpoolExpired) {
		t.Fatalf("read after eviction returned %v; want expired", err)
	}
}

func TestRenameResultSpoolAggregateQuotaSerializesIndependentOwners(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	worktreeID := testUUID(t, "25333333-3333-4333-8333-333333333333")
	firstOptions := testStoreOptionsAtHome(t, home, worktreeID)
	firstOptions.Create = true
	first, err := openIndex(ctx, firstOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	secondOptions := testStoreOptionsAtHome(t, home, worktreeID)
	second, err := openIndex(ctx, secondOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	payload := []byte("same bounded result")
	ids := []model.UUID{
		testUUID(t, "25444444-4444-4444-8444-444444444444"),
		testUUID(t, "25555555-5555-4555-8555-555555555555"),
	}
	stores := []*Store{first, second}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	var group sync.WaitGroup
	for index := range stores {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			results <- stores[index].writeRenameResultSpool(ctx, ids[index], payload, model.SHA256(payload), int64(len(payload)), int64(len(payload)))
		}(index)
	}
	close(start)
	group.Wait()
	close(results)
	accepted, rejected := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrAdmissionResourceLimit) {
			rejected++
		} else {
			t.Fatalf("concurrent reservation returned an unexpected error: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("aggregate reservation admitted=%d rejected=%d; want exactly one each", accepted, rejected)
	}
	journals, err := first.ownership.readJournalAnyConfig()
	if err != nil || len(journals.Entries) != 0 {
		t.Fatalf("settled concurrent reservations left journal entries=%d err=%v", len(journals.Entries), err)
	}
}

func TestRenameResultReservationRecoversAnExitedOwner(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "25666666-6666-4666-8666-666666666666"))
	options.Create = true
	store, err := openIndex(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	resultID := testUUID(t, "25777777-7777-4777-8777-777777777777")
	dead, err := store.ownership.admitRenameResultGrowth(ctx, resultID, 128, 256)
	if err != nil {
		t.Fatalf("reserve result for exited owner: %v", err)
	}
	deadOperation := dead.operationID
	if err := dead.lease.Close(); err != nil {
		t.Fatalf("release simulated dead owner lease: %v", err)
	}
	dead.lease = nil

	recovered, err := store.ownership.admitIndexGrowth(ctx, 1<<20)
	if err != nil {
		t.Fatalf("recover dead rename reservation: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatalf("settle new owner after recovery: %v", err)
	}
	journal, err := store.ownership.readJournalAnyConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range journal.Entries {
		if entry.OperationID == deadOperation {
			t.Fatalf("recovery retained exited rename operation %s", deadOperation)
		}
	}
	if len(journal.Entries) != 0 {
		t.Fatalf("recovery left %d operation entries", len(journal.Entries))
	}
}

func TestRenameResultSpoolRejectsAlreadyCanceledWrite(t *testing.T) {
	options := testStoreOptions(t, testUUID(t, "25888888-8888-4888-8888-888888888888"))
	options.Create = true
	store, err := openIndex(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resultID := testUUID(t, "25999999-9999-4999-8999-999999999999")
	raw := []byte("bounded result")
	if err := store.WriteRenameResultSpool(ctx, resultID, raw, model.SHA256(raw)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled result write returned %v", err)
	}
	if _, err := store.ReadRenameResultSpool(context.Background(), resultID, model.SHA256(raw)); !errors.Is(err, ErrRenameResultSpoolExpired) {
		t.Fatalf("canceled write left a visible spool: %v", err)
	}
}
