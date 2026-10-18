package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

func TestPublishRejectsModuleCycleBeforeChangingGeneration(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	spec := testGenerationSpec(t, "41000000-0000-4000-8000-000000000001", "42000000-0000-4000-8000-000000000001", "a")
	if _, err := store.CreateGeneration(ctx, spec); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	if err := stageGenerationMarker(ctx, store, spec); err != nil {
		t.Fatalf("stage generation: %v", err)
	}
	for _, module := range []struct {
		id, parent string
	}{{"root", ""}, {"child", "root"}} {
		if _, err := store.writer.ExecContext(ctx, `INSERT INTO modules(
			generation_id,module_id,parent_module_id,label,kind,plane,source_path,start_byte,end_byte
		) VALUES(?,?,?,?,?,'intended',NULL,NULL,NULL)`, string(spec.Key.GenerationID), module.id,
			nullIfEmpty(module.parent), module.id, "module"); err != nil {
			t.Fatalf("insert module %s: %v", module.id, err)
		}
	}
	if _, err := store.writer.ExecContext(ctx, `UPDATE modules SET parent_module_id='child'
		WHERE generation_id=? AND module_id='root'`, string(spec.Key.GenerationID)); err != nil {
		t.Fatalf("close module cycle: %v", err)
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: spec, State: GenerationReady}); !errors.Is(err, ErrGenerationCycle) {
		t.Fatalf("cyclic module publish error=%v; want ErrGenerationCycle", err)
	}
	var state string
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT state FROM generations WHERE generation_id=?`,
		string(spec.Key.GenerationID)).Scan(&state); err != nil {
		t.Fatalf("read generation after rejected publish: %v", err)
	}
	if state != string(GenerationBuilding) {
		t.Fatalf("generation state after rejected publish=%s; want BUILDING", state)
	}
	var active int
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM active_generations WHERE generation_id=?`,
		string(spec.Key.GenerationID)).Scan(&active); err != nil {
		t.Fatalf("read active pointer after rejected publish: %v", err)
	}
	if active != 0 {
		t.Fatal("cyclic generation became active")
	}
}

func TestGenerationGCRespectsRetentionSharedBlobsAndIsIdempotent(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	profile := model.Hash(strings.Repeat("a", 64))
	generations := []GenerationSpec{
		testGenerationSpec(t, "43000000-0000-4000-8000-000000000001", "44000000-0000-4000-8000-000000000001", "b"),
		testGenerationSpec(t, "43000000-0000-4000-8000-000000000002", "44000000-0000-4000-8000-000000000002", "c"),
		testGenerationSpec(t, "43000000-0000-4000-8000-000000000003", "44000000-0000-4000-8000-000000000003", "d"),
	}
	blobHash := model.SHA256([]byte("shared source"))
	for index := range generations {
		generations[index].Key.ProfileID = profile
		if _, err := store.CreateGeneration(ctx, generations[index]); err != nil {
			t.Fatalf("create generation %d: %v", index, err)
		}
		if err := addGenerationFile(ctx, store, generations[index], blobHash); err != nil {
			t.Fatalf("add generation %d file: %v", index, err)
		}
		if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: generations[index], State: GenerationReady}); err != nil {
			t.Fatalf("publish generation %d: %v", index, err)
		}
	}
	plan, err := store.PlanGenerationGC(ctx, 1)
	if err != nil {
		t.Fatalf("plan GC: %v", err)
	}
	if len(plan.Candidates) != 2 || plan.Candidates[0].GenerationID != generations[0].Key.GenerationID ||
		plan.Candidates[1].GenerationID != generations[1].Key.GenerationID {
		t.Fatalf("GC candidates=%+v; want the two oldest non-active generations", plan.Candidates)
	}
	result, err := store.CollectGenerationGC(ctx, plan)
	if err != nil {
		t.Fatalf("collect GC plan: %v", err)
	}
	if result.CollectedGenerations != 2 || result.DeletedContentBlobs != 0 {
		t.Fatalf("GC result=%+v; want two generations and shared blob retained", result)
	}
	var activeCount, blobCount int
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM active_generations WHERE generation_id=?`,
		string(generations[2].Key.GenerationID)).Scan(&activeCount); err != nil {
		t.Fatalf("read active generation after GC: %v", err)
	}
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM content_blobs WHERE content_hash=?`,
		string(blobHash)).Scan(&blobCount); err != nil {
		t.Fatalf("read shared blob after GC: %v", err)
	}
	if activeCount != 1 || blobCount != 1 {
		t.Fatalf("active count=%d shared blob count=%d; want 1 and 1", activeCount, blobCount)
	}
	retry, err := store.CollectGenerationGC(ctx, plan)
	if err != nil {
		t.Fatalf("idempotent GC retry: %v", err)
	}
	if retry.CollectedGenerations != 0 || retry.AlreadyMissing != int64(len(plan.Candidates)) {
		t.Fatalf("idempotent GC result=%+v; want both candidates already missing", retry)
	}
	assertSQLiteIntegrity(t, store)
}

