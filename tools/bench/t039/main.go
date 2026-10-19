// Command t039 measures bounded engine operations on deterministic synthetic
// fixtures. It never invokes a model, network service, or billing API.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/contextpack"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/search"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
	_ "modernc.org/sqlite"
)

const (
	sampleCount = 30
	warmupCount = 3
	fileCount   = 1000
	fileSize    = 20_000
	searchRows  = 512
	graphNodes  = 1000
	searchGenID = model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	queryTarget = "Target0256"
	graphTarget = "symbol-0511"
)

type latency struct {
	CacheState string  `json:"cache_state"`
	Samples    int     `json:"samples"`
	P50NS      int64   `json:"p50_ns"`
	P95NS      int64   `json:"p95_ns"`
	P50MS      float64 `json:"p50_ms"`
	P95MS      float64 `json:"p95_ms"`
	MinNS      int64   `json:"min_ns"`
	MaxNS      int64   `json:"max_ns"`
	MinMS      float64 `json:"min_ms"`
	MaxMS      float64 `json:"max_ms"`
}

type report struct {
	FormatVersion int               `json:"format_version"`
	Task          string            `json:"task"`
	Revision      string            `json:"revision"`
	GoVersion     string            `json:"go_version"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	LogicalCPUs   int               `json:"logical_cpus"`
	ClockSource   string            `json:"clock_source"`
	Hardware      string            `json:"hardware_profile,omitempty"`
	Filesystem    string            `json:"filesystem_class,omitempty"`
	Method        string            `json:"method"`
	Fixture       fixture           `json:"fixture"`
	Measurements  []measurement     `json:"measurements"`
	NotRun        map[string]string `json:"not_run"`
}

type fixture struct {
	Synthetic            bool  `json:"synthetic"`
	Repositories         int   `json:"repositories_in_task_corpus"`
	Tasks                int   `json:"tasks_in_task_corpus"`
	FreshnessFiles       int   `json:"freshness_files"`
	FreshnessSourceBytes int64 `json:"freshness_source_bytes"`
	SearchSymbols        int   `json:"search_symbols"`
	SearchDocuments      int   `json:"search_documents"`
	GraphNodes           int   `json:"graph_nodes"`
}

type measurement struct {
	Name                string  `json:"name"`
	Latency             latency `json:"latency"`
	Rows                int     `json:"rows,omitempty"`
	InputNodes          int     `json:"input_nodes,omitempty"`
	OutputBytes         int     `json:"output_bytes,omitempty"`
	OutputFiles         int     `json:"output_files,omitempty"`
	Files               int     `json:"files,omitempty"`
	StorageBytes        int64   `json:"storage_bytes,omitempty"`
	Pending             bool    `json:"pending"`
	CorrectnessOracle   string  `json:"correctness_oracle"`
	ReferenceTargetMS   float64 `json:"reference_target_ms"`
	ReferenceTargetPass *bool   `json:"reference_target_pass,omitempty"`
}

func main() {
	result, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (report, error) {
	revision := strings.TrimSpace(os.Getenv("MCE4D_BENCH_REVISION"))
	if !validGitRevision(revision) {
		return report{}, fmt.Errorf("MCE4D_BENCH_REVISION must be a full lowercase Git commit SHA (40 or 64 hexadecimal characters)")
	}
	if strings.TrimSpace(os.Getenv("MCE4D_BENCH_HARDWARE_PROFILE")) == "" || strings.TrimSpace(os.Getenv("MCE4D_BENCH_FILESYSTEM_CLASS")) == "" {
		return report{}, fmt.Errorf("hardware profile and filesystem class are required for benchmark evidence")
	}
	root, err := os.MkdirTemp("", "mce4d-t039-")
	if err != nil {
		return report{}, fmt.Errorf("create private synthetic fixture root: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	freshness, sourceBytes, err := profileFreshness(filepath.Join(root, "freshness"))
	if err != nil {
		return report{}, err
	}
	searchMeasurement, err := profileSearch(filepath.Join(root, "search.sqlite"))
	if err != nil {
		return report{}, err
	}
	packMeasurement, err := profileContextPack()
	if err != nil {
		return report{}, err
	}

	return report{
		FormatVersion: 1, Task: "T039", Revision: revision,
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		LogicalCPUs: runtime.NumCPU(), ClockSource: benchmarkClockSource(), Hardware: os.Getenv("MCE4D_BENCH_HARDWARE_PROFILE"),
		Filesystem: os.Getenv("MCE4D_BENCH_FILESYSTEM_CLASS"),
		Method:     "30 warm measured calls after 3 warmups; nearest-rank percentiles; each result checked against a deterministic fixture oracle; timings exclude fixture construction except the freshness scan, which includes directory enumeration, reads, SHA-256, and snapshot comparison.",
		Fixture: fixture{Synthetic: true, Repositories: 3, Tasks: 30, FreshnessFiles: fileCount,
			FreshnessSourceBytes: sourceBytes, SearchSymbols: searchRows, SearchDocuments: searchRows, GraphNodes: graphNodes},
		Measurements: []measurement{freshness, searchMeasurement, packMeasurement},
		NotRun: map[string]string{
			"model_task_abc":    "NOT_RUN: no separately authorized task-runner/host usage stream; the harness imports host-produced data and does not call models.",
			"model_tokens":      "NOT_RUN: no host token telemetry; no token estimate is derived from bytes or reads.",
			"cold_cache":        "NOT_RUN: the OS page cache cannot be deterministically evicted by this harness.",
			"full_index":        "NOT_RUN: this checkout has no end-to-end index command; analyzer microbenchmark and inventory/capture fixture are reported separately.",
			"real_repositories": "NOT_RUN: all task fixtures and measured query/freshness/graph fixtures are synthetic.",
		},
	}, nil
}

func profileSearch(databasePath string) (measurement, error) {
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return measurement{}, fmt.Errorf("open synthetic search database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := createSearchFixture(db); err != nil {
		_ = db.Close()
		return measurement{}, err
	}
	if err := db.Close(); err != nil {
		return measurement{}, fmt.Errorf("close synthetic search fixture: %w", err)
	}
	info, err := os.Stat(databasePath)
	if err != nil {
		return measurement{}, fmt.Errorf("stat synthetic search fixture: %w", err)
	}
	storageBytes := info.Size()
	db, err = sql.Open("sqlite", databasePath)
	if err != nil {
		return measurement{}, fmt.Errorf("reopen synthetic search fixture: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	searcher := search.New(db)
	request := search.Request{GenerationID: searchGenID, Mode: search.ModeSymbol, Match: search.MatchExact, Query: queryTarget, Limit: 7}
	query := func() (search.Result, error) { return searcher.Search(context.Background(), request) }
	var last search.Result
	durations, err := sample(func() error {
		result, err := query()
		if err != nil {
			return err
		}
		if len(result.Hits) != 1 || result.Hits[0].Name != queryTarget || result.Truncated || result.Partial {
			return fmt.Errorf("search oracle mismatch: hits=%d truncated=%t partial=%t", len(result.Hits), result.Truncated, result.Partial)
		}
		last = result
		return nil
	})
	if err != nil {
		return measurement{}, fmt.Errorf("profile warm indexed search: %w", err)
	}
	encoded, err := json.Marshal(last)
	if err != nil {
		return measurement{}, fmt.Errorf("encode search output for proxy size: %w", err)
	}
	return measured("warm_symbol_search", durations, 500, measurement{
		Rows: searchRows, OutputBytes: len(encoded), OutputFiles: 1, StorageBytes: storageBytes,
		Pending:           last.Partial || last.Truncated,
		CorrectnessOracle: "exact symbol query returns only Target0256 in a published synthetic generation; bounded result has no pending/truncated state.",
	}), nil
}

func createSearchFixture(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE generations (generation_id TEXT PRIMARY KEY, state TEXT NOT NULL)`,
		`CREATE TABLE packages (generation_id TEXT NOT NULL, package_key TEXT NOT NULL, import_path TEXT NOT NULL, PRIMARY KEY(generation_id, package_key))`,
		`CREATE INDEX packages_import ON packages(generation_id, import_path)`,
		`CREATE TABLE files (generation_id TEXT NOT NULL, path TEXT NOT NULL, status TEXT NOT NULL, package_key TEXT, PRIMARY KEY(generation_id, path))`,
		`CREATE TABLE symbols (generation_id TEXT NOT NULL, symbol_id TEXT NOT NULL, path TEXT, package_key TEXT, kind TEXT NOT NULL, name TEXT NOT NULL, qualified_name TEXT NOT NULL, scope_key TEXT NOT NULL, start_byte INTEGER, end_byte INTEGER, signature TEXT NOT NULL, exported INTEGER NOT NULL, PRIMARY KEY(generation_id, symbol_id))`,
		`CREATE INDEX symbols_name ON symbols(generation_id, name, symbol_id)`,
		`CREATE INDEX symbols_qualified ON symbols(generation_id, qualified_name, symbol_id)`,
		`CREATE TABLE search_documents (rowid INTEGER PRIMARY KEY, generation_id TEXT NOT NULL, item_id TEXT NOT NULL, path TEXT NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL, qualified_name TEXT NOT NULL, body TEXT NOT NULL, UNIQUE(generation_id, item_id))`,
		`CREATE VIRTUAL TABLE search_fts USING fts5(name, qualified_name, body, content='search_documents', content_rowid='rowid', tokenize='unicode61')`,
		`CREATE TRIGGER search_documents_ai AFTER INSERT ON search_documents BEGIN INSERT INTO search_fts(rowid,name,qualified_name,body) VALUES(new.rowid,new.name,new.qualified_name,new.body); END`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("create search fixture schema: %w", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO generations(generation_id,state) VALUES(?, 'READY')`, searchGenID); err != nil {
		return fmt.Errorf("publish search fixture generation: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin search fixture insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO packages(generation_id,package_key,import_path) VALUES(?,?,?)`, searchGenID, "bench", "example.test/bench"); err != nil {
		return fmt.Errorf("insert search fixture package: %w", err)
	}
	for i := 0; i < searchRows; i++ {
		name := fmt.Sprintf("Target%04d", i)
		id := fmt.Sprintf("symbol-%04d", i)
		path := fmt.Sprintf("pkg/file-%04d.go", i)
		if _, err := tx.Exec(`INSERT INTO files(generation_id,path,status,package_key) VALUES(?,?, 'indexed',?)`, searchGenID, path, "bench"); err != nil {
			return fmt.Errorf("insert search fixture file %d: %w", i, err)
		}
		if _, err := tx.Exec(`INSERT INTO symbols(generation_id,symbol_id,path,package_key,kind,name,qualified_name,scope_key,start_byte,end_byte,signature,exported) VALUES(?,?,?,?,?,?,?,?,0,1,?,1)`, searchGenID, id, path, "bench", "function", name, "example.test/bench."+name, "scope-"+id, "func()"+name); err != nil {
			return fmt.Errorf("insert search fixture symbol %d: %w", i, err)
		}
		if _, err := tx.Exec(`INSERT INTO search_documents(generation_id,item_id,path,kind,name,qualified_name,body) VALUES(?,?,?,?,?,?,?)`, searchGenID, id, path, "symbol", name, "example.test/bench."+name, "deterministic synthetic search fixture"); err != nil {
			return fmt.Errorf("insert search fixture document %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search fixture: %w", err)
	}
	return nil
}

func profileContextPack() (measurement, error) {
	scope, identity, err := benchmarkScope()
	if err != nil {
		return measurement{}, err
	}
	graph := contextpack.Graph{Nodes: make([]contextpack.Node, 0, graphNodes), Complete: true}
	contentHash := model.SHA256([]byte("package bench\nfunc Target() {}\n"))
	for i := 0; i < graphNodes; i++ {
		name := fmt.Sprintf("Target%04d", i)
		id := fmt.Sprintf("symbol-%04d", i)
		qualified := "example.test/bench." + name
		graph.Nodes = append(graph.Nodes, contextpack.Node{
			Ref:  contextpack.Ref{Kind: contextpack.RefSymbol, Scope: identity, ID: id},
			Kind: "function", Name: name, Label: qualified, QualifiedName: qualified, Plane: "observed_static",
			SourceRefs: []model.SourceRef{{WorktreeID: identity.WorktreeID, ProfileID: identity.ProfileID,
				GenerationID: identity.GenerationID, Path: "pkg/bench.go", ContentHash: contentHash,
				Range: model.ByteRange{StartByte: 0, EndByte: 1}}},
		})
	}
	input := contextpack.Input{Scope: scope, Graph: graph, Request: contextpack.Request{
		Selector: contextpack.Selector{Kind: contextpack.SelectorSymbol, SymbolID: graphTarget},
		TaskKind: contextpack.TaskExplore,
		Limits:   contextpack.Limits{MaxGraphNodes: 1000, MaxGraphRelations: 1, MaxCandidates: 1000, MaxNodes: 100, MaxRelations: 1, MaxDepth: 8},
	}}
	var last contextpack.Result
	durations, err := sample(func() error {
		result, err := contextpack.Build(context.Background(), input)
		if err != nil {
			return err
		}
		if result.Status != contextpack.StatusResolved || result.Target == nil || result.Target.ID != graphTarget || !result.EnumerationComplete {
			return fmt.Errorf("context-pack oracle mismatch: status=%s target=%v complete=%t", result.Status, result.Target, result.EnumerationComplete)
		}
		last = result
		return nil
	})
	if err != nil {
		return measurement{}, fmt.Errorf("profile graph-only context pack: %w", err)
	}
	encoded, err := json.Marshal(last)
	if err != nil {
		return measurement{}, fmt.Errorf("encode context-pack output for proxy size: %w", err)
	}
	return measured("warm_graph_only_context_pack", durations, 1500, measurement{
		InputNodes: graphNodes, OutputBytes: len(encoded),
		Pending:           last.ContinuationRequired || last.RequiredPending == nil || *last.RequiredPending != 0,
		CorrectnessOracle: "exact pinned symbol selector resolves symbol-0511; supplied 1,000-node graph is complete and returns no pending continuations.",
	}), nil
}

func profileFreshness(root string) (measurement, int64, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return measurement{}, 0, fmt.Errorf("create freshness fixture root: %w", err)
	}
	worktreeID := model.UUID("11111111-1111-4111-8111-111111111111")
	profileID := model.SHA256([]byte("t039-synthetic-profile"))
	policyDigest := model.SHA256([]byte("t039-synthetic-policy"))
	providerDigest := model.SHA256([]byte("t039-synthetic-provider"))
	inputs := make([]snapshot.FileInput, 0, fileCount)
	var sourceBytes int64
	for i := 0; i < fileCount; i++ {
		content := make([]byte, fileSize)
		header := []byte(fmt.Sprintf("package p%04d\n", i))
		copy(content, header)
		for j := len(header); j < len(content); j++ {
			content[j] = 'x'
		}
		path := fmt.Sprintf("pkg/file-%04d.go", i)
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			return measurement{}, 0, fmt.Errorf("create freshness fixture directory: %w", err)
		}
		if err := os.WriteFile(fullPath, content, 0o600); err != nil {
			return measurement{}, 0, fmt.Errorf("write freshness fixture %s: %w", path, err)
		}
		inputs = append(inputs, snapshot.FileInput{Path: path, Status: snapshot.FileCaptured, Content: content})
		sourceBytes += int64(len(content))
	}
	captured, err := snapshot.Build(snapshot.Input{
		SchemaVersion: 1, WorktreeID: worktreeID, ProfileID: profileID,
		PolicyDigest: policyDigest, ProviderDigest: providerDigest,
		DependencyInputDigests: []model.Hash{}, Files: inputs,
		Git: snapshot.GitState{HeadState: snapshot.HeadUnborn}, Scope: snapshot.ScopeComplete,
	})
	if err != nil {
		return measurement{}, 0, fmt.Errorf("build synthetic capture manifest: %w", err)
	}
	generationID := model.UUID("22222222-2222-4222-8222-222222222222")
	indexIncarnation := model.UUID("44444444-4444-4444-8444-444444444444")
	recordsIncarnation := model.UUID("55555555-5555-4555-8555-555555555555")
	vector := model.ScopeVector{Kind: "single", Members: []model.ScopeMember{{
		WorktreeID: worktreeID, ProfileID: profileID, GenerationID: generationID,
		SnapshotDigest: captured.Manifest.SnapshotDigest, PolicyDigest: policyDigest,
		PolicyEpoch: 1, BindingRevision: 1, RecordEpoch: 0,
		IndexIncarnation: indexIncarnation, RecordsIncarnation: recordsIncarnation,
	}}, CatalogIncarnation: indexIncarnation, UnavailableMembers: []model.UnavailableMember{}}
	vector.VectorDigest, err = vector.ComputeDigest()
	if err != nil {
		return measurement{}, 0, fmt.Errorf("compute synthetic scope digest: %w", err)
	}
	observation := snapshot.ScopeObservation{Vector: vector, ProviderDigest: providerDigest, DependencyInputDigests: []model.Hash{}, Complete: true}
	profiled, err := sample(func() error {
		observed, err := inventoryAndHash(root)
		if err != nil {
			return err
		}
		result, err := snapshot.CompareValidation(snapshot.ValidationInput{
			Expected: captured.Manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
			ObservedFiles: observed, ObservedComplete: true, ObservedScope: observation,
			CheckedAt: time.Now().UTC(),
		})
		if err != nil {
			return err
		}
		if result.Freshness.Status != snapshot.FreshnessObservedCurrent || result.Freshness.ChangedCount == nil || *result.Freshness.ChangedCount != 0 {
			return fmt.Errorf("freshness oracle mismatch: status=%s changed=%v", result.Freshness.Status, result.Freshness.ChangedCount)
		}
		return nil
	})
	if err != nil {
		return measurement{}, 0, fmt.Errorf("profile full synthetic freshness scan: %w", err)
	}
	return measured("warm_full_byte_freshness_scan_and_compare", profiled, 5000, measurement{
		Files:             fileCount,
		CorrectnessOracle: "directory enumeration and SHA-256 of every captured fixture file; CompareValidation reports observed_current and zero changed paths.",
	}), sourceBytes, nil
}

