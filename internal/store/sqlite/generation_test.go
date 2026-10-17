package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestGenerationPublishSwitchesPointerAtomicallyAndIsIdempotent(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	first := testGenerationSpec(t, "10000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000001", "a")
	second := testGenerationSpec(t, "10000000-0000-4000-8000-000000000002", "20000000-0000-4000-8000-000000000002", "b")
	second.Key.ProfileID = first.Key.ProfileID

	if _, err := store.CreateGeneration(ctx, first); err != nil {
		t.Fatalf("create first generation: %v", err)
	}
	if err := stageGenerationMarker(ctx, store, first); err != nil {
		t.Fatalf("stage first generation: %v", err)
	}
	firstResult, err := store.PublishGeneration(ctx, PublishRequest{Spec: first, State: GenerationReady})
	if err != nil {
		t.Fatalf("publish first generation: %v", err)
	}
	if !firstResult.Activated || firstResult.Generation.State != GenerationReady {
		t.Fatalf("first publish result=%+v; want READY and active", firstResult)
	}

	if _, err := store.CreateGeneration(ctx, second); err != nil {
		t.Fatalf("create second generation: %v", err)
	}
	if err := stageGenerationMarker(ctx, store, second); err != nil {
		t.Fatalf("stage second generation: %v", err)
	}
	if _, err := store.writer.ExecContext(ctx, `CREATE TRIGGER t008_fail_active_update
		BEFORE UPDATE ON active_generations WHEN new.generation_id='`+string(second.Key.GenerationID)+`'
		BEGIN SELECT RAISE(ABORT,'injected publish failure'); END`); err != nil {
		t.Fatalf("install active-pointer fault: %v", err)
	}
	_, err = store.PublishGeneration(ctx, PublishRequest{Spec: second, State: GenerationReady})
	if err == nil {
		t.Fatal("publish unexpectedly passed the injected active-pointer fault")
	}
	resolution, resolveErr := store.ResolvePublication(ctx, second.Key)
	if resolveErr != nil {
		t.Fatalf("resolve failed publication: %v", resolveErr)
	}
	if resolution.Generation.State != GenerationBuilding || resolution.Active {
		t.Fatalf("failed candidate resolution=%+v; want BUILDING and inactive", resolution)
	}
	if active := readActiveGeneration(t, store, first.Key.ProfileID); active != first.Key.GenerationID {
		t.Fatalf("active generation after rolled-back publish=%s; want %s", active, first.Key.GenerationID)
	}
	if _, err := store.writer.ExecContext(ctx, `DROP TRIGGER t008_fail_active_update`); err != nil {
		t.Fatalf("remove active-pointer fault: %v", err)
	}
	secondResult, err := store.PublishGeneration(ctx, PublishRequest{Spec: second, State: GenerationReady})
	if err != nil {
		t.Fatalf("publish second generation after fault removal: %v", err)
	}
	if !secondResult.Activated || readActiveGeneration(t, store, first.Key.ProfileID) != second.Key.GenerationID {
		t.Fatalf("second publish did not atomically switch active pointer: %+v", secondResult)
	}
	retry, err := store.PublishGeneration(ctx, PublishRequest{Spec: second, State: GenerationReady})
	if err != nil {
		t.Fatalf("idempotent publish retry: %v", err)
	}
	if !retry.AlreadyPublished || !retry.Activated {
		t.Fatalf("retry result=%+v; want already-published active receipt", retry)
	}
	assertSQLiteIntegrity(t, store)
}

