package main

import (
	"context"
	"database/sql"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/types"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/tools/go/packages"
	_ "modernc.org/sqlite"
)

const (
	expectedGo           = "go1.27.2"
	expectedSQLite       = "3.53.4"
	expectedSQLiteModule = "v1.60.1"
	expectedLibc         = "v1.77.1"
	expectedMCPSDK       = "v1.8.0"
	expectedTools        = "v0.52.0"
	expectedProtocol     = "2026-07-28"
	deliveryID           = "f6b9bc8a-3772-49a3-8623-2d1b7d75b8e1"
)

type moduleResult struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum"`
}

type sqliteResult struct {
	Version             string   `json:"version"`
	SourceID            string   `json:"source_id"`
	CompileOptions      []string `json:"compile_options"`
	FTS5CreateAndQuery  bool     `json:"fts5_create_and_query"`
	BusyTimeoutDefault  []int    `json:"busy_timeout_default_5000_each_connection"`
	BusyTimeoutOverride []int    `json:"busy_timeout_override_1234_each_connection"`
	ConcurrentLeases    int      `json:"concurrent_physical_connection_leases"`
	DatabaseFilesystems []string `json:"database_filesystems"`
}

type mcpResult struct {
	ProtocolVersions   []string `json:"protocol_versions"`
	NegotiatedProtocol string   `json:"negotiated_protocol"`
	Handshake          bool     `json:"in_memory_handshake"`
	ToolCall           bool     `json:"in_memory_tool_call"`
}

type loaderResult struct {
	PackagePath   string `json:"package_path"`
	FilesLoaded   int    `json:"files_loaded"`
	TypedMarker   bool   `json:"typed_marker"`
	TypedValue    bool   `json:"typed_value_field"`
	LoaderVersion string `json:"golang_x_tools_version"`
}

type report struct {
	Result              string         `json:"result"`
	GoVersion           string         `json:"go_version"`
	GOOS                string         `json:"goos"`
	GOARCH              string         `json:"goarch"`
	CGOEnabled          bool           `json:"cgo_enabled"`
	Modules             []moduleResult `json:"modules"`
	SQLite              sqliteResult   `json:"sqlite"`
	MCP                 mcpResult      `json:"mcp"`
	GoPackages          loaderResult   `json:"go_packages"`
	CGOSettingObserved  bool           `json:"cgo_setting_observed"`
	RunID               string         `json:"run_id"`
	RetainedFixtureRoot string         `json:"retained_fixture_root_relative"`
	StartedAtUTC        string         `json:"started_at_utc"`
	CompletedUTC        string         `json:"completed_at_utc"`
}

func main() {
	out := flag.String("out", "", "new JSON report path inside artifacts/preflight/T002")
	fixture := flag.String("db-root", "", "fresh SQLite fixture root inside the allocated T002 delivery staging area")
	runID := flag.String("run-id", "", "unique UUID for this retained smoke fixture")
	flag.Parse()
	if *out == "" || *fixture == "" || *runID == "" {
		fatal(errors.New("missing required output, fixture root, or run ID"))
	}
	fixtureRelative, err := createFreshFixtureRoot(*fixture, *runID)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := run(ctx, *fixture)
	if err != nil {
		fatal(err)
	}
	r.RunID = *runID
	r.RetainedFixtureRoot = fixtureRelative
	if err := writeReport(*out, r); err != nil {
		fatal(err)
	}
	fmt.Printf("PASS %s/%s SQLite %s FTS5 MCP %s go/packages %s\n", r.GOOS, r.GOARCH, r.SQLite.Version, expectedMCPSDK, expectedTools)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "FAIL %s\n", err)
	os.Exit(1)
}

