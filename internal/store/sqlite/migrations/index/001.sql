PRAGMA foreign_keys=ON;
CREATE TABLE schema_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 schema_version INTEGER NOT NULL CHECK(schema_version>=1),
 min_reader_version INTEGER NOT NULL CHECK(min_reader_version>=1)
) STRICT;
INSERT INTO schema_meta VALUES(1,1,1);
CREATE TABLE store_identity (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 worktree_id TEXT NOT NULL UNIQUE,
 incarnation TEXT NOT NULL CHECK(length(incarnation)=36)
) STRICT;
CREATE TABLE generations (
 generation_id TEXT PRIMARY KEY NOT NULL,
 profile_id TEXT NOT NULL CHECK(length(profile_id)=64),
 snapshot_digest TEXT CHECK(snapshot_digest IS NULL OR length(snapshot_digest)=64),
 state TEXT NOT NULL CHECK(state IN ('BUILDING','READY','READY_PARTIAL','FAILED','CANCELLED','RETIRED')),
 owner_job_id TEXT NOT NULL,
 policy_digest TEXT NOT NULL CHECK(length(policy_digest)=64),
 policy_epoch INTEGER NOT NULL CHECK(policy_epoch>=1),
 provider_fingerprint TEXT NOT NULL CHECK(length(provider_fingerprint)=64),
 manifest_json TEXT CHECK(manifest_json IS NULL OR json_valid(manifest_json)),
 created_at TEXT NOT NULL, completed_at TEXT,
 UNIQUE(profile_id,generation_id),
 CHECK(state NOT IN ('READY','READY_PARTIAL','RETIRED') OR
       (snapshot_digest IS NOT NULL AND manifest_json IS NOT NULL AND completed_at IS NOT NULL))
) STRICT;
CREATE INDEX generations_profile ON generations(profile_id,completed_at,generation_id);
CREATE TABLE active_generations (
 profile_id TEXT PRIMARY KEY NOT NULL,
 generation_id TEXT NOT NULL UNIQUE,
 FOREIGN KEY(profile_id,generation_id) REFERENCES generations(profile_id,generation_id) ON DELETE RESTRICT
) STRICT;
CREATE TABLE content_blobs (
 content_hash TEXT PRIMARY KEY NOT NULL CHECK(length(content_hash)=64),
 bytes BLOB NOT NULL,
 byte_length INTEGER NOT NULL CHECK(byte_length>=0 AND byte_length=length(bytes))
) STRICT;
CREATE TABLE packages (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 package_key TEXT NOT NULL,
 import_path TEXT NOT NULL, module_key TEXT NOT NULL,
 variant TEXT NOT NULL CHECK(variant IN ('production','internal_test','external_test','other_supported')),
 input_digest TEXT NOT NULL CHECK(length(input_digest)=64),
 semantic_status TEXT NOT NULL CHECK(semantic_status IN ('complete_in_profile','partial','unsupported','unknown')),
 PRIMARY KEY(generation_id,package_key)
) STRICT;
CREATE INDEX packages_import ON packages(generation_id,import_path,variant);
CREATE TABLE files (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 path TEXT NOT NULL,
 content_hash TEXT REFERENCES content_blobs(content_hash) ON DELETE RESTRICT,
 kind TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('indexed','metadata_only','excluded','missing','read_error','parse_error')),
 byte_length INTEGER CHECK(byte_length IS NULL OR byte_length>=0),
 package_key TEXT,
 generated INTEGER NOT NULL DEFAULT 0 CHECK(generated IN (0,1)),
 reason_code TEXT,
 PRIMARY KEY(generation_id,path),
 FOREIGN KEY(generation_id,package_key) REFERENCES packages(generation_id,package_key) ON DELETE RESTRICT,
 CHECK(status NOT IN ('indexed','parse_error') OR content_hash IS NOT NULL),
 CHECK(status NOT IN ('excluded','missing','read_error') OR content_hash IS NULL),
 CHECK(status='indexed' OR reason_code IS NOT NULL)
) STRICT;
CREATE INDEX files_status ON files(generation_id,status,path);
CREATE TABLE symbols (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 symbol_id TEXT NOT NULL,
 path TEXT, package_key TEXT, kind TEXT NOT NULL,
 name TEXT NOT NULL, qualified_name TEXT NOT NULL, scope_key TEXT NOT NULL,
 start_byte INTEGER, end_byte INTEGER,
 signature TEXT NOT NULL, exported INTEGER NOT NULL CHECK(exported IN(0,1)),
 PRIMARY KEY(generation_id,symbol_id),
 FOREIGN KEY(generation_id,path) REFERENCES files(generation_id,path) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,package_key) REFERENCES packages(generation_id,package_key) ON DELETE RESTRICT,
 CHECK((path IS NULL AND kind='external' AND start_byte IS NULL AND end_byte IS NULL)
   OR (path IS NOT NULL AND start_byte IS NOT NULL AND end_byte IS NOT NULL AND start_byte>=0 AND end_byte>=start_byte))
) STRICT;
CREATE INDEX symbols_name ON symbols(generation_id,name,symbol_id);
CREATE INDEX symbols_qualified ON symbols(generation_id,qualified_name,symbol_id);
CREATE INDEX symbols_range ON symbols(generation_id,path,start_byte,end_byte);
CREATE TABLE relations (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 relation_id TEXT NOT NULL,
 source_symbol_id TEXT NOT NULL, target_symbol_id TEXT,
 kind TEXT NOT NULL,
 evidence_kind TEXT NOT NULL CHECK(evidence_kind IN
 ('syntax_exact','type_resolved','type_possible','explicit_document','text_match','unresolved')),
 site_path TEXT NOT NULL, start_byte INTEGER NOT NULL CHECK(start_byte>=0),
 end_byte INTEGER NOT NULL CHECK(end_byte>=start_byte),
 reason TEXT NOT NULL, unresolved_label TEXT,
 PRIMARY KEY(generation_id,relation_id),
 FOREIGN KEY(generation_id,source_symbol_id) REFERENCES symbols(generation_id,symbol_id) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,target_symbol_id) REFERENCES symbols(generation_id,symbol_id) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,site_path) REFERENCES files(generation_id,path) ON DELETE RESTRICT,
 CHECK(target_symbol_id IS NOT NULL OR (evidence_kind='unresolved' AND unresolved_label IS NOT NULL))
) STRICT;
CREATE INDEX relations_out ON relations(generation_id,source_symbol_id,kind,relation_id);
CREATE INDEX relations_in ON relations(generation_id,target_symbol_id,kind,relation_id);
CREATE TABLE diagnostics (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 diagnostic_id TEXT NOT NULL, path TEXT, package_key TEXT,
 code TEXT NOT NULL, severity TEXT NOT NULL CHECK(severity IN('info','warning','error')),
 sanitized_message TEXT NOT NULL,
 scope_json TEXT NOT NULL CHECK(json_valid(scope_json)),
 PRIMARY KEY(generation_id,diagnostic_id),
 FOREIGN KEY(generation_id,path) REFERENCES files(generation_id,path) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,package_key) REFERENCES packages(generation_id,package_key) ON DELETE RESTRICT
) STRICT;
CREATE INDEX diagnostics_code ON diagnostics(generation_id,severity,code);
CREATE TABLE capability_results (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 capability_key TEXT NOT NULL, scope_key TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN('supported_scope_complete','partial','unsupported','unknown')),
 summary_json TEXT NOT NULL CHECK(json_valid(summary_json)),
 PRIMARY KEY(generation_id,capability_key,scope_key)
) STRICT;
CREATE TABLE search_documents (
 rowid INTEGER PRIMARY KEY,
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 item_id TEXT NOT NULL, path TEXT NOT NULL, kind TEXT NOT NULL,
 name TEXT NOT NULL, qualified_name TEXT NOT NULL, body TEXT NOT NULL,
 UNIQUE(generation_id,item_id)
) STRICT;
CREATE INDEX search_scope ON search_documents(generation_id,path,item_id);
CREATE VIRTUAL TABLE search_fts USING fts5(
 name,qualified_name,body,content='search_documents',content_rowid='rowid',tokenize='unicode61'
);
CREATE TRIGGER search_documents_ai AFTER INSERT ON search_documents BEGIN
 INSERT INTO search_fts(rowid,name,qualified_name,body) VALUES(new.rowid,new.name,new.qualified_name,new.body);
