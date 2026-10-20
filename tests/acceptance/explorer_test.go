package acceptance_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	appexplorer "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/explorer"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	transport "github.com/LonelyTraderBay/maps-context-engine-4d/internal/explorer"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
	_ "modernc.org/sqlite"
)

const (
	explorerWorktreeID   model.UUID = "90000000-0000-4000-8000-000000000084"
	explorerGenerationID model.UUID = "90000000-0000-4000-8000-000000000085"
	explorerProfileID    model.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type explorerFixture struct {
	home       string
	worktreeID model.UUID
	profileID  model.Hash
	generation model.UUID
	policy     model.Hash
	source     []byte
	mainRange  model.ByteRange
}

type explorerStartup struct {
	mu   sync.Mutex
	data bytes.Buffer
	once sync.Once
	line chan struct{}
}

func (writer *explorerStartup) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	n, err := writer.data.Write(value)
	if strings.Contains(writer.data.String(), "Bootstrap code: ") {
		writer.once.Do(func() { close(writer.line) })
	}
	return n, err
}

func (writer *explorerStartup) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.data.String()
}

func TestExplorerPublishedSnapshotHTTP(t *testing.T) {
	fixture := createExplorerFixture(t, sqlite.GenerationReady, false, true)
	service, err := appexplorer.NewService(appexplorer.Options{HomePath: fixture.home, WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID})
	if err != nil {
		t.Fatal(err)
	}
	client, baseURL, code, stop := startExplorerAPI(t, service)
	defer stop()

	bootstrap := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/session/bootstrap", "", "", map[string]any{"code": code})
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body)
	}
	var bootstrapEnvelope struct {
		Data struct {
			Bearer string `json:"bearer"`
		} `json:"data"`
	}
	decodeExplorerJSON(t, bootstrap.Body, &bootstrapEnvelope)
	if bootstrapEnvelope.Data.Bearer == "" {
		t.Fatal("bootstrap did not return a bearer")
	}

	workspaceRequest, err := http.NewRequest(http.MethodGet, baseURL+"/api/explorer/v1/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRequest.Header.Set("Authorization", "Bearer "+bootstrapEnvelope.Data.Bearer)
	workspaceResponse, err := client.Do(workspaceRequest)
	if err != nil {
		t.Fatal(err)
	}
	workspaceBody, err := io.ReadAll(workspaceResponse.Body)
	_ = workspaceResponse.Body.Close()
	if err != nil || workspaceResponse.StatusCode != http.StatusOK {
		t.Fatalf("workspace status=%d err=%v body=%s", workspaceResponse.StatusCode, err, workspaceBody)
	}
	var workspace struct {
		Status string `json:"status"`
		Data   struct {
			IndexState string `json:"index_state"`
		} `json:"data"`
	}
	decodeExplorerJSON(t, string(workspaceBody), &workspace)
	if workspace.Data.IndexState != "published" {
		t.Fatalf("workspace index_state=%q; want published", workspace.Data.IndexState)
	}

	csrf := explorerCookie(t, client, baseURL, "mce4d_csrf")
	forgedOrigin := postExplorerJSONWithOrigin(t, client, baseURL, "http://localhost:1234", "/api/explorer/v1/graph/query", bootstrapEnvelope.Data.Bearer, csrf, map[string]any{
		"root": map[string]any{"kind": "workspace"}, "depth": 3, "node_limit": 200, "edge_limit": 500,
	})
	if forgedOrigin.Code != http.StatusForbidden || !strings.Contains(forgedOrigin.Body, `"code":"ACCESS_DENIED"`) || strings.Contains(forgedOrigin.Body, "explorerfixture") || forgedOrigin.Headers.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("forged Origin status/body=%d/%s; want sanitized 403", forgedOrigin.Code, forgedOrigin.Body)
	}
	graph := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/graph/query", bootstrapEnvelope.Data.Bearer, "", map[string]any{
		"root": map[string]any{"kind": "workspace"}, "depth": 3, "node_limit": 200, "edge_limit": 500,
	})
	if graph.Code != http.StatusOK {
		t.Fatalf("graph status without an unnecessary CSRF header=%d body=%s", graph.Code, graph.Body)
	}
	var graphEnvelope struct {
		Status string `json:"status"`
		Data   struct {
			Nodes []struct {
				NodeID    string            `json:"node_id"`
				Label     string            `json:"label"`
				SourceRef []model.SourceRef `json:"source_refs"`
			} `json:"nodes"`
			Edges          []json.RawMessage `json:"edges"`
			SubgraphDigest model.Hash        `json:"subgraph_digest"`
		} `json:"data"`
	}
	decodeExplorerJSON(t, graph.Body, &graphEnvelope)
	if graphEnvelope.Status != "ok" || len(graphEnvelope.Data.Nodes) != 2 || len(graphEnvelope.Data.Edges) != 1 {
		t.Fatalf("graph status/nodes/edges=%s/%d/%d; want ok/2/1", graphEnvelope.Status, len(graphEnvelope.Data.Nodes), len(graphEnvelope.Data.Edges))
	}
	if _, err := model.ParseHash(string(graphEnvelope.Data.SubgraphDigest)); err != nil {
		t.Fatalf("invalid subgraph digest: %v", err)
	}
	var mainRef *model.SourceRef
	for _, node := range graphEnvelope.Data.Nodes {
		if strings.Contains(node.Label, "Main") && len(node.SourceRef) == 1 {
			ref := node.SourceRef[0]
			mainRef = &ref
		}
	}
	if mainRef == nil {
		t.Fatalf("published graph omitted Main source provenance: %s", graph.Body)
	}

	source := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/source/read", bootstrapEnvelope.Data.Bearer, csrf, map[string]any{
		"source_ref": mainRef, "max_output_bytes": 65_536,
	})
	if source.Code != http.StatusOK {
		t.Fatalf("source status=%d body=%s", source.Code, source.Body)
	}
	var sourceEnvelope struct {
		Data struct {
			SourceRef model.SourceRef `json:"source_ref"`
			Content   string          `json:"content"`
		} `json:"data"`
	}
	decodeExplorerJSON(t, source.Body, &sourceEnvelope)
	want := string(fixture.source[mainRef.Range.StartByte:mainRef.Range.EndByte])
	if sourceEnvelope.Data.SourceRef != *mainRef || sourceEnvelope.Data.Content != want || sourceEnvelope.Data.Content != "func Main() { Helper() }" {
		t.Fatalf("source response did not match raw indexed bytes: ref=%+v content=%q want=%q", sourceEnvelope.Data.SourceRef, sourceEnvelope.Data.Content, want)
	}

	invalidRoot := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/graph/query", bootstrapEnvelope.Data.Bearer, csrf, map[string]any{
		"root": map[string]any{"kind": "symbol", "symbol_id": "missing"},
	})
	if invalidRoot.Code != http.StatusOK || !strings.Contains(invalidRoot.Body, `"code":"INVALID_ARGUMENT"`) {
		t.Fatalf("unsupported graph root response=%d %s", invalidRoot.Code, invalidRoot.Body)
	}
	for _, test := range []struct {
		name string
		body map[string]any
	}{
		{name: "negative depth", body: map[string]any{"root": map[string]any{"kind": "workspace"}, "depth": -1}},
		{name: "depth beyond schema", body: map[string]any{"root": map[string]any{"kind": "workspace"}, "depth": 9}},
		{name: "node limit beyond schema", body: map[string]any{"root": map[string]any{"kind": "workspace"}, "node_limit": 501}},
		{name: "edge limit beyond schema", body: map[string]any{"root": map[string]any{"kind": "workspace"}, "edge_limit": 1501}},
		{name: "kind count beyond schema", body: map[string]any{"root": map[string]any{"kind": "workspace"}, "kinds": strings.Fields(strings.Repeat("symbol ", 33))}},
	} {
		t.Run("invalid "+test.name, func(t *testing.T) {
			response := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/graph/query", bootstrapEnvelope.Data.Bearer, csrf, test.body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body, `"code":"INVALID_ARGUMENT"`) {
				t.Fatalf("graph request outside frozen schema status/body=%d/%s", response.Code, response.Body)
			}
		})
	}
}

