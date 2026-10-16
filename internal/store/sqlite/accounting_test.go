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

func TestAdmissionJournalRejectsMoreThan256Entries(t *testing.T) {
	journal, _ := validAdmissionJournal(t)
	entries := make([]model.VolumeAdmissionEntry, 257)
	for index := range entries {
		entry := journal.Entries[0]
		operationID := testUUID(t, fmt.Sprintf("%08x-aaaa-4aaa-8aaa-%012x", index+1, index+1))
		entry.OperationID = operationID
		entry.OwnershipRef = model.AdmissionOwnedPath("operation-leases/" + string(operationID) + ".lock")
		entries[index] = entry
	}
	journal.Entries = entries
	if _, err := encodeAdmissionJournal(journal); err == nil {
		t.Fatal("journal with more than 256 entries was accepted")
	}
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