func run(ctx context.Context, fixtureRoot string) (report, error) {
	started := time.Now().UTC()
	r := report{Result: "PASS", GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, StartedAtUTC: started.Format(time.RFC3339Nano)}
	if r.GoVersion != expectedGo {
		return report{}, fmt.Errorf("toolchain version mismatch: %s", r.GoVersion)
	}
	info, err := buildinfo.ReadFile(os.Args[0])
	if err != nil {
		return report{}, errors.New("cannot read compiled build information")
	}
	for _, setting := range info.Settings {
		if setting.Key == "CGO_ENABLED" {
			r.CGOSettingObserved = true
			r.CGOEnabled = setting.Value == "1"
		}
	}
	if !r.CGOSettingObserved || r.CGOEnabled {
		return report{}, errors.New("smoke binary lacks an explicit CGO_ENABLED=0 build setting")
	}
	modules := map[string]string{
		"github.com/modelcontextprotocol/go-sdk": expectedMCPSDK,
		"golang.org/x/tools":                     expectedTools,
		"modernc.org/libc":                       expectedLibc,
		"modernc.org/sqlite":                     expectedSQLiteModule,
	}
	for _, dep := range info.Deps {
		if want, exists := modules[dep.Path]; exists {
			if dep.Version != want || !strings.HasPrefix(dep.Sum, "h1:") {
				return report{}, fmt.Errorf("module pin or checksum mismatch: %s", dep.Path)
			}
			r.Modules = append(r.Modules, moduleResult{Path: dep.Path, Version: dep.Version, Sum: dep.Sum})
			delete(modules, dep.Path)
		}
	}
	if len(modules) != 0 {
		return report{}, fmt.Errorf("build info omits %d pinned direct modules", len(modules))
	}
	sort.Slice(r.Modules, func(i, j int) bool { return r.Modules[i].Path < r.Modules[j].Path })

	r.SQLite, err = runSQLite(ctx, fixtureRoot)
	if err != nil {
		return report{}, err
	}
	r.MCP, err = runMCP(ctx)
	if err != nil {
		return report{}, err
	}
	r.GoPackages, err = runPackages(ctx)
	if err != nil {
		return report{}, err
	}
	r.CompletedUTC = time.Now().UTC().Format(time.RFC3339Nano)
	return r, nil
}

func runSQLite(ctx context.Context, tmp string) (sqliteResult, error) {
	var result sqliteResult
	for _, timeout := range []int{5000, 1234} {
		path := filepath.Join(tmp, fmt.Sprintf("sqlite-%d.db", timeout))
		busy, err := openSQLiteDB(ctx, path, timeout, timeout == 5000, &result)
		if err != nil {
			return sqliteResult{}, err
		}
		if timeout == 5000 {
			result.BusyTimeoutDefault = busy
		} else {
			result.BusyTimeoutOverride = busy
		}
		result.DatabaseFilesystems = append(result.DatabaseFilesystems, filesystemName(path))
	}
	result.ConcurrentLeases = 4
	if result.Version != expectedSQLite {
		return sqliteResult{}, fmt.Errorf("embedded SQLite version mismatch: %s", result.Version)
	}
	if !hasOption(result.CompileOptions, "ENABLE_FTS5") || !result.FTS5CreateAndQuery {
		return sqliteResult{}, errors.New("SQLite FTS5 compile/runtime check failed")
	}
	for i, values := range [][]int{result.BusyTimeoutDefault, result.BusyTimeoutOverride} {
		if len(values) != 4 {
			return sqliteResult{}, errors.New("SQLite pool did not lease four connections")
		}
		want := []int{5000, 1234}[i]
		for _, got := range values {
			if got != want {
				return sqliteResult{}, fmt.Errorf("busy_timeout mismatch: got %d want %d", got, want)
			}
		}
	}
	if result.SourceID == "" || compareVersion(result.Version, "3.51.3") < 0 {
		return sqliteResult{}, errors.New("SQLite patch provenance/version gate failed")
	}
	return result, nil
}

