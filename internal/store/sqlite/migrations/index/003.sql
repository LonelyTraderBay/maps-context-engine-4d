CREATE TABLE domain_entities (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 entity_id TEXT NOT NULL, kind TEXT NOT NULL, qualified_key TEXT NOT NULL,
 plane TEXT NOT NULL CHECK(plane IN('intended','observed_static')),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json)),
 source_refs_json TEXT NOT NULL CHECK(json_valid(source_refs_json)),
 PRIMARY KEY(generation_id,entity_id)
) STRICT;
CREATE INDEX entities_kind ON domain_entities(generation_id,kind,qualified_key,entity_id);
CREATE TABLE entity_links (
 generation_id TEXT NOT NULL REFERENCES generations(generation_id) ON DELETE CASCADE,
 link_id TEXT NOT NULL, source_symbol_id TEXT, source_entity_id TEXT,
 target_symbol_id TEXT, target_entity_id TEXT, kind TEXT NOT NULL,
 evidence_json TEXT NOT NULL CHECK(json_valid(evidence_json)),
 resolution_status TEXT NOT NULL CHECK(resolution_status IN('exact_in_supported_scope','candidate','unresolved')),
 PRIMARY KEY(generation_id,link_id),
 FOREIGN KEY(generation_id,source_symbol_id) REFERENCES symbols(generation_id,symbol_id) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,target_symbol_id) REFERENCES symbols(generation_id,symbol_id) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,source_entity_id) REFERENCES domain_entities(generation_id,entity_id) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,target_entity_id) REFERENCES domain_entities(generation_id,entity_id) ON DELETE RESTRICT,
 CHECK((source_symbol_id IS NOT NULL)+(source_entity_id IS NOT NULL)=1),
 CHECK((target_symbol_id IS NOT NULL)+(target_entity_id IS NOT NULL)=1)
) STRICT;
CREATE INDEX entity_links_out ON entity_links(generation_id,source_entity_id,kind,link_id);
CREATE INDEX entity_links_in ON entity_links(generation_id,target_entity_id,kind,link_id);
CREATE INDEX symbol_entity_out ON entity_links(generation_id,source_symbol_id,kind,link_id);
CREATE INDEX symbol_entity_in ON entity_links(generation_id,target_symbol_id,kind,link_id);
CREATE TABLE contract_fields (
 generation_id TEXT NOT NULL, entity_id TEXT NOT NULL, field_path TEXT NOT NULL,
 direction TEXT NOT NULL CHECK(direction IN('input','output','bidirectional','stored')),
 field_json TEXT NOT NULL CHECK(json_valid(field_json)),
 PRIMARY KEY(generation_id,entity_id,field_path,direction),
 FOREIGN KEY(generation_id,entity_id) REFERENCES domain_entities(generation_id,entity_id) ON DELETE CASCADE
) STRICT;
CREATE TABLE state_transitions (
 generation_id TEXT NOT NULL, machine_entity_id TEXT NOT NULL,
 transition_id TEXT NOT NULL, from_entity_id TEXT NOT NULL, to_entity_id TEXT NOT NULL,
 guard_ref_json TEXT CHECK(guard_ref_json IS NULL OR json_valid(guard_ref_json)),
 effect_refs_json TEXT NOT NULL CHECK(json_valid(effect_refs_json)),
 evidence_json TEXT NOT NULL CHECK(json_valid(evidence_json)),
 PRIMARY KEY(generation_id,machine_entity_id,transition_id),
 FOREIGN KEY(generation_id,machine_entity_id) REFERENCES domain_entities(generation_id,entity_id) ON DELETE CASCADE,
 FOREIGN KEY(generation_id,from_entity_id) REFERENCES domain_entities(generation_id,entity_id) ON DELETE RESTRICT,
 FOREIGN KEY(generation_id,to_entity_id) REFERENCES domain_entities(generation_id,entity_id) ON DELETE RESTRICT
) STRICT;
UPDATE schema_meta SET schema_version=3,min_reader_version=3 WHERE singleton=1;
CREATE TRIGGER domain_entities_insert_frozen BEFORE INSERT ON domain_entities
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER domain_entities_update_frozen BEFORE UPDATE ON domain_entities
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER domain_entities_delete_frozen BEFORE DELETE ON domain_entities
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER entity_links_insert_frozen BEFORE INSERT ON entity_links
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER entity_links_update_frozen BEFORE UPDATE ON entity_links
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER entity_links_delete_frozen BEFORE DELETE ON entity_links
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER contract_fields_insert_frozen BEFORE INSERT ON contract_fields
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER contract_fields_update_frozen BEFORE UPDATE ON contract_fields
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER contract_fields_delete_frozen BEFORE DELETE ON contract_fields
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER state_transitions_insert_frozen BEFORE INSERT ON state_transitions
WHEN (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER state_transitions_update_frozen BEFORE UPDATE ON state_transitions
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id)!='BUILDING'
 OR (SELECT state FROM generations WHERE generation_id=new.generation_id)!='BUILDING'
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
CREATE TRIGGER state_transitions_delete_frozen BEFORE DELETE ON state_transitions
WHEN (SELECT state FROM generations WHERE generation_id=old.generation_id) IN('READY','READY_PARTIAL')
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_GENERATION'); END;
