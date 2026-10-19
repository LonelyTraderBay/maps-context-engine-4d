//go:build windows && amd64

package platform_test

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	"golang.org/x/sys/windows"
	_ "modernc.org/sqlite"
)

const windowsSQLiteHelperPathEnv = "MCE4D_T037_SQLITE_HELPER_PATH"

func TestWindowsAMD64FilesystemAndSQLiteRuntimeEvidence(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".t037-windows-platform-")
	if err != nil {
		t.Fatalf("create probe directory beside the platform test: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove test-owned Windows platform probe: %v", err)
		}
	})
	absoluteDirectory, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	volumeRoot := filepath.VolumeName(absoluteDirectory) + `\`
	volumeRootUTF16, err := windows.UTF16PtrFromString(volumeRoot)
	if err != nil {
		t.Fatal(err)
	}
	fileSystemName := make([]uint16, 64)
	var volumeSerial, maxComponentLength, fileSystemFlags uint32
	if err := windows.GetVolumeInformation(volumeRootUTF16, nil, 0, &volumeSerial, &maxComponentLength, &fileSystemFlags,
		&fileSystemName[0], uint32(len(fileSystemName))); err != nil {
		t.Fatalf("read target volume metadata: %v", err)
	}
	fileSystem := windows.UTF16ToString(fileSystemName)
	if !strings.EqualFold(fileSystem, "NTFS") {
		t.Fatalf("Windows runtime acceptance requires the measured NTFS target; got %q", fileSystem)
	}
	version := windows.RtlGetVersion()
	if version == nil || version.MajorVersion < 10 {
		t.Fatalf("unsupported or unavailable Windows version: %+v", version)
	}

	root, err := mceruntime.OpenRoot(absoluteDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	caseMode, err := root.CaseMode()
	if err != nil || caseMode != paths.CaseInsensitive {
		t.Fatalf("NTFS temporary root case mode=%d err=%v; want the observed case-insensitive mode", caseMode, err)
	}
	volumeID, homeID, err := root.Identity()
	if err != nil || volumeID == "" || homeID == "" || volumeID == homeID {
		t.Fatalf("opened-root identities volume=%q home=%q err=%v", volumeID, homeID, err)
	}
	availableBytes, err := root.AvailableBytes()
	if err != nil || availableBytes <= 0 {
		t.Fatalf("NTFS headroom=%d err=%v", availableBytes, err)
	}
	if err := root.EnsurePrivateDirs("state"); err != nil {
		t.Fatalf("create ACL-protected state directory: %v", err)
	}
	stateDirectory := filepath.Join(absoluteDirectory, "state")
	assertWindowsACLOracle(t, stateDirectory, true)

	databasePath := filepath.Join(stateDirectory, "windows-runtime.sqlite")
	db, err := openWindowsSQLite(databasePath, 1234, 4)
	if err != nil {
		t.Fatalf("open file-backed SQLite database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close Windows SQLite acceptance database: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatalf("disable automatic checkpoint during bounded WAL probe: %v", err)
	}
	var journalMode, sqliteVersion, sqliteSourceID string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		t.Fatalf("enable WAL on the measured NTFS database: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("SQLite journal mode=%q; want WAL", journalMode)
	}
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version(), sqlite_source_id()").Scan(&sqliteVersion, &sqliteSourceID); err != nil {
		t.Fatalf("read embedded SQLite version and patch source ID: %v", err)
	}
	if sqliteSourceID == "" || !sqliteVersionAtLeast(sqliteVersion, 3, 51, 3) {
		t.Fatalf("SQLite version/source ID does not meet the WAL-reset floor: %q/%q", sqliteVersion, sqliteSourceID)
	}
	options, err := db.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		t.Fatalf("read embedded SQLite compile options: %v", err)
	}
	hasFTS5 := false
	for options.Next() {
		var option string
		if err := options.Scan(&option); err != nil {
			_ = options.Close()
			t.Fatalf("scan SQLite compile option: %v", err)
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
	if _, err := db.ExecContext(ctx, "CREATE VIRTUAL TABLE platform_fts USING fts5(body)"); err != nil {
		t.Fatalf("create FTS5 table on NTFS: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO platform_fts(body) VALUES (?)", "windows ntfs wal fixture"); err != nil {
		t.Fatalf("write FTS5 fixture on NTFS: %v", err)
	}
	var matches int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM platform_fts WHERE platform_fts MATCH ?", "windows ntfs wal fixture").Scan(&matches); err != nil || matches != 1 {
		t.Fatalf("FTS5 fixture matches=%d err=%v; want one row", matches, err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE recovery_probe(value TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("create cross-process recovery fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO recovery_probe(value) VALUES ('committed')"); err != nil {
		t.Fatalf("write committed recovery fixture: %v", err)
	}

	connections := make([]*sql.Conn, 0, 4)
	for index := 0; index < 4; index++ {
		connection, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("acquire pooled SQLite connection %d: %v", index+1, err)
		}
		connections = append(connections, connection)
		var foreignKeys, busyTimeout int
		if err := connection.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("read foreign_keys on pooled connection %d: %v", index+1, err)
		}
		if err := connection.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("read busy_timeout on pooled connection %d: %v", index+1, err)
		}
		if foreignKeys != 1 || busyTimeout != 1234 {
			t.Fatalf("pooled connection %d has foreign_keys=%d busy_timeout=%d; want 1/1234", index+1, foreignKeys, busyTimeout)
		}
	}
	for index := len(connections) - 1; index >= 0; index-- {
		if err := connections[index].Close(); err != nil {
			t.Errorf("close pooled SQLite connection %d: %v", index+1, err)
		}
	}
	if _, err := os.Stat(databasePath + "-wal"); err != nil {
		t.Fatalf("WAL sidecar was not materialized on NTFS: %v", err)
	}
	assertWindowsACLOracle(t, databasePath, false)
	assertWindowsACLOracle(t, databasePath+"-wal", false)

	t.Logf("platform=windows/%s windows_version=%d.%d.%d filesystem=%s volume_serial=%08x case_mode=insensitive root_identity=PASS available_bytes=%d",
		runtime.GOARCH, version.MajorVersion, version.MinorVersion, version.BuildNumber, fileSystem, volumeSerial, availableBytes)
	t.Logf("sqlite_version=%s source_id=%s journal_mode=%s fts5=PASS pool_connections=4 busy_timeout_ms=1234 acl_owner_only=PASS database_and_wal=PASS",
		sqliteVersion, sqliteSourceID, journalMode)
}

func TestWindowsSQLiteWALMultiProcessBusyAndCrashRecovery(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "recovery.sqlite")
	db, err := openWindowsSQLite(databasePath, 1234, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("enable WAL for process recovery fixture: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE recovery_probe(value TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("create process recovery table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO recovery_probe(value) VALUES ('committed')"); err != nil {
		t.Fatalf("write committed recovery row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	helper := startWindowsSQLiteWriter(t, databasePath)
	blockedDB, err := openWindowsSQLite(databasePath, 1234, 2)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, blockedErr := blockedDB.ExecContext(ctx, "INSERT INTO recovery_probe(value) VALUES ('blocked-writer')")
	cancel()
	blockedDuration := time.Since(started)
	if blockedErr == nil || (!strings.Contains(strings.ToLower(blockedErr.Error()), "locked") && !strings.Contains(strings.ToLower(blockedErr.Error()), "busy")) {
		t.Fatalf("second Windows process write error=%v; want bounded SQLite BUSY while the first writer is held", blockedErr)
	}
	if blockedDuration < 800*time.Millisecond || blockedDuration > 5*time.Second {
		t.Fatalf("busy_timeout contention duration=%s; want bounded wait near configured 1234ms", blockedDuration)
	}
	if err := blockedDB.Close(); err != nil {
		t.Fatal(err)
	}
	helper.rollbackAndWait(t)

	crashedWriter := startWindowsSQLiteWriter(t, databasePath)
	if err := crashedWriter.command.Process.Kill(); err != nil {
		t.Fatalf("terminate test-owned writer during an uncommitted WAL transaction: %v", err)
	}
	if err := crashedWriter.command.Wait(); err == nil {
		t.Fatal("SQLite writer process survived the controlled test-only crash")
	}
	crashedWriter.waited = true

	recovered, err := openWindowsSQLite(databasePath, 1234, 4)
	if err != nil {
		t.Fatalf("reopen SQLite after a process crash: %v", err)
	}
	defer func() {
		if err := recovered.Close(); err != nil {
			t.Errorf("close recovered SQLite database: %v", err)
		}
	}()
	var integrity string
	if err := recovered.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("post-crash SQLite integrity=%q err=%v; want ok", integrity, err)
	}
	for _, row := range []struct {
		value string
		want  int
	}{{"committed", 1}, {"blocked-writer", 0}, {"uncommitted", 0}} {
		var count int
		if err := recovered.QueryRow("SELECT count(*) FROM recovery_probe WHERE value=?", row.value).Scan(&count); err != nil || count != row.want {
			t.Fatalf("post-crash row %q count=%d err=%v; want %d", row.value, count, err, row.want)
		}
	}
	var busy, logFrames, checkpointed int
	if err := recovered.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil || busy != 0 {
		t.Fatalf("post-recovery WAL checkpoint busy=%d frames=%d checkpointed=%d err=%v", busy, logFrames, checkpointed, err)
	}
	t.Logf("sqlite=Windows-native WAL multiprocess busy_timeout_ms=1234 contention_ms=%d crash_recovery=PASS integrity_check=%s checkpoint_busy=%d checkpointed_frames=%d",
		blockedDuration.Milliseconds(), integrity, busy, checkpointed)
}

func TestWindowsSQLiteWriterProcessHelper(t *testing.T) {
	databasePath := os.Getenv(windowsSQLiteHelperPathEnv)
	if databasePath == "" {
		return
	}
	db, err := openWindowsSQLite(databasePath, 1234, 1)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = connection.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "INSERT INTO recovery_probe(value) VALUES ('uncommitted')"); err != nil {
		_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		_ = connection.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "READY"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ROLLBACK" {
		t.Fatalf("test-owned SQLite writer release command=%q err=%v", line, err)
	}
	if _, err := connection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

type windowsSQLiteWriter struct {
	command *osexec.Cmd
	stdin   io.WriteCloser
	waited  bool
}

func startWindowsSQLiteWriter(t *testing.T, databasePath string) *windowsSQLiteWriter {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := osexec.Command(executable, "-test.run=^TestWindowsSQLiteWriterProcessHelper$")
	command.Env = append(os.Environ(), windowsSQLiteHelperPathEnv+"="+databasePath)
	command.Stderr = os.Stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	writer := &windowsSQLiteWriter{command: command, stdin: stdin}
	t.Cleanup(func() {
		if !writer.waited && writer.command.ProcessState == nil {
			_ = writer.command.Process.Kill()
			_ = writer.command.Wait()
			writer.waited = true
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			_ = writer.command.Process.Kill()
			_ = writer.command.Wait()
			writer.waited = true
			t.Fatalf("test-owned SQLite process handshake=%q; want READY", line)
		}
	case <-time.After(10 * time.Second):
		_ = writer.command.Process.Kill()
		_ = writer.command.Wait()
		writer.waited = true
		t.Fatal("test-owned SQLite process did not reach its bounded writer handshake")
	}
	return writer
}

func (writer *windowsSQLiteWriter) rollbackAndWait(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(writer.stdin, "ROLLBACK\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.command.Wait(); err != nil {
		t.Fatalf("test-owned SQLite writer rollback process: %v", err)
	}
	writer.waited = true
}

func openWindowsSQLite(path string, busyTimeoutMS, maxConnections int) (*sql.DB, error) {
	slashPath := filepath.ToSlash(path)
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
		return nil, err
	}
	db.SetMaxOpenConns(maxConnections)
	db.SetMaxIdleConns(maxConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func assertWindowsACLOracle(t *testing.T, path string, directory bool) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		t.Fatalf("read native Windows security descriptor: %v", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("read current Windows user SID: %v", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatal("private object ACL owner does not match the current Windows user")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 3 {
		t.Fatalf("Windows ACL has %v entries (err=%v); want the current user, SYSTEM, and Administrators only", dacl, err)
	}
	sddl := descriptor.String()
	if strings.Count(sddl, "(A;") != 3 || strings.Count(sddl, ";;;") != 3 {
		t.Fatal("private object ACL contains an unexpected access rule")
	}
	if directory && !strings.Contains(sddl, "D:P") {
		t.Fatal("private state directory ACL is not protected from ambient inheritance")
	}
	for _, sid := range []string{"SY", "BA", user.User.Sid.String()} {
		if !strings.Contains(sddl, ";FA;;;"+sid+")") {
			t.Fatal("private object ACL does not grant the expected owner/SYSTEM/Administrators access")
		}
	}
}

func sqliteVersionAtLeast(version string, wantMajor, wantMinor, wantPatch int) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 3 {
		return false
	}
	got := [3]int{}
	for index := range got {
		parsed, err := strconv.Atoi(parts[index])
		if err != nil {
			return false
		}
		got[index] = parsed
	}
	want := [3]int{wantMajor, wantMinor, wantPatch}
	for index := range got {
		if got[index] != want[index] {
			return got[index] > want[index]
		}
	}
	return true
}
