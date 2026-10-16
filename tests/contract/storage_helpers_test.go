package contract_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func storageRepoRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve storage test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
}

func openStorageDB(t *testing.T, name string, busyTimeoutMS int) (*sql.DB, string) {
	t.Helper()
	if busyTimeoutMS < 0 {
		t.Fatalf("busy timeout must not be negative: %d", busyTimeoutMS)
	}
	dbPath := filepath.Join(t.TempDir(), name+".sqlite")
	slashPath := filepath.ToSlash(dbPath)
	if len(slashPath) >= 3 && slashPath[1] == ':' && slashPath[2] == '/' {
		slashPath = "/" + slashPath
	}
	dsn := url.URL{Scheme: "file", Path: slashPath}
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	dsn.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatalf("open pinned SQLite driver: %v", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close SQLite database: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("open file-backed SQLite database: %v", err)
	}
	return db, dbPath
}

func applyStorageMigration(t *testing.T, db *sql.DB, relativePath string) {
	t.Helper()
	path := filepath.Join(storageRepoRoot(t), "internal", "store", "sqlite", "migrations", filepath.FromSlash(relativePath))
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration %s: %v", relativePath, err)
	}
	if _, err := db.ExecContext(context.Background(), string(sqlBytes)); err != nil {
		t.Fatalf("execute migration %s on temporary database: %v", relativePath, err)
	}
}

func ddlFromSpec(t *testing.T, marker string) string {
	t.Helper()
	specPath := filepath.Join(storageRepoRoot(t), "Maps-Context-Engine-4D_SPEC.md")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read storage DDL authority: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	begin := "<!-- BEGIN " + marker + " -->"
	end := "<!-- END " + marker + " -->"
	start := -1
	stop := -1
	for i, line := range lines {
		if line == begin {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("missing DDL begin marker %s", marker)
	}
	if start >= len(lines) || lines[start] != "```sql" {
		t.Fatalf("DDL %s is not in a fenced SQL block", marker)
	}
	start++
	for i := start; i < len(lines); i++ {
		if lines[i] == "```" {
			stop = i
			break
		}
	}
	if stop < 0 || stop+1 >= len(lines) || lines[stop+1] != end {
		t.Fatalf("missing DDL end marker %s", marker)
	}
	return strings.Join(lines[start:stop], "\n") + "\n"
}

func expectStorageError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLite error containing %q", contains)
	}
	if !strings.Contains(strings.ToUpper(err.Error()), strings.ToUpper(contains)) {
		t.Fatalf("SQLite error %q does not contain %q", err, contains)
	}
}

func addBuildingGeneration(t *testing.T, db *sql.DB, generationID, profileID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO generations (
		generation_id, profile_id, state, owner_job_id, policy_digest,
		policy_epoch, provider_fingerprint, created_at
	) VALUES (?, ?, 'BUILDING', 'job-fixture', ?, 1, ?, '2026-10-16T00:00:00Z')`,
		generationID, profileID, strings.Repeat("a", 64), strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("insert BUILDING generation %q: %v", generationID, err)
	}
}

func publishStorageGeneration(t *testing.T, db *sql.DB, generationID string) {
	t.Helper()
	result, err := db.Exec(`UPDATE generations
		SET state='READY', snapshot_digest=?, manifest_json=COALESCE(manifest_json,'{}'), completed_at='2026-10-16T00:00:01Z'
		WHERE generation_id=? AND state='BUILDING'`, strings.Repeat("c", 64), generationID)
	if err != nil {
		t.Fatalf("publish generation %q: %v", generationID, err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		t.Fatalf("publish generation affected=%d err=%v; want exactly one BUILDING row", count, err)
	}
}