func TestGenerationGCUsesRequiredDomainDeleteOrder(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	first := testGenerationSpec(t, "43500000-0000-4000-8000-000000000001", "44500000-0000-4000-8000-000000000001", "a")
	second := testGenerationSpec(t, "43500000-0000-4000-8000-000000000002", "44500000-0000-4000-8000-000000000002", "b")
	for index, spec := range []GenerationSpec{first, second} {
		if _, err := store.CreateGeneration(ctx, spec); err != nil {
			t.Fatalf("create generation %d: %v", index, err)
		}
		if index == 0 {
			if err := addGenerationFile(ctx, store, spec, model.SHA256([]byte("domain GC source"))); err != nil {
				t.Fatalf("add retired generation file: %v", err)
			}
			filePath := fmt.Sprintf("source-%s.go", string(spec.Key.GenerationID)[35:])
			if _, err := store.writer.ExecContext(ctx, `INSERT INTO symbols(
				generation_id,symbol_id,path,package_key,kind,name,qualified_name,scope_key,start_byte,end_byte,signature,exported
			) VALUES(?,?,?,NULL,'function','f','p.f','p',0,1,'func f()',1)`, spec.Key.GenerationID, "symbol-a", filePath); err != nil {
				t.Fatalf("insert retired generation symbol: %v", err)
			}
			if _, err := store.writer.ExecContext(ctx, `INSERT INTO relations(
				generation_id,relation_id,source_symbol_id,target_symbol_id,kind,evidence_kind,site_path,start_byte,end_byte,reason,unresolved_label
			) VALUES(?,?,'symbol-a',NULL,'calls','unresolved',?,0,1,'unknown target','target')`, spec.Key.GenerationID, "relation-a", filePath); err != nil {
				t.Fatalf("insert retired generation relation: %v", err)
			}
		}
		if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: spec, State: GenerationReady}); err != nil {
			t.Fatalf("publish generation %d: %v", index, err)
		}
	}

	for _, table := range []string{"entity_links", "state_transitions", "contract_fields", "domain_entities"} {
		if _, err := store.writer.ExecContext(ctx, `CREATE TABLE `+table+` (
			generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
			row_id TEXT NOT NULL, PRIMARY KEY(generation_id,row_id)
		) STRICT`); err != nil {
			t.Fatalf("create synthetic %s capability: %v", table, err)
		}
		if _, err := store.writer.ExecContext(ctx, `INSERT INTO `+table+`(generation_id,row_id) VALUES(?,?)`, first.Key.GenerationID, table); err != nil {
			t.Fatalf("insert synthetic %s row: %v", table, err)
		}
	}
	if _, err := store.writer.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER t035_domain_children_before_relations BEFORE DELETE ON relations
		WHEN old.generation_id='%s' AND (
		 EXISTS(SELECT 1 FROM entity_links WHERE generation_id=old.generation_id) OR
		 EXISTS(SELECT 1 FROM state_transitions WHERE generation_id=old.generation_id) OR
		 EXISTS(SELECT 1 FROM contract_fields WHERE generation_id=old.generation_id)
		) BEGIN SELECT RAISE(ABORT,'domain children must be deleted first'); END`, first.Key.GenerationID)); err != nil {
		t.Fatalf("install domain-before-relations guard: %v", err)
	}
	if _, err := store.writer.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER t035_domain_entities_after_symbols BEFORE DELETE ON symbols
		WHEN old.generation_id='%s' AND NOT EXISTS(SELECT 1 FROM domain_entities WHERE generation_id=old.generation_id)
		BEGIN SELECT RAISE(ABORT,'domain entities must follow symbols'); END`, first.Key.GenerationID)); err != nil {
		t.Fatalf("install domain-entities-after-symbols guard: %v", err)
	}
	if _, err := store.writer.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER t035_domain_entities_before_files BEFORE DELETE ON files
		WHEN old.generation_id='%s' AND EXISTS(SELECT 1 FROM domain_entities WHERE generation_id=old.generation_id)
		BEGIN SELECT RAISE(ABORT,'domain entities must be deleted before files'); END`, first.Key.GenerationID)); err != nil {
		t.Fatalf("install domain-entities-before-files guard: %v", err)
	}

	plan, err := store.PlanGenerationGC(ctx, 1)
	if err != nil {
		t.Fatalf("plan domain GC: %v", err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].GenerationID != first.Key.GenerationID {
		t.Fatalf("domain GC candidates=%+v; want only the retired generation", plan.Candidates)
	}
	if _, err := store.CollectGenerationGC(ctx, plan); err != nil {
		t.Fatalf("collect with exact domain order: %v", err)
	}
	assertSQLiteIntegrity(t, store)
}

func TestGenerationGCRemovesOnlyVerifiedUnreferencedManifest(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	manifestA := []byte("verified capture manifest A")
	manifestAHash := model.SHA256(manifestA)
	refA := model.StoredCaptureManifestRef{
		FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + string(manifestAHash) + ".json",
		ByteLength: int64(len(manifestA)), ArtifactSHA256: manifestAHash,
		SnapshotDigest: model.SHA256([]byte("snapshot A")),
	}
	manifestB := []byte("verified capture manifest B")
	manifestBHash := model.SHA256(manifestB)
	refB := model.StoredCaptureManifestRef{
		FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + string(manifestBHash) + ".json",
		ByteLength: int64(len(manifestB)), ArtifactSHA256: manifestBHash,
		SnapshotDigest: model.SHA256([]byte("snapshot B")),
	}
	worktreePath := "worktrees/" + string(store.WorktreeID())
	if err := store.ownership.root.EnsurePrivateDirs(worktreePath + "/manifests"); err != nil {
		t.Fatal(err)
	}
	manifestPathA := worktreePath + "/" + refA.Path
	if err := store.ownership.root.WriteAtomicFile(manifestPathA, manifestA, 0o600); err != nil {
		t.Fatal(err)
	}

	first := testGenerationSpec(t, "43600000-0000-4000-8000-000000000001", "44600000-0000-4000-8000-000000000001", "a")
	second := testGenerationSpec(t, "43600000-0000-4000-8000-000000000002", "44600000-0000-4000-8000-000000000002", "b")
	for index, spec := range []GenerationSpec{first, second} {
		spec.SnapshotDigest = refA.SnapshotDigest
		spec.ManifestRef = refA
		if _, err := store.CreateGeneration(ctx, spec); err != nil {
			t.Fatalf("create shared-manifest generation %d: %v", index, err)
		}
		if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: spec, State: GenerationReady}); err != nil {
			t.Fatalf("publish shared-manifest generation %d: %v", index, err)
		}
		if index == 0 {
			first = spec
		} else {
			second = spec
		}
	}
	plan, err := store.PlanGenerationGC(ctx, 1)
	if err != nil {
		t.Fatalf("plan first manifest GC: %v", err)
	}
	if _, err := store.CollectGenerationGC(ctx, plan); err != nil {
		t.Fatalf("collect first shared-manifest generation: %v", err)
	}
	manifest, err := store.ownership.root.OpenRegularFile(manifestPathA)
	if err != nil {
		t.Fatalf("GC deleted a still-referenced manifest: %v", err)
	}
	if err := manifest.Close(); err != nil {
		t.Fatal(err)
	}

	third := testGenerationSpec(t, "43600000-0000-4000-8000-000000000003", "44600000-0000-4000-8000-000000000003", "c")
	third.SnapshotDigest = refB.SnapshotDigest
	third.ManifestRef = refB
	if _, err := store.CreateGeneration(ctx, third); err != nil {
		t.Fatalf("create generation with new manifest: %v", err)
	}
	if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: third, State: GenerationReady}); err != nil {
		t.Fatalf("publish generation with new manifest: %v", err)
	}
	plan, err = store.PlanGenerationGC(ctx, 1)
	if err != nil {
		t.Fatalf("plan unreferenced manifest GC: %v", err)
	}
	result, err := store.CollectGenerationGC(ctx, plan)
	if err != nil && !errors.Is(err, mceruntime.ErrDirectoryFlushUnsupported) {
		t.Fatalf("collect unreferenced manifest generation: %v", err)
	}
	if result.CollectedGenerations != 1 || len(plan.Candidates) != 1 || plan.Candidates[0].GenerationID != second.Key.GenerationID {
		t.Fatalf("unreferenced manifest GC result=%+v candidates=%+v", result, plan.Candidates)
	}
	if _, err := store.ownership.root.OpenRegularFile(manifestPathA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced manifest remains or cannot be verified absent: %v", err)
	}
	assertSQLiteIntegrity(t, store)
}

func TestGenerationGCRollsBackWholeGenerationAfterInjectedFailure(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	first := testGenerationSpec(t, "45000000-0000-4000-8000-000000000001", "46000000-0000-4000-8000-000000000001", "e")
	second := testGenerationSpec(t, "45000000-0000-4000-8000-000000000002", "46000000-0000-4000-8000-000000000002", "f")
	second.Key.ProfileID = first.Key.ProfileID
	blobHash := model.SHA256([]byte("retained failure fixture"))
	for index, spec := range []GenerationSpec{first, second} {
		if _, err := store.CreateGeneration(ctx, spec); err != nil {
			t.Fatalf("create generation %d: %v", index, err)
		}
		if err := addGenerationFile(ctx, store, spec, blobHash); err != nil {
			t.Fatalf("add generation %d file: %v", index, err)
		}
		if _, err := store.PublishGeneration(ctx, PublishRequest{Spec: spec, State: GenerationReady}); err != nil {
			t.Fatalf("publish generation %d: %v", index, err)
		}
	}
	plan, err := store.PlanGenerationGC(ctx, 1)
	if err != nil {
		t.Fatalf("plan GC: %v", err)
	}
	if _, err := store.writer.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER t035_fail_gc_files BEFORE DELETE ON files
		WHEN old.generation_id='%s' BEGIN SELECT RAISE(ABORT,'injected GC failure'); END`, first.Key.GenerationID)); err != nil {
		t.Fatalf("install GC failure trigger: %v", err)
	}
	if _, err := store.CollectGenerationGC(ctx, plan); err == nil {
		t.Fatal("GC passed the injected failure")
	}
	var state string
	var fileCount int
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT state FROM generations WHERE generation_id=?`,
		string(first.Key.GenerationID)).Scan(&state); err != nil {
		t.Fatalf("read failed generation state: %v", err)
	}
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM files WHERE generation_id=?`,
		string(first.Key.GenerationID)).Scan(&fileCount); err != nil {
		t.Fatalf("read failed generation files: %v", err)
	}
	if state != string(GenerationReady) || fileCount != 1 {
		t.Fatalf("after failed GC state=%s files=%d; want READY and one row", state, fileCount)
	}
	if _, err := store.writer.ExecContext(ctx, `DROP TRIGGER t035_fail_gc_files`); err != nil {
		t.Fatalf("remove GC failure trigger: %v", err)
	}
	if _, err := store.CollectGenerationGC(ctx, plan); err != nil {
		t.Fatalf("retry GC after injected failure: %v", err)
	}
	assertSQLiteIntegrity(t, store)
}

