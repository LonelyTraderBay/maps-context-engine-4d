CREATE TABLE modules (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 module_id TEXT NOT NULL, parent_module_id TEXT,
 label TEXT NOT NULL, kind TEXT NOT NULL,
 plane TEXT NOT NULL CHECK(plane IN('intended','observed_static')),
 source_path TEXT, start_byte INTEGER, end_byte INTEGER,
 PRIMARY KEY(generation_id,module_id),
 FOREIGN KEY(generation_id,parent_module_id) REFERENCES modules(generation_id,module_id) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,source_path) REFERENCES files(generation_id,path) ON DELETE RESTRICT
) STRICT;
CREATE TABLE module_members (
 generation_id TEXT NOT NULL, module_id TEXT NOT NULL, package_key TEXT NOT NULL,
 PRIMARY KEY(generation_id,module_id,package_key),
 FOREIGN KEY(generation_id,module_id) REFERENCES modules(generation_id,module_id) ON DELETE CASCADE,
 FOREIGN KEY(generation_id,package_key) REFERENCES packages(generation_id,package_key) ON DELETE RESTRICT
) STRICT;
CREATE INDEX module_members_reverse ON module_members(generation_id,package_key,module_id);
UPDATE schema_meta SET schema_version=2,min_reader_version=2 WHERE singleton=1;
CREATE TRIGGER modules_insert_frozen BEFORE INSERT ON modules
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER modules_update_frozen BEFORE UPDATE ON modules
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER modules_delete_frozen BEFORE DELETE ON modules
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER module_members_insert_frozen BEFORE INSERT ON module_members
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER module_members_update_frozen BEFORE UPDATE ON module_members
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER module_members_delete_frozen BEFORE DELETE ON module_members
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