END;
CREATE TRIGGER search_documents_ad AFTER DELETE ON search_documents BEGIN
 INSERT INTO search_fts(search_fts,rowid,name,qualified_name,body) VALUES('delete',old.rowid,old.name,old.qualified_name,old.body);
END;
CREATE TRIGGER search_documents_au AFTER UPDATE ON search_documents BEGIN
 INSERT INTO search_fts(search_fts,rowid,name,qualified_name,body) VALUES('delete',old.rowid,old.name,old.qualified_name,old.body);
 INSERT INTO search_fts(rowid,name,qualified_name,body) VALUES(new.rowid,new.name,new.qualified_name,new.body);
END;
CREATE TRIGGER active_insert_guard BEFORE INSERT ON active_generations BEGIN
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM generations g WHERE g.generation_id=new.generation_id
 AND g.profile_id=new.profile_id AND g.state IN('READY','READY_PARTIAL'))
 THEN RAISE(ABORT,'GENERATION_NOT_PUBLISHED') END;
END;
CREATE TRIGGER active_update_guard BEFORE UPDATE ON active_generations BEGIN
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM generations g WHERE g.generation_id=new.generation_id
 AND g.profile_id=new.profile_id AND g.state IN('READY','READY_PARTIAL'))
 THEN RAISE(ABORT,'GENERATION_NOT_PUBLISHED') END;