func openSQLiteDB(ctx context.Context, path string, timeout int, testFTS bool, result *sqliteResult) ([]int, error) {
	cleanPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && len(cleanPath) >= 2 && cleanPath[1] == ':' {
		cleanPath = "/" + cleanPath
	}
	u := url.URL{Scheme: "file", Path: cleanPath}
	q := u.Query()
	q.Set("_busy_timeout", fmt.Sprint(timeout))
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, errors.New("open pinned SQLite driver failed")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping pinned SQLite driver failed: %w", err)
	}
	var version, sourceID string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version(), sqlite_source_id()").Scan(&version, &sourceID); err != nil {
		_ = db.Close()
		return nil, errors.New("read embedded SQLite version/source ID failed")
	}
	if result.Version == "" {
		result.Version, result.SourceID = version, sourceID
		rows, err := db.QueryContext(ctx, "PRAGMA compile_options")
		if err != nil {
			_ = db.Close()
			return nil, errors.New("read SQLite compile options failed")
		}
		for rows.Next() {
			var option string
			if err := rows.Scan(&option); err != nil {
				_ = rows.Close()
				_ = db.Close()
				return nil, errors.New("scan SQLite compile option failed")
			}
			result.CompileOptions = append(result.CompileOptions, option)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			_ = db.Close()
			return nil, errors.New("iterate SQLite compile options failed")
		}
		_ = rows.Close()
		sort.Strings(result.CompileOptions)
		if testFTS {
			if _, err := db.ExecContext(ctx, "CREATE VIRTUAL TABLE preflight_fts USING fts5(body)"); err != nil {
				_ = db.Close()
				return nil, errors.New("create SQLite FTS5 table failed")
			}
			if _, err := db.ExecContext(ctx, "INSERT INTO preflight_fts(body) VALUES(?)", "bounded storage fixture"); err != nil {
				_ = db.Close()
				return nil, errors.New("insert SQLite FTS5 fixture failed")
			}
			var matches int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM preflight_fts WHERE preflight_fts MATCH ?", "storage").Scan(&matches); err != nil || matches != 1 {
				_ = db.Close()
				return nil, errors.New("query SQLite FTS5 fixture failed")
			}
			result.FTS5CreateAndQuery = true
		}
	}
	conns := make([]*sql.Conn, 0, 4)
	values := make([]int, 0, 4)
	for range 4 {
		conn, err := db.Conn(ctx)
		if err != nil {
			for _, leased := range conns {
				_ = leased.Close()
			}
			_ = db.Close()
			return nil, errors.New("lease pooled SQLite connection failed")
		}
		conns = append(conns, conn)
		var actual int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&actual); err != nil {
			for _, leased := range conns {
				_ = leased.Close()
			}
			_ = db.Close()
			return nil, errors.New("read pooled SQLite busy_timeout failed")
		}
		values = append(values, actual)
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			_ = db.Close()
			return nil, errors.New("release pooled SQLite connection failed")
		}
	}
	if err := db.Close(); err != nil {
		return nil, errors.New("close pooled SQLite database failed")
	}
	return values, nil
}

func filesystemName(path string) string {
	if runtime.GOOS == "windows" {
		return "NTFS"
	}
	if strings.HasPrefix(path, "/mnt/") {
		return "WSL mounted filesystem"
	}
	return "Linux filesystem"
}

func hasOption(options []string, want string) bool {
	for _, option := range options {
		if option == want {
			return true
		}
	}
	return false
}

func compareVersion(got, want string) int {
	parse := func(v string) []int {
		parts := strings.Split(v, ".")
		out := make([]int, len(parts))
		for i, part := range parts {
			fmt.Sscanf(part, "%d", &out[i])
		}
		return out
	}
	a, b := parse(got), parse(want)
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

func runMCP(ctx context.Context) (mcpResult, error) {
	result := mcpResult{ProtocolVersions: mcp.SupportedProtocolVersions()}
	if !hasOption(result.ProtocolVersions, expectedProtocol) {
		return result, errors.New("pinned MCP SDK does not advertise expected protocol")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "mce4d-preflight", Version: "0.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "preflight_ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return result, errors.New("MCP server transport connect failed")
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "mce4d-preflight-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return result, errors.New("MCP client handshake failed")
	}
	defer clientSession.Close()
	negotiated := clientSession.InitializeResult()
	if negotiated == nil || negotiated.ProtocolVersion != expectedProtocol {
		return result, errors.New("MCP handshake negotiated an unexpected protocol version")
	}
	result.NegotiatedProtocol = negotiated.ProtocolVersion
	result.Handshake = true
	call, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "preflight_ping"})
	if err != nil || call.IsError {
		return result, errors.New("MCP in-memory tool call failed")
	}
	for _, content := range call.Content {
		if text, ok := content.(*mcp.TextContent); ok && text.Text == "pong" {
			result.ToolCall = true
		}
	}
	if !result.ToolCall {
		return result, errors.New("MCP response content did not match")
	}
	return result, nil
}

