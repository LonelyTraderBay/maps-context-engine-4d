package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

func invoke(args []string, stdin string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(nil, args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeCandidate(t *testing.T, dir, name, raw string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeConfigData(t *testing.T, raw string) runtimeconfig.RuntimeConfigData {
	t.Helper()
	var data runtimeconfig.RuntimeConfigData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("decode JSON result %q: %v", raw, err)
	}
	return data
}

func TestConfigValidateIsWholeCandidateAndDoesNotWriteHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "missing-home")
	t.Setenv("MCE4D_HOME", home)
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(home, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	old := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"max_indexed_files":100}}`)
	if err := store.CompareAndSwapCanonical(nilContext(), nil, old); err != nil {
		t.Fatal(err)
	}
	candidatePath := writeCandidate(t, root, "candidate.json", `{"schema_version":1,"core":{"query_timeout_ms":2200}}`)
	before, _, err := store.ReadCanonical(nilContext())
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := invoke([]string{"config", "validate", "--json", "--input-json", candidatePath}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("validate: code=%d stderr=%q", code, stderr)
	}
	data := decodeConfigData(t, stdout)
	if data.SourceKind != "canonical_home" || len(data.EffectiveSources) != 2 || data.EffectiveSources[1] != "candidate_input" {
		t.Fatalf("candidate provenance = %#v", data)
	}
	if data.Config.Core.MaxIndexedFiles == 100 || data.Config.Core.QueryTimeoutMS != 2200 {
		t.Fatalf("validate merged the old home config or lost the candidate: %#v", data.Config.Core)
	}
	after, present, err := store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(before, after) {
		t.Fatalf("validate changed canonical home bytes: present=%v err=%v", present, err)
	}
}

func TestGroupCLIUsesStrictRequestsAndRevisionCAS(t *testing.T) {
	home := createCLIGroupCatalog(t)
	t.Setenv("MCE4D_HOME", home)
	profileID := model.Hash(strings.Repeat("c", 64))
	first := cliGroupConfig("00000001-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "payments", profileID)
	second := cliGroupConfig("00000002-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "orders", profileID)

	code, stdout, stderr := invokeGroupCLI(t, "put", map[string]any{"action": "put", "expected_revision": 0, "config": first})
	if code != 0 || stderr != "" {
		t.Fatalf("group put: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if response := decodeGroupCLIResponse(t, stdout); response.Data.Groups[0].Revision != 1 || response.Data.Groups[0].Status != "active" {
		t.Fatalf("group put response: %#v", response.Data.Groups)
	}
	code, stdout, stderr = invokeGroupCLI(t, "put", map[string]any{"action": "put", "expected_revision": 0, "config": second})
	if code != 0 || stderr != "" {
		t.Fatalf("second group put: code=%d stderr=%q", code, stderr)
	}

	code, stdout, stderr = invokeGroupCLI(t, "get", map[string]any{"action": "get", "group_id": first.GroupID})
	if code != 0 || stderr != "" || decodeGroupCLIResponse(t, stdout).Data.Groups[0].Config.GroupID != first.GroupID {
		t.Fatalf("group get: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	code, stdout, stderr = invokeGroupCLI(t, "list", map[string]any{"action": "list", "limit": 1})
	if code != 0 || stderr != "" {
		t.Fatalf("group list: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	page := decodeGroupCLIResponse(t, stdout)
	if !page.Pagination.Truncated || page.Pagination.NextCursor == nil || len(page.Data.Groups) != 1 {
		t.Fatalf("group list pagination: %#v", page)
	}

	updated := first
	updated.Name = "payments-v2"
	code, stdout, stderr = invokeGroupCLI(t, "put", map[string]any{"action": "put", "expected_revision": 1, "config": updated})
	if code != 0 || stderr != "" || decodeGroupCLIResponse(t, stdout).Data.Groups[0].Revision != 2 {
		t.Fatalf("group CAS update: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	code, _, stderr = invokeGroupCLI(t, "list", map[string]any{"action": "list", "limit": 1, "cursor": *page.Pagination.NextCursor})
	if code != 3 || !strings.Contains(stderr, "stale") {
		t.Fatalf("catalog mutation did not invalidate the list cursor: code=%d stderr=%q", code, stderr)
	}

	code, stdout, stderr = invokeGroupCLI(t, "remove", map[string]any{"action": "remove", "group_id": first.GroupID, "expected_revision": 2})
	if code != 0 || stderr != "" {
		t.Fatalf("group remove: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	removed := decodeGroupCLIResponse(t, stdout).Data.Groups[0]
	if removed.Status != "removed" || removed.Revision != 3 || len(removed.Config.Members) != 2 {
		t.Fatalf("group removal did not retain its tombstone and members: %#v", removed)
	}
	code, _, stderr = invokeGroupCLI(t, "remove", map[string]any{"action": "remove", "group_id": first.GroupID, "expected_revision": 2})
	if code != 3 || !strings.Contains(stderr, "conflict") {
		t.Fatalf("stale group removal was accepted: code=%d stderr=%q", code, stderr)
	}

	code, _, stderr = invokeGroupCLI(t, "list", map[string]any{"action": "list", "unexpected": true})
	if code != 2 || !strings.Contains(stderr, "contract") {
		t.Fatalf("unknown group field was accepted: code=%d stderr=%q", code, stderr)
	}
	code, _, stderr = invokeGroupCLI(t, "get", map[string]any{"action": "list"})
	if code != 2 || !strings.Contains(stderr, "action") {
		t.Fatalf("action mismatch was accepted: code=%d stderr=%q", code, stderr)
	}
}

func TestGroupAdminRemainsCLIOnly(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	operations := validator.Operations()
	if _, ok := operations["CLI group"]; !ok {
		t.Fatal("CLI group contract is missing")
	}
	for name := range operations {
		if strings.Contains(strings.ToLower(name), "group") && name != "CLI group" {
			t.Fatalf("group admin was exposed outside the CLI: %q", name)
		}
	}
}

type groupCLIResponse struct {
	Data struct {
		Groups []catalog.GroupSummary `json:"groups"`
	} `json:"data"`
	Pagination struct {
		Truncated  bool    `json:"truncated"`
		NextCursor *string `json:"next_cursor"`
	} `json:"pagination"`
}

func invokeGroupCLI(t *testing.T, action string, request any) (int, string, string) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return invoke([]string{"group", action, "--input-json", "-"}, string(raw))
}

func decodeGroupCLIResponse(t *testing.T, raw string) groupCLIResponse {
	t.Helper()
	var response groupCLIResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("decode group response: %v: %s", err, raw)
	}
	return response
}

func cliGroupConfig(id, name string, profileID model.Hash) catalog.GroupConfig {
	member := catalog.GroupMemberConfig{WorktreeID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ProfileID: profileID,
		Namespace: catalog.ServiceNamespace{Environment: "prod", Protocol: "http"}}
	first, second := member, member
	first.ServiceID, second.ServiceID = "payments-api", "payments-worker"
	return catalog.GroupConfig{GroupID: model.UUID(id), Name: name, Members: []catalog.GroupMemberConfig{first, second}}
}

func createCLIGroupCatalog(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "catalog.sqlite"))
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
		`INSERT INTO profiles VALUES('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb','cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc','{}','1111111111111111111111111111111111111111111111111111111111111111','2222222222222222222222222222222222222222222222222222222222222222','2026-01-01T00:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("create CLI group catalog fixture: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestConfigShowAlternateAndOverridesKeepCanonicalSource(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("MCE4D_HOME", home)
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(home, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	homeCandidate := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"max_indexed_files":300}}`)
	if err := store.CompareAndSwapCanonical(nilContext(), nil, homeCandidate); err != nil {
		t.Fatal(err)
	}
	before, _, err := store.ReadCanonical(nilContext())
	if err != nil {
		t.Fatal(err)
	}
	alternate := writeCandidate(t, root, "alternate.json", `{"schema_version":1,"core":{"query_timeout_ms":1600}}`)
	code, stdout, stderr := invoke([]string{"config", "show", "--json", "--alternate-json", alternate, "--override", "query_timeout_ms=1800"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("show: code=%d stderr=%q", code, stderr)
	}
	data := decodeConfigData(t, stdout)
	homeSource, err := pipeline.ParseSource(homeCandidate, true)
	if err != nil {
		t.Fatal(err)
	}
	if data.SourceKind != "canonical_home" || data.SourceDigest == nil || *data.SourceDigest != *homeSource.Digest() {
		t.Fatalf("canonical source provenance = %#v", data)
	}
	if data.Config.Core.MaxIndexedFiles != 300 || data.Config.Core.QueryTimeoutMS != 1800 {
		t.Fatalf("effective layer values = %#v", data.Config.Core)
	}
	want := []string{"built_in", "canonical_home", "approved_alternate", "cli_process"}
	if strings.Join(data.EffectiveSources, ",") != strings.Join(want, ",") {
		t.Fatalf("effective sources = %v, want %v", data.EffectiveSources, want)
	}
	after, present, err := store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(before, after) {
		t.Fatalf("show changed canonical home bytes: present=%v err=%v", present, err)
	}
}

func TestConfigSetUsesExpectedDigestAndPreservesBytesOnConflict(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("MCE4D_HOME", home)
	pipeline := testPipeline(t)
	store, err := newHomeConfigStore(home, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	initial := canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":1300}}`)
	if err := store.CompareAndSwapCanonical(nilContext(), nil, initial); err != nil {
		t.Fatal(err)
	}
	candidate := writeCandidate(t, root, "candidate.json", `{"schema_version":1,"core":{"query_timeout_ms":2400}}`)
	before, _, err := store.ReadCanonical(nilContext())
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := invoke([]string{"config", "set", "--json", "--input-json", candidate, "--expected-digest", "absent"}, "")
	if code != 3 || stdout != "" || stderr == "" {
		t.Fatalf("stale set: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	after, present, err := store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(before, after) {
		t.Fatalf("stale set changed file bytes: present=%v err=%v", present, err)
	}
	current, err := pipeline.ParseSource(before, true)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = invoke([]string{"config", "set", "--json", "--input-json", candidate, "--expected-digest", string(*current.Digest())}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("set with current digest: code=%d stderr=%q", code, stderr)
	}
	data := decodeConfigData(t, stdout)
	if data.Config.Core.QueryTimeoutMS != 2400 || data.SourceKind != "canonical_home" || data.SourceDigest == nil {
		t.Fatalf("set result = %#v", data)
	}
	after, present, err = store.ReadCanonical(nilContext())
	if err != nil || !present || !bytes.Equal(after, canonicalCandidate(t, pipeline, `{"schema_version":1,"core":{"query_timeout_ms":2400}}`)) {
		t.Fatalf("successful set did not replace the complete canonical source: present=%v err=%v", present, err)
	}
}

