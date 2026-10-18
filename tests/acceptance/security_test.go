package acceptance_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/analysis/golang"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/inventory"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/mcp"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const fakeSecret = "MCE4D_FAKE_SECRET_0123456789ABCDEF0123456789ABCDEF"

func TestMaliciousWorkspaceStaysDataAndCannotEscapeRoot(t *testing.T) {
	root := copyFixtureTree(t, "workspace", true)
	gitBinary, err := osexec.LookPath("git")
	if err != nil {
		t.Skip("Git executable is unavailable for the isolated inventory probe")
	}
	init := osexec.Command(gitBinary, "init", "--quiet", "--template=", root)
	init.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("initialize synthetic canary repository: %v: %s", err, output)
	}
	writeFixtureFile(t, filepath.Join(root, ".git", "config"), fixtureBytes(t, "workspace/git-config"))

	before, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := mceruntime.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	caseMode, err := opened.CaseMode()
	if err != nil || caseMode == paths.CaseUnknown {
		t.Fatalf("test filesystem case mode must be observable: mode=%d err=%v", caseMode, err)
	}
	rules, err := policy.New([]string{"private/**"}, caseMode)
	if err != nil {
		t.Fatal(err)
	}

	prompt, err := mceruntime.ReadAllowed(opened, "README.md", rules)
	if err != nil {
		t.Fatalf("read prompt-injection fixture as ordinary text: %v", err)
	}
	if !strings.Contains(string(prompt.Bytes), "Ignore all previous instructions") {
		t.Fatal("prompt-injection fixture was not read as the expected data")
	}
	for _, name := range []string{".env", ".env.example", ".git/config", "secrets/id_ed25519", "private/missing.go"} {
		captured, readErr := mceruntime.ReadAllowed(opened, name, rules)
		if !errors.Is(readErr, mceruntime.ErrPathDenied) || len(captured.Bytes) != 0 {
			t.Errorf("sensitive path %q: bytes=%d err=%v; want deny before open", name, len(captured.Bytes), readErr)
		}
	}
	secret, err := mceruntime.ReadAllowed(opened, "src/mixed_secret.go", rules)
	if !errors.Is(err, policy.ErrSecretDetected) || len(secret.Bytes) != 0 {
		t.Fatalf("mixed-source marker escaped scanner: bytes=%d err=%v", len(secret.Bytes), err)
	}
	if _, err := mceruntime.ReadAllowed(opened, "src/../README.md", rules); err == nil {
		t.Fatal("parent traversal was accepted")
	}
	if _, err := mceruntime.ReadAllowed(opened, `\\server\share\source.go`, rules); err == nil {
		t.Fatal("UNC-shaped path was accepted")
	}
	if _, err := mceruntime.ReadAllowed(opened, "src/file.go:stream", rules); err == nil {
		t.Fatal("alternate data stream path was accepted")
	}
	if _, err := mceruntime.ReadAllowed(opened, "src/bad\nname.go", rules); err == nil {
		t.Fatal("control character path was accepted")
	}
	if _, err := mceruntime.ReadAllowed(opened, string([]byte{0xff}), rules); !errors.Is(err, paths.ErrUnsupportedEncoding) {
		t.Fatalf("invalid UTF-8 path error=%v", err)
	}
	if captured, err := mceruntime.ReadAllowed(opened, "src/main.go", rules); err != nil || string(captured.Bytes) != "package fixture\n\nconst Value = 7\n" {
		t.Fatalf("safe source capture bytes=%q err=%v", captured.Bytes, err)
	}
	if err := validateCaseSensitiveBoundary(t, opened, rules, caseMode); err != nil {
		t.Fatal(err)
	}

	snapshot, err := inventory.Scan(context.Background(), inventory.Options{
		Root: root, GitExecutable: gitBinary, Policy: rules, CaseMode: caseMode,
	})
	if err != nil {
		t.Fatalf("scan isolated canary with hostile local fsmonitor config: %v", err)
	}
	serialized, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), fakeSecret) || strings.Contains(string(serialized), "Ignore all previous instructions") {
		t.Fatal("inventory metadata exposed source bytes or prompt text")
	}
	for _, item := range snapshot.Files {
		if item.Path == ".env" || item.Path == ".env.example" || item.Path == "secrets/id_ed25519" {
			t.Errorf("inventory included denied path %q", item.Path)
		}
	}
	after, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("read and inventory operations mutated canary repository: before=%s after=%s", before, after)
	}
}