func runPackages(ctx context.Context) (loaderResult, error) {
	dir := filepath.Join("artifacts", "preflight", "T002", "loader-fixture")
	config := &packages.Config{Context: ctx, Dir: dir, Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo}
	loaded, err := packages.Load(config, ".")
	if err != nil || len(loaded) != 1 {
		return loaderResult{}, errors.New("go/packages failed to load the bounded fixture")
	}
	pkg := loaded[0]
	if len(pkg.Errors) != 0 || pkg.Types == nil || pkg.TypesInfo == nil || len(pkg.Syntax) == 0 {
		return loaderResult{}, errors.New("go/packages returned incomplete type information")
	}
	marker := pkg.Types.Scope().Lookup("Marker")
	if marker == nil {
		return loaderResult{}, errors.New("go/packages typed fixture lacks Marker")
	}
	structure, ok := marker.Type().Underlying().(*types.Struct)
	if !ok {
		return loaderResult{}, errors.New("go/packages Marker is not a struct")
	}
	field := structure.Field(0)
	if structure.NumFields() != 1 || field.Name() != "Value" || !types.Identical(field.Type(), types.Typ[types.Int]) {
		return loaderResult{}, errors.New("go/packages typed field mismatch")
	}
	return loaderResult{PackagePath: pkg.PkgPath, FilesLoaded: len(pkg.GoFiles), TypedMarker: true, TypedValue: true, LoaderVersion: expectedTools}, nil
}

func writeReport(path string, r report) error {
	root, err := filepath.Abs(filepath.Join("artifacts", "preflight", "T002"))
	if err != nil {
		return errors.New("cannot resolve preflight artifact root")
	}
	output, err := filepath.Abs(path)
	if err != nil {
		return errors.New("cannot resolve report path")
	}
	rel, err := filepath.Rel(root, output)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("report path escaped the preflight artifact root")
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return errors.New("cannot create new preflight report")
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(r)
	syncErr := file.Sync()
	closeErr := file.Close()
	if encodeErr != nil {
		return errors.New("cannot encode preflight report")
	}
	if syncErr != nil {
		return errors.New("cannot sync preflight report")
	}
	if closeErr != nil {
		return errors.New("cannot close preflight report")
	}
	return nil
}

func createFreshFixtureRoot(requested, runID string) (string, error) {
	parentRelative := filepath.Join("artifacts", "git-delivery", ".staging-"+deliveryID, "scratch", "T002")
	if len(runID) != 36 || runID[8] != '-' || runID[13] != '-' || runID[18] != '-' || runID[23] != '-' {
		return "", errors.New("smoke run ID is not a UUID")
	}
	for _, ch := range runID {
		if ch != '-' && !strings.ContainsRune("0123456789abcdefABCDEF", ch) {
			return "", errors.New("smoke run ID is not a UUID")
		}
	}
	platformParent := filepath.Join(parentRelative, runtime.GOOS)
	want := filepath.Join(platformParent, runID)
	requestedAbs, err := filepath.Abs(requested)
	if err != nil {
		return "", errors.New("cannot resolve SQLite fixture path")
	}
	wantAbs, err := filepath.Abs(want)
	if err != nil || filepath.Clean(requestedAbs) != filepath.Clean(wantAbs) {
		return "", errors.New("SQLite fixture path does not match the allocated T002 scope")
	}
	for current := filepath.Clean(parentRelative); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("SQLite fixture parent is missing or linked")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	if info, err := os.Lstat(platformParent); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("SQLite platform fixture root is linked")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(platformParent, 0700); err != nil {
			return "", errors.New("cannot create SQLite platform fixture root")
		}
	} else {
		return "", errors.New("cannot inspect SQLite platform fixture root")
	}
	if _, err := os.Lstat(want); err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("SQLite fixture root is not fresh")
	}
	if err := os.Mkdir(want, 0700); err != nil {
		return "", errors.New("cannot exclusively create SQLite fixture root")
	}
	return filepath.ToSlash(want), nil
}
