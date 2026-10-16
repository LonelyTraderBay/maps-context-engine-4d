PRAGMA foreign_keys=ON;
CREATE TABLE schema_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 schema_version INTEGER NOT NULL CHECK(schema_version>=1),
 min_reader_version INTEGER NOT NULL CHECK(min_reader_version>=1)
) STRICT;
INSERT INTO schema_meta VALUES(1,1,1);
CREATE TABLE records_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), worktree_id TEXT NOT NULL,
 incarnation TEXT NOT NULL CHECK(length(incarnation)=36),
 record_epoch INTEGER NOT NULL DEFAULT 0 CHECK(record_epoch>=0)
) STRICT;
CREATE TABLE record_versions (
 record_id TEXT NOT NULL, version INTEGER NOT NULL CHECK(version>=1),
 record_kind TEXT NOT NULL CHECK(record_kind IN('decision','constraint','vocabulary','ui_mapping','intended_architecture','state_model')),
 declared_status TEXT NOT NULL CHECK(declared_status IN('proposed','approved','revoked','superseded')),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json)),
 source_refs_json TEXT NOT NULL CHECK(json_valid(source_refs_json)),
 actor_id TEXT NOT NULL, reason TEXT NOT NULL,
 payload_hash TEXT NOT NULL CHECK(length(payload_hash)=64), created_at TEXT NOT NULL,
 PRIMARY KEY(record_id,version)
) STRICT;
CREATE TABLE record_heads (
 record_id TEXT PRIMARY KEY NOT NULL, version INTEGER NOT NULL,
 FOREIGN KEY(record_id,version) REFERENCES record_versions(record_id,version) ON DELETE RESTRICT
) STRICT;
CREATE INDEX records_kind ON record_versions(record_kind,record_id,version);
CREATE TRIGGER record_versions_immutable BEFORE UPDATE ON record_versions BEGIN
 SELECT RAISE(ABORT,'IMMUTABLE_RECORD_VERSION');
END;
CREATE TABLE mutation_receipts (
 client_mutation_id TEXT PRIMARY KEY NOT NULL,
 principal_id TEXT NOT NULL,
 request_hash TEXT NOT NULL CHECK(length(request_hash)=64),
 record_id TEXT NOT NULL, result_version INTEGER NOT NULL,
 created_at TEXT NOT NULL,
 FOREIGN KEY(record_id,result_version) REFERENCES record_versions(record_id,version) ON DELETE RESTRICT
) STRICT;
