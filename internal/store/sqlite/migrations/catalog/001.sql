PRAGMA foreign_keys=ON;
CREATE TABLE schema_meta (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 schema_version INTEGER NOT NULL CHECK(schema_version>=1),
 min_reader_version INTEGER NOT NULL CHECK(min_reader_version>=1)
) STRICT;
INSERT INTO schema_meta VALUES(1,1,1);
CREATE TABLE catalog_identity (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 incarnation TEXT NOT NULL CHECK(length(incarnation)=36)
) STRICT;
CREATE TABLE repositories (
 repo_id TEXT PRIMARY KEY NOT NULL,
 common_dir TEXT NOT NULL UNIQUE,
 filesystem_identity TEXT NOT NULL UNIQUE,
 created_at TEXT NOT NULL
) STRICT;
CREATE TABLE worktrees (
 worktree_id TEXT PRIMARY KEY NOT NULL,
 repo_id TEXT NOT NULL REFERENCES repositories(repo_id) ON DELETE RESTRICT,
 canonical_root TEXT NOT NULL UNIQUE,
 filesystem_identity TEXT NOT NULL UNIQUE,
 binding_revision INTEGER NOT NULL DEFAULT 1 CHECK(binding_revision>=1),
 status TEXT NOT NULL CHECK(status IN ('registered','unavailable','removed')),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX worktrees_repo ON worktrees(repo_id,status);
CREATE TABLE policies (
 worktree_id TEXT NOT NULL REFERENCES worktrees(worktree_id) ON DELETE RESTRICT,
 policy_digest TEXT NOT NULL CHECK(length(policy_digest)=64),
 config_json TEXT NOT NULL CHECK(json_valid(config_json)),
 created_at TEXT NOT NULL,
 PRIMARY KEY(worktree_id,policy_digest)
) STRICT;
CREATE TABLE worktree_policy_heads (
 worktree_id TEXT PRIMARY KEY NOT NULL,
 policy_digest TEXT NOT NULL,
 policy_epoch INTEGER NOT NULL CHECK(policy_epoch>=1),
 updated_at TEXT NOT NULL,
 FOREIGN KEY(worktree_id,policy_digest) REFERENCES policies(worktree_id,policy_digest) ON DELETE RESTRICT
) STRICT;
CREATE TABLE profiles (
 worktree_id TEXT NOT NULL REFERENCES worktrees(worktree_id) ON DELETE RESTRICT,
 profile_id TEXT NOT NULL CHECK(length(profile_id)=64),
 config_json TEXT NOT NULL CHECK(json_valid(config_json)),
 toolchain_digest TEXT NOT NULL CHECK(length(toolchain_digest)=64),
 provider_digest TEXT NOT NULL CHECK(length(provider_digest)=64),
 created_at TEXT NOT NULL,
 PRIMARY KEY(worktree_id,profile_id)
) STRICT;
CREATE TABLE profile_heads (
 worktree_id TEXT NOT NULL,
 profile_name TEXT NOT NULL,
 profile_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=1),
 PRIMARY KEY(worktree_id,profile_name),
 FOREIGN KEY(worktree_id,profile_id) REFERENCES profiles(worktree_id,profile_id) ON DELETE RESTRICT
) STRICT;
CREATE TABLE repo_groups (
 group_id TEXT PRIMARY KEY NOT NULL,
 name TEXT NOT NULL UNIQUE,
 status TEXT NOT NULL DEFAULT 'active' CHECK(status IN('active','removed')),
 revision INTEGER NOT NULL CHECK(revision>=1),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
) STRICT;
CREATE TABLE group_members (
 group_id TEXT NOT NULL REFERENCES repo_groups(group_id) ON DELETE CASCADE,
 service_id TEXT NOT NULL,
 worktree_id TEXT NOT NULL,
 profile_id TEXT NOT NULL,
 namespace_json TEXT NOT NULL CHECK(json_valid(namespace_json)),
 PRIMARY KEY(group_id,service_id),
 FOREIGN KEY(worktree_id,profile_id) REFERENCES profiles(worktree_id,profile_id) ON DELETE RESTRICT
) STRICT;
CREATE INDEX group_members_scope ON group_members(worktree_id,profile_id);
CREATE TRIGGER policies_immutable BEFORE UPDATE ON policies BEGIN SELECT RAISE(ABORT,'IMMUTABLE_POLICY'); END;
CREATE TRIGGER profiles_immutable BEFORE UPDATE ON profiles BEGIN SELECT RAISE(ABORT,'IMMUTABLE_PROFILE'); END;
CREATE TABLE preview_permission_versions (
 profile_id TEXT NOT NULL, version INTEGER NOT NULL CHECK(version>=1),
 declared_status TEXT NOT NULL CHECK(declared_status IN('approved','revoked')),
 config_json TEXT NOT NULL CHECK(json_valid(config_json)),
 config_digest TEXT NOT NULL CHECK(length(config_digest)=64),
 actor_id TEXT NOT NULL, reason TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(profile_id,version)
) STRICT;
CREATE TABLE preview_permission_heads (
 profile_id TEXT PRIMARY KEY NOT NULL, version INTEGER NOT NULL,
 FOREIGN KEY(profile_id,version) REFERENCES preview_permission_versions(profile_id,version) ON DELETE RESTRICT
) STRICT;
CREATE TRIGGER preview_permissions_immutable BEFORE UPDATE ON preview_permission_versions
BEGIN SELECT RAISE(ABORT,'IMMUTABLE_PERMISSION_VERSION'); END;