func TestExplorerMissingPartialAndPolicyChangedStates(t *testing.T) {
	t.Run("missing index", func(t *testing.T) {
		fixture := createExplorerFixture(t, sqlite.GenerationReady, false, false)
		service, err := appexplorer.NewService(appexplorer.Options{HomePath: fixture.home, WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID})
		if err != nil {
			t.Fatal(err)
		}
		client, baseURL, code, stop := startExplorerAPI(t, service)
		defer stop()
		_, bearer := bootstrapExplorer(t, client, baseURL, code)
		response := getExplorerWorkspace(t, client, baseURL, bearer)
		if response.IndexState != "missing" {
			t.Fatalf("index_state=%q; want missing", response.IndexState)
		}
		graph := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/graph/query", bearer, "", map[string]any{"root": map[string]any{"kind": "workspace"}})
		if graph.Code != http.StatusOK || !strings.Contains(graph.Body, `"code":"INDEX_MISSING"`) {
			t.Fatalf("missing-index graph response=%d %s; want INDEX_MISSING", graph.Code, graph.Body)
		}
	})

	t.Run("ready partial", func(t *testing.T) {
		fixture := createExplorerFixture(t, sqlite.GenerationReadyPartial, false, true)
		service, err := appexplorer.NewService(appexplorer.Options{HomePath: fixture.home, WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID})
		if err != nil {
			t.Fatal(err)
		}
		client, baseURL, code, stop := startExplorerAPI(t, service)
		defer stop()
		_, bearer := bootstrapExplorer(t, client, baseURL, code)
		response := getExplorerWorkspace(t, client, baseURL, bearer)
		if response.Status != "partial" || response.IndexState != "published" {
			t.Fatalf("partial generation status/state=%q/%q", response.Status, response.IndexState)
		}
	})

	t.Run("policy changed", func(t *testing.T) {
		fixture := createExplorerFixture(t, sqlite.GenerationReady, true, true)
		service, err := appexplorer.NewService(appexplorer.Options{HomePath: fixture.home, WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID})
		if err != nil {
			t.Fatal(err)
		}
		client, baseURL, code, stop := startExplorerAPI(t, service)
		defer stop()
		_, bearer := bootstrapExplorer(t, client, baseURL, code)
		response := getExplorerWorkspace(t, client, baseURL, bearer)
		if response.IndexState != "unavailable" {
			t.Fatalf("policy-mismatched generation index_state=%q; want unavailable", response.IndexState)
		}
		graph := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/graph/query", bearer, "", map[string]any{"root": map[string]any{"kind": "workspace"}})
		if graph.Code != http.StatusOK || !strings.Contains(graph.Body, `"code":"INDEX_STALE"`) || strings.Contains(graph.Body, "explorerfixture") {
			t.Fatalf("policy-mismatched graph response=%d %s; want sanitized INDEX_STALE", graph.Code, graph.Body)
		}
	})
}