func TestGenerationGCRollsBackCorruptModuleCycleAndDryRunIsReadOnly(t *testing.T) {
	store := openTestIndex(t, 1_234)
	ctx := context.Background()
	spec := testGenerationSpec(t, "47000000-0000-4000-8000-000000000001", "48000000-0000-4000-8000-000000000001", "1")
	if _, err := store.CreateGeneration(ctx, spec); err != nil {
		t.Fatalf("create generation: %v", err)
	}
	for _, module := range []struct {
		id, parent string
	}{{"left", ""}, {"right", "left"}} {
		if _, err := store.writer.ExecContext(ctx, `INSERT INTO modules(
			generation_id,module_id,parent_module_id,label,kind,plane,source_path,start_byte,end_byte
		) VALUES(?,?,?,?,?,'intended',NULL,NULL,NULL)`, string(spec.Key.GenerationID), module.id,
			nullIfEmpty(module.parent), module.id, "module"); err != nil {
			t.Fatalf("insert module %s: %v", module.id, err)
		}
	}
	if _, err := store.writer.ExecContext(ctx, `UPDATE modules SET parent_module_id='right'
		WHERE generation_id=? AND module_id='left'`, string(spec.Key.GenerationID)); err != nil {
		t.Fatalf("close module cycle: %v", err)
	}
	if err := store.AbortGeneration(ctx, spec.Key, GenerationFailed); err != nil {
		t.Fatalf("retain failed generation evidence: %v", err)
	}
	before, err := store.PlanGenerationGC(ctx, 1)
	if err != nil {
		t.Fatalf("dry-run GC plan: %v", err)
	}
	var stateBefore string
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT state FROM generations WHERE generation_id=?`,
		string(spec.Key.GenerationID)).Scan(&stateBefore); err != nil {
		t.Fatalf("read state before dry-run: %v", err)
	}
	if len(before.Candidates) != 1 || stateBefore != string(GenerationFailed) {
		t.Fatalf("dry-run candidates=%d state=%s; want one failed generation", len(before.Candidates), stateBefore)
	}
	if _, err := store.CollectGenerationGC(ctx, before); !errors.Is(err, ErrGenerationCycle) {
		t.Fatalf("collect corrupt cycle error=%v; want ErrGenerationCycle", err)
	}
	var stateAfter string
	var modules int
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT state FROM generations WHERE generation_id=?`,
		string(spec.Key.GenerationID)).Scan(&stateAfter); err != nil {
		t.Fatalf("read state after cycle rollback: %v", err)
	}
	if err := store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM modules WHERE generation_id=?`,
		string(spec.Key.GenerationID)).Scan(&modules); err != nil {
		t.Fatalf("read modules after cycle rollback: %v", err)
	}
	if stateAfter != string(GenerationFailed) || modules != 2 {
		t.Fatalf("after cycle rollback state=%s modules=%d; want FAILED and 2", stateAfter, modules)
	}
}

func addGenerationFile(ctx context.Context, store *Store, spec GenerationSpec, blobHash model.Hash) error {
	content := []byte("shared source")
	return store.WriteGenerationBatch(ctx, spec.Key, manifestWriteOverhead, func(batch *GenerationBatch) error {
		if _, err := batch.Exec(`INSERT INTO content_blobs(content_hash,bytes,byte_length) VALUES(?,?,?)
			ON CONFLICT(content_hash) DO NOTHING`, string(blobHash), content, len(content)); err != nil {
			return err
		}
		_, err := batch.Exec(`INSERT INTO files(generation_id,path,content_hash,kind,status,byte_length,package_key,generated,reason_code)
			VALUES(?,? ,?,'source','indexed',?,NULL,0,NULL)`, string(spec.Key.GenerationID),
			fmt.Sprintf("source-%s.go", string(spec.Key.GenerationID)[35:]), string(blobHash), len(content))
		return err
	})
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
