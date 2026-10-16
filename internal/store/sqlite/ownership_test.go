package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

func TestStoreLifecycleAndWriterLeasesStayScopedToTheirWorktree(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	firstID := testUUID(t, "12111111-1111-4111-8111-111111111111")
	secondID := testUUID(t, "12222222-2222-4222-8222-222222222222")
	firstOptions := testStoreOptionsAtHome(t, home, firstID)
	firstOptions.Create = true
	first, err := openIndex(ctx, firstOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	probeRoot, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer probeRoot.Close()
	if _, err := mceruntime.AcquireHomeLifecycle(ctx, probeRoot, mceruntime.LockExclusive, 25*time.Millisecond); !errors.Is(err, mceruntime.ErrLockTimeout) {
		t.Fatalf("exclusive home maintenance entered while a store pool was open: %v", err)
	}

	writerA, err := first.ownership.acquireWriter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reservationA, err := first.ownership.admitIndexGrowth(ctx, 1<<20)
	if err != nil {
		_ = writerA.Close()
		t.Fatal(err)
	}
	secondOptions := testStoreOptionsAtHome(t, home, secondID)
	secondOptions.Create = true
	second, err := openIndex(ctx, secondOptions)
	if err != nil {
		_ = reservationA.Close()
		_ = writerA.Close()
		t.Fatalf("worktree B was blocked by worktree A's writer lease: %v", err)
	}
	if err := second.readers.PingContext(ctx); err != nil {
		_ = second.Close()
		_ = reservationA.Close()
		_ = writerA.Close()
		t.Fatalf("worktree B reader pool did not progress: %v", err)
	}
	journal, err := first.ownership.readJournalAnyConfig()
	if err != nil || len(journal.Entries) != 1 || journal.Entries[0].OperationID != reservationA.operationID {
		_ = second.Close()
		_ = reservationA.Close()
		_ = writerA.Close()
		t.Fatalf("worktree B changed worktree A's live admission entry: entries=%v err=%v", journal.Entries, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reservationA.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writerA.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	maintenance, err := mceruntime.AcquireHomeLifecycle(ctx, probeRoot, mceruntime.LockExclusive, time.Second)
	if err != nil {
		t.Fatalf("exclusive home lifecycle lease remained after pools closed: %v", err)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreOwnershipWaitsForHomeLifecycleBeforeCreatingWorktreeLocks(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	worktreeID := testUUID(t, "12555555-5555-4555-8555-555555555555")
	root, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	maintenance, err := mceruntime.AcquireHomeLifecycle(ctx, root, mceruntime.LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()

	options := Options{
		HomePath: home, Path: filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite"),
		WorktreeID: worktreeID, AdmissionConfig: testAdmissionConfig(), LockWaitMS: 25,
	}
	if _, err := newStoreOwnership(ctx, options, false); !errors.Is(err, mceruntime.ErrLockTimeout) {
		t.Fatalf("ownership initialization under exclusive home maintenance returned %v; want bounded lock timeout", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "worktrees")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree lock layout changed before home lifecycle admission: stat err=%v", err)
	}
}

func TestAdmissionConfigDriftWaitsForLiveOwnerAndUpdatesEmptyJournal(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "13111111-1111-4111-8111-111111111111"))
	options.Create = true
	config := testAdmissionConfig()
	options.AdmissionConfigProvider = func(context.Context) (model.AdmissionConfig, error) { return config, nil }
	store, err := openIndex(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	reservation, err := store.ownership.admitIndexGrowth(ctx, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	config.VolumeWarningBytes++
	if _, err := store.ownership.admitIndexGrowth(ctx, 1<<20); !errors.Is(err, ErrAdmissionConfigConflict) {
		t.Fatalf("config drift crossed a live reservation: %v", err)
	}
	configWriter, err := mceruntime.AcquireHomeConfig(ctx, store.ownership.root, mceruntime.LockExclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Close(); err != nil {
		_ = configWriter.Close()
		t.Fatalf("release live owner under prior digest: %v", err)
	}
	if err := configWriter.Close(); err != nil {
		t.Fatal(err)
	}
	settledJournal, err := store.ownership.readJournalAnyConfig()
	if err != nil || len(settledJournal.Entries) != 0 {
		t.Fatalf("settling under exclusive config lock retained admission entries: entries=%d err=%v", len(settledJournal.Entries), err)
	}
	settled, err := store.ownership.admitIndexGrowth(ctx, 1<<20)
	if err != nil {
		t.Fatalf("empty journal did not adopt the revalidated config digest: %v", err)
	}
	if err := settled.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionHoldsSharedConfigLockThroughJournalWrite(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "13555555-5555-4555-8555-555555555555"))
	options.Create = true
	store, err := openIndex(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	providerStarted := make(chan struct{}, 1)
	providerContinue := make(chan struct{})
	store.ownership.configProvider = func(context.Context) (model.AdmissionConfig, error) {
		select {
		case providerStarted <- struct{}{}:
		default:
		}
		<-providerContinue
		return testAdmissionConfig(), nil
	}
	result := make(chan error, 1)
	go func() {
		reservation, err := store.ownership.admitIndexGrowth(ctx, 1<<20)
		if err == nil {
			err = reservation.Close()
		}
		result <- err
	}()
	select {
	case <-providerStarted:
	case <-time.After(time.Second):
		close(providerContinue)
		t.Fatal("admission did not read its config snapshot")
	}
	configWriter, lockErr := mceruntime.AcquireHomeConfig(ctx, store.ownership.root, mceruntime.LockExclusive, 25*time.Millisecond)
	if lockErr == nil {
		_ = configWriter.Close()
	}
	close(providerContinue)
	select {
	case err := <-result:
		if !errors.Is(lockErr, mceruntime.ErrLockTimeout) {
			t.Fatalf("config writer crossed an in-flight admission snapshot: %v", lockErr)
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission did not finish after the config provider returned")
	}
}

func TestAdmissionRecoveryReclaimsOnlyAnUnlockedVerifiedOwner(t *testing.T) {
	ctx := context.Background()
	options := testStoreOptions(t, testUUID(t, "14111111-1111-4111-8111-111111111111"))
	options.Create = true
	store, err := openIndex(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	dead, err := store.ownership.admitIndexGrowth(ctx, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.lease.Close(); err != nil {
		t.Fatal(err)
	}
	dead.lease = nil // Leave the durable journal entry to model an exited owner.
	recovered, err := store.ownership.admitIndexGrowth(ctx, 1<<20)
	if err != nil {
		t.Fatalf("unlocked verified owner was not recovered: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := store.ownership.readJournalAnyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Entries) != 0 {
		t.Fatalf("recovery left %d operation entries in the journal", len(journal.Entries))
	}
}

func TestOpenIndexAdmissionRejectsPeakBeforeCreatingSQLite(t *testing.T) {
	options := testStoreOptions(t, testUUID(t, "15111111-1111-4111-8111-111111111111"))
	options.Create = true
	options.AdmissionConfig.MaxWorktreeCacheBytes = 1
	if _, err := openIndex(context.Background(), options); !errors.Is(err, ErrAdmissionResourceLimit) {
		t.Fatalf("oversized initial growth returned %v; want RESOURCE_LIMIT", err)
	}
	if _, err := os.Lstat(options.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index was created before admission: stat err=%v", err)
	}
}

func testStoreOptionsAtHome(t *testing.T, home string, worktreeID model.UUID) Options {
	t.Helper()
	path := filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return Options{HomePath: home, Path: path, WorktreeID: worktreeID, AdmissionConfig: testAdmissionConfig(), LockWaitMS: 500}
}