END;
CREATE TRIGGER generation_retire_guard BEFORE UPDATE OF state ON generations
WHEN new.state='RETIRED' BEGIN
 SELECT CASE WHEN EXISTS(SELECT 1 FROM active_generations WHERE generation_id=old.generation_id)
 THEN RAISE(ABORT,'ACTIVE_GENERATION_CANNOT_RETIRE') END;
END;
CREATE TRIGGER content_blob_no_update BEFORE UPDATE ON content_blobs BEGIN
 SELECT RAISE(ABORT,'IMMUTABLE_CONTENT');
END;
CREATE TRIGGER packages_insert_frozen BEFORE INSERT ON packages
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER packages_update_frozen BEFORE UPDATE ON packages
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER packages_delete_frozen BEFORE DELETE ON packages
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER files_insert_frozen BEFORE INSERT ON files
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER files_update_frozen BEFORE UPDATE ON files
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER files_delete_frozen BEFORE DELETE ON files
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER symbols_insert_frozen BEFORE INSERT ON symbols
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER symbols_update_frozen BEFORE UPDATE ON symbols
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER symbols_delete_frozen BEFORE DELETE ON symbols
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER relations_insert_frozen BEFORE INSERT ON relations
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER relations_update_frozen BEFORE UPDATE ON relations
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER relations_delete_frozen BEFORE DELETE ON relations
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER diagnostics_insert_frozen BEFORE INSERT ON diagnostics
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER diagnostics_update_frozen BEFORE UPDATE ON diagnostics
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER diagnostics_delete_frozen BEFORE DELETE ON diagnostics
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER capability_results_insert_frozen BEFORE INSERT ON capability_results
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER capability_results_update_frozen BEFORE UPDATE ON capability_results
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER capability_results_delete_frozen BEFORE DELETE ON capability_results
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER search_documents_insert_frozen BEFORE INSERT ON search_documents
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER search_documents_update_frozen BEFORE UPDATE ON search_documents
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER search_documents_delete_frozen BEFORE DELETE ON search_documents
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER generation_state_guard BEFORE UPDATE OF state ON generations
WHEN NOT (new.state=old.state OR (old.state='BUILDING' AND new.state IN('READY','READY_PARTIAL','FAILED','CANCELLED'))
 OR (old.state IN('READY','READY_PARTIAL') AND new.state='RETIRED'))
BEGIN SELECT RAISE(ABORT,'INVALID_GENERATION_TRANSITION'); END;
CREATE TRIGGER generation_metadata_guard BEFORE UPDATE ON generations
WHEN old.state!='BUILDING' AND (new.generation_id IS NOT old.generation_id OR new.profile_id IS NOT old.profile_id
 OR new.snapshot_digest IS NOT old.snapshot_digest OR new.policy_digest IS NOT old.policy_digest
 OR new.provider_fingerprint IS NOT old.provider_fingerprint OR new.manifest_json IS NOT old.manifest_json
 OR new.created_at IS NOT old.created_at OR new.completed_at IS NOT old.completed_at
 OR new.owner_job_id IS NOT old.owner_job_id OR new.policy_epoch IS NOT old.policy_epoch)
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION_METADATA'); END;
CREATE TRIGGER generation_delete_guard BEFORE DELETE ON generations
WHEN old.state NOT IN('RETIRED','FAILED','CANCELLED')
BEGIN SELECT RAISE(ABORT,'GENERATION_NOT_RETIRED'); END;