func TestExplorerRejectsSecretBytesAndStaleSource(t *testing.T) {
	secret := []byte("package secret\nconst api_key = \"MCE4D_FAKE_SECRET_0123456789ABCDEF0123456789ABCDEF\"\n")
	fixture := createExplorerFixtureForSource(t, sqlite.GenerationReady, false, true, secret)
	service, err := appexplorer.NewService(appexplorer.Options{HomePath: fixture.home, WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID})
	if err != nil {
		t.Fatal(err)
	}
	client, baseURL, code, stop := startExplorerAPI(t, service)
	defer stop()
	_, bearer := bootstrapExplorer(t, client, baseURL, code)
	csrf := explorerCookie(t, client, baseURL, "mce4d_csrf")
	graph := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/graph/query", bearer, csrf, map[string]any{
		"root": map[string]any{"kind": "workspace"}, "depth": 3, "node_limit": 200, "edge_limit": 500,
	})
	if graph.Code != http.StatusOK || !strings.Contains(graph.Body, `"status":"ok"`) || strings.Contains(graph.Body, string(secret)) {
		t.Fatalf("secret graph response was not safely omitted: status=%d body=%s", graph.Code, graph.Body)
	}
	secretRef := model.SourceRef{
		WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID, GenerationID: fixture.generation,
		Path: "src/main.go", ContentHash: model.SHA256(secret), Range: model.ByteRange{StartByte: 0, EndByte: int64(len(secret))},
	}
	response := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/source/read", bearer, csrf, map[string]any{"source_ref": secretRef, "max_output_bytes": 65_536})
	if response.Code != http.StatusOK || !strings.Contains(response.Body, `"code":"SOURCE_PRECONDITION_FAILED"`) || strings.Contains(response.Body, string(secret)) {
		t.Fatalf("secret source response=%d %s", response.Code, response.Body)
	}
	staleRef := model.SourceRef{
		WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID, GenerationID: "90000000-0000-4000-8000-000000000099",
		Path: "src/main.go", ContentHash: model.SHA256(secret), Range: model.ByteRange{StartByte: 0, EndByte: int64(len(secret))},
	}
	response = postExplorerJSON(t, client, baseURL, "/api/explorer/v1/source/read", bearer, csrf, map[string]any{"source_ref": staleRef, "max_output_bytes": 65_536})
	if response.Code != http.StatusOK || !strings.Contains(response.Body, `"code":"SOURCE_PRECONDITION_FAILED"`) || strings.Contains(response.Body, string(secret)) {
		t.Fatalf("stale/secret source response=%d %s", response.Code, response.Body)
	}
}