func TestGenerationCommitFailureRollsBackStateAndActivePointer(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	first := testGenerationSpec(t, "11000000-0000-4000-8000-000000000001", "21000000-0000-4000-8000-000000000001", "c")
	second := testGenerationSpec(t, "11000000-0000-4000-8000-000000000002", "21000000-0000-4000-8000-000000000002", "d")
	second.Key.ProfileID = first.Key.ProfileID
	for _, spec := range []GenerationSpec{first, second} {
		if _, err := store.CreateGeneration(ctx, spec); err != nil {
			t.Fatalf("create generation %s: %v", spec.Key.GenerationID, err)
		}
		if err := stageGenerationMarker(ctx, store, spec); err != nil {
			t.Fatalf("stage generation %s: %v", spec.Key.GenerationID, err)
		}
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: first, State: GenerationReady}); err != nil {
		t.Fatalf("publish initial generation: %v", err)
	}
	if _, err := store.writer.ExecContext(ctx, `CREATE TABLE t008_commit_parent(id TEXT PRIMARY KEY) STRICT`); err != nil {
		t.Fatalf("create deferred-constraint parent: %v", err)
	}
	if _, err := store.writer.ExecContext(ctx, `CREATE TABLE t008_commit_child(parent_id TEXT NOT NULL REFERENCES t008_commit_parent(id) DEFERRABLE INITIALLY DEFERRED) STRICT`); err != nil {
		t.Fatalf("create deferred-constraint child: %v", err)
	}
	trigger := `CREATE TRIGGER t008_fail_commit AFTER UPDATE OF state ON generations
		WHEN new.generation_id='` + string(second.Key.GenerationID) + `'
		BEGIN INSERT INTO t008_commit_child(parent_id) VALUES('missing-parent'); END`
	if _, err := store.writer.ExecContext(ctx, trigger); err != nil {
		t.Fatalf("install deferred commit fault: %v", err)
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: second, State: GenerationReady}); err == nil {
		t.Fatal("publish unexpectedly passed the injected deferred-commit fault")
	}
	resolution, err := store.ResolvePublication(ctx, second.Key)
	if err != nil {
		t.Fatalf("resolve generation after failed COMMIT: %v", err)
	}
	if resolution.Generation.State != GenerationBuilding || resolution.Active {
		t.Fatalf("post-commit-fault resolution=%+v; want BUILDING and inactive", resolution)
	}
	if active := readActiveGeneration(t, store, first.Key.ProfileID); active != first.Key.GenerationID {
		t.Fatalf("active generation after failed COMMIT=%s; want %s", active, first.Key.GenerationID)
	}
	assertSQLiteIntegrity(t, store)
}