func inventoryAndHash(root string) ([]snapshot.WorkspaceFile, error) {
	files := make([]snapshot.WorkspaceFile, 0, fileCount)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected non-regular fixture entry %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, snapshot.WorkspaceFile{Path: filepath.ToSlash(relative), ContentHash: model.SHA256(data), ByteLength: int64(len(data))})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate and hash synthetic workspace: %w", err)
	}
	return files, nil
}

func benchmarkScope() (model.ScopeVector, contextpack.ScopeIdentity, error) {
	member := model.ScopeMember{
		WorktreeID: "11111111-1111-4111-8111-111111111111", ProfileID: model.SHA256([]byte("t039-graph-profile")),
		GenerationID: "22222222-2222-4222-8222-222222222222", SnapshotDigest: model.SHA256([]byte("t039-graph-snapshot")),
		PolicyDigest: model.SHA256([]byte("t039-graph-policy")), PolicyEpoch: 1, BindingRevision: 1,
		IndexIncarnation: "33333333-3333-4333-8333-333333333333", RecordsIncarnation: "44444444-4444-4444-8444-444444444444",
	}
	scope := model.ScopeVector{Kind: "single", Members: []model.ScopeMember{member}, UnavailableMembers: []model.UnavailableMember{}, CatalogIncarnation: "55555555-5555-4555-8555-555555555555"}
	var err error
	scope.VectorDigest, err = scope.ComputeDigest()
	if err != nil {
		return model.ScopeVector{}, contextpack.ScopeIdentity{}, fmt.Errorf("compute graph scope vector: %w", err)
	}
	if err := scope.Validate(); err != nil {
		return model.ScopeVector{}, contextpack.ScopeIdentity{}, fmt.Errorf("validate graph scope vector: %w", err)
	}
	return scope, contextpack.ScopeIdentity{WorktreeID: member.WorktreeID, ProfileID: member.ProfileID, GenerationID: member.GenerationID}, nil
}

