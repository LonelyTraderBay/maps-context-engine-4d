package contract_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPinnedSQLiteDriverAppliesPerConnectionSettingsOnDisk(t *testing.T) {
	for _, timeout := range []int{5000, 1234} {
		t.Run(fmt.Sprintf("busy_timeout_%d_ms", timeout), func(t *testing.T) {
			db, dbPath := openStorageDB(t, "pool-settings", timeout)
			if info, err := os.Stat(dbPath); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("expected a file-backed SQLite database at %s; stat=%v err=%v", dbPath, info, err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			connections := make([]interface{ Close() error }, 0, 4)
			defer func() {
				for _, conn := range connections {
					if err := conn.Close(); err != nil {
						t.Errorf("close pooled SQLite connection: %v", err)
					}
				}
			}()
			for i := 0; i < 4; i++ {
				conn, err := db.Conn(ctx)
				if err != nil {
					t.Fatalf("acquire pooled connection %d: %v", i+1, err)
				}
				connections = append(connections, conn)
				var foreignKeys, busyTimeout int
				if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
					t.Fatalf("read foreign_keys on pooled connection %d: %v", i+1, err)
				}
				if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
					t.Fatalf("read busy_timeout on pooled connection %d: %v", i+1, err)
				}
				if foreignKeys != 1 || busyTimeout != timeout {
					t.Fatalf("pooled connection %d has foreign_keys=%d busy_timeout=%d; want 1/%d", i+1, foreignKeys, busyTimeout, timeout)
				}
				t.Logf("pooled_connection=%d foreign_keys=%d busy_timeout_ms=%d database=%s", i+1, foreignKeys, busyTimeout, dbPath)
			}
		})
	}
}

