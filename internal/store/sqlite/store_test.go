package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestOpenIndexCreatesOrderedSchemaAndAppliesEveryPoolSetting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	worktreeID := testUUID(t, "11111111-1111-4111-8111-111111111111")
	first, err := openIndex(ctx, Options{Path: path, WorktreeID: worktreeID, BusyTimeoutMS: 1234, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPool(ctx, first.writer, writerPoolSize, 1234, false); err != nil {
		t.Fatalf("writer pool settings: %v", err)
	}
	if err := verifyPool(ctx, first.readers, readerPoolSize, 1234, true); err != nil {
		t.Fatalf("reader pool settings: %v", err)
	}
	if first.WorktreeID() != worktreeID || first.Incarnation() == "" {
		t.Fatalf("stored identity=%s/%s, want worktree %s and generated incarnation", first.WorktreeID(), first.Incarnation(), worktreeID)
	}
	var version, minReader int64
	if err := first.readers.QueryRowContext(ctx, `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&version, &minReader); err != nil {
		t.Fatal(err)
	}
	if version != 2 || minReader != 2 {
		t.Fatalf("schema metadata=%d/%d; want 2/2", version, minReader)
	}
	if _, err := first.readers.ExecContext(ctx, `CREATE TABLE must_not_be_created (id INTEGER)`); err == nil {
		t.Fatal("read-only reader pool unexpectedly created a table")
	}
	incarnation := first.Incarnation()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := openIndex(ctx, Options{Path: path, WorktreeID: worktreeID, BusyTimeoutMS: 1234})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close reopened index: %v", err)
		}
	})
	if second.Incarnation() != incarnation {
		t.Fatalf("reopen changed index incarnation from %s to %s", incarnation, second.Incarnation())
	}
	if _, err := openIndex(ctx, Options{Path: path, WorktreeID: testUUID(t, "22222222-2222-4222-8222-222222222222")}); !errors.Is(err, ErrWorktreeMismatch) {
		t.Fatalf("opening under another worktree returned %v; want ErrWorktreeMismatch", err)
	}
}

func TestOpenIndexRejectsInvalidFreshIdentityBeforeMutatingPath(t *testing.T) {
	ctx := context.Background()
	for _, testCase := range []struct {
		name       string
		createFile bool
		worktreeID model.UUID
	}{
		{name: "empty worktree on missing path"},
		{name: "malformed worktree on missing path", worktreeID: "not-a-uuid"},
		{name: "empty worktree on empty file", createFile: true},
		{name: "malformed worktree on empty file", createFile: true, worktreeID: "not-a-uuid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.sqlite")
			if testCase.createFile {
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			_, err := openIndex(ctx, Options{Path: path, WorktreeID: testCase.worktreeID, Create: true})
			if !errors.Is(err, ErrInvalidIndex) {
				t.Fatalf("openIndex error=%v; want ErrInvalidIndex", err)
			}
			info, statErr := os.Stat(path)
			if testCase.createFile {
				if statErr != nil {
					t.Fatalf("inspect preserved empty index: %v", statErr)
				}
				if info.Size() != 0 {
					t.Fatalf("failed open mutated empty index to %d bytes", info.Size())
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed open unexpectedly created index; stat error=%v", statErr)
			}
		})
	}
}

func TestOpenIndexMigratesV1AndPreservesIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index-v1.sqlite")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ddl, err := indexMigrations.ReadFile("migrations/index/001.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(ctx, string(ddl)); err != nil {
		t.Fatalf("create v1 fixture: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	worktreeID := testUUID(t, "33333333-3333-4333-8333-333333333333")
	store, err := openIndex(ctx, Options{Path: path, WorktreeID: worktreeID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close migrated index: %v", err)
		}
	})
	var version int64
	if err := store.readers.QueryRowContext(ctx, `SELECT schema_version FROM schema_meta WHERE singleton=1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != indexSchemaVersion {
		t.Fatalf("schema version after migration=%d; want %d", version, indexSchemaVersion)
	}
	if store.WorktreeID() != worktreeID {
		t.Fatalf("migrated index owner=%s, want %s", store.WorktreeID(), worktreeID)
	}
}

func TestOpenIndexRejectsFutureSchemaWithoutChangingFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "future-index.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001.sql", "002.sql"} {
		ddl, err := indexMigrations.ReadFile("migrations/index/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(ddl)); err != nil {
			t.Fatalf("create current schema fixture with %s: %v", name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE schema_meta SET schema_version=99,min_reader_version=99 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = openIndex(ctx, Options{Path: path, WorktreeID: testUUID(t, "44444444-4444-4444-8444-444444444444"), Create: true})
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("openIndex error=%v; want ErrSchemaTooNew", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejecting a future schema changed the SQLite file bytes")
	}
}

func TestOpenIndexRejectsNonemptyUnmanagedDatabaseWithoutMutation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "unmanaged.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE unrelated (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = openIndex(ctx, Options{Path: path, WorktreeID: testUUID(t, "55555555-5555-4555-8555-555555555555"), Create: true})
	if !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("openIndex error=%v; want ErrInvalidIndex", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejecting an unmanaged SQLite schema changed the file bytes")
	}
}

func TestCaptureManifestReferenceIsCanonicalAndBoundToGeneration(t *testing.T) {
	ctx := context.Background()
	store := openTestIndex(t, 1234)
	_, err := store.writer.ExecContext(ctx, `INSERT INTO generations (
		generation_id,profile_id,state,owner_job_id,policy_digest,policy_epoch,provider_fingerprint,created_at
	) VALUES ('gen-manifest',?,'BUILDING','job-test',?,1,?,'2026-10-16T00:00:00Z')`, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64))
	if err != nil {
		t.Fatal(err)
	}
	ref := validManifestRef()
	if err := store.putCaptureManifestRef(ctx, "gen-manifest", ref); err != nil {
		t.Fatal(err)
	}
	got, err := store.CaptureManifestRef(ctx, "gen-manifest")
	if err != nil {
		t.Fatal(err)
	}
	if got != ref {
		t.Fatalf("round trip ref=%+v; want %+v", got, ref)
	}
	if _, err := EncodeStoredCaptureManifestRef(model.StoredCaptureManifestRef{
		FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/wrong.json", ByteLength: 10,
		ArtifactSHA256: ref.ArtifactSHA256, SnapshotDigest: ref.SnapshotDigest,
	}); err == nil {
		t.Fatal("descriptor with an unbound artifact path was accepted")
	}
	if _, err := DecodeStoredCaptureManifestRef([]byte(`{"format_version":1,"format_version":1}`)); err == nil {
		t.Fatal("duplicate JSON object member was accepted")
	}
	if _, err := DecodeStoredCaptureManifestRef([]byte(`{"unknown":true}`)); err == nil {
		t.Fatal("unknown descriptor member was accepted")
	}
	if _, err := store.writer.ExecContext(ctx, `UPDATE generations SET manifest_json='{"unknown":true}' WHERE generation_id='gen-manifest'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureManifestRef(ctx, "gen-manifest"); err == nil {
		t.Fatal("database descriptor tampering was accepted on read")
	}
}

func TestCaptureManifestReadRejectsSnapshotDigestMismatch(t *testing.T) {
	ctx := context.Background()
	store := openTestIndex(t, 5000)
	ref := validManifestRef()
	_, err := store.writer.ExecContext(ctx, `INSERT INTO generations (
		generation_id,profile_id,state,owner_job_id,policy_digest,policy_epoch,provider_fingerprint,manifest_json,created_at
	) VALUES ('gen-mismatch',?,'BUILDING','job-test',?,1,?,?, '2026-10-16T00:00:00Z')`, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), string(mustEncodeRef(t, ref)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.writer.ExecContext(ctx, `UPDATE generations SET snapshot_digest=? WHERE generation_id='gen-mismatch'`, strings.Repeat("4", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureManifestRef(ctx, "gen-mismatch"); err == nil {
		t.Fatal("manifest descriptor with a different snapshot digest was accepted")
	}
}

func TestSQLiteContextDeadlineInterruptsBusyWriter(t *testing.T) {
	store := openTestIndex(t, 1234)
	ctx := context.Background()
	if _, err := store.writer.ExecContext(ctx, `CREATE TABLE lock_probe (value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	blocker, err := sql.Open("sqlite", storeDSNForTest(t, store.path, 1234))
	if err != nil {
		t.Fatal(err)
	}
	blocker.SetMaxOpenConns(1)
	blocker.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = blocker.Close() })
	tx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lock_probe(value) VALUES(1)`); err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	blocked, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = store.withWriteTx(blocked, func(tx *writeTx) error {
		_, err := tx.ExecContext(blocked, `INSERT INTO lock_probe(value) VALUES(2)`)
		return err
	})
	if err == nil {
		t.Fatal("write unexpectedly passed a conflicting transaction")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write error=%v; want deadline/cancellation evidence", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled write took %s despite a 150ms context deadline", elapsed)
	}
	if err := verifyPool(ctx, store.writer, writerPoolSize, 1234, false); err != nil {
		t.Fatalf("write pool settings after canceled transaction: %v", err)
	}
}

func openTestIndex(t *testing.T, timeout int64) *Store {
	t.Helper()
	store, err := openIndex(context.Background(), Options{
		Path: filepath.Join(t.TempDir(), "index.sqlite"), WorktreeID: testUUID(t, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		BusyTimeoutMS: timeout, Create: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close SQLite index: %v", err)
		}
	})
	return store
}

func testUUID(t *testing.T, value string) model.UUID {
	t.Helper()
	uuid, err := model.ParseUUID(value)
	if err != nil {
		t.Fatal(err)
	}
	return uuid
}

func validManifestRef() model.StoredCaptureManifestRef {
	artifact := model.Hash(strings.Repeat("a", 64))
	return model.StoredCaptureManifestRef{
		FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + string(artifact) + ".json",
		ByteLength: 64, ArtifactSHA256: artifact, SnapshotDigest: model.Hash(strings.Repeat("c", 64)),
	}
}

func mustEncodeRef(t *testing.T, ref model.StoredCaptureManifestRef) []byte {
	t.Helper()
	raw, err := EncodeStoredCaptureManifestRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func storeDSNForTest(t *testing.T, path string, timeout int64) string {
	t.Helper()
	dsn, err := makeDSN(path, "rw", timeout, false)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}
