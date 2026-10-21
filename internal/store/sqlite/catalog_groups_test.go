package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/ports"
	_ "modernc.org/sqlite"
)

func TestGroupCatalogCASRemovalAndRecreationKeepTombstoneHistory(t *testing.T) {
	home, profileID := createGroupCatalogFixture(t)
	store, err := OpenGroupCatalog(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	config := sqliteGroupConfig("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "payments", profileID)
	created, err := store.PutGroup(context.Background(), config, 0)
	if err != nil || created.Group.Revision != 1 || created.Group.Status != "active" {
		t.Fatalf("create group: snapshot=%#v err=%v", created, err)
	}
	if _, err := store.PutGroup(context.Background(), config, 0); !errors.Is(err, ports.ErrGroupConflict) {
		t.Fatalf("stale create CAS was accepted: %v", err)
	}
	removed, err := store.RemoveGroup(context.Background(), config.GroupID, 1)
	if err != nil || removed.Group.Revision != 2 || removed.Group.Status != "removed" || len(removed.Group.Config.Members) != 2 {
		t.Fatalf("remove must tombstone without deleting member history: snapshot=%#v err=%v", removed, err)
	}
	recreatedConfig := config
	recreatedConfig.Name = "payments-v2"
	recreated, err := store.PutGroup(context.Background(), recreatedConfig, 2)
	if err != nil || recreated.Group.Revision != 3 || recreated.Group.Status != "active" {
		t.Fatalf("same ID recreation must advance the tombstone revision: snapshot=%#v err=%v", recreated, err)
	}
	if _, err := store.RemoveGroup(context.Background(), config.GroupID, 1); !errors.Is(err, ports.ErrGroupConflict) {
		t.Fatalf("old revision removed recreated group: %v", err)
	}
}

func TestGroupCatalogAtomicMembersPaginationAndStaleCursor(t *testing.T) {
	home, profileID := createGroupCatalogFixture(t)
	store, err := OpenGroupCatalog(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	configs := []catalog.GroupConfig{
		sqliteGroupConfig("00000001-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "group-one", profileID),
		sqliteGroupConfig("00000002-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "group-two", profileID),
		sqliteGroupConfig("00000003-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "group-three", profileID),
	}
	for _, config := range configs {
		if _, err := store.PutGroup(context.Background(), config, 0); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ListGroups(context.Background(), 1, "")
	if err != nil || len(first.Groups) != 1 || first.Groups[0].Config.GroupID != configs[0].GroupID || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first stable page: %#v err=%v", first, err)
	}
	second, err := store.ListGroups(context.Background(), 2, first.NextCursor)
	if err != nil || len(second.Groups) != 2 || second.Groups[0].Config.GroupID != configs[1].GroupID || second.Groups[1].Config.GroupID != configs[2].GroupID || second.HasMore {
		t.Fatalf("second stable page: %#v err=%v", second, err)
	}
	if _, err := store.PutGroup(context.Background(), configs[0], 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListGroups(context.Background(), 1, first.NextCursor); !errors.Is(err, ports.ErrGroupStalePage) {
		t.Fatalf("cursor survived a catalog mutation: %v", err)
	}

	badConfig := sqliteGroupConfig("ffffffff-ffff-4fff-8fff-ffffffffffff", "invalid-reference", model.Hash(strings.Repeat("d", 64)))
	if _, err := store.PutGroup(context.Background(), badConfig, 0); err == nil {
		t.Fatal("foreign-key-invalid member was accepted")
	}
	if _, err := store.GetGroup(context.Background(), badConfig.GroupID); !errors.Is(err, ports.ErrGroupNotFound) {
		t.Fatalf("failed multi-row write left a group row behind: %v", err)
	}
}

func TestGroupCatalogConcurrentCASHasOneWinner(t *testing.T) {
	home, profileID := createGroupCatalogFixture(t)
	store, err := OpenGroupCatalog(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := sqliteGroupConfig("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "payments", profileID)
	if _, err := store.PutGroup(context.Background(), config, 0); err != nil {
		t.Fatal(err)
	}
	first, second := config, config
	first.Name, second.Name = "payments-first", "payments-second"
	var wait sync.WaitGroup
	wait.Add(2)
	results := make(chan error, 2)
	for _, candidate := range []catalog.GroupConfig{first, second} {
		candidate := candidate
		go func() {
			defer wait.Done()
			_, err := store.PutGroup(context.Background(), candidate, 1)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ports.ErrGroupConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent CAS error: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("concurrent CAS winners=%d conflicts=%d", winners, conflicts)
	}
}

func createGroupCatalogFixture(t *testing.T) (string, model.Hash) {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "catalog.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE schema_meta(singleton INTEGER PRIMARY KEY CHECK(singleton=1),schema_version INTEGER NOT NULL,min_reader_version INTEGER NOT NULL) STRICT`,
		`INSERT INTO schema_meta VALUES(1,1,1)`,
		`CREATE TABLE catalog_identity(singleton INTEGER PRIMARY KEY CHECK(singleton=1),incarnation TEXT NOT NULL) STRICT`,
		`INSERT INTO catalog_identity VALUES(1,'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee')`,
		`CREATE TABLE repositories(repo_id TEXT PRIMARY KEY,common_dir TEXT UNIQUE,filesystem_identity TEXT UNIQUE,created_at TEXT NOT NULL) STRICT`,
		`CREATE TABLE worktrees(worktree_id TEXT PRIMARY KEY,repo_id TEXT NOT NULL REFERENCES repositories(repo_id) ON DELETE RESTRICT,canonical_root TEXT UNIQUE,filesystem_identity TEXT UNIQUE,binding_revision INTEGER NOT NULL,status TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL) STRICT`,
		`CREATE TABLE profiles(worktree_id TEXT NOT NULL REFERENCES worktrees(worktree_id) ON DELETE RESTRICT,profile_id TEXT NOT NULL,config_json TEXT NOT NULL,toolchain_digest TEXT NOT NULL,provider_digest TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(worktree_id,profile_id)) STRICT`,
		`CREATE TABLE repo_groups(group_id TEXT PRIMARY KEY NOT NULL,name TEXT NOT NULL UNIQUE,status TEXT NOT NULL DEFAULT 'active' CHECK(status IN('active','removed')),revision INTEGER NOT NULL CHECK(revision>=1),created_at TEXT NOT NULL,updated_at TEXT NOT NULL) STRICT`,
		`CREATE TABLE group_members(group_id TEXT NOT NULL REFERENCES repo_groups(group_id) ON DELETE CASCADE,service_id TEXT NOT NULL,worktree_id TEXT NOT NULL,profile_id TEXT NOT NULL,namespace_json TEXT NOT NULL CHECK(json_valid(namespace_json)),PRIMARY KEY(group_id,service_id),FOREIGN KEY(worktree_id,profile_id) REFERENCES profiles(worktree_id,profile_id) ON DELETE RESTRICT) STRICT`,
		`INSERT INTO repositories VALUES('11111111-1111-4111-8111-111111111111','/repo','repo-fs','2026-01-01T00:00:00Z')`,
		`INSERT INTO worktrees VALUES('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','11111111-1111-4111-8111-111111111111','/repo','tree-fs',1,'registered','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
	}
	profileID := model.Hash(strings.Repeat("c", 64))
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fixture statement %q: %v", statement, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO profiles VALUES(?,?,?,?,?,?)`, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", string(profileID), `{}`, strings.Repeat("1", 64), strings.Repeat("2", 64), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(fmt.Errorf("protect fixture catalog: %w", err))
	}
	return home, profileID
}

func sqliteGroupConfig(id, name string, profileID model.Hash) catalog.GroupConfig {
	member := catalog.GroupMemberConfig{WorktreeID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ProfileID: profileID,
		Namespace: catalog.ServiceNamespace{Environment: "prod", Protocol: "http"}}
	first, second := member, member
	first.ServiceID, second.ServiceID = "payments-api", "payments-worker"
	return catalog.GroupConfig{GroupID: model.UUID(id), Name: name, Members: []catalog.GroupMemberConfig{first, second}}
}