func sample(operation func() error) ([]int64, error) {
	for i := 0; i < warmupCount; i++ {
		if err := operation(); err != nil {
			return nil, fmt.Errorf("warmup %d/%d: %w", i+1, warmupCount, err)
		}
	}
	durations := make([]int64, 0, sampleCount)
	for i := 0; i < sampleCount; i++ {
		started := benchmarkClockNow()
		if err := operation(); err != nil {
			return nil, fmt.Errorf("sample %d/%d: %w", i+1, sampleCount, err)
		}
		durations = append(durations, benchmarkClockElapsedNS(started))
	}
	return durations, nil
}

func measured(name string, durations []int64, targetMS float64, value measurement) measurement {
	sorted := append([]int64(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pass := float64(sorted[percentileIndex(len(sorted), 0.95)])/1e6 <= targetMS
	value.Name = name
	value.Latency = latency{CacheState: "warm", Samples: len(sorted),
		P50NS: sorted[percentileIndex(len(sorted), 0.50)],
		P95NS: sorted[percentileIndex(len(sorted), 0.95)],
		P50MS: float64(sorted[percentileIndex(len(sorted), 0.50)]) / 1e6,
		P95MS: float64(sorted[percentileIndex(len(sorted), 0.95)]) / 1e6,
		MinNS: sorted[0], MaxNS: sorted[len(sorted)-1],
		MinMS: float64(sorted[0]) / 1e6, MaxMS: float64(sorted[len(sorted)-1]) / 1e6}
	value.ReferenceTargetMS = targetMS
	value.ReferenceTargetPass = &pass
	return value
}

func percentileIndex(count int, percentile float64) int {
	if count < 1 {
		return 0
	}
	index := int(float64(count)*percentile + 0.999999999)
	if index < 1 {
		index = 1
	}
	if index > count {
		index = count
	}
	return index - 1
}

func validGitRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