func TestPinnedSQLiteVersionFTS5AndGenerationScopedQueries(t *testing.T) {
	const defaultBusyTimeoutMS = 5000
	assertPinnedSQLiteModule(t)
	db, dbPath := openStorageDB(t, "fts-contract", defaultBusyTimeoutMS)
	if info, err := os.Stat(dbPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("expected a file-backed SQLite database; stat=%v err=%v", info, err)
	}
	var version, sourceID string
	if err := db.QueryRow("SELECT sqlite_version(), sqlite_source_id()").Scan(&version, &sourceID); err != nil {
		t.Fatalf("read embedded SQLite version/source ID: %v", err)
	}
	if sourceID == "" {
		t.Fatal("SQLite source ID must be present for patch provenance")
	}
	options, err := db.Query("PRAGMA compile_options")
	if err != nil {
		t.Fatalf("read embedded SQLite compile options: %v", err)
	}
	hasFTS5 := false
	for options.Next() {
		var option string
		if err := options.Scan(&option); err != nil {
			_ = options.Close()
			t.Fatalf("read SQLite compile option: %v", err)
		}
		if option == "ENABLE_FTS5" {
			hasFTS5 = true
		}
	}
	if err := options.Err(); err != nil {
		_ = options.Close()
		t.Fatalf("iterate SQLite compile options: %v", err)
	}
	if err := options.Close(); err != nil {
		t.Fatalf("close SQLite compile options: %v", err)
	}
	if !hasFTS5 {
		t.Fatal("SQLite compile options do not include ENABLE_FTS5")
	}
	t.Logf("embedded_sqlite_version=%s source_id=%s compatibility_floor=3.51.3 fts5_compile_option=PASS driver=modernc.org/sqlite@v1.60.1 platform=%s/%s database=%s", version, sourceID, runtime.GOOS, runtime.GOARCH, dbPath)
	if !sqliteVersionAtLeast(version, 3, 51, 3) {
		t.Fatalf("embedded SQLite version %q is below WAL-reset compatibility floor 3.51.3", version)
	}

	applyStorageMigration(t, db, "index/001.sql")
	applyStorageMigration(t, db, "index/002.sql")
	addBuildingGeneration(t, db, "gen-search-a", strings.Repeat("1", 64))
	addBuildingGeneration(t, db, "gen-search-b", strings.Repeat("2", 64))
	for _, row := range []struct{ generation, item, body string }{
		{"gen-search-a", "item-a", "onlyredxylophone"},
		{"gen-search-b", "item-b", "onlybluexylophone"},
	} {
		if _, err := db.Exec(`INSERT INTO search_documents
			(generation_id,item_id,path,kind,name,qualified_name,body)
			VALUES (?,?,?,'symbol',?,?,?)`, row.generation, row.item, "src/main.go", row.item, row.item, row.body); err != nil {
			t.Fatalf("insert FTS document for %s: %v", row.generation, err)
		}
	}
	queryMatches := func(term, generation string) int {
		t.Helper()
		var count int
		err := db.QueryRow(`SELECT count(*) FROM search_fts
			JOIN search_documents d ON d.rowid=search_fts.rowid
			WHERE search_fts MATCH ? AND d.generation_id=?`, term, generation).Scan(&count)
		if err != nil {
			t.Fatalf("search FTS term %q for %s: %v", term, generation, err)
		}
		return count
	}
	if got := queryMatches("onlyredxylophone", "gen-search-a"); got != 1 {
		t.Fatalf("generation A FTS matches=%d; want 1", got)
	}
	if got := queryMatches("onlyredxylophone", "gen-search-b"); got != 0 {
		t.Fatalf("generation B leaked a result from A: matches=%d", got)
	}
	if _, err := db.Exec(`UPDATE search_documents SET body='onlygreenxylophone' WHERE generation_id='gen-search-a'`); err != nil {
		t.Fatalf("update FTS source document: %v", err)
	}
	if got := queryMatches("onlyredxylophone", "gen-search-a"); got != 0 {
		t.Fatalf("FTS update trigger retained old term: matches=%d", got)
	}
	if got := queryMatches("onlygreenxylophone", "gen-search-a"); got != 1 {
		t.Fatalf("FTS update trigger missed new term: matches=%d", got)
	}
	if _, err := db.Exec(`DELETE FROM search_documents WHERE generation_id='gen-search-b'`); err != nil {
		t.Fatalf("delete FTS source document: %v", err)
	}
	if got := queryMatches("onlybluexylophone", "gen-search-b"); got != 0 {
		t.Fatalf("FTS delete trigger retained removed term: matches=%d", got)
	}
	for _, id := range []string{"gen-search-a", "gen-search-b"} {
		publishStorageGeneration(t, db, id)
	}
	if got := queryMatches("onlygreenxylophone", "gen-search-a"); got != 1 {
		t.Fatalf("published generation A lost its FTS document: matches=%d", got)
	}
	if got := queryMatches("onlybluexylophone", "gen-search-b"); got != 0 {
		t.Fatalf("published generation B retained a deleted FTS term: matches=%d", got)
	}
}

func assertPinnedSQLiteModule(t *testing.T) {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("Go build information is unavailable")
	}
	for _, dependency := range info.Deps {
		if dependency.Path == "modernc.org/sqlite" {
			if dependency.Version != "v1.60.1" || !strings.HasPrefix(dependency.Sum, "h1:") {
				t.Fatalf("SQLite driver version=%s; go.mod pin requires v1.60.1", dependency.Version)
			}
			t.Logf("go_module_pin=%s@%s sum=%s", dependency.Path, dependency.Version, dependency.Sum)
			return
		}
	}
	t.Fatal("pinned modernc.org/sqlite dependency is absent from the test binary build info")
}

func sqliteVersionAtLeast(version string, wantMajor, wantMinor, wantPatch int) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 3 {
		return false
	}
	got := [3]int{}
	for i := range got {
		part := parts[i]
		for j, char := range part {
			if char < '0' || char > '9' {
				part = part[:j]
				break
			}
		}
		parsed, err := strconv.Atoi(part)
		if err != nil {
			return false
		}
		got[i] = parsed
	}
	want := [3]int{wantMajor, wantMinor, wantPatch}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i]
		}
	}
	return true
}
