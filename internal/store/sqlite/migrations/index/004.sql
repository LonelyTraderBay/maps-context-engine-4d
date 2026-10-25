CREATE TABLE ui_build_maps (
 build_map_id TEXT PRIMARY KEY NOT NULL CHECK(length(build_map_id)=64),
 schema_version INTEGER NOT NULL CHECK(schema_version=1),
 profile_id TEXT NOT NULL, provider_version TEXT NOT NULL,
 provider_digest TEXT NOT NULL CHECK(length(provider_digest)=64),
 build_config_digest TEXT NOT NULL CHECK(length(build_config_digest)=64),
 input_manifest_digest TEXT NOT NULL CHECK(length(input_manifest_digest)=64),
 mode TEXT NOT NULL CHECK(mode IN('development','test')),
 artifact_hash TEXT NOT NULL CHECK(length(artifact_hash)=64),
 metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json)), created_at TEXT NOT NULL
) STRICT;
CREATE TABLE ui_source_anchors (
 build_map_id TEXT NOT NULL REFERENCES ui_build_maps(build_map_id) ON DELETE CASCADE,
 anchor_id TEXT NOT NULL CHECK(length(anchor_id)=64),
 kind TEXT NOT NULL CHECK(kind IN('element_site','component_definition','component_render_site','event_binding_site')),
 path TEXT NOT NULL, source_hash TEXT NOT NULL CHECK(length(source_hash)=64),
 start_byte INTEGER NOT NULL CHECK(start_byte>=0), end_byte INTEGER NOT NULL CHECK(end_byte>=start_byte),
 owner_anchor_id TEXT,
 PRIMARY KEY(build_map_id,anchor_id),
 FOREIGN KEY(build_map_id,owner_anchor_id) REFERENCES ui_source_anchors(build_map_id,anchor_id) DEFERRABLE INITIALLY DEFERRED
) STRICT;
CREATE INDEX anchors_source ON ui_source_anchors(build_map_id,path,start_byte,end_byte);
CREATE TABLE ui_build_bindings (
 binding_id TEXT PRIMARY KEY NOT NULL CHECK(length(binding_id)=64),
 build_map_id TEXT NOT NULL REFERENCES ui_build_maps(build_map_id) ON DELETE CASCADE,
 profile_id TEXT NOT NULL, source_generation_id TEXT NOT NULL,
 policy_digest TEXT NOT NULL CHECK(length(policy_digest)=64),
 resolver_digest TEXT NOT NULL CHECK(length(resolver_digest)=64),
 evidence_digest TEXT NOT NULL CHECK(length(evidence_digest)=64),
 created_at TEXT NOT NULL,
 UNIQUE(binding_id,build_map_id),
 UNIQUE(build_map_id,profile_id,source_generation_id,policy_digest,resolver_digest),
 FOREIGN KEY(profile_id,source_generation_id) REFERENCES generations(profile_id,generation_id) ON DELETE RESTRICT
) STRICT;
CREATE INDEX build_bindings_gen ON ui_build_bindings(source_generation_id,build_map_id);
CREATE TABLE ui_anchor_links (
 binding_id TEXT NOT NULL, build_map_id TEXT NOT NULL, link_id TEXT NOT NULL, anchor_id TEXT NOT NULL,
 target_ref_json TEXT NOT NULL CHECK(json_valid(target_ref_json)),
 kind TEXT NOT NULL, resolution_status TEXT NOT NULL,
 evidence_json TEXT NOT NULL CHECK(json_valid(evidence_json)),
 PRIMARY KEY(binding_id,link_id),
 FOREIGN KEY(binding_id,build_map_id) REFERENCES ui_build_bindings(binding_id,build_map_id) ON DELETE CASCADE,
 FOREIGN KEY(build_map_id,anchor_id) REFERENCES ui_source_anchors(build_map_id,anchor_id) ON DELETE CASCADE
) STRICT;
CREATE INDEX anchors_reverse ON ui_anchor_links(binding_id,anchor_id,link_id);
UPDATE schema_meta SET schema_version=4,min_reader_version=4 WHERE singleton=1;
CREATE TRIGGER ui_build_maps_immutable BEFORE UPDATE ON ui_build_maps BEGIN SELECT RAISE(ABORT,'IMMUTABLE_UI_METADATA'); END;
CREATE TRIGGER ui_source_anchors_immutable BEFORE UPDATE ON ui_source_anchors BEGIN SELECT RAISE(ABORT,'IMMUTABLE_UI_METADATA'); END;
CREATE TRIGGER ui_build_bindings_immutable BEFORE UPDATE ON ui_build_bindings BEGIN SELECT RAISE(ABORT,'IMMUTABLE_UI_METADATA'); END;
CREATE TRIGGER ui_anchor_links_immutable BEFORE UPDATE ON ui_anchor_links BEGIN SELECT RAISE(ABORT,'IMMUTABLE_UI_METADATA'); END;
CREATE TRIGGER ui_anchors_bound_insert BEFORE INSERT ON ui_source_anchors
WHEN EXISTS(SELECT 1 FROM ui_build_bindings WHERE build_map_id=new.build_map_id)
BEGIN SELECT RAISE(ABORT,'BUILD_MAP_ALREADY_BOUND'); END;
