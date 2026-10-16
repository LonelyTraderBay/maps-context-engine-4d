package contract_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestIndexPublicationGuardsForeignKeysAndFrozenFacts(t *testing.T) {
	db, _ := openStorageDB(t, "publication", 5000)
	applyStorageMigration(t, db, "index/001.sql")
	applyStorageMigration(t, db, "index/002.sql")
	profileA := strings.Repeat("1", 64)
	addBuildingGeneration(t, db, "gen-a", profileA)

	if _, err := db.Exec(`INSERT INTO active_generations(profile_id,generation_id) VALUES(?,?)`, profileA, "gen-a"); err == nil {
		t.Fatal("BUILDING generation must not become active")
	} else {
		expectStorageError(t, err, "GENERATION_NOT_PUBLISHED")
	}

	if _, err := db.Exec(`INSERT INTO packages
		(generation_id,package_key,import_path,module_key,variant,input_digest,semantic_status)
		VALUES ('gen-a','pkg-a','example/a','mod-a','production',?,'complete_in_profile')`, strings.Repeat("2", 64)); err != nil {
		t.Fatalf("insert package into BUILDING generation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO files
		(generation_id,path,kind,status,package_key,reason_code)
		VALUES ('gen-a','src/a.go','go','missing','pkg-a','not_captured')`); err != nil {
		t.Fatalf("insert missing file with required reason: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO symbols
		(generation_id,symbol_id,path,package_key,kind,name,qualified_name,scope_key,start_byte,end_byte,signature,exported)
		VALUES ('gen-a','symbol-a','src/a.go','pkg-a','function','A','example.A','pkg-a',0,1,'func A()',1)`); err != nil {
		t.Fatalf("insert symbol into generation A: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO modules(generation_id,module_id,parent_module_id,label,kind,plane)
		VALUES ('gen-a','module-a',NULL,'A','package','observed_static')`); err != nil {
		t.Fatalf("insert module into BUILDING generation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO module_members(generation_id,module_id,package_key)
		VALUES ('gen-a','module-a','pkg-a')`); err != nil {
		t.Fatalf("insert module membership: %v", err)
	}

	addBuildingGeneration(t, db, "gen-b", strings.Repeat("3", 64))
	if _, err := db.Exec(`INSERT INTO files
		(generation_id,path,kind,status,package_key,reason_code)
		VALUES ('gen-b','src/b.go','go','missing',NULL,'not_captured')`); err != nil {
		t.Fatalf("insert generation B file fixture: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO symbols
		(generation_id,symbol_id,path,package_key,kind,name,qualified_name,scope_key,start_byte,end_byte,signature,exported)
		VALUES ('gen-b','symbol-b','src/b.go',NULL,'function','B','example.B','gen-b',0,1,'func B()',1)`); err != nil {
		t.Fatalf("insert symbol into generation B: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO relations
		(generation_id,relation_id,source_symbol_id,target_symbol_id,kind,evidence_kind,site_path,start_byte,end_byte,reason)
		VALUES ('gen-b','cross-relation','symbol-b','symbol-a','calls','syntax_exact','src/b.go',0,1,'cross-generation fixture')`); err == nil {
		t.Fatal("relation target must not reference a symbol from another generation")
	} else {
		expectStorageError(t, err, "FOREIGN KEY")
	}
	if _, err := db.Exec(`INSERT INTO files
		(generation_id,path,kind,status,package_key,reason_code)
		VALUES ('gen-b','src/cross.go','go','missing','pkg-a','not_captured')`); err == nil {
		t.Fatal("a file in generation B must not bind to generation A package")
	} else {
		expectStorageError(t, err, "FOREIGN KEY")
	}

	publishStorageGeneration(t, db, "gen-a")
	if _, err := db.Exec(`INSERT INTO active_generations(profile_id,generation_id) VALUES(?,?)`, profileA, "gen-a"); err != nil {
		t.Fatalf("activate a published generation: %v", err)
	}
	addBuildingGeneration(t, db, "gen-a-next", profileA)
	publishStorageGeneration(t, db, "gen-a-next")
	result, err := db.Exec(`UPDATE active_generations SET generation_id='gen-a-next'
		WHERE profile_id=? AND generation_id='gen-a'`, profileA)
	if err != nil {
		t.Fatalf("compare-and-swap active generation: %v", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		t.Fatalf("active generation CAS affected=%d err=%v; want 1", changed, err)
	}
	result, err = db.Exec(`UPDATE active_generations SET generation_id='gen-a'
		WHERE profile_id=? AND generation_id='gen-a'`, profileA)
	if err != nil {
		t.Fatalf("stale active generation CAS: %v", err)
	}
	changed, err = result.RowsAffected()
	if err != nil || changed != 0 {
		t.Fatalf("stale active generation CAS affected=%d err=%v; want 0", changed, err)
	}
	if _, err := db.Exec(`INSERT INTO packages
		(generation_id,package_key,import_path,module_key,variant,input_digest,semantic_status)
		VALUES ('gen-a','late','example/late','mod-a','production',?,'complete_in_profile')`, strings.Repeat("4", 64)); err == nil {
		t.Fatal("published generation facts must be frozen")
	} else {
		expectStorageError(t, err, "IMMUTABLE_GENERATION")
	}
	if _, err := db.Exec(`UPDATE generations SET state='RETIRED' WHERE generation_id='gen-a-next'`); err == nil {
		t.Fatal("active generation must not retire")
	} else {
		expectStorageError(t, err, "ACTIVE_GENERATION_CANNOT_RETIRE")
	}
}

func TestIndexGCRequiresLeafOrderAndTransactionRollback(t *testing.T) {
	db, _ := openStorageDB(t, "gc-rollback", 5000)
	applyStorageMigration(t, db, "index/001.sql")
	applyStorageMigration(t, db, "index/002.sql")
	addBuildingGeneration(t, db, "gen-gc", strings.Repeat("5", 64))
	sharedContentHash := strings.Repeat("f", 64)
	if _, err := db.Exec(`INSERT INTO content_blobs(content_hash,bytes,byte_length) VALUES(?,X'61',1)`, sharedContentHash); err != nil {
		t.Fatalf("insert shared immutable content blob: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO packages
		(generation_id,package_key,import_path,module_key,variant,input_digest,semantic_status)
		VALUES ('gen-gc','pkg','example/gc','module-root','production',?,'complete_in_profile')`, strings.Repeat("6", 64)); err != nil {
		t.Fatalf("insert GC package: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO files
		(generation_id,path,content_hash,kind,status,byte_length,package_key)
		VALUES ('gen-gc','src/gc.go',?,'go','indexed',1,'pkg')`, sharedContentHash); err != nil {
		t.Fatalf("insert GC source file: %v", err)
	}
	for i, module := range []struct{ id, parent string }{
		{"root", ""}, {"middle", "root"}, {"leaf", "middle"},
	} {
		var parent any
		if module.parent != "" {
			parent = module.parent
		}
		if _, err := db.Exec(`INSERT INTO modules
			(generation_id,module_id,parent_module_id,label,kind,plane)
			VALUES ('gen-gc',?,?,?,'package','observed_static')`, module.id, parent, fmt.Sprintf("node-%d", i)); err != nil {
			t.Fatalf("insert module %s: %v", module.id, err)
		}
		if _, err := db.Exec(`INSERT INTO module_members(generation_id,module_id,package_key)
			VALUES ('gen-gc',?,'pkg')`, module.id); err != nil {
			t.Fatalf("insert membership for %s: %v", module.id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO search_documents
		(generation_id,item_id,path,kind,name,qualified_name,body)
		VALUES ('gen-gc','item','src/gc.go','symbol','retainedterm','retainedterm','retainedterm')`); err != nil {
		t.Fatalf("insert GC FTS row: %v", err)
	}
	addBuildingGeneration(t, db, "gen-retained", strings.Repeat("d", 64))
	if _, err := db.Exec(`INSERT INTO packages
		(generation_id,package_key,import_path,module_key,variant,input_digest,semantic_status)
		VALUES ('gen-retained','pkg-retained','example/retained','module-retained','production',?,'complete_in_profile')`, strings.Repeat("e", 64)); err != nil {
		t.Fatalf("insert retained package: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO files
		(generation_id,path,content_hash,kind,status,byte_length,package_key)
		VALUES ('gen-retained','src/retained.go',?,'go','indexed',1,'pkg-retained')`, sharedContentHash); err != nil {
		t.Fatalf("insert retained source sharing content blob: %v", err)
	}
	publishStorageGeneration(t, db, "gen-retained")
	if _, err := db.Exec(`INSERT INTO active_generations(profile_id,generation_id)
		VALUES(?, 'gen-retained')`, strings.Repeat("d", 64)); err != nil {
		t.Fatalf("activate retained generation: %v", err)
	}
	publishStorageGeneration(t, db, "gen-gc")
	if _, err := db.Exec(`INSERT INTO active_generations(profile_id,generation_id)
		VALUES(?, 'gen-gc')`, strings.Repeat("5", 64)); err != nil {
		t.Fatalf("activate GC generation: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM active_generations WHERE generation_id='gen-gc'`); err != nil {
		t.Fatalf("deactivate generation before retirement: %v", err)
	}
	if _, err := db.Exec(`UPDATE generations SET state='RETIRED' WHERE generation_id='gen-gc'`); err != nil {
		t.Fatalf("retire inactive generation: %v", err)
	}

	parentTx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parentTx.Exec(`DELETE FROM search_documents WHERE generation_id='gen-gc'`); err != nil {
		_ = parentTx.Rollback()
		t.Fatalf("stage rollback before parent-order counterexample: %v", err)
	}
	if _, err := parentTx.Exec(`DELETE FROM modules WHERE generation_id='gen-gc' AND module_id='root'`); err == nil {
		_ = parentTx.Rollback()
		t.Fatal("a module parent must not be deleted before its child")
	} else {
		expectStorageError(t, err, "FOREIGN KEY")
	}
	if err := parentTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var rollbackDocs int
	if err := db.QueryRow(`SELECT count(*) FROM search_documents WHERE generation_id='gen-gc'`).Scan(&rollbackDocs); err != nil || rollbackDocs != 1 {
		t.Fatalf("parent-order failure did not roll back earlier FTS deletion: docs=%d err=%v", rollbackDocs, err)
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM search_documents WHERE generation_id='gen-gc'`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("begin partial GC: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE generation_id='gen-gc'`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("stage file deletion before package-order counterexample: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM packages WHERE generation_id='gen-gc'`); err == nil {
		_ = tx.Rollback()
		t.Fatal("GC must not delete packages while modules still reference them")
	} else {
		expectStorageError(t, err, "FOREIGN KEY")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback failed GC transaction: %v", err)
	}
	var retainedDocs, retainedFiles, retainedModules int
	if err := db.QueryRow(`SELECT count(*) FROM search_documents WHERE generation_id='gen-gc'`).Scan(&retainedDocs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM files WHERE generation_id='gen-gc'`).Scan(&retainedFiles); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM modules WHERE generation_id='gen-gc'`).Scan(&retainedModules); err != nil {
		t.Fatal(err)
	}
	if retainedDocs != 1 || retainedFiles != 1 || retainedModules != 3 {
		t.Fatalf("rollback left search_documents=%d files=%d modules=%d; want 1/1/3", retainedDocs, retainedFiles, retainedModules)
	}

	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DELETE FROM search_documents WHERE generation_id='gen-gc'`,
		`DELETE FROM module_members WHERE generation_id='gen-gc'`,
		`DELETE FROM modules WHERE generation_id='gen-gc' AND module_id='leaf'`,
		`DELETE FROM modules WHERE generation_id='gen-gc' AND module_id='middle'`,
		`DELETE FROM modules WHERE generation_id='gen-gc' AND module_id='root'`,
		`DELETE FROM files WHERE generation_id='gen-gc'`,
		`DELETE FROM packages WHERE generation_id='gen-gc'`,
		`DELETE FROM generations WHERE generation_id='gen-gc'`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			_ = tx.Rollback()
			t.Fatalf("ordered GC statement %q: %v", statement, err)
		}
	}
	foreignKeyRows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("run final foreign-key check: %v", err)
	}
	if foreignKeyRows.Next() {
		var table string
		var rowID any
		var parent string
		var constraintID int
		if err := foreignKeyRows.Scan(&table, &rowID, &parent, &constraintID); err != nil {
			_ = foreignKeyRows.Close()
			_ = tx.Rollback()
			t.Fatalf("read foreign-key violation: %v", err)
		}
		_ = foreignKeyRows.Close()
		_ = tx.Rollback()
		t.Fatalf("ordered GC left foreign-key violation: table=%s row=%v parent=%s id=%d", table, rowID, parent, constraintID)
	}
	if err := foreignKeyRows.Err(); err != nil {
		_ = foreignKeyRows.Close()
		_ = tx.Rollback()
		t.Fatalf("read final foreign-key check: %v", err)
	}
	if err := foreignKeyRows.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close final foreign-key check rows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit ordered GC: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM generations WHERE generation_id='gen-gc'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("GC left generation rows=%d err=%v", remaining, err)
	}
	var retainedGenerationAfterGC, retainedFilesAfterGC, retainedBlobs int
	if err := db.QueryRow(`SELECT count(*) FROM generations WHERE generation_id='gen-retained'`).Scan(&retainedGenerationAfterGC); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM files WHERE generation_id='gen-retained'`).Scan(&retainedFilesAfterGC); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM content_blobs WHERE content_hash=?`, sharedContentHash).Scan(&retainedBlobs); err != nil {
		t.Fatal(err)
	}
	if retainedGenerationAfterGC != 1 || retainedFilesAfterGC != 1 || retainedBlobs != 1 {
		t.Fatalf("GC touched retained data: generation=%d files=%d shared_blobs=%d", retainedGenerationAfterGC, retainedFilesAfterGC, retainedBlobs)
	}
	if _, err := db.Exec(`DELETE FROM content_blobs WHERE content_hash=?`, sharedContentHash); err == nil {
		t.Fatal("shared content blob must remain while a retained generation references it")
	} else {
		expectStorageError(t, err, "FOREIGN KEY")
	}
}

func TestModuleCycleNeedsPublishTimeValidationBeyondForeignKeys(t *testing.T) {
	db, _ := openStorageDB(t, "module-cycle", 5000)
	applyStorageMigration(t, db, "index/001.sql")
	applyStorageMigration(t, db, "index/002.sql")
	addBuildingGeneration(t, db, "gen-cycle", strings.Repeat("c", 64))
	for _, module := range []struct {
		id     string
		parent any
	}{
		{"root", nil}, {"child", "root"},
	} {
		if _, err := db.Exec(`INSERT INTO modules(generation_id,module_id,parent_module_id,label,kind,plane)
			VALUES ('gen-cycle',?,?,?,'package','observed_static')`, module.id, module.parent, module.id); err != nil {
			t.Fatalf("insert module %s: %v", module.id, err)
		}
	}
	if _, err := db.Exec(`UPDATE modules SET parent_module_id='child' WHERE generation_id='gen-cycle' AND module_id='root'`); err != nil {
		t.Fatalf("form a valid-FK but cyclic module tree: %v", err)
	}
	var cycles int
	err := db.QueryRow(`WITH RECURSIVE walk(start_id,parent_id,path,cycle) AS (
		SELECT module_id,parent_module_id,json_array(module_id),0
		FROM modules WHERE generation_id='gen-cycle'
		UNION ALL
		SELECT walk.start_id,parent.module_id,json_insert(walk.path,'$[#]',parent.module_id),
			EXISTS(SELECT 1 FROM json_each(walk.path) WHERE value=parent.module_id)
		FROM walk JOIN modules parent ON parent.generation_id='gen-cycle' AND parent.module_id=walk.parent_id
		WHERE walk.parent_id IS NOT NULL AND walk.cycle=0
	) SELECT count(*) FROM walk WHERE cycle=1`).Scan(&cycles)
	if err != nil {
		t.Fatalf("run publish-time cycle validation fixture: %v", err)
	}
	if cycles == 0 {
		t.Fatal("cycle fixture was not detected; FK-only proof is incomplete")
	}
	t.Log("DDL foreign keys permit an existing-row parent cycle; publish must run the separate cycle validator before READY")
}

func TestCatalogAndIndexIdentityRowsStoreNewIncarnations(t *testing.T) {
	oldIncarnation := "22222222-2222-4222-8222-222222222222"
	newIncarnation := "33333333-3333-4333-8333-333333333333"
	catalog, _ := openStorageDB(t, "catalog-incarnation", 5000)
	applyStorageMigration(t, catalog, "catalog/001.sql")
	if _, err := catalog.Exec(`INSERT INTO catalog_identity(singleton,incarnation) VALUES(1,?)`, oldIncarnation); err != nil {
		t.Fatalf("insert initial catalog incarnation: %v", err)
	}
	if _, err := catalog.Exec(`UPDATE catalog_identity SET incarnation=? WHERE singleton=1 AND incarnation=?`, newIncarnation, oldIncarnation); err != nil {
		t.Fatalf("rotate catalog incarnation in storage fixture: %v", err)
	}
	var catalogIncarnation string
	if err := catalog.QueryRow(`SELECT incarnation FROM catalog_identity WHERE singleton=1`).Scan(&catalogIncarnation); err != nil || catalogIncarnation != newIncarnation {
		t.Fatalf("catalog incarnation=%q err=%v", catalogIncarnation, err)
	}

	index, _ := openStorageDB(t, "index-incarnation", 5000)
	applyStorageMigration(t, index, "index/001.sql")
	worktreeID := "11111111-1111-4111-8111-111111111111"
	if _, err := index.Exec(`INSERT INTO store_identity(singleton,worktree_id,incarnation) VALUES(1,?,?)`, worktreeID, oldIncarnation); err != nil {
		t.Fatalf("insert initial index incarnation: %v", err)
	}
	if _, err := index.Exec(`UPDATE store_identity SET incarnation=? WHERE singleton=1 AND incarnation=?`, newIncarnation, oldIncarnation); err != nil {
		t.Fatalf("rotate index incarnation in storage fixture: %v", err)
	}
	var storedWorktreeID, indexIncarnation string
	if err := index.QueryRow(`SELECT worktree_id,incarnation FROM store_identity WHERE singleton=1`).Scan(&storedWorktreeID, &indexIncarnation); err != nil {
		t.Fatal(err)
	}
	if storedWorktreeID != worktreeID || indexIncarnation != newIncarnation {
		t.Fatalf("index identity changed unexpectedly: worktree=%q incarnation=%q", storedWorktreeID, indexIncarnation)
	}
}

func TestRecordsHeadCASIncarnationAndPermissionHistory(t *testing.T) {
	worktreeID := "11111111-1111-4111-8111-111111111111"
	oldIncarnation := "22222222-2222-4222-8222-222222222222"
	newIncarnation := "33333333-3333-4333-8333-333333333333"
	const recordID = "decision-1"
	db, _ := openStorageDB(t, "records", 5000)
	applyStorageMigration(t, db, "records/001.sql")
	if _, err := db.Exec(`INSERT INTO records_meta(singleton,worktree_id,incarnation,record_epoch)
		VALUES(1,?,?,0)`, worktreeID, oldIncarnation); err != nil {
		t.Fatalf("insert records identity: %v", err)
	}

	insertRecord := func(version int, body string) error {
		_, err := db.Exec(`INSERT INTO record_versions
			(record_id,version,record_kind,declared_status,payload_json,source_refs_json,
			 actor_id,reason,payload_hash,created_at)
			VALUES (?,?,'decision','proposed',?,'[]','server-actor','fixture',?,'2026-10-16T00:00:00Z')`,
			recordID, version, body, strings.Repeat(fmt.Sprint(version), 64))
		return err
	}
	if err := insertRecord(1, `{"decision":"one"}`); err != nil {
		t.Fatalf("insert record version 1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO record_heads(record_id,version) VALUES(?,1)`, recordID); err != nil {
		t.Fatalf("insert record head: %v", err)
	}
	if _, err := db.Exec(`UPDATE records_meta SET record_epoch=1 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}

	// This fixture exercises the SQL CAS shape from DB-03; it is not a product record service.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO record_versions
		(record_id,version,record_kind,declared_status,payload_json,source_refs_json,
		 actor_id,reason,payload_hash,created_at)
		VALUES (?,2,'decision','approved','{"decision":"two"}','[]','server-actor','approve',?,'2026-10-16T00:00:01Z')`,
		recordID, strings.Repeat("4", 64)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert record version 2: %v", err)
	}
	result, err := tx.Exec(`UPDATE record_heads SET version=2 WHERE record_id=? AND version=1`, recordID)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		_ = tx.Rollback()
		t.Fatalf("head CAS affected=%d err=%v", changed, err)
	}
	if _, err := tx.Exec(`UPDATE records_meta SET record_epoch=record_epoch+1 WHERE singleton=1`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO mutation_receipts
		(client_mutation_id,principal_id,request_hash,record_id,result_version,created_at)
		VALUES ('mutation-1','principal-1',?,?,2,'2026-10-16T00:00:01Z')`,
		strings.Repeat("5", 64), recordID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert mutation receipt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO mutation_receipts
		(client_mutation_id,principal_id,request_hash,record_id,result_version,created_at)
		VALUES ('mutation-1','different-principal',?,?,2,'2026-10-16T00:00:02Z')`,
		strings.Repeat("a", 64), recordID); err == nil {
		t.Fatal("mutation receipt identity must be unique; changed principal/body cannot add a duplicate receipt")
	} else {
		expectStorageError(t, err, "UNIQUE")
	}

	// A stale expected version cannot publish a new version or increment the epoch.
	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO record_versions
		(record_id,version,record_kind,declared_status,payload_json,source_refs_json,
		 actor_id,reason,payload_hash,created_at)
		VALUES (?,3,'decision','approved','{"decision":"three"}','[]','server-actor','stale',?,'2026-10-16T00:00:02Z')`,
		recordID, strings.Repeat("6", 64)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert stale candidate version: %v", err)
	}
	result, err = tx.Exec(`UPDATE record_heads SET version=3 WHERE record_id=? AND version=1`, recordID)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	changed, err = result.RowsAffected()
	if err != nil || changed != 0 {
		_ = tx.Rollback()
		t.Fatalf("stale head CAS affected=%d err=%v; want conflict", changed, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var head, epoch, versionCount, receiptCount int
	if err := db.QueryRow(`SELECT version FROM record_heads WHERE record_id=?`, recordID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT record_epoch FROM records_meta WHERE singleton=1`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM record_versions WHERE record_id=?`, recordID).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM mutation_receipts WHERE record_id=?`, recordID).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if head != 2 || epoch != 2 || versionCount != 2 || receiptCount != 1 {
		t.Fatalf("stale CAS changed state: head=%d epoch=%d versions=%d receipts=%d", head, epoch, versionCount, receiptCount)
	}
	if _, err := db.Exec(`UPDATE record_versions SET reason='rewritten' WHERE record_id=? AND version=1`, recordID); err == nil {
		t.Fatal("historical record versions must be immutable")
	} else {
		expectStorageError(t, err, "IMMUTABLE_RECORD_VERSION")
	}

	// Restoring records rotates incarnation while retaining durable record rows and epoch.
	if _, err := db.Exec(`UPDATE records_meta SET incarnation=? WHERE singleton=1 AND incarnation=?`, newIncarnation, oldIncarnation); err != nil {
		t.Fatalf("rotate records incarnation in fixture: %v", err)
	}
	var gotIncarnation string
	if err := db.QueryRow(`SELECT incarnation FROM records_meta WHERE singleton=1`).Scan(&gotIncarnation); err != nil || gotIncarnation != newIncarnation {
		t.Fatalf("restored records incarnation=%q err=%v", gotIncarnation, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM record_versions WHERE record_id=?`, recordID).Scan(&versionCount); err != nil || versionCount != 2 {
		t.Fatalf("restored record history=%d err=%v; want preserved 2 versions", versionCount, err)
	}

	catalog, _ := openStorageDB(t, "catalog-permissions", 5000)
	applyStorageMigration(t, catalog, "catalog/001.sql")
	if _, err := catalog.Exec(`INSERT INTO preview_permission_versions
		(profile_id,version,declared_status,config_json,config_digest,actor_id,reason,created_at)
		VALUES(?,1,'approved','{}',?,'server-actor','approve','2026-10-16T00:00:00Z')`,
		strings.Repeat("7", 64), strings.Repeat("8", 64)); err != nil {
		t.Fatalf("insert permission version: %v", err)
	}
	if _, err := catalog.Exec(`INSERT INTO preview_permission_heads(profile_id,version) VALUES(?,1)`, strings.Repeat("7", 64)); err != nil {
		t.Fatalf("insert permission head: %v", err)
	}
	if _, err := catalog.Exec(`UPDATE preview_permission_versions SET declared_status='revoked' WHERE profile_id=? AND version=1`, strings.Repeat("7", 64)); err == nil {
		t.Fatal("permission version history must be immutable")
	} else {
		expectStorageError(t, err, "IMMUTABLE_PERMISSION_VERSION")
	}
	if _, err := catalog.Exec(`INSERT INTO preview_permission_versions
		(profile_id,version,declared_status,config_json,config_digest,actor_id,reason,created_at)
		VALUES(?,2,'revoked','{}',?,'server-actor','revoke','2026-10-16T00:00:01Z')`,
		strings.Repeat("7", 64), strings.Repeat("9", 64)); err != nil {
		t.Fatalf("append permission revocation version: %v", err)
	}
	result, err = catalog.Exec(`UPDATE preview_permission_heads SET version=2 WHERE profile_id=? AND version=1`, strings.Repeat("7", 64))
	if err != nil {
		t.Fatalf("permission head CAS: %v", err)
	}
	changed, err = result.RowsAffected()
	if err != nil || changed != 1 {
		t.Fatalf("permission head CAS affected=%d err=%v", changed, err)
	}
	var permissionHead int
	if err := catalog.QueryRow(`SELECT version FROM preview_permission_heads WHERE profile_id=?`, strings.Repeat("7", 64)).Scan(&permissionHead); err != nil || permissionHead != 2 {
		t.Fatalf("permission head=%d err=%v; want appended version 2", permissionHead, err)
	}
}