func TestConfigValidateFromStdinAndInputLimit(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent-home")
	t.Setenv("MCE4D_HOME", home)
	code, stdout, stderr := invoke([]string{"config", "validate", "--json", "--input-json", "-"}, `{"schema_version":1,"core":{"query_timeout_ms":2000}}`)
	if code != 0 || stderr != "" || decodeConfigData(t, stdout).Config.Core.QueryTimeoutMS != 2000 {
		t.Fatalf("stdin validate: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("validate created the MCE4D home: stat err=%v", err)
	}
	tooLarge := strings.Repeat(" ", maxRuntimeConfigBytes+1)
	code, stdout, stderr = invoke([]string{"config", "validate", "--json", "--input-json", "-"}, tooLarge)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "1 MiB") {
		t.Fatalf("oversized stdin: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestHelpVersionAndUnknownArguments(t *testing.T) {
	code, stdout, stderr := invoke([]string{"version", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("version: code=%d stderr=%q", code, stderr)
	}
	var versionResult map[string]any
	if err := json.Unmarshal([]byte(stdout), &versionResult); err != nil || versionResult["version"] != version {
		t.Fatalf("version JSON = %q (parse error %v)", stdout, err)
	}
	code, stdout, stderr = invoke([]string{"config", "set", "--json", "--input-json", "-", "--bogus"}, `{}`)
	if code != 2 || stdout != "" || stderr == "" {
		t.Fatalf("unknown flag: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = invoke([]string{"--help"}, "")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "config validate") {
		t.Fatalf("help: code=%d stderr=%q", code, stderr)
	}
}

func TestDoctorReportsSQLiteAndDoesNotCreateHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("MCE4D_HOME", home)
	code, stdout, stderr := invoke([]string{"doctor", "--json"}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor: code=%d stderr=%q", code, stderr)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor JSON %q: %v", stdout, err)
	}
	if report.Operation != "doctor" || report.Platform.OS == "" || report.SQLiteFTS5.Available == false && report.SQLiteFTS5.Issue == "" {
		t.Fatalf("doctor omitted platform or FTS status: %#v", report)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("doctor created the MCE4D home: stat err=%v", err)
	}
}

func TestHelpCommandsArePresentInCLIReference(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "CLI.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"mce4d version --json",
		"mce4d doctor --json",
		"mce4d config show [--json] [--alternate-json <file>] [--override <key=value>...]",
		"mce4d config validate --input-json <file-or-dash>",
		"mce4d config set --input-json <file-or-dash> --expected-digest <sha256-or-absent>",
		"mce4d store backup --target worktree --worktree-id <uuid> --output <owned-backup-dir> --json",
		"mce4d store gc --target worktree --worktree-id <uuid> --keep-per-profile <count> --json [--apply]",
		"mce4d store restore --manifest <approved-local-file> --confirm --wait-ms <bounded> --json",
		"mce4d store recover --target worktree --worktree-id <uuid> --report-only --json",
		"mce4d store recover --target worktree --worktree-id <uuid> --apply --confirm [--wait-ms <bounded>] --json",
	} {
		if !strings.Contains(Help(), command) || !strings.Contains(string(docs), command) {
			t.Errorf("help and docs must both include %q", command)
		}
	}
}

func TestStoreBackupCLIRequiresAndWritesOwnedOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MCE4D_HOME", home)
	worktreeID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	incarnation := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	indexDirectory := filepath.Join(home, "worktrees", worktreeID)
	if err := os.MkdirAll(indexDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(indexDirectory, "index.sqlite")
	indexDB, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{"001.sql", "002.sql"} {
		ddl, err := os.ReadFile(filepath.Join("..", "store", "sqlite", "migrations", "index", migration))
		if err != nil {
			_ = indexDB.Close()
			t.Fatal(err)
		}
		if _, err := indexDB.ExecContext(context.Background(), string(ddl)); err != nil {
			_ = indexDB.Close()
			t.Fatalf("apply index fixture migration %s: %v", migration, err)
		}
	}
	if _, err := indexDB.ExecContext(context.Background(), `INSERT INTO store_identity(singleton,worktree_id,incarnation) VALUES(1,?,?)`, worktreeID, incarnation); err != nil {
		_ = indexDB.Close()
		t.Fatalf("bind index fixture identity: %v", err)
	}
	if err := indexDB.Close(); err != nil {
		t.Fatal(err)
	}

	operationID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	output := filepath.Join(indexDirectory, "backups", operationID)
	code, stdout, stderr := invoke([]string{
		"store", "backup", "--target", "worktree", "--worktree-id", worktreeID,
		"--output", output, "--json",
	}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("store backup: code=%d stderr=%q", code, stderr)
	}
	var result worktreeBackupCLIResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode worktree backup result %q: %v", stdout, err)
	}
	if result.Operation != "store_backup" || result.Output != output || result.Manifest.OperationID != model.UUID(operationID) ||
		result.Manifest.Consistency != "single_store" || len(result.Manifest.Stores) != 1 {
		t.Fatalf("worktree backup CLI result=%+v", result)
	}
	manifestPath := filepath.Join(output, "manifest.json")
	restoreArgs := []string{"store", "restore", "--manifest", manifestPath, "--wait-ms", "5000", "--json"}
	if code, _, stderr := invoke(restoreArgs, ""); code == 0 || stderr == "" {
		t.Fatalf("store restore without confirmation: code=%d stderr=%q", code, stderr)
	}
	code, stdout, stderr = invoke(append(restoreArgs, "--confirm"), "")
	if code != 0 || stderr != "" {
		t.Fatalf("store restore: code=%d stderr=%q", code, stderr)
	}
	var restored worktreeRestoreCLIResult
	if err := json.Unmarshal([]byte(stdout), &restored); err != nil {
		t.Fatalf("decode store restore result %q: %v", stdout, err)
	}
	if restored.Operation != "store_restore" || restored.Result.Phase != "VERIFIED" || restored.Result.Outcome != "restored" ||
		restored.Result.WorktreeID != model.UUID(worktreeID) || restored.Result.OperationID == "" {
		t.Fatalf("store restore result=%+v", restored)
	}
	indexDB, err = sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var restoredIncarnation string
	if err := indexDB.QueryRowContext(context.Background(), `SELECT incarnation FROM store_identity WHERE singleton=1`).Scan(&restoredIncarnation); err != nil {
		_ = indexDB.Close()
		t.Fatal(err)
	}
	if err := indexDB.Close(); err != nil {
		t.Fatal(err)
	}
	if restoredIncarnation == string(result.Manifest.Stores[0].Incarnation) {
		t.Fatalf("store restore reused backup incarnation %s", restoredIncarnation)
	}
	code, stdout, stderr = invoke([]string{
		"store", "recover", "--target", "worktree", "--worktree-id", worktreeID, "--report-only", "--json",
	}, "")
	if code != 0 || stderr != "" {
		t.Fatalf("store recover report-only: code=%d stderr=%q", code, stderr)
	}
	var recovery worktreeRecoveryCLIResult
	if err := json.Unmarshal([]byte(stdout), &recovery); err != nil {
		t.Fatalf("decode store recover result %q: %v", stdout, err)
	}
	if recovery.Operation != "store_recover" || recovery.Report.Pending {
		t.Fatalf("store recover result=%+v", recovery)
	}
}