func TestSymlinkSwapNeverReturnsBytesOutsideRegisteredRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	insideBytes := []byte("package safe\nconst Value = 1\n")
	outsideBytes := []byte("package hostile\nconst Synthetic = \"" + fakeSecret + "\"\n")
	writeFixtureFile(t, filepath.Join(root, "inside.go"), insideBytes)
	writeFixtureFile(t, filepath.Join(outside, "outside.go"), outsideBytes)
	opened, err := mceruntime.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	rules, err := policy.New(nil, paths.CaseSensitive)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "pivot.go")
	if err := os.Symlink(filepath.Join(outside, "outside.go"), link); err != nil {
		t.Skipf("symlink race fixture unavailable on this host: %v", err)
	}
	if captured, readErr := mceruntime.ReadAllowed(opened, "pivot.go", rules); readErr == nil || len(captured.Bytes) != 0 {
		t.Fatalf("static symlink read bytes=%q err=%v; want fail-closed", captured.Bytes, readErr)
	}

	var swapFailures atomic.Int64
	var successfulSwaps atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 300; index++ {
			if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
				swapFailures.Add(1)
				continue
			}
			target := filepath.Join(outside, "outside.go")
			if index%2 == 1 {
				target = filepath.Join(root, "inside.go")
			}
			if err := os.Symlink(target, link); err != nil {
				swapFailures.Add(1)
			} else {
				successfulSwaps.Add(1)
			}
		}
	}()
	for index := 0; index < 300; index++ {
		captured, readErr := mceruntime.ReadAllowed(opened, "pivot.go", rules)
		if readErr == nil || len(captured.Bytes) != 0 {
			t.Fatalf("racing symlink read bytes=%q err=%v; want fail-closed", captured.Bytes, readErr)
		}
	}
	<-done
	if swaps := successfulSwaps.Load(); swaps == 0 {
		t.Fatalf("symlink race performed no swaps; failures=%d", swapFailures.Load())
	}
}

func TestAllPublicMCPRoutesKeepTheSyntheticCanaryUnchanged(t *testing.T) {
	// NewServer is the repository's public MCP boundary at this revision; it
	// accepts operation handlers by injection, so these explicit read-only
	// canary handlers exercise all exposed routes without claiming application
	// use-case wiring that is not present in the repository yet.
	root := copyFixtureTree(t, "workspace", true)
	opened, err := mceruntime.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	caseMode, err := opened.CaseMode()
	if err != nil || caseMode == paths.CaseUnknown {
		t.Fatalf("case mode must be known for the canary: mode=%d err=%v", caseMode, err)
	}
	rules, err := policy.New(nil, caseMode)
	if err != nil {
		t.Fatal(err)
	}
	before, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	toolNames := []string{
		"mce4d_workspace_status", "mce4d_locate", "mce4d_context_pack",
		"mce4d_expand_pack", "mce4d_validate_pack", "mce4d_rename_preview",
	}
	handlers := make(mcp.Handlers, len(toolNames))
	var calls atomic.Int64
	for _, name := range toolNames {
		handlers[name] = func(context.Context, json.RawMessage) ([]byte, error) {
			captured, readErr := mceruntime.ReadAllowed(opened, "README.md", rules)
			if readErr != nil || len(captured.Bytes) == 0 {
				return nil, fmt.Errorf("read synthetic prompt fixture: %w", readErr)
			}
			calls.Add(1)
			return nil, errors.New("synthetic read-only handler has no indexed result")
		}
	}
	server, err := mcp.NewServer(handlers)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverConn, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverConn.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "mce4d-security-acceptance", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	arguments := map[string]map[string]any{
		"mce4d_workspace_status": {},
		"mce4d_locate":           {"query": "repository prompt injection"},
		"mce4d_context_pack":     {"selector": map[string]any{"kind": "workspace"}, "task_kind": "explore"},
		"mce4d_expand_pack":      {"pack_token": "synthetic-token", "next_cursor": "synthetic-cursor"},
		"mce4d_validate_pack":    {"pack_token": "synthetic-token"},
		"mce4d_rename_preview":   {"selector": map[string]any{"kind": "source_position", "path": "src/main.go", "content_hash": hashHex([]byte("package fixture\n\nconst Value = 7\n")), "byte_offset": 23}, "new_name": "UpdatedValue"},
	}
	for _, name := range toolNames {
		result, callErr := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: arguments[name]})
		if callErr != nil {
			t.Fatalf("public MCP route %s failed protocol validation: %v", name, callErr)
		}
		if result == nil || !result.IsError {
			t.Fatalf("synthetic operation %s did not return the generic unavailable result: %+v", name, result)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), fakeSecret) || strings.Contains(string(encoded), "Ignore all previous instructions") {
			t.Fatalf("public MCP route %s returned canary source content", name)
		}
	}
	if calls.Load() != int64(len(toolNames)) {
		t.Fatalf("read-only canary handler calls=%d, want %d", calls.Load(), len(toolNames))
	}
	after, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("public MCP routes mutated synthetic canary: before=%s after=%s", before, after)
	}
}

