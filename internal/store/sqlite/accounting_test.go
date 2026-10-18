package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestMeasureIndexCacheReportsLogicalFilesAndPageAccounting(t *testing.T) {
	store := openTestIndex(t, 1234)
	ctx := context.Background()
	measurement, err := store.measureIndexCache(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if measurement.DatabaseFileBytes <= 0 || measurement.IndexFileBytes < measurement.DatabaseFileBytes {
		t.Fatalf("index file accounting=%+v; expected a nonzero database file included in index total", measurement)
	}
	if measurement.ObservedAt.IsZero() || measurement.ObservedAt.Location() != time.UTC {
		t.Fatalf("observed_at=%v; want a UTC timestamp", measurement.ObservedAt)
	}
	var pageSize, pageCount, freePages int64
	for _, query := range []struct {
		statement string
		dest      *int64
	}{
		{`PRAGMA page_size`, &pageSize},
		{`PRAGMA page_count`, &pageCount},
		{`PRAGMA freelist_count`, &freePages},
	} {
		if err := store.readers.QueryRowContext(ctx, query.statement).Scan(query.dest); err != nil {
			t.Fatal(err)
		}
	}
	if measurement.LivePageBytes != pageSize*(pageCount-freePages) || measurement.FreePageBytes != pageSize*freePages {
		t.Fatalf("page accounting=%+v, PRAGMA=%d*%d with %d free pages", measurement, pageSize, pageCount, freePages)
	}
	if got, ok := addNonnegative(measurement.DatabaseFileBytes, measurement.WALBytes); !ok || got > measurement.IndexFileBytes {
		t.Fatalf("index total %d omitted a measured index sidecar", measurement.IndexFileBytes)
	}
	if _, ok := multiplyNonnegative(int64(^uint64(0)>>1), 2); ok {
		t.Fatal("page-byte overflow was accepted")
	}
	if _, _, err := regularFileLengthNoFollow(t.TempDir(), false); !errors.Is(err, ErrInvalidStorePath) {
		t.Fatalf("directory as index file returned %v; want ErrInvalidStorePath", err)
	}
}

func TestOwnedCacheAccountingIncludesRenameAndOtherScratchFiles(t *testing.T) {
	store := openTestIndex(t, 1234)
	owner := store.ownership
	worktreeID := store.WorktreeID()
	before, err := owner.worktreeCacheBytesFor(worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.root.EnsurePrivateDirs("worktrees/" + string(worktreeID) + "/scratch/rename"); err != nil {
		t.Fatal(err)
	}
	rootPath, err := owner.root.FinalPath()
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(rootPath, "worktrees", string(worktreeID), "scratch", "inventory.cache")
	if err := os.WriteFile(scratch, []byte("cache-accounting"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := owner.worktreeCacheBytesFor(worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if after-before != int64(len("cache-accounting")) {
		t.Fatalf("worktree cache did not include scratch growth: before=%d after=%d", before, after)
	}

	spoolDirectory := filepath.Join(rootPath, "worktrees", string(worktreeID), "scratch", "rename")
	for name, body := range map[string]string{"result.json": "rename-result", ".atomic-write.tmp": "interrupted-temp"} {
		if err := os.WriteFile(filepath.Join(spoolDirectory, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	renameBytes, err := owner.renameResultBytesFor(worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len("rename-result") + len("interrupted-temp"))
	if renameBytes != want {
		t.Fatalf("rename aggregate measured %d bytes; want result and interrupted temp total %d", renameBytes, want)
	}
}

func TestDurableAdmissionUsesVolumeBudgetAndPreservesCrashOutput(t *testing.T) {
	store := openTestIndex(t, 1234)
	owner := store.ownership
	ctx := context.Background()
	worktreeID := store.WorktreeID()
	backupID := testUUID(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	backupPath := model.AdmissionOwnedPath("worktrees/" + string(worktreeID) + "/backups/" + string(backupID))
	cacheBefore, err := owner.worktreeCacheBytesFor(worktreeID)
	if err != nil {
		t.Fatal(err)
	}

	reservation, err := owner.admitDurableGrowth(ctx, "backup", backupPath, 4096)
	if err != nil {
		t.Fatalf("admit durable backup growth: %v", err)
	}
	journal, err := owner.readJournalAnyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Entries) != 1 || journal.Entries[0].OperationKind != "backup" ||
		journal.Entries[0].VolumeReservedRemainingBytes != 4096 || journal.Entries[0].CacheReservedRemainingBytes != 0 {
		t.Fatalf("durable reservation has incorrect cache/volume classification: %+v", journal.Entries)
	}
	if err := owner.root.EnsurePrivateDirs(string(backupPath)); err != nil {
		t.Fatal(err)
	}
	backupFile := string(backupPath) + "/snapshot.sqlite"
	if err := owner.root.WriteAtomicFile(backupFile, []byte("durable snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheAfter, err := owner.worktreeCacheBytesFor(worktreeID)
	if err != nil {
		t.Fatal(err)
	}
	if cacheAfter != cacheBefore {
		t.Fatalf("backup growth changed cache accounting: before=%d after=%d", cacheBefore, cacheAfter)
	}
	if got, err := owner.measureOwnedPathBytes(backupPath, journal); err != nil || got != int64(len("durable snapshot")) {
		t.Fatalf("durable backup measurement=%d, err=%v", got, err)
	}
	if err := reservation.Close(); err != nil {
		t.Fatalf("settle durable backup reservation: %v", err)
	}

	interruptedID := testUUID(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbc")
	interruptedPath := model.AdmissionOwnedPath("worktrees/" + string(worktreeID) + "/backups/" + string(interruptedID))
	interrupted, err := owner.admitDurableGrowth(ctx, "backup", interruptedPath, 4096)
	if err != nil {
		t.Fatalf("admit interrupted backup: %v", err)
	}
	if err := owner.root.EnsurePrivateDirs(string(interruptedPath)); err != nil {
		t.Fatal(err)
	}
	if err := owner.root.WriteAtomicFile(string(interruptedPath)+"/manifest.json", []byte("preserve interrupted backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := interrupted.lease.Close(); err != nil {
		t.Fatalf("simulate backup process exit: %v", err)
	}
	resumed, err := owner.admitDurableGrowth(ctx, "backup", interruptedPath, 4096)
	if err != nil {
		t.Fatalf("recover interrupted reservation using output operation identity: %v", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatalf("settle resumed backup reservation: %v", err)
	}

	crashID := testUUID(t, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	crashPath := model.AdmissionOwnedPath("worktrees/" + string(worktreeID) + "/backups/" + string(crashID))
	if err := owner.root.EnsurePrivateDirs(string(crashPath)); err != nil {
		t.Fatal(err)
	}
	retainedFile := string(crashPath) + "/manifest.json"
	if err := owner.root.WriteAtomicFile(retainedFile, []byte("preserve after interrupted backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err = owner.readJournalAnyConfig()
	if err != nil {
		t.Fatal(err)
	}
	journal.Entries = append(journal.Entries, model.VolumeAdmissionEntry{
		OperationID: crashID, ScopeKind: "worktree", WorktreeID: &worktreeID,
		OwnerIdentity: model.Identifier("lease-" + string(crashID)),
		OwnershipRef:  model.AdmissionOwnedPath("operation-leases/" + string(crashID) + ".lock"),
		OperationKind: "backup", AllowedPaths: []model.AdmissionOwnedPath{crashPath},
		VolumeReservedRemainingBytes: 128, CacheReservedRemainingBytes: 0,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err := owner.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	recovered, err := owner.recoverDeadOwners(ctx, journal)
	if err != nil {
		t.Fatalf("recover interrupted durable reservation: %v", err)
	}
	if len(recovered.Entries) != 0 {
		t.Fatalf("dead durable operation lease remains reserved: %+v", recovered.Entries)
	}
	retained, err := owner.root.OpenRegularFile(retainedFile)
	if err != nil {
		t.Fatalf("recovery removed or obscured crash evidence: %v", err)
	}
	if err := retained.Close(); err != nil {
		t.Fatalf("close preserved crash evidence: %v", err)
	}
}

func TestAdmissionJournalCanonicalBoundsBindingAndAtomicReadback(t *testing.T) {
	journal, binding := validAdmissionJournal(t)
	raw, err := encodeAdmissionJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxAdmissionJournalBytes || !bytes.HasPrefix(raw, []byte(`{"admission_config_digest"`)) {
		t.Fatalf("encoded journal is not canonical/bounded: %s", raw)
	}
	decoded, err := decodeAdmissionJournal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.FormatVersion != journal.FormatVersion || decoded.VolumeID != journal.VolumeID || decoded.HomeIdentity != journal.HomeIdentity || decoded.AdmissionConfigDigest != journal.AdmissionConfigDigest || len(decoded.Entries) != 1 || decoded.Entries[0].OperationID != journal.Entries[0].OperationID {
		t.Fatalf("journal round trip=%+v, want %+v", decoded, journal)
	}
	duplicateKey := bytes.Replace(raw, []byte(`"format_version":1`), []byte(`"format_version":1,"format_version":1`), 1)
	if _, err := decodeAdmissionJournal(duplicateKey); err == nil {
		t.Fatal("duplicate journal object key was accepted")
	}
	unknownKey := bytes.Replace(raw, []byte(`"format_version":1`), []byte(`"format_version":1,"unknown":true`), 1)
	if _, err := decodeAdmissionJournal(unknownKey); err == nil {
		t.Fatal("unknown journal member was accepted")
	}
	duplicateOperation := journal
	duplicateOperation.Entries = append(append([]model.VolumeAdmissionEntry(nil), journal.Entries...), journal.Entries[0])
	if _, err := encodeAdmissionJournal(duplicateOperation); err == nil {
		t.Fatal("duplicate operation ID was accepted")
	}
	totals, err := sumAdmissionReservations(journal)
	if err != nil {
		t.Fatal(err)
	}
	worktreeID := journal.Entries[0].WorktreeID
	if totals.VolumeRemainingBytes != 128 || totals.CacheByWorktree[*worktreeID] != 96 {
		t.Fatalf("reservation totals=%+v; want volume=128/cache=96", totals)
	}
	second := journal.Entries[0]
	second.OperationID = testUUID(t, "cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	second.OwnershipRef = model.AdmissionOwnedPath("operation-leases/" + string(second.OperationID) + ".lock")
	journalWithTwo := journal
	journalWithTwo.Entries = append(append([]model.VolumeAdmissionEntry(nil), journal.Entries...), second)
	totals, err = sumAdmissionReservations(journalWithTwo)
	if err != nil {
		t.Fatal(err)
	}
	if totals.VolumeRemainingBytes != 256 || totals.CacheByWorktree[*worktreeID] != 192 {
		t.Fatalf("shared reservation totals=%+v; want volume=256/cache=192", totals)
	}
	second.VolumeReservedRemainingBytes = model.MaxSafeInteger
	second.CacheReservedRemainingBytes = 0
	second.OperationKind = "record_write"
	second.AllowedPaths = []model.AdmissionOwnedPath{model.AdmissionOwnedPath("worktrees/" + string(*worktreeID) + "/records")}
	second.OwnershipRef = model.AdmissionOwnedPath("operation-leases/" + string(second.OperationID) + ".lock")
	overflowJournal := journal
	overflowJournal.Entries = []model.VolumeAdmissionEntry{second, second}
	overflowJournal.Entries[1].OperationID = testUUID(t, "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	overflowJournal.Entries[1].OwnershipRef = model.AdmissionOwnedPath("operation-leases/" + string(overflowJournal.Entries[1].OperationID) + ".lock")
	if _, err := sumAdmissionReservations(overflowJournal); !errors.Is(err, ErrAdmissionReservationOverflow) {
		t.Fatalf("reservation overflow error=%v; want ErrAdmissionReservationOverflow", err)
	}
	nonUTCTime := journal
	nonUTCTime.Entries = append([]model.VolumeAdmissionEntry(nil), journal.Entries...)
	nonUTCTime.Entries[0].CreatedAt = "2026-10-16T07:00:00+07:00"
	if _, err := encodeAdmissionJournal(nonUTCTime); !errors.Is(err, ErrInvalidAdmissionJournal) {
		t.Fatalf("non-UTC journal time error=%v; want ErrInvalidAdmissionJournal", err)
	}
	if _, err := decodeAdmissionJournal(bytes.Repeat([]byte{'x'}, maxAdmissionJournalBytes+1)); !errors.Is(err, ErrAdmissionJournalTooLarge) {
		t.Fatalf("oversized journal error=%v; want ErrAdmissionJournalTooLarge", err)
	}

	path := filepath.Join(t.TempDir(), "volume.json")
	if err := writeAdmissionJournalAtomic(path, journal); err != nil {
		t.Fatal(err)
	}
	readBack, err := readAdmissionJournal(path, binding)
	if err != nil {
		t.Fatal(err)
	}
	if readBack.AdmissionConfigDigest != binding.ConfigDigest {
		t.Fatalf("read digest=%s, want %s", readBack.AdmissionConfigDigest, binding.ConfigDigest)
	}
	updated := journal
	updated.Entries = append([]model.VolumeAdmissionEntry(nil), journal.Entries...)
	updated.Entries[0].VolumeReservedRemainingBytes = 0
	updated.Entries[0].CacheReservedRemainingBytes = 0
	if err := writeAdmissionJournalAtomic(path, updated); err != nil {
		t.Fatalf("replace journal atomically: %v", err)
	}
	readBack, err = readAdmissionJournal(path, binding)
	if err != nil {
		t.Fatal(err)
	}
	if readBack.Entries[0].VolumeReservedRemainingBytes != 0 {
		t.Fatalf("atomic replacement left old reservation: %+v", readBack.Entries[0])
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".admission-journal-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary journal files remain: %v, error=%v", matches, err)
	}
	if _, err := readAdmissionJournal(path, admissionJournalBinding{VolumeID: binding.VolumeID, HomeIdentity: binding.HomeIdentity, ConfigDigest: model.Hash(strings.Repeat("b", 64))}); !errors.Is(err, ErrAdmissionConfigConflict) {
		t.Fatalf("config digest mismatch error=%v; want ErrAdmissionConfigConflict", err)
	}
	if _, err := readAdmissionJournal(path, admissionJournalBinding{VolumeID: "another-volume", HomeIdentity: binding.HomeIdentity, ConfigDigest: binding.ConfigDigest}); !errors.Is(err, ErrAdmissionIdentityMismatch) {
		t.Fatalf("volume mismatch error=%v; want ErrAdmissionIdentityMismatch", err)
	}
	if err := writeAdmissionJournalAtomic(filepath.Join(t.TempDir(), "missing", "volume.json"), journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write without an owned parent directory returned %v; want os.ErrNotExist", err)
	}
	if _, err := admissionJournalPath("relative/volume.json"); !errors.Is(err, ErrAdmissionJournalPath) {
		t.Fatalf("relative journal path returned %v; want ErrAdmissionJournalPath", err)
	}
}

func TestAdmissionJournalAccepts256Entries(t *testing.T) {
	journal := admissionJournalWithEntryCount(t, 256)
	raw, err := encodeAdmissionJournal(journal)
	if err != nil {
		t.Fatalf("journal with 256 entries was rejected: %v", err)
	}
	if len(raw) > maxAdmissionJournalBytes {
		t.Fatalf("256-entry journal has %d bytes; maximum is %d", len(raw), maxAdmissionJournalBytes)
	}
	readBack, err := decodeAdmissionJournal(raw)
	if err != nil {
		t.Fatalf("decode 256-entry journal: %v", err)
	}
	if len(readBack.Entries) != 256 {
		t.Fatalf("round-tripped journal has %d entries; want 256", len(readBack.Entries))
	}
	seen := make(map[model.UUID]struct{}, len(readBack.Entries))
	for _, entry := range readBack.Entries {
		if _, exists := seen[entry.OperationID]; exists {
			t.Fatalf("round-tripped journal contains duplicate operation ID %q", entry.OperationID)
		}
		seen[entry.OperationID] = struct{}{}
	}
}

func TestAdmissionJournalRejectsMoreThan256Entries(t *testing.T) {
	journal := admissionJournalWithEntryCount(t, 257)
	if _, err := encodeAdmissionJournal(journal); !errors.Is(err, model.ErrInvalidValue) {
		t.Fatalf("257-entry journal error=%v; want model.ErrInvalidValue", err)
	}
}

func admissionJournalWithEntryCount(t *testing.T, count int) model.VolumeAdmissionJournal {
	t.Helper()
	journal, _ := validAdmissionJournal(t)
	entries := make([]model.VolumeAdmissionEntry, count)
	for index := range entries {
		entry := journal.Entries[0]
		operationID := testUUID(t, fmt.Sprintf("%08x-aaaa-4aaa-8aaa-%012x", index+1, index+1))
		entry.OperationID = operationID
		entry.OwnershipRef = model.AdmissionOwnedPath("operation-leases/" + string(operationID) + ".lock")
		entry.AllowedPaths = []model.AdmissionOwnedPath{
			model.AdmissionOwnedPath(fmt.Sprintf("worktrees/%s/index-%03d.sqlite", *entry.WorktreeID, index+1)),
		}
		entries[index] = entry
	}
	journal.Entries = entries
	return journal
}

func validAdmissionJournal(t *testing.T) (model.VolumeAdmissionJournal, admissionJournalBinding) {
	t.Helper()
	worktreeID := testUUID(t, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	operationID := testUUID(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	config := model.AdmissionConfig{
		SchemaVersion: 1, MaxWorktreeCacheBytes: 1024, VolumeReserveBytes: 64, VolumeWarningBytes: 128,
	}
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	entry := model.VolumeAdmissionEntry{
		OperationID: operationID, ScopeKind: "worktree", WorktreeID: &worktreeID,
		OwnerIdentity: "owner-test", OwnershipRef: model.AdmissionOwnedPath("operation-leases/" + string(operationID) + ".lock"),
		OperationKind: "index", AllowedPaths: []model.AdmissionOwnedPath{
			model.AdmissionOwnedPath("worktrees/" + string(worktreeID) + "/index.sqlite"),
		},
		VolumeReservedRemainingBytes: 128, CacheReservedRemainingBytes: 96,
		CreatedAt: "2026-10-16T00:00:00Z",
	}
	journal := model.VolumeAdmissionJournal{
		FormatVersion: 1, VolumeID: "volume-test", HomeIdentity: "home-test",
		AdmissionConfigDigest: digest, Entries: []model.VolumeAdmissionEntry{entry},
	}
	return journal, admissionJournalBinding{VolumeID: journal.VolumeID, HomeIdentity: journal.HomeIdentity, ConfigDigest: digest}
}