func TestExplorerBrowserAcceptance(t *testing.T) {
	if os.Getenv("MCE4D_EXPLORER_BROWSER_ACCEPTANCE") != "1" {
		t.Skip("set MCE4D_EXPLORER_BROWSER_ACCEPTANCE=1 after the pinned UI build to run Playwright")
	}
	fixture := createExplorerFixture(t, sqlite.GenerationReady, false, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startup := &explorerStartup{line: make(chan struct{})}
	runDone := make(chan error, 1)
	go func() {
		runDone <- appexplorer.Run(ctx, appexplorer.Options{HomePath: fixture.home, WorktreeID: fixture.worktreeID, ProfileID: fixture.profileID}, "127.0.0.1:0", startup)
	}()
	select {
	case <-startup.line:
	case <-time.After(15 * time.Second):
		t.Fatalf("Explorer did not report startup details: %s", startup.String())
	}
	defer func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("stop Explorer: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Explorer did not stop after context cancellation")
		}
	}()
	parts := strings.Split(strings.TrimSpace(startup.String()), "\n")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "Explorer: http://127.0.0.1:") || !strings.HasPrefix(parts[1], "Bootstrap code: ") {
		t.Fatalf("unexpected Explorer startup output %q", startup.String())
	}
	root := projectRoot(t)
	node := os.Getenv("MCE4D_EXPLORER_NODE")
	if node == "" {
		node = "node"
	}
	command := execCommand(node, filepath.Join(root, "tests", "acceptance", "explorer_browser.mjs"))
	command.Env = append(os.Environ(), "MCE4D_EXPLORER_URL="+strings.TrimPrefix(parts[0], "Explorer: "), "MCE4D_EXPLORER_BOOTSTRAP_CODE="+strings.TrimPrefix(parts[1], "Bootstrap code: "), "MCE4D_EXPLORER_SOURCE=func Main() { Helper() }")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Playwright Explorer acceptance failed: %v\n%s", err, output)
	}
	t.Logf("Playwright result: %s", bytes.TrimSpace(output))
}

type explorerHTTPResult struct {
	Code    int
	Body    string
	Headers http.Header
}

type explorerWorkspaceResult struct {
	Status     string `json:"status"`
	IndexState string `json:"index_state"`
}