func TestGenerationBatchFailureRollsBackStagedFacts(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	spec := testGenerationSpec(t, "11500000-0000-4000-8000-000000000001", "21500000-0000-4000-8000-000000000001", "7")
	if _, err := store.CreateGeneration(ctx, spec); err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	wantErr := errors.New("injected batch fault")
	err := store.WriteGenerationBatch(ctx, spec.Key, manifestWriteOverhead, func(batch *GenerationBatch) error {
		if _, err := batch.Exec(`INSERT INTO files(
			generation_id,path,content_hash,kind,status,byte_length,package_key,generated,reason_code
		) VALUES(?, 'partial-marker', NULL, 'test_marker', 'missing', NULL, NULL, 0, 'test_missing')`,
			string(spec.Key.GenerationID)); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("staging batch error=%v; want injected error", err)
	}
	var count int
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM files WHERE generation_id=?`, string(spec.Key.GenerationID)).Scan(&count); err != nil {
		t.Fatalf("count staged files after rollback: %v", err)
	}
	if count != 0 {
		t.Fatalf("staged file count after rollback=%d; want 0", count)
	}
	assertSQLiteIntegrity(t, store)
}

func TestGenerationWriterLeaseCoversMultipleShortBatches(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	first := testGenerationSpec(t, "11600000-0000-4000-8000-000000000001", "21600000-0000-4000-8000-000000000001", "8")
	second := testGenerationSpec(t, "11600000-0000-4000-8000-000000000002", "21600000-0000-4000-8000-000000000002", "9")
	second.Key.ProfileID = first.Key.ProfileID
	started := make(chan struct{})
	release := make(chan struct{})
	operationDone := make(chan error, 1)
	var leasedContext context.Context
	go func() {
		operationDone <- store.WithGenerationWriter(ctx, func(writerCtx context.Context) error {
			leasedContext = writerCtx
			if _, err := store.CreateGeneration(writerCtx, first); err != nil {
				return err
			}
			if err := stageGenerationMarker(writerCtx, store, first); err != nil {
				return err
			}
			close(started)
			select {
			case <-release:
				return nil
			case <-time.After(2 * time.Second):
				return errors.New("test writer lease release timed out")
			}
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("generation writer did not stage its short batches")
	}
	blockedCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := store.CreateGeneration(blockedCtx, second); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		<-operationDone
		t.Fatalf("concurrent generation create error=%v; want writer-lease deadline", err)
	}
	close(release)
	if err := <-operationDone; err != nil {
		t.Fatalf("finish generation writer: %v", err)
	}
	if leasedContext == nil {
		t.Fatal("generation writer did not supply its scoped context")
	}
	if _, err := store.CreateGeneration(leasedContext, second); err != nil {
		t.Fatalf("escaped lease context did not reacquire a fresh lease: %v", err)
	}
}

func TestGenerationOwnerCASCancellationAndRollbackPreserveActive(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	active := testGenerationSpec(t, "12000000-0000-4000-8000-000000000001", "22000000-0000-4000-8000-000000000001", "e")
	candidate := testGenerationSpec(t, "12000000-0000-4000-8000-000000000002", "22000000-0000-4000-8000-000000000002", "f")
	candidate.Key.ProfileID = active.Key.ProfileID
	for _, spec := range []GenerationSpec{active, candidate} {
		if _, err := store.CreateGeneration(ctx, spec); err != nil {
			t.Fatalf("create generation: %v", err)
		}
		if err := stageGenerationMarker(ctx, store, spec); err != nil {
			t.Fatalf("stage generation: %v", err)
		}
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: active, State: GenerationReady}); err != nil {
		t.Fatalf("publish initial generation: %v", err)
	}
	wrongOwner := candidate
	wrongOwner.Key.OwnerJobID = model.UUID("32000000-0000-4000-8000-000000000009")
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: wrongOwner, State: GenerationReady}); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("wrong-owner publish error=%v; want ErrGenerationConflict", err)
	}
	wrongProfile := candidate
	wrongProfile.Key.ProfileID = model.Hash(strings.Repeat("9", 64))
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: wrongProfile, State: GenerationReady}); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("wrong-profile publish error=%v; want ErrGenerationConflict", err)
	}
	if err := store.AbortGeneration(ctx, wrongOwner.Key, GenerationCancelled); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("wrong-owner cancel error=%v; want ErrGenerationConflict", err)
	}
	if err := store.AbortGeneration(ctx, candidate.Key, GenerationCancelled); err != nil {
		t.Fatalf("cancel exact candidate: %v", err)
	}
	if err := store.AbortGeneration(ctx, candidate.Key, GenerationCancelled); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: candidate, State: GenerationReady}); !errors.Is(err, ErrGenerationState) {
		t.Fatalf("publish canceled candidate error=%v; want ErrGenerationState", err)
	}
	if activeID := readActiveGeneration(t, store, active.Key.ProfileID); activeID != active.Key.GenerationID {
		t.Fatalf("active pointer changed after owner/cancel failure: %s", activeID)
	}
}

func TestPublishedSnapshotRemainsConsistentWhileWriterAdvances(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	first := testGenerationSpec(t, "13000000-0000-4000-8000-000000000001", "23000000-0000-4000-8000-000000000001", "1")
	second := testGenerationSpec(t, "13000000-0000-4000-8000-000000000002", "23000000-0000-4000-8000-000000000002", "2")
	second.Key.ProfileID = first.Key.ProfileID
	for _, spec := range []GenerationSpec{first, second} {
		if _, err := store.CreateGeneration(ctx, spec); err != nil {
			t.Fatalf("create generation: %v", err)
		}
		if err := stageGenerationMarker(ctx, store, spec); err != nil {
			t.Fatalf("stage generation: %v", err)
		}
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: first, State: GenerationReady}); err != nil {
		t.Fatalf("publish first generation: %v", err)
	}
	resolved := make(chan model.UUID, 1)
	continueRead := make(chan struct{})
	readResult := make(chan error, 1)
	go func() {
		readResult <- store.WithPublishedSnapshot(ctx, first.Key.ProfileID, "", func(snapshot *ReadSnapshot) error {
			resolved <- snapshot.Generation.Key.GenerationID
			<-continueRead
			var activeID string
			row, err := snapshot.QueryRowContext(ctx, `SELECT generation_id FROM active_generations WHERE profile_id=?`, string(first.Key.ProfileID))
			if err != nil {
				return err
			}
			if err := row.Scan(&activeID); err != nil {
				return err
			}
			var marker string
			row, err = snapshot.QueryRowContext(ctx, `SELECT path FROM files WHERE generation_id=? AND path=?`,
				string(snapshot.Generation.Key.GenerationID), string(snapshot.Generation.Key.GenerationID))
			if err != nil {
				return err
			}
			if err := row.Scan(&marker); err != nil {
				return err
			}
			if model.UUID(activeID) != snapshot.Generation.Key.GenerationID || marker != activeID {
				return fmt.Errorf("mixed generation snapshot: pointer=%s marker=%s pinned=%s", activeID, marker, snapshot.Generation.Key.GenerationID)
			}
			return nil
		})
	}()
	select {
	case id := <-resolved:
		if id != first.Key.GenerationID {
			close(continueRead)
			t.Fatalf("reader pinned %s; want %s", id, first.Key.GenerationID)
		}
	case <-time.After(2 * time.Second):
		close(continueRead)
		t.Fatal("reader did not resolve the active generation")
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: second, State: GenerationReady}); err != nil {
		close(continueRead)
		t.Fatalf("advance active generation while reader is open: %v", err)
	}
	close(continueRead)
	select {
	case err := <-readResult:
		if err != nil {
			t.Fatalf("pinned read observed inconsistent generation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pinned read did not finish")
	}
	if active := readActiveGeneration(t, store, first.Key.ProfileID); active != second.Key.GenerationID {
		t.Fatalf("current pointer=%s; want second generation %s", active, second.Key.GenerationID)
	}
}

func TestPublishedSnapshotNeverExposesBuildingGeneration(t *testing.T) {
	store := openTestIndex(t, 1_234)
	spec := testGenerationSpec(t, "14000000-0000-4000-8000-000000000001", "24000000-0000-4000-8000-000000000001", "3")
	if _, err := store.CreateGeneration(context.Background(), spec); err != nil {
		t.Fatalf("create BUILDING generation: %v", err)
	}
	if err := store.WithPublishedSnapshot(context.Background(), spec.Key.ProfileID, "", func(*ReadSnapshot) error {
		t.Fatal("BUILDING generation reached the reader callback")
		return nil
	}); !errors.Is(err, ErrGenerationUnavailable) {
		t.Fatalf("read BUILDING generation error=%v; want ErrGenerationUnavailable", err)
	}
	if err := store.WithPublishedSnapshot(context.Background(), spec.Key.ProfileID, spec.Key.GenerationID, func(*ReadSnapshot) error {
		t.Fatal("explicit BUILDING generation reached the reader callback")
		return nil
	}); !errors.Is(err, ErrGenerationUnavailable) {
		t.Fatalf("read explicit BUILDING generation error=%v; want ErrGenerationUnavailable", err)
	}
}

func TestContinuousReadersStayGenerationConsistentAcrossRepeatedPublishes(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	stopReaders := make(chan struct{})
	first := testGenerationSpec(t, "15000000-0000-4000-8000-000000000001", "25000000-0000-4000-8000-000000000001", "0")
	if _, err := store.CreateGeneration(ctx, first); err != nil {
		t.Fatalf("create first generation: %v", err)
	}
	if err := stageGenerationMarker(ctx, store, first); err != nil {
		t.Fatalf("stage first generation: %v", err)
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: first, State: GenerationReady}); err != nil {
		t.Fatalf("publish first generation: %v", err)
	}

	const readerCount = 3
	started := make(chan struct{}, readerCount)
	failures := make(chan error, readerCount)
	var readers sync.WaitGroup
	for range readerCount {
		readers.Add(1)
		go func() {
			defer readers.Done()
			started <- struct{}{}
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				err := store.WithPublishedSnapshot(ctx, first.Key.ProfileID, "", func(snapshot *ReadSnapshot) error {
					id := string(snapshot.Generation.Key.GenerationID)
					var marker string
					row, err := snapshot.QueryRowContext(ctx, `SELECT path FROM files WHERE generation_id=? AND path=?`, id, id)
					if err != nil {
						return err
					}
					if err := row.Scan(&marker); err != nil {
						return err
					}
					if marker != id {
						return fmt.Errorf("reader mixed generation %s with marker %s", id, marker)
					}
					if snapshot.Generation.State != GenerationReady && snapshot.Generation.State != GenerationReadyPartial {
						return fmt.Errorf("reader observed unpublished state %s", snapshot.Generation.State)
					}
					return nil
				})
				if err != nil {
					select {
					case failures <- err:
					default:
					}
					return
				}
			}
		}()
	}
	for range readerCount {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(stopReaders)
			readers.Wait()
			t.Fatal("readers did not start")
		}
	}
	for i := 2; i <= 6; i++ {
		generationID := fmt.Sprintf("15000000-0000-4000-8000-%012d", i)
		ownerID := fmt.Sprintf("25000000-0000-4000-8000-%012d", i)
		spec := testGenerationSpec(t, generationID, ownerID, fmt.Sprintf("%x", i))
		spec.Key.ProfileID = first.Key.ProfileID
		if _, err := store.CreateGeneration(ctx, spec); err != nil {
			close(stopReaders)
			readers.Wait()
			t.Fatalf("create generation %d: %v", i, err)
		}
		if err := stageGenerationMarker(ctx, store, spec); err != nil {
			close(stopReaders)
			readers.Wait()
			t.Fatalf("stage generation %d: %v", i, err)
		}
		if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: spec, State: GenerationReady}); err != nil {
			close(stopReaders)
			readers.Wait()
			t.Fatalf("publish generation %d: %v", i, err)
		}
		select {
		case err := <-failures:
			close(stopReaders)
			readers.Wait()
			t.Fatalf("reader consistency failure: %v", err)
		default:
		}
	}
	close(stopReaders)
	readers.Wait()
	select {
	case err := <-failures:
		t.Fatalf("reader consistency failure: %v", err)
	default:
	}
	assertSQLiteIntegrity(t, store)
}

func testGenerationSpec(t *testing.T, generationID, ownerID, hashByte string) GenerationSpec {
	t.Helper()
	snapshotDigest := model.Hash(strings.Repeat(hashByte, 64))
	manifestHash := model.Hash(strings.Repeat("a", 64))
	if hashByte == "a" {
		manifestHash = model.Hash(strings.Repeat("b", 64))
	}
	return GenerationSpec{
		Key: GenerationKey{
			GenerationID: model.UUID(generationID),
			ProfileID:    model.Hash(strings.Repeat("f", 64)),
			OwnerJobID:   model.UUID(ownerID),
		},
		SnapshotDigest:      snapshotDigest,
		PolicyDigest:        model.Hash(strings.Repeat("d", 64)),
		PolicyEpoch:         1,
		ProviderFingerprint: model.Hash(strings.Repeat("e", 64)),
		ManifestRef: model.StoredCaptureManifestRef{
			FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + string(manifestHash) + ".json",
			ByteLength: 128, ArtifactSHA256: manifestHash, SnapshotDigest: snapshotDigest,
		},
	}
}

func stageGenerationMarker(ctx context.Context, store *Store, spec GenerationSpec) error {
	return store.WriteGenerationBatch(ctx, spec.Key, manifestWriteOverhead, func(batch *GenerationBatch) error {
		_, err := batch.Exec(`INSERT INTO files(
			generation_id,path,content_hash,kind,status,byte_length,package_key,generated,reason_code
		) VALUES(?, ?, NULL, 'test_marker', 'missing', NULL, NULL, 0, 'test_missing')`,
			string(spec.Key.GenerationID), string(spec.Key.GenerationID))
		return err
	})
}

func readActiveGeneration(t *testing.T, store *Store, profileID model.Hash) model.UUID {
	t.Helper()
	var active model.UUID
	err := store.WithPublishedSnapshot(context.Background(), profileID, "", func(snapshot *ReadSnapshot) error {
		active = snapshot.Generation.Key.GenerationID
		return nil
	})
	if err != nil {
		t.Fatalf("read active generation: %v", err)
	}
	return active
}

func assertSQLiteIntegrity(t *testing.T, store *Store) {
	t.Helper()
	var integrity string
	if err := store.ReadDB().QueryRowContext(context.Background(), `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("SQLite integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("SQLite integrity_check=%q; want ok", integrity)
	}
	rows, err := store.ReadDB().QueryContext(context.Background(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("SQLite foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, rowID, parent, foreignKeyID any
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			t.Fatalf("scan SQLite foreign_key_check: %v", err)
		}
		t.Fatalf("SQLite foreign_key_check found violation table=%v row=%v parent=%v fk=%v", table, rowID, parent, foreignKeyID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("finish SQLite foreign_key_check: %v", err)
	}
}