func TestStoreGCDryRunIsReadOnlyAndApplyIsExplicit(t *testing.T) {
	home := t.TempDir()
	worktreeID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	t.Setenv("MCE4D_HOME", home)
	indexDirectory := filepath.Join(home, "worktrees", worktreeID)
	if err := os.MkdirAll(indexDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(indexDirectory, "index.sqlite")
	indexDB, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range []string{"001.sql", "002.sql"} {
		ddl, err := os.ReadFile(filepath.Join("..", "store", "sqlite", "migrations", "index", migration))
		if err != nil {
			_ = indexDB.Close()
			t.Fatal(err)
		}
		if _, err := indexDB.ExecContext(context.Background(), string(ddl)); err != nil {
			_ = indexDB.Close()
			t.Fatalf("apply index fixture migration %s: %v", migration, err)
		}
	}
	if _, err := indexDB.ExecContext(context.Background(), `INSERT INTO store_identity(singleton,worktree_id,incarnation) VALUES(1,?,?)`,
		worktreeID, "ffffffff-ffff-4fff-8fff-ffffffffffff"); err != nil {
		_ = indexDB.Close()
		t.Fatalf("bind index fixture identity: %v", err)
	}
	if err := indexDB.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := mceruntime.EnsureWorktreeLockLayout(root, worktreeID); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	for _, acquire := range []func() (*mceruntime.Lease, error){
		func() (*mceruntime.Lease, error) {
			return mceruntime.AcquireHomeLifecycle(context.Background(), root, mceruntime.LockShared, 0)
		},
		func() (*mceruntime.Lease, error) {
			return mceruntime.AcquireWorktreeLifecycle(context.Background(), root, worktreeID, mceruntime.LockShared, 0)
		},
		func() (*mceruntime.Lease, error) {
			return mceruntime.AcquireHomeConfig(context.Background(), root, mceruntime.LockShared, 0)
		},
	} {
		lease, err := acquire()
		if err != nil {
			_ = root.Close()
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			_ = root.Close()
			t.Fatal(err)
		}
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{"store", "gc", "--target", "worktree", "--worktree-id", worktreeID, "--keep-per-profile", "2", "--json"}
	code, stdout, stderr := invoke(baseArgs, "")
	if code != 0 || stderr != "" {
		t.Fatalf("store gc dry-run: code=%d stderr=%q", code, stderr)
	}
	var dryRun worktreeGCCLIResult
	if err := json.Unmarshal([]byte(stdout), &dryRun); err != nil {
		t.Fatalf("decode store gc dry-run result %q: %v", stdout, err)
	}
	if dryRun.Operation != "store_gc" || dryRun.Applied || dryRun.Result != nil || dryRun.Plan.KeepPerProfile != 2 {
		t.Fatalf("store gc dry-run result=%+v", dryRun)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("store gc dry-run changed index bytes")
	}

	code, stdout, stderr = invoke(append(baseArgs, "--apply"), "")
	if code != 0 || stderr != "" {
		t.Fatalf("store gc apply: code=%d stderr=%q", code, stderr)
	}
	var applied worktreeGCCLIResult
	if err := json.Unmarshal([]byte(stdout), &applied); err != nil {
		t.Fatalf("decode store gc apply result %q: %v", stdout, err)
	}
	if applied.Operation != "store_gc" || !applied.Applied || applied.Result == nil {
		t.Fatalf("store gc apply result=%+v", applied)
	}
}

func TestParseExpectedDigestRejectsMalformedValue(t *testing.T) {
	if digest, err := parseExpectedDigest("not-a-digest"); err == nil || digest != nil {
		t.Fatalf("malformed expected digest accepted: %#v, %v", digest, err)
	}
	if digest, err := parseExpectedDigest("absent"); err != nil || digest != nil {
		t.Fatalf("absent source digest = %#v, %v", digest, err)
	}
	valid, err := model.ParseHash(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := parseExpectedDigest(string(valid)); err != nil || digest == nil || *digest != valid {
		t.Fatalf("valid expected digest = %#v, %v", digest, err)
	}
}

func nilContext() context.Context { return context.Background() }