func startExplorerAPI(t *testing.T, service transport.Services) (*http.Client, string, string, func()) {
	t.Helper()
	listener, err := transport.ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	server, err := transport.NewServer(listener, service)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	code, ok := server.TakeBootstrapCode()
	if !ok {
		_ = server.Close()
		t.Fatal("Explorer did not issue its one-time bootstrap code")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	baseURL := "http://" + server.Host()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop Explorer API: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Explorer API did not stop")
		}
	}
	return client, baseURL, code, stop
}

func bootstrapExplorer(t *testing.T, client *http.Client, baseURL, code string) (string, string) {
	t.Helper()
	response := postExplorerJSON(t, client, baseURL, "/api/explorer/v1/session/bootstrap", code, "", map[string]any{"code": code})
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", response.Code, response.Body)
	}
	var envelope struct {
		Data struct {
			Bearer string `json:"bearer"`
		} `json:"data"`
	}
	decodeExplorerJSON(t, response.Body, &envelope)
	if envelope.Data.Bearer == "" {
		t.Fatal("bootstrap response has no bearer")
	}
	return response.Body, envelope.Data.Bearer
}

func getExplorerWorkspace(t *testing.T, client *http.Client, baseURL, bearer string) explorerWorkspaceResult {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/explorer/v1/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			IndexState string `json:"index_state"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("workspace status=%d", response.StatusCode)
	}
	return explorerWorkspaceResult{Status: envelope.Status, IndexState: envelope.Data.IndexState}
}

func postExplorerJSON(t *testing.T, client *http.Client, baseURL, route, bearer, csrf string, value any) explorerHTTPResult {
	t.Helper()
	return postExplorerJSONWithOrigin(t, client, baseURL, baseURL, route, bearer, csrf, value)
}

func postExplorerJSONWithOrigin(t *testing.T, client *http.Client, baseURL, origin, route, bearer, csrf string, value any) explorerHTTPResult {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+route, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if csrf != "" {
		request.Header.Set("X-MCE4D-CSRF", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return explorerHTTPResult{Code: response.StatusCode, Body: string(result), Headers: response.Header.Clone()}
}

func explorerCookie(t *testing.T, client *http.Client, baseURL, name string) string {
	t.Helper()
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	t.Fatalf("Explorer cookie %q was not set", name)
	return ""
}

func decodeExplorerJSON(t *testing.T, raw string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		t.Fatalf("decode Explorer JSON %q: %v", raw, err)
	}
}

func createExplorerFixture(t *testing.T, state sqlite.GenerationState, policyMismatch, withIndex bool) explorerFixture {
	t.Helper()
	return createExplorerFixtureForSource(t, state, policyMismatch, withIndex, []byte("package explorerfixture\n\nfunc Main() { Helper() }\n\nfunc Helper() {}\n"))
}

func createExplorerFixtureForSource(t *testing.T, state sqlite.GenerationState, policyMismatch, withIndex bool, source []byte) explorerFixture {
	t.Helper()
	home := t.TempDir()
	worktreeDir := filepath.Join(home, "worktrees", string(explorerWorktreeID))
	if err := os.MkdirAll(worktreeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := mceruntime.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	homeLease, err := mceruntime.AcquireHomeLifecycle(context.Background(), root, mceruntime.LockShared, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := homeLease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mceruntime.EnsureWorktreeLockLayout(root, string(explorerWorktreeID)); err != nil {
		t.Fatal(err)
	}
	worktreeLease, err := mceruntime.AcquireWorktreeLifecycle(context.Background(), root, string(explorerWorktreeID), mceruntime.LockShared, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := worktreeLease.Close(); err != nil {
		t.Fatal(err)
	}
	config := catalog.PolicyConfig{
		SchemaVersion: 1, AllowedRoots: []string{"fixture-root"}, DeniedGlobs: []string{},
		TrustedRulePaths: []string{}, DependencyRoots: []string{}, AllowedExecutables: []catalog.AllowedExecutable{},
		MaxFileBytes: catalog.DefaultPolicyMaxFile, MaxSourceBytes: catalog.DefaultPolicyMaxSource,
		MaxOutputBytes: catalog.DefaultPolicyMaxOutput, SecretRulesVersion: catalog.SecretRulesVersion,
	}
	policyDigest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := seedCatalog(t, home, config, policyDigest); err != nil {
		t.Fatal(err)
	}
	if err := seedRecords(t, home); err != nil {
		t.Fatal(err)
	}
	fixture := explorerFixture{home: home, worktreeID: explorerWorktreeID, profileID: explorerProfileID, generation: explorerGenerationID, policy: policyDigest}
	if !withIndex {
		return fixture
	}
	indexPolicy := policyDigest
	if policyMismatch {
		indexPolicy = model.SHA256([]byte("stale-policy"))
	}
	fixture.source = source
	if mainStart := bytes.Index(source, []byte("func Main()")); mainStart >= 0 {
		mainEnd := bytes.Index(source[mainStart:], []byte("\n"))
		if mainEnd < 0 {
			mainEnd = len(source)
		} else {
			mainEnd += mainStart
		}
		fixture.mainRange = model.ByteRange{StartByte: int64(mainStart), EndByte: int64(mainEnd)}
	}
	seedPublishedIndex(t, home, explorerWorktreeID, explorerProfileID, explorerGenerationID, indexPolicy, state, "src/main.go", source)
	return fixture
}

func seedCatalog(t *testing.T, home string, config catalog.PolicyConfig, policyDigest model.Hash) error {
	t.Helper()
	path := filepath.Join(home, "catalog.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := applyMigration(t, db, filepath.Join(projectRoot(t), "internal", "store", "sqlite", "migrations", "catalog", "001.sql")); err != nil {
		return err
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(`INSERT INTO catalog_identity(singleton,incarnation) VALUES(1,?)`, "90000000-0000-4000-8000-000000000001")
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO repositories(repo_id,common_dir,filesystem_identity,created_at) VALUES(?,?,?,?)`,
		"90000000-0000-4000-8000-000000000002", filepath.Join(home, "fixture.git"), "fixture-repository-identity", now)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO worktrees(worktree_id,repo_id,canonical_root,filesystem_identity,binding_revision,status,created_at,updated_at)
		VALUES(?,?,?,?,3,'registered',?,?)`, string(explorerWorktreeID), "90000000-0000-4000-8000-000000000002", filepath.Join(home, "target"), "fixture-worktree-identity", now, now)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO policies(worktree_id,policy_digest,config_json,created_at) VALUES(?,?,?,?)`, string(explorerWorktreeID), string(policyDigest), string(configJSON), now)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO worktree_policy_heads(worktree_id,policy_digest,policy_epoch,updated_at) VALUES(?,?,5,?)`, string(explorerWorktreeID), string(policyDigest), now)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO profiles(worktree_id,profile_id,config_json,toolchain_digest,provider_digest,created_at)
		VALUES(?,?,'{}',?,?,?)`, string(explorerWorktreeID), string(explorerProfileID), strings.Repeat("1", 64), strings.Repeat("2", 64), now)
	return err
}

