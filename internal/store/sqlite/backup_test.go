package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

func TestOnlineSQLiteBackupCopiesSnapshotAndRefusesOverwrite(t *testing.T) {
	home := t.TempDir()
	root, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close backup test home: %v", err)
		}
	})
	worktreeID := testUUID(t, "bbaaaaaa-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err := root.EnsurePrivateDirs("worktrees/" + string(worktreeID)); err != nil {
		t.Fatal(err)
	}
	sourceRelative := "worktrees/" + string(worktreeID) + "/index.sqlite"
	destinationRelative := "worktrees/" + string(worktreeID) + "/backups/" + string(testUUID(t, "ccaaaaaa-cccc-4ccc-8ccc-cccccccccccc")) + "/index.sqlite"
	homePath, err := root.FinalPath()
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := comparableOwnedPath(filepath.Join(homePath, filepath.FromSlash(sourceRelative)))
	sourceURI, err := makeDSN(sourcePath, "rwc", 1_234, false)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB, err := sql.Open("sqlite", sourceURI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceDB.Exec(`CREATE TABLE backup_fixture(value TEXT NOT NULL); INSERT INTO backup_fixture(value) VALUES('snapshot value')`); err != nil {
		_ = sourceDB.Close()
		t.Fatalf("populate SQLite backup source: %v", err)
	}
	if err := sourceDB.Close(); err != nil {
		t.Fatal(err)
	}
	gotHash, gotBytes, err := backupSQLiteFile(context.Background(), root, sourceRelative, destinationRelative, 1_234)
	if err != nil {
		t.Fatalf("online SQLite backup: %v", err)
	}
	if _, err := model.ParseHash(string(gotHash)); err != nil || gotBytes < 1 {
		t.Fatalf("backup digest/length=%s/%d err=%v", gotHash, gotBytes, err)
	}
	backupPath := comparableOwnedPath(filepath.Join(homePath, filepath.FromSlash(destinationRelative)))
	backupDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := backupDB.QueryRow(`SELECT value FROM backup_fixture`).Scan(&value); err != nil {
		_ = backupDB.Close()
		t.Fatalf("read SQLite backup: %v", err)
	}
	if err := backupDB.Close(); err != nil {
		t.Fatal(err)
	}
	if value != "snapshot value" {
		t.Fatalf("backup value=%q; want snapshot value", value)
	}
	if _, _, err := backupSQLiteFile(context.Background(), root, sourceRelative, destinationRelative, 1_234); !errors.Is(err, mceruntime.ErrAlreadyPublished) {
		t.Fatalf("backup overwrite error=%v; want ErrAlreadyPublished", err)
	}
}

