package contracts

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestIndex003EnforcesGenerationScopedDomainIntegrity(t *testing.T) {
	db := openIndexFixture(t)
	var version, minReader int
	if err := db.QueryRow(`SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&version, &minReader); err != nil {
		t.Fatal(err)
	}
	if version != 3 || minReader != 3 {
		t.Fatalf("schema version=(%d,%d), want (3,3)", version, minReader)
	}
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d, err=%v; want enabled", foreignKeys, err)
	}

	const generationA = "11111111-1111-4111-8111-111111111111"
	const generationB = "22222222-2222-4222-8222-222222222222"
	insertBuildingGeneration(t, db, generationA, "a")
	insertBuildingGeneration(t, db, generationB, "b")
	insertEntity(t, db, generationA, "source", "sales.order")
	insertEntity(t, db, generationA, "target", "sales.receipt")
	insertEntity(t, db, generationB, "other-generation-only", "sales.order")
	insertSymbol(t, db, generationA, "create-order")

	if _, err := db.Exec(`INSERT INTO entity_links(generation_id,link_id,source_entity_id,target_entity_id,kind,evidence_json,resolution_status)
		VALUES(?,?,?,?,?,?,?)`, generationA, "valid-edge", "source", "target", "creates", `[]`, "candidate"); err != nil {
		t.Fatalf("valid generation-local entity link rejected: %v", err)
	}
	assertSQLRejected(t, db, `INSERT INTO entity_links(generation_id,link_id,source_symbol_id,source_entity_id,target_entity_id,kind,evidence_json,resolution_status)
		VALUES(?,?,?,?,?,?,?,?)`, generationA, "xor-edge", "create-order", "source", "target", "creates", `[]`, "candidate")
	assertSQLRejected(t, db, `INSERT INTO entity_links(generation_id,link_id,source_entity_id,target_entity_id,kind,evidence_json,resolution_status)
		VALUES(?,?,?,?,?,?,?)`, generationA, "cross-generation-edge", "source", "other-generation-only", "creates", `[]`, "candidate")
	assertSQLRejected(t, db, `INSERT INTO entity_links(generation_id,link_id,source_entity_id,target_entity_id,kind,evidence_json,resolution_status)
		VALUES(?,?,?,?,?,?,?)`, generationA, "bad-status-edge", "source", "target", "creates", `[]`, "exact_without_scope")
	assertSQLRejected(t, db, `INSERT INTO entity_links(generation_id,link_id,source_entity_id,target_entity_id,kind,evidence_json,resolution_status)
		VALUES(?,?,?,?,?,?,?)`, generationA, "dangling-edge", "missing", "target", "creates", `[]`, "candidate")

	if _, err := db.Exec(`INSERT INTO contract_fields(generation_id,entity_id,field_path,direction,field_json)
		VALUES(?,?,?,?,?)`, generationA, "source", "amount", "stored", `{"type":"decimal","currency":"USD","precision":"0.0001"}`); err != nil {
		t.Fatalf("valid contract field rejected: %v", err)
	}
	assertSQLRejected(t, db, `INSERT INTO contract_fields(generation_id,entity_id,field_path,direction,field_json)
		VALUES(?,?,?,?,?)`, generationA, "source", "bad", "request", `{}`)
	if _, err := db.Exec(`INSERT INTO state_transitions(generation_id,machine_entity_id,transition_id,from_entity_id,to_entity_id,guard_ref_json,effect_refs_json,evidence_json)
		VALUES(?,?,?,?,?,?,?,?)`, generationA, "source", "approve", "source", "target", nil, `[]`, `[]`); err != nil {
		t.Fatalf("valid state transition declaration rejected: %v", err)
	}
	assertSQLRejected(t, db, `INSERT INTO state_transitions(generation_id,machine_entity_id,transition_id,from_entity_id,to_entity_id,guard_ref_json,effect_refs_json,evidence_json)
		VALUES(?,?,?,?,?,?,?,?)`, generationA, "source", "cross-generation-transition", "source", "other-generation-only", nil, `[]`, `[]`)

	if _, err := db.Exec(`UPDATE generations SET snapshot_digest=?,manifest_json=?,completed_at=?,state='READY' WHERE generation_id=?`, strings.Repeat("c", 64), `{}`, "2026-10-20T00:00:00Z", generationA); err != nil {
		t.Fatalf("could not seal fixture generation: %v", err)
	}
	assertFrozenTable(t, db, frozenTableCase{
		name:       "domain_entities",
		insert:     `INSERT INTO domain_entities(generation_id,entity_id,kind,qualified_key,plane,payload_json,source_refs_json) VALUES(?,?,?,?,?,?,?)`,
		insertArgs: []any{generationA, "frozen-insert", "business_entity", "sales.frozen", "intended", `{}`, `[]`},
		update:     `UPDATE domain_entities SET qualified_key='sales.changed' WHERE generation_id=? AND entity_id='source'`,
		updateArgs: []any{generationA},
		delete:     `DELETE FROM domain_entities WHERE generation_id=? AND entity_id='source'`,
		deleteArgs: []any{generationA},
	})
	assertFrozenTable(t, db, frozenTableCase{
		name:       "entity_links",
		insert:     `INSERT INTO entity_links(generation_id,link_id,source_entity_id,target_entity_id,kind,evidence_json,resolution_status) VALUES(?,?,?,?,?,?,?)`,
		insertArgs: []any{generationA, "frozen-insert", "source", "target", "creates", `[]`, "candidate"},
		update:     `UPDATE entity_links SET kind='changes' WHERE generation_id=? AND link_id='valid-edge'`,
		updateArgs: []any{generationA},
		delete:     `DELETE FROM entity_links WHERE generation_id=? AND link_id='valid-edge'`,
		deleteArgs: []any{generationA},
	})
	assertFrozenTable(t, db, frozenTableCase{
		name:       "contract_fields",
		insert:     `INSERT INTO contract_fields(generation_id,entity_id,field_path,direction,field_json) VALUES(?,?,?,?,?)`,
		insertArgs: []any{generationA, "source", "frozen", "stored", `{}`},
		update:     `UPDATE contract_fields SET field_json='{}' WHERE generation_id=? AND entity_id='source' AND field_path='amount'`,
		updateArgs: []any{generationA},
		delete:     `DELETE FROM contract_fields WHERE generation_id=? AND entity_id='source' AND field_path='amount'`,
		deleteArgs: []any{generationA},
	})
	assertFrozenTable(t, db, frozenTableCase{
		name:       "state_transitions",
		insert:     `INSERT INTO state_transitions(generation_id,machine_entity_id,transition_id,from_entity_id,to_entity_id,guard_ref_json,effect_refs_json,evidence_json) VALUES(?,?,?,?,?,?,?,?)`,
		insertArgs: []any{generationA, "source", "frozen-insert", "source", "target", nil, `[]`, `[]`},
		update:     `UPDATE state_transitions SET effect_refs_json='[]' WHERE generation_id=? AND machine_entity_id='source' AND transition_id='approve'`,
		updateArgs: []any{generationA},
		delete:     `DELETE FROM state_transitions WHERE generation_id=? AND machine_entity_id='source' AND transition_id='approve'`,
		deleteArgs: []any{generationA},
	})
}

type frozenTableCase struct {
	name       string
	insert     string
	insertArgs []any
	update     string
	updateArgs []any
	delete     string
	deleteArgs []any
}

func assertFrozenTable(t *testing.T, db *sql.DB, test frozenTableCase) {
	t.Helper()
	t.Run(test.name, func(t *testing.T) {
		assertFrozenSQLRejected(t, db, test.insert, test.insertArgs...)
		assertFrozenSQLRejected(t, db, test.update, test.updateArgs...)
		assertFrozenSQLRejected(t, db, test.delete, test.deleteArgs...)
	})
}

func openIndexFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	root := repositoryRoot(t)
	for _, name := range []string{"001.sql", "002.sql", "003.sql"} {
		path := filepath.Join(root, "internal", "store", "sqlite", "migrations", "index", name)
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not return a test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func insertBuildingGeneration(t *testing.T, db *sql.DB, generationID, suffix string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO generations(generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,provider_fingerprint,manifest_json,created_at,completed_at)
		VALUES(?,?,NULL,'BUILDING',?,?,1,?,NULL,?,NULL)`, generationID, strings.Repeat(suffix, 64), "job-"+suffix, strings.Repeat("d", 64), strings.Repeat("e", 64), "2026-10-20T00:00:00Z")
	if err != nil {
		t.Fatalf("insert BUILDING generation %s: %v", generationID, err)
	}
}