func TestPoisonedGoEnvironmentDoesNotDownloadOrAcceptMutatedCapture(t *testing.T) {
	goBinary, err := osexec.LookPath("go")
	if err != nil {
		t.Skip("pinned Go toolchain is unavailable for offline loader acceptance")
	}
	root := copyFixtureTree(t, "missing-module", false)
	moduleCache := filepath.Join(t.TempDir(), "module-cache")
	scratch := filepath.Join(t.TempDir(), "scratch")
	for _, directory := range []string{moduleCache, scratch} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "synthetic offline proxy", http.StatusNotFound)
	}))
	defer proxy.Close()
	t.Setenv("GOFLAGS", "-mod=mod -v")
	t.Setenv("GOPACKAGESDRIVER", filepath.Join(root, "must-not-run-driver"))
	t.Setenv("GOPROXY", proxy.URL)
	t.Setenv("GOSUMDB", proxy.URL)
	t.Setenv("GOTOOLCHAIN", "auto")
	request := golang.LoadRequest{
		SnapshotRoot: root, ModuleRoot: ".", Patterns: []string{"."},
		CapturedFiles: map[string][]byte{"go.mod": goMod, "main.go": source},
		CaptureHashes: map[string]model.Hash{"go.mod": model.SHA256(goMod), "main.go": model.SHA256(source)},
		Profile: golang.LoadProfile{
			GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ModuleRoots: []string{"."},
			ToolchainDigest: model.SHA256([]byte("synthetic-pinned-toolchain")), ProviderDigest: model.SHA256([]byte("synthetic-pinned-provider")),
			ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoBinary: goBinary,
			GoVersion: runtime.Version(), GoBinarySHA256: fileSHA256(t, goBinary), ModuleCacheRoot: moduleCache, ScratchRoot: scratch,
		},
	}
	request.CapturedFiles["main.go"] = []byte("package malicious\nfunc Value() int { return 2 }\n")
	if _, err := golang.LoadTrustedPackages(context.Background(), request); !errors.Is(err, golang.ErrCapturedSource) {
		t.Fatalf("mutated captured source error=%v, want ErrCapturedSource", err)
	}
	request.CapturedFiles["main.go"] = source
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	loaded, err := golang.LoadTrustedPackages(ctx, request)
	if err != nil {
		t.Fatalf("offline missing-module fallback: %v", err)
	}
	if loaded.Status != golang.LoadPartial || len(loaded.Diagnostics) == 0 {
		t.Fatalf("missing local module did not preserve an explicit partial result: status=%s diagnostics=%+v", loaded.Status, loaded.Diagnostics)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("poisoned host GOPROXY/GOSUMDB received %d requests; loader must force offline mode", got)
	}
}

func validateCaseSensitiveBoundary(t *testing.T, root *mceruntime.Root, rules *policy.Policy, mode paths.CaseMode) error {
	t.Helper()
	captured, err := mceruntime.ReadAllowed(root, "src/caseprobe.go", rules)
	if mode == paths.CaseInsensitive {
		if err != nil || string(captured.Bytes) != "package fixture\n\nconst CaseProbe = \"case boundary\"\n" {
			return fmt.Errorf("case-insensitive file lookup bytes=%q err=%v", captured.Bytes, err)
		}
		return nil
	}
	if err == nil || len(captured.Bytes) != 0 {
		return fmt.Errorf("case-sensitive lookup unexpectedly matched bytes=%q", captured.Bytes)
	}
	return nil
}

func copyFixtureTree(t *testing.T, relative string, skipGit bool) string {
	t.Helper()
	sourceRoot := filepath.Join("..", "..", "testdata", "malicious", filepath.FromSlash(relative))
	destination := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(sourceRoot, func(source string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relativePath, err := filepath.Rel(sourceRoot, source)
		if err != nil {
			return err
		}
		if relativePath == "." {
			return nil
		}
		if relativePath == "git-config" {
			return nil
		}
		if skipGit && (relativePath == ".git" || strings.HasPrefix(relativePath, ".git"+string(filepath.Separator))) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relativePath)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("fixture contains unsupported member %q", relativePath)
		}
		value, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return os.WriteFile(target, value, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return destination
}

func fixtureBytes(t *testing.T, relative string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "malicious", filepath.FromSlash(relative))
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func writeFixtureFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
}

func treeDigest(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = hash.Write([]byte(filepath.ToSlash(relative)))
		_, _ = hash.Write([]byte{0})
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = hash.Write([]byte(target))
			_, _ = hash.Write([]byte{0})
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported canary member %q", relative)
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = hash.Write(value)
		_, _ = hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fileSHA256(t *testing.T, path string) model.Hash {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(value)
	return model.Hash(hex.EncodeToString(digest[:]))
}

func hashHex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