func TestBackupWorktreeCapturesConsistentStoreSetAndReferencedArtifacts(t *testing.T) {
	store := openTestIndex(t, 1_234)
	worktreeID := store.WorktreeID()
	home, err := store.ownership.root.FinalPath()
	if err != nil {
		t.Fatal(err)
	}
	home = comparableOwnedPath(home)

	manifestBytes := []byte("capture manifest backup fixture")
	manifestHash := model.SHA256(manifestBytes)
	ref := model.StoredCaptureManifestRef{
		FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + string(manifestHash) + ".json",
		ByteLength: int64(len(manifestBytes)), ArtifactSHA256: manifestHash,
		SnapshotDigest: model.SHA256([]byte("snapshot for backup")),
	}
	if err := store.ownership.root.EnsurePrivateDirs("worktrees/" + string(worktreeID) + "/manifests"); err != nil {
		t.Fatal(err)
	}
	if err := store.ownership.root.WriteAtomicFile("worktrees/"+string(worktreeID)+"/"+ref.Path, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testGenerationSpec(t, "bbaaaaaa-bbbb-4bbb-8bbb-bbbbbbbbbbbc", "ccaaaaaa-cccc-4ccc-8ccc-cccccccccccd", "a")
	spec.SnapshotDigest = ref.SnapshotDigest
	spec.ManifestRef = ref
	if _, err := store.CreateGeneration(context.Background(), spec); err != nil {
		t.Fatalf("create generation bound to backup artifact: %v", err)
	}
	if _, err := store.PublishGeneration(context.Background(), PublishRequest{Spec: spec, State: GenerationReady}); err != nil {
		t.Fatalf("publish generation bound to backup artifact: %v", err)
	}
	oldIndexIncarnation := store.Incarnation()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recordsPath := filepath.Join(home, "worktrees", string(worktreeID), "records.sqlite")
	recordsURI, err := makeDSN(recordsPath, "rwc", 1_234, false)
	if err != nil {
		t.Fatal(err)
	}
	recordsDB, err := sql.Open("sqlite", recordsURI)
	if err != nil {
		t.Fatal(err)
	}
	recordsDDL, err := os.ReadFile(filepath.Join("migrations", "records", "001.sql"))
	if err != nil {
		_ = recordsDB.Close()
		t.Fatal(err)
	}
	if _, err := recordsDB.ExecContext(context.Background(), string(recordsDDL)); err != nil {
		_ = recordsDB.Close()
		t.Fatalf("create records fixture schema: %v", err)
	}
	recordsIncarnation := testUUID(t, "ddaaaaaa-dddd-4ddd-8ddd-dddddddddddd")
	if _, err := recordsDB.ExecContext(context.Background(), `INSERT INTO records_meta(singleton,worktree_id,incarnation,record_epoch) VALUES(1,?,?,7)`, worktreeID, recordsIncarnation); err != nil {
		_ = recordsDB.Close()
		t.Fatalf("bind records fixture identity: %v", err)
	}
	if err := recordsDB.Close(); err != nil {
		t.Fatal(err)
	}

	manifest, err := BackupWorktree(context.Background(), WorktreeBackupOptions{
		HomePath: home, WorktreeID: worktreeID, BusyTimeoutMS: 1_234,
		AdmissionConfig: testAdmissionConfig(), LockWaitMS: 500,
	})
	if err != nil {
		t.Fatalf("backup worktree store set: %v", err)
	}
	if manifest.Consistency != "quiesced_worktree_set" || len(manifest.Stores) != 2 || len(manifest.Artifacts) != 1 ||
		manifest.RecordEpoch != 7 || manifest.RecordVersionCount != 0 {
		t.Fatalf("worktree backup manifest=%+v; want a paired snapshot and one artifact", manifest)
	}
	if manifest.Stores[0].Kind != "index" || manifest.Stores[1].Kind != "records" {
		t.Fatalf("backup store order=%+v; want index then records", manifest.Stores)
	}
	backupRoot := "worktrees/" + string(worktreeID) + "/backups/" + string(manifest.OperationID)
	backupIndex := filepath.Join(home, filepath.FromSlash(backupRoot+"/index.sqlite"))
	backupIndexURI, err := makeDSN(backupIndex, "ro", 1_234, true)
	if err != nil {
		t.Fatal(err)
	}
	backupIndexDB, err := sql.Open("sqlite", backupIndexURI)
	if err != nil {
		t.Fatal(err)
	}
	indexIdentity, err := readIdentity(context.Background(), backupIndexDB)
	if err != nil {
		_ = backupIndexDB.Close()
		t.Fatalf("read backed-up index identity: %v", err)
	}
	if err := backupIndexDB.Close(); err != nil {
		t.Fatal(err)
	}
	if indexIdentity.worktree != worktreeID || indexIdentity.incarnation != manifest.Stores[0].Incarnation {
		t.Fatalf("backup identity=%+v; want worktree %s and manifest incarnation %s", indexIdentity, worktreeID, manifest.Stores[0].Incarnation)
	}
	artifactPath := filepath.Join(home, filepath.FromSlash(backupRoot+"/"+manifest.Artifacts[0].BackupPath))
	artifactBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read backed-up capture manifest: %v", err)
	}
	if !bytes.Equal(artifactBytes, manifestBytes) || manifest.Artifacts[0].SHA256 != manifestHash {
		t.Fatalf("backup artifact bytes or digest differ: %q / %s", artifactBytes, manifest.Artifacts[0].SHA256)
	}
	manifestPath := filepath.Join(home, filepath.FromSlash(backupRoot+"/manifest.json"))
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read backup manifest: %v", err)
	}
	var decoded WorktreeBackupManifest
	if err := json.Unmarshal(manifestRaw, &decoded); err != nil {
		t.Fatalf("decode backup manifest: %v", err)
	}
	if decoded.OperationID != manifest.OperationID || decoded.Consistency != manifest.Consistency || len(decoded.Stores) != 2 {
		t.Fatalf("persisted manifest differs from returned manifest: %+v", decoded)
	}

	changedIndexIncarnation := testUUID(t, "eeaaaaaa-eeee-4eee-8eee-eeeeeeeeeeee")
	changedRecordsIncarnation := testUUID(t, "ffaaaaaa-ffff-4fff-8fff-ffffffffffff")
	indexDB, err := sql.Open("sqlite", filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := indexDB.ExecContext(context.Background(), `UPDATE store_identity SET incarnation=? WHERE singleton=1`, changedIndexIncarnation); err != nil {
		_ = indexDB.Close()
		t.Fatalf("change live index incarnation fixture: %v", err)
	}
	if err := indexDB.Close(); err != nil {
		t.Fatal(err)
	}
	recordsDB, err = sql.Open("sqlite", filepath.Join(home, "worktrees", string(worktreeID), "records.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordsDB.ExecContext(context.Background(), `UPDATE records_meta SET incarnation=?,record_epoch=99 WHERE singleton=1`, changedRecordsIncarnation); err != nil {
		_ = recordsDB.Close()
		t.Fatalf("change live records identity fixture: %v", err)
	}
	if err := recordsDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "worktrees", string(worktreeID), filepath.FromSlash(ref.Path))); err != nil {
		t.Fatalf("remove disposable live capture manifest before restore: %v", err)
	}
	backupManifestPath := filepath.Join(home, filepath.FromSlash(backupRoot+"/manifest.json"))
	restored, err := RestoreWorktree(context.Background(), WorktreeRestoreOptions{
		HomePath: home, ManifestPath: backupManifestPath, WorktreeID: worktreeID,
		Confirm: true, BusyTimeoutMS: 1_234, LockWaitMS: 500, AdmissionConfig: testAdmissionConfig(),
	})
	if err != nil {
		t.Fatalf("restore worktree backup: %v", err)
	}
	if restored.Phase != "VERIFIED" || restored.Outcome != "restored" || restored.WorktreeID != worktreeID || len(restored.Stores) != 2 {
		t.Fatalf("worktree restore result=%+v", restored)
	}
	restoredStore, err := OpenIndexReadOnly(context.Background(), Options{
		Path: filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite"), HomePath: home, WorktreeID: worktreeID,
	})
	if err != nil {
		t.Fatalf("open restored index read-only: %v", err)
	}
	if restoredStore.Incarnation() == oldIndexIncarnation || restoredStore.Incarnation() == changedIndexIncarnation {
		_ = restoredStore.Close()
		t.Fatalf("restored index reused an old incarnation: %s", restoredStore.Incarnation())
	}
	var restoredGenerations int
	if err := restoredStore.ReadDB().QueryRowContext(context.Background(), `SELECT count(*) FROM generations`).Scan(&restoredGenerations); err != nil || restoredGenerations != 1 {
		_ = restoredStore.Close()
		t.Fatalf("restored generation count=%d err=%v; want 1", restoredGenerations, err)
	}
	if err := restoredStore.Close(); err != nil {
		t.Fatal(err)
	}
	restoredRecords := comparableOwnedPath(filepath.Join(home, "worktrees", string(worktreeID), "records.sqlite"))
	restoredRecordsDB, err := sql.Open("sqlite", restoredRecords)
	if err != nil {
		t.Fatal(err)
	}
	var restoredRecordsIncarnation model.UUID
	var restoredEpoch int64
	if err := restoredRecordsDB.QueryRowContext(context.Background(), `SELECT incarnation,record_epoch FROM records_meta WHERE singleton=1`).Scan(&restoredRecordsIncarnation, &restoredEpoch); err != nil {
		_ = restoredRecordsDB.Close()
		t.Fatal(err)
	}
	if err := restoredRecordsDB.Close(); err != nil {
		t.Fatal(err)
	}
	if restoredRecordsIncarnation == recordsIncarnation || restoredRecordsIncarnation == changedRecordsIncarnation || restoredEpoch != 7 {
		t.Fatalf("restored records identity/epoch=%s/%d; want a new incarnation and epoch 7", restoredRecordsIncarnation, restoredEpoch)
	}
	restoredArtifact, err := os.ReadFile(filepath.Join(home, "worktrees", string(worktreeID), filepath.FromSlash(ref.Path)))
	if err != nil || !bytes.Equal(restoredArtifact, manifestBytes) {
		t.Fatalf("restored capture artifact=%q err=%v", restoredArtifact, err)
	}
	maintenanceRoot, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := "worktrees/" + string(worktreeID) + "/maintenance/" + string(restored.OperationID) + "/restore-journal.json"
	journal, err := readWorktreeRestoreJournal(maintenanceRoot, journalPath, restored.OperationID, worktreeID)
	if err != nil {
		_ = maintenanceRoot.Close()
		t.Fatal(err)
	}
	if err := writeRestoreJournal(maintenanceRoot, journalPath, &journal, "NEEDS_RECOVERY"); err != nil {
		_ = maintenanceRoot.Close()
		t.Fatalf("simulate interrupted restore journal: %v", err)
	}
	markerPath := "worktrees/" + string(worktreeID) + "/maintenance/restore-active.json"
	markerRaw, err := encodeRestoreActiveMarker(restoreActiveMarker{
		FormatVersion: 1, OperationID: restored.OperationID, WorktreeID: worktreeID, JournalPath: journalPath,
	})
	if err != nil {
		_ = maintenanceRoot.Close()
		t.Fatal(err)
	}
	markerResult, markerErr := maintenanceRoot.PublishAtomicFile(markerPath, markerRaw, 0o600)
	if !markerResult.Published || markerErr != nil && !errors.Is(markerErr, mceruntime.ErrDirectoryFlushUnsupported) {
		_ = maintenanceRoot.Close()
		t.Fatalf("publish simulated recovery gate: result=%+v err=%v", markerResult, markerErr)
	}
	if err := maintenanceRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if blocked, err := OpenIndexReadOnly(context.Background(), Options{
		Path: filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite"), HomePath: home, WorktreeID: worktreeID,
	}); !errors.Is(err, ErrStoreRecoveryRequired) {
		if blocked != nil {
			_ = blocked.Close()
		}
		t.Fatalf("store open while recovery is pending: store=%v err=%v", blocked, err)
	}
	recovered, err := RecoverWorktreeRestore(context.Background(), WorktreeRestoreRecoveryOptions{
		HomePath: home, WorktreeID: worktreeID, Apply: true, Confirm: true,
		LockWaitMS: 500, BusyTimeoutMS: 1_234, AdmissionConfig: testAdmissionConfig(),
	})
	if err != nil || !recovered.Applied || recovered.Pending || recovered.Phase != "VERIFIED" {
		t.Fatalf("apply restore recovery: report=%+v err=%v", recovered, err)
	}
	recoveryReport, err := InspectWorktreeRestore(context.Background(), home, worktreeID, time.Second)
	if err != nil || recoveryReport.Pending {
		t.Fatalf("completed restore recovery report=%+v err=%v", recoveryReport, err)
	}
	maintenanceRoot, err = mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	journal, err = readWorktreeRestoreJournal(maintenanceRoot, journalPath, restored.OperationID, worktreeID)
	if err != nil {
		_ = maintenanceRoot.Close()
		t.Fatal(err)
	}
	if err := writeRestoreJournal(maintenanceRoot, journalPath, &journal, "NEEDS_RECOVERY"); err != nil {
		_ = maintenanceRoot.Close()
		t.Fatalf("simulate interrupted replacement: %v", err)
	}
	rootPath, err := maintenanceRoot.FinalPath()
	if err != nil {
		_ = maintenanceRoot.Close()
		t.Fatal(err)
	}
	stagePath := filepath.Join(rootPath, "worktrees", string(worktreeID), "maintenance", string(restored.OperationID), "stage-index.sqlite")
	stageBytes, err := os.ReadFile(stagePath)
	if err != nil {
		_ = maintenanceRoot.Close()
		t.Fatal(err)
	}
	stageBytes[len(stageBytes)-1] ^= 0xff
	if err := os.WriteFile(stagePath, stageBytes, 0o600); err != nil {
		_ = maintenanceRoot.Close()
		t.Fatal(err)
	}
	markerResult, markerErr = maintenanceRoot.PublishAtomicFile(markerPath, markerRaw, 0o600)
	if !markerResult.Published || markerErr != nil && !errors.Is(markerErr, mceruntime.ErrDirectoryFlushUnsupported) {
		_ = maintenanceRoot.Close()
		t.Fatalf("publish rollback recovery gate: result=%+v err=%v", markerResult, markerErr)
	}
	if err := maintenanceRoot.Close(); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := RecoverWorktreeRestore(context.Background(), WorktreeRestoreRecoveryOptions{
		HomePath: home, WorktreeID: worktreeID, Apply: true, Confirm: true,
		LockWaitMS: 500, BusyTimeoutMS: 1_234, AdmissionConfig: testAdmissionConfig(),
	})
	if err != nil || !rolledBack.Applied || rolledBack.Pending || rolledBack.Outcome != "rolled_back" {
		t.Fatalf("restore rollback recovery: report=%+v err=%v", rolledBack, err)
	}
	rolledBackIndex, err := OpenIndexReadOnly(context.Background(), Options{
		Path: filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite"), HomePath: home, WorktreeID: worktreeID,
	})
	if err != nil {
		t.Fatalf("open rolled-back index: %v", err)
	}
	if rolledBackIndex.Incarnation() == changedIndexIncarnation || rolledBackIndex.Incarnation() == restoredStore.Incarnation() {
		_ = rolledBackIndex.Close()
		t.Fatalf("rollback reused an old index incarnation: %s", rolledBackIndex.Incarnation())
	}
	if err := rolledBackIndex.Close(); err != nil {
		t.Fatal(err)
	}
	rolledBackRecordsDB, err := sql.Open("sqlite", restoredRecords)
	if err != nil {
		t.Fatal(err)
	}
	var rolledBackEpoch int64
	if err := rolledBackRecordsDB.QueryRowContext(context.Background(), `SELECT record_epoch FROM records_meta WHERE singleton=1`).Scan(&rolledBackEpoch); err != nil {
		_ = rolledBackRecordsDB.Close()
		t.Fatal(err)
	}
	if err := rolledBackRecordsDB.Close(); err != nil {
		t.Fatal(err)
	}
	if rolledBackEpoch != 99 {
		t.Fatalf("rollback records epoch=%d; want the pre-restore epoch 99", rolledBackEpoch)
	}
}