func insertEntity(t *testing.T, db *sql.DB, generationID, entityID, qualifiedKey string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO domain_entities(generation_id,entity_id,kind,qualified_key,plane,payload_json,source_refs_json)
		VALUES(?,?,?,?,?,?,?)`, generationID, entityID, "business_entity", qualifiedKey, "intended", `{}`, `[]`)
	if err != nil {
		t.Fatalf("insert entity %s: %v", entityID, err)
	}
}

func insertSymbol(t *testing.T, db *sql.DB, generationID, symbolID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO symbols(generation_id,symbol_id,path,package_key,kind,name,qualified_name,scope_key,start_byte,end_byte,signature,exported)
		VALUES(?,?,NULL,NULL,'external',?,?,?,NULL,NULL,'',0)`, generationID, symbolID, symbolID, "pkg."+symbolID, "pkg")
	if err != nil {
		t.Fatalf("insert symbol %s: %v", symbolID, err)
	}
}

func assertSQLRejected(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.Exec(statement, args...); err == nil {
		t.Fatalf("SQL unexpectedly succeeded: %s", statement)
	}
}

func assertFrozenSQLRejected(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.Exec(statement, args...); err == nil || !strings.Contains(err.Error(), "IMMUTABLE_GENERATION") {
		t.Fatalf("SQL error = %v, want IMMUTABLE_GENERATION: %s", err, statement)
	}
}
