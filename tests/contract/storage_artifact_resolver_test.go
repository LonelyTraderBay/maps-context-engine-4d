package contract_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestStoredCaptureManifestDescriptorMatchesPublishedFile(t *testing.T) {
	const defaultBusyTimeoutMS = 5000
	db, _ := openStorageDB(t, "manifest-reference", defaultBusyTimeoutMS)
	applyStorageMigration(t, db, "index/001.sql")

	artifact := []byte(`{"files":[],"schema_version":1}`)
	artifactHash := model.SHA256(artifact)
	snapshotDigest := model.SHA256([]byte("logical capture identity"))
	if artifactHash == snapshotDigest {
		t.Fatal("fixture must distinguish raw artifact hash from logical snapshot digest")
	}
	ref := model.StoredCaptureManifestRef{
		FormatVersion:  1,
		Kind:           "external_capture_manifest",
		Path:           "manifests/" + string(artifactHash) + ".json",
		ByteLength:     int64(len(artifact)),
		ArtifactSHA256: artifactHash,
		SnapshotDigest: snapshotDigest,
	}
	if err := ref.Validate(); err != nil {
		t.Fatalf("valid stored manifest reference: %v", err)
	}
	descriptor, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptor) > 4096 {
		t.Fatalf("fixture descriptor is %d bytes; limit is 4096", len(descriptor))
	}
	root := t.TempDir()
	worktreeRoot := filepath.Join(root, "worktree")
	artifactPath := filepath.Join(worktreeRoot, filepath.FromSlash(ref.Path))
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	storedBytes, err := os.ReadFile(artifactPath)
	if err != nil || model.SHA256(storedBytes) != artifactHash {
		t.Fatalf("published raw artifact digest mismatch: err=%v", err)
	}

	addBuildingGeneration(t, db, "gen-manifest", strings.Repeat("a", 64))
	// The verified artifact is already present before the one-statement SQL publish commit.
	result, err := db.Exec(`UPDATE generations
		SET state='READY',snapshot_digest=?,manifest_json=?,completed_at='2026-10-16T00:00:01Z'
		WHERE generation_id='gen-manifest' AND state='BUILDING'`, string(ref.SnapshotDigest), string(descriptor))
	if err != nil {
		t.Fatalf("publish scalar descriptor after file verification: %v", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		t.Fatalf("manifest publish affected=%d err=%v", changed, err)
	}
	var persisted string
	var snapshot string
	if err := db.QueryRow(`SELECT manifest_json,snapshot_digest FROM generations WHERE generation_id='gen-manifest'`).Scan(&persisted, &snapshot); err != nil {
		t.Fatal(err)
	}
	var readBack model.StoredCaptureManifestRef
	if err := json.Unmarshal([]byte(persisted), &readBack); err != nil {
		t.Fatalf("decode persisted descriptor: %v", err)
	}
	if err := readBack.Validate(); err != nil {
		t.Fatalf("validate persisted descriptor: %v", err)
	}
	if readBack != ref || persisted != string(descriptor) || snapshot != string(ref.SnapshotDigest) {
		t.Fatalf("persisted descriptor drifted: got=%+v snapshot=%s want=%+v", readBack, snapshot, ref)
	}
	if info, err := os.Stat(artifactPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("published manifest file missing after SQL publish: stat=%v err=%v", info, err)
	}

	// The following is a test-only parity check, not a product backup or GC service.
	backupCopy := filepath.Join(root, "backup", filepath.FromSlash(ref.Path))
	if err := os.MkdirAll(filepath.Dir(backupCopy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupCopy, storedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(backupCopy)
	if err != nil || model.SHA256(backupBytes) != ref.ArtifactSHA256 {
		t.Fatalf("backup file-set parity hash mismatch: err=%v", err)
	}
}

func TestResolverNaturalKeyAllowsVersionsAndRejectsDuplicates(t *testing.T) {
	db, _ := openStorageDB(t, "resolver-natural-key", 5000)
	applyStorageMigration(t, db, "index/001.sql")
	applyStorageMigration(t, db, "index/002.sql")
	addBuildingGeneration(t, db, "gen-source", strings.Repeat("b", 64))
	publishStorageGeneration(t, db, "gen-source")

	// index_004 is deliberately read from the SPEC only in this isolated file-backed fixture;
	// it is not installed as a shipped migration by T106.
	if _, err := db.ExecContext(context.Background(), ddlFromSpec(t, "MCE4D_DDL_INDEX_004")); err != nil {
		t.Fatalf("apply isolated resolver DDL fixture: %v", err)
	}
	mapID := strings.Repeat("c", 64)
	profileID := strings.Repeat("b", 64)
	policyDigest := strings.Repeat("d", 64)
	if _, err := db.Exec(`INSERT INTO ui_build_maps
		(build_map_id,schema_version,profile_id,provider_version,provider_digest,build_config_digest,
		 input_manifest_digest,mode,artifact_hash,metadata_json,created_at)
		VALUES (?,1,?,'fixture',?,?,?,'development',?,'{}','2026-10-16T00:00:00Z')`,
		mapID, profileID, strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("1", 64), strings.Repeat("2", 64)); err != nil {
		t.Fatalf("insert immutable producer map: %v", err)
	}
	anchorID := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO ui_source_anchors
		(build_map_id,anchor_id,kind,path,source_hash,start_byte,end_byte)
		VALUES (?,?,'element_site','src/App.vue',?,0,4)`, mapID, anchorID, strings.Repeat("b", 64)); err != nil {
		t.Fatalf("insert immutable source anchor before binding: %v", err)
	}

	insertBinding := func(bindingID, resolverDigest, evidenceDigest string) error {
		_, err := db.Exec(`INSERT INTO ui_build_bindings
			(binding_id,build_map_id,profile_id,source_generation_id,policy_digest,resolver_digest,evidence_digest,created_at)
			VALUES (?,?,?,?,?,?,?,'2026-10-16T00:00:01Z')`,
			bindingID, mapID, profileID, "gen-source", policyDigest, resolverDigest, evidenceDigest)
		return err
	}
	firstResolver := strings.Repeat("3", 64)
	secondResolver := strings.Repeat("4", 64)
	firstBinding := strings.Repeat("5", 64)
	secondBinding := strings.Repeat("6", 64)
	if err := insertBinding(firstBinding, firstResolver, strings.Repeat("7", 64)); err != nil {
		t.Fatalf("insert resolver version 1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ui_anchor_links
		(binding_id,build_map_id,link_id,anchor_id,target_ref_json,kind,resolution_status,evidence_json)
		VALUES (?,?, 'link-1',?,'{}','element_site','exact','{}')`, firstBinding, mapID, anchorID); err != nil {
		t.Fatalf("insert binding-specific anchor link: %v", err)
	}
	if err := insertBinding(secondBinding, secondResolver, strings.Repeat("8", 64)); err != nil {
		t.Fatalf("insert resolver version 2 beside version 1: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM ui_build_bindings WHERE build_map_id=? AND profile_id=?
		AND source_generation_id=? AND policy_digest=?`, mapID, profileID, "gen-source", policyDigest).Scan(&count); err != nil || count != 2 {
		t.Fatalf("resolver history count=%d err=%v; want two bindings", count, err)
	}
	if err := insertBinding(strings.Repeat("9", 64), firstResolver, strings.Repeat("a", 64)); err == nil {
		t.Fatal("same natural resolver key must reject a third binding")
	} else {
		expectStorageError(t, err, "UNIQUE")
	}
	if _, err := db.Exec(`UPDATE ui_build_bindings SET evidence_digest=? WHERE binding_id=?`, strings.Repeat("a", 64), firstBinding); err == nil {
		t.Fatal("resolver binding content must be immutable")
	} else {
		expectStorageError(t, err, "IMMUTABLE_UI_METADATA")
	}
	if _, err := db.Exec(`UPDATE ui_build_maps SET artifact_hash=? WHERE build_map_id=?`, strings.Repeat("a", 64), mapID); err == nil {
		t.Fatal("producer map content must remain immutable after import")
	} else {
		expectStorageError(t, err, "IMMUTABLE_UI_METADATA")
	}
	if _, err := db.Exec(`INSERT INTO ui_source_anchors
		(build_map_id,anchor_id,kind,path,source_hash,start_byte,end_byte)
		VALUES (?,?,'element_site','src/Other.vue',?,0,4)`, mapID, strings.Repeat("c", 64), strings.Repeat("d", 64)); err == nil {
		t.Fatal("a bound build map must reject later source anchors")
	} else {
		expectStorageError(t, err, "BUILD_MAP_ALREADY_BOUND")
	}
	var digest string
	if err := db.QueryRow(`SELECT resolver_digest FROM ui_build_bindings WHERE binding_id=?`, firstBinding).Scan(&digest); err != nil || digest != firstResolver {
		t.Fatalf("first resolver binding changed: got=%q err=%v", digest, err)
	}
}
