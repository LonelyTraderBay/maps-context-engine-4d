//go:build linux && amd64

package platform_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLinuxAMD64FilesystemAndSQLiteRuntimeEvidence(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".t036-platform-evidence-")
	if err != nil {
		t.Fatalf("create probe directory beside the platform test: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove platform probe directory: %v", err)
		}
	})
	var filesystem syscall.Statfs_t
	if err := syscall.Statfs(directory, &filesystem); err != nil {
		t.Fatalf("inspect temporary directory filesystem: %v", err)
	}
	if filesystem.Type == 0 || filesystem.Bsize <= 0 {
		t.Fatalf("filesystem metadata is incomplete: type=%#x block_size=%d", filesystem.Type, filesystem.Bsize)
	}
	kernelRelease, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		t.Fatalf("read Linux kernel release: %v", err)
	}
	kernel := strings.TrimSpace(string(kernelRelease))
	if kernel == "" {
		t.Fatal("Linux kernel release is empty")
	}

	databasePath := filepath.Join(directory, "platform-evidence.sqlite")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open file-backed SQLite database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close file-backed SQLite database: %v", err)
		}
	}()
	db.SetMaxOpenConns(1)
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		t.Fatalf("enable WAL on file-backed database: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("file-backed database journal mode=%q; want WAL", journalMode)
	}
	var version, sourceID string
	if err := db.QueryRow("SELECT sqlite_version(), sqlite_source_id()").Scan(&version, &sourceID); err != nil {
		t.Fatalf("read embedded SQLite version and source ID: %v", err)
	}
	if version == "" || sourceID == "" {
		t.Fatalf("SQLite runtime version/source ID must be present: %q/%q", version, sourceID)
	}
	options, err := db.Query("PRAGMA compile_options")
	if err != nil {
		t.Fatalf("read SQLite compile options: %v", err)
	}
	hasFTS5 := false
	for options.Next() {
		var option string
		if err := options.Scan(&option); err != nil {
			_ = options.Close()
			t.Fatalf("read SQLite compile option: %v", err)
		}
		hasFTS5 = hasFTS5 || option == "ENABLE_FTS5"
	}
	if err := options.Err(); err != nil {
		_ = options.Close()
		t.Fatalf("iterate SQLite compile options: %v", err)
	}
	if err := options.Close(); err != nil {
		t.Fatalf("close SQLite compile options: %v", err)
	}
	if !hasFTS5 {
		t.Fatal("embedded SQLite does not report ENABLE_FTS5")
	}
	if _, err := db.Exec("CREATE VIRTUAL TABLE platform_fts USING fts5(body)"); err != nil {
		t.Fatalf("create FTS5 table on target filesystem: %v", err)
	}
	if _, err := db.Exec("INSERT INTO platform_fts(body) VALUES ('linux wal platform probe')"); err != nil {
		t.Fatalf("insert FTS5 probe row: %v", err)
	}
	var matches int
	if err := db.QueryRow("SELECT count(*) FROM platform_fts WHERE platform_fts MATCH ?", "linux wal platform probe").Scan(&matches); err != nil || matches != 1 {
		t.Fatalf("FTS5 probe matches=%d err=%v; want one indexed row", matches, err)
	}
	info, err := os.Stat(databasePath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("SQLite database is not a regular on-disk file: info=%v err=%v", info, err)
	}

	t.Logf("platform=linux/%s kernel=%s wsl2=%t filesystem_magic=%#x block_size=%d", runtime.GOARCH, kernel, strings.Contains(strings.ToLower(kernel), "microsoft"), filesystem.Type, filesystem.Bsize)
	t.Logf("sqlite_version=%s source_id=%s journal_mode=%s fts5=PASS database=%s", version, sourceID, journalMode, databasePath)
}