func seedRecords(t *testing.T, home string) error {
	t.Helper()
	path := filepath.Join(home, "worktrees", string(explorerWorktreeID), "records.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := applyMigration(t, db, filepath.Join(projectRoot(t), "internal", "store", "sqlite", "migrations", "records", "001.sql")); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO records_meta(singleton,worktree_id,incarnation,record_epoch) VALUES(1,?,?,7)`, string(explorerWorktreeID), "90000000-0000-4000-8000-000000000003")
	return err
}

func seedPublishedIndex(t *testing.T, home string, worktreeID model.UUID, profileID model.Hash, generationID model.UUID, policyDigest model.Hash, state sqlite.GenerationState, sourcePath string, source []byte) {
	t.Helper()
	path := filepath.Join(home, "worktrees", string(worktreeID), "index.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, migration := range []string{"001.sql", "002.sql"} {
		if err := applyMigration(t, db, filepath.Join(projectRoot(t), "internal", "store", "sqlite", "migrations", "index", migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO store_identity(singleton,worktree_id,incarnation) VALUES(1,?,?)`, string(worktreeID), "90000000-0000-4000-8000-000000000004"); err != nil {
		t.Fatal(err)
	}
	snapshotDigest := model.SHA256([]byte("synthetic fixture snapshot"))
	if state == sqlite.GenerationReady && policyDigest != model.SHA256([]byte("stale-policy")) {
		// Keep fixture identity deterministic; policy mismatch is carried separately by its digest.
	}
	if policyDigest == model.SHA256([]byte("stale-policy")) {
		snapshotDigest = model.SHA256([]byte("synthetic stale-policy fixture snapshot"))
	}
	artifactDigest := model.SHA256([]byte("synthetic manifest descriptor"))
	manifest, err := sqlite.EncodeStoredCaptureManifestRef(model.StoredCaptureManifestRef{
		FormatVersion: 1, Kind: "external_capture_manifest", Path: "manifests/" + string(artifactDigest) + ".json",
		ByteLength: 64, ArtifactSHA256: artifactDigest, SnapshotDigest: snapshotDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO generations(generation_id,profile_id,snapshot_digest,state,owner_job_id,policy_digest,policy_epoch,provider_fingerprint,manifest_json,created_at,completed_at)
		VALUES(?,?,?,'BUILDING',?,?,?,?,?,?,NULL)`, string(generationID), string(profileID), string(snapshotDigest),
		"90000000-0000-4000-8000-000000000005", string(policyDigest), 5, strings.Repeat("3", 64), string(manifest), now); err != nil {
		t.Fatal(err)
	}
	contentHash := model.SHA256(source)
	if _, err := db.Exec(`INSERT INTO content_blobs(content_hash,bytes,byte_length) VALUES(?,?,?)`, string(contentHash), source, len(source)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO files(generation_id,path,content_hash,kind,status,byte_length,generated,reason_code)
		VALUES(?,?,?,'go_source','indexed',?,0,NULL)`, string(generationID), sourcePath, string(contentHash), len(source)); err != nil {
		t.Fatal(err)
	}
	mainStart := bytes.Index(source, []byte("func Main()"))
	helperStart := bytes.Index(source, []byte("func Helper()"))
	if mainStart >= 0 && helperStart >= 0 {
		mainEnd := mainStart + len("func Main() { Helper() }")
		helperEnd := helperStart + len("func Helper() {}")
		for _, symbol := range []struct {
			id, name, qualified, signature string
			start, end                     int64
		}{
			{"symbol-main", "Main", "explorerfixture.Main", "func Main()", int64(mainStart), int64(mainEnd)},
			{"symbol-helper", "Helper", "explorerfixture.Helper", "func Helper()", int64(helperStart), int64(helperEnd)},
		} {
			if _, err := db.Exec(`INSERT INTO symbols(generation_id,symbol_id,path,package_key,kind,name,qualified_name,scope_key,start_byte,end_byte,signature,exported)
			VALUES(?,?,?,NULL,'function',?,?,?,?,?,?,1)`, string(generationID), symbol.id, sourcePath, symbol.name, symbol.qualified, symbol.name, symbol.start, symbol.end, symbol.signature); err != nil {
				t.Fatal(err)
			}
		}
		callStart := int64(bytes.Index(source, []byte("Helper()")))
		callEnd := callStart + int64(len("Helper()"))
		if _, err := db.Exec(`INSERT INTO relations(generation_id,relation_id,source_symbol_id,target_symbol_id,kind,evidence_kind,site_path,start_byte,end_byte,reason,unresolved_label)
		VALUES(?, 'relation-main-helper', 'symbol-main', 'symbol-helper', 'calls', 'syntax_exact', ?, ?, ?, '', NULL)`, string(generationID), sourcePath, callStart, callEnd); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE generations SET state=?,completed_at=? WHERE generation_id=?`, string(state), now, string(generationID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO active_generations(profile_id,generation_id) VALUES(?,?)`, string(profileID), string(generationID)); err != nil {
		t.Fatal(err)
	}
}

func applyMigration(t *testing.T, db *sql.DB, path string) error {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, err = db.Exec(string(raw))
	if err != nil {
		return fmt.Errorf("apply fixture DDL %s: %w", filepath.Base(path), err)
	}
	return nil
}

func projectRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve acceptance source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
