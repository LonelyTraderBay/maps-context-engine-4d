package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestLoadTrustedPackagesUsesCapturedSourceAndOfflineProfile(t *testing.T) {
	goBinary, err := osexec.LookPath("go")
	if err != nil {
		t.Skip("pinned Go toolchain is not available on PATH")
	}
	root := t.TempDir()
	moduleCache := filepath.Join(root, "module-cache")
	scratch := filepath.Join(root, "scratch")
	if err := os.Mkdir(moduleCache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	goMod := []byte("module example.test/captured\n\ngo 1.27.0\n")
	capturedSource := []byte("package captured\nfunc Add(left, right int) int { return left + right }\n")
	diskSource := []byte("package captured\nfunc Add(left, right int) int { panic(\"disk source must not win\") }\n")
	for relative, data := range map[string][]byte{"go.mod": goMod, "main.go": diskSource} {
		if err := os.WriteFile(filepath.Join(root, relative), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	goHash := sha256File(t, goBinary)
	request := LoadRequest{
		SnapshotRoot: root, ModuleRoot: ".", Patterns: []string{"."},
		CapturedFiles: map[string][]byte{"go.mod": goMod, "main.go": capturedSource},
		CaptureHashes: map[string]model.Hash{"go.mod": hashBytes(goMod), "main.go": hashBytes(capturedSource)},
		Profile: LoadProfile{
			GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ModuleRoots: []string{"."},
			ToolchainDigest: hashBytes([]byte("pinned-go-toolchain")), ProviderDigest: hashBytes([]byte("pinned-go-packages")),
			ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoBinary: goBinary,
			GoVersion: runtime.Version(), GoBinarySHA256: goHash, ModuleCacheRoot: moduleCache, ScratchRoot: scratch,
		},
	}
	// Host overrides are deliberately hostile; go/packages must see only the
	// closed profile built by the loader.
	t.Setenv("GOFLAGS", "-mod=mod -v")
	t.Setenv("GOPACKAGESDRIVER", filepath.Join(root, "must-not-run-driver"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run, err := LoadTrustedPackages(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != LoadComplete {
		t.Fatalf("load status = %s, diagnostics = %+v", run.Status, run.Diagnostics)
	}
	if run.ID == "" || run.ProfileID == "" || len(run.Packages()) != 1 {
		t.Fatalf("load run missing identity/profile/package: %+v", run)
	}
	pkg := run.Packages()[0]
	if pkg.Types == nil || pkg.TypesInfo == nil || pkg.Fset == nil || len(pkg.Syntax) != 1 {
		t.Fatalf("typed package data is incomplete: types=%v info=%v files=%d", pkg.Types != nil, pkg.TypesInfo != nil, len(pkg.Syntax))
	}
	if !hasFunction(pkg.Syntax[0], "Add") {
		t.Fatal("loaded AST does not contain the captured Add function")
	}
	if hasCall(pkg.Syntax[0], "panic") {
		t.Fatal("loader parsed on-disk bytes instead of the captured overlay")
	}
	if len(run.typesRegistry) != 1 {
		t.Fatalf("one load run should own exactly one type registry entry, got %d", len(run.typesRegistry))
	}
	secondRun, err := LoadTrustedPackages(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if secondRun.Status != LoadComplete || secondRun.ID == run.ID || secondRun.ProfileID != run.ProfileID {
		t.Fatalf("separate loads must retain one profile but distinct run identities: first=%+v second=%+v", run, secondRun)
	}
	if secondRun.Packages()[0].Types == pkg.Types {
		t.Fatal("go/types package object was reused across independent loads")
	}
	changedProfile := request.Profile
	if changedProfile.GOOS == "windows" {
		changedProfile.GOOS = "linux"
	} else {
		changedProfile.GOOS = "windows"
	}
	changedProfileID, err := effectiveGoProfileID(changedProfile)
	if err != nil {
		t.Fatal(err)
	}
	if changedProfileID == run.ProfileID {
		t.Fatal("GOOS change did not change the effective profile identity")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("per-run Go cache was not removed: %v", entries)
	}
}

func TestLoadTrustedPackagesKeepsSyntaxFallbackWhenOfflineDependencyIsMissing(t *testing.T) {
	goBinary, err := osexec.LookPath("go")
	if err != nil {
		t.Skip("pinned Go toolchain is not available on PATH")
	}
	root := t.TempDir()
	moduleCache := filepath.Join(root, "module-cache")
	scratch := filepath.Join(root, "scratch")
	for _, dir := range []string{moduleCache, scratch} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	goMod := []byte("module example.test/offline\n\ngo 1.27.0\n\nrequire example.test/missing v1.0.0\n")
	source := []byte("package offline\nimport \"example.test/missing\"\nfunc Value() int { return missing.Value() }\n")
	for relative, data := range map[string][]byte{"go.mod": goMod, "main.go": source} {
		if err := os.WriteFile(filepath.Join(root, relative), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	request := LoadRequest{
		SnapshotRoot: root, ModuleRoot: ".", Patterns: []string{"."},
		CapturedFiles: map[string][]byte{"go.mod": goMod, "main.go": source},
		CaptureHashes: map[string]model.Hash{"go.mod": hashBytes(goMod), "main.go": hashBytes(source)},
		Profile: LoadProfile{
			GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ModuleRoots: []string{"."},
			ToolchainDigest: hashBytes([]byte("toolchain")), ProviderDigest: hashBytes([]byte("provider")),
			ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoBinary: goBinary,
			GoVersion: runtime.Version(), GoBinarySHA256: sha256File(t, goBinary),
			ModuleCacheRoot: moduleCache, ScratchRoot: scratch,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run, err := LoadTrustedPackages(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != LoadPartial || len(run.Diagnostics) == 0 {
		t.Fatalf("missing offline dependency must produce a partial fallback: %+v", run)
	}
	for _, pkg := range run.Packages() {
		if pkg.Types != nil || pkg.TypesInfo != nil {
			t.Fatal("incomplete offline package leaked semantic types")
		}
	}
}

func TestLoadTrustedPackagesFallsBackForUnapprovedClosureAndToolDrift(t *testing.T) {
	root := t.TempDir()
	moduleCache := filepath.Join(root, "module-cache")
	scratch := filepath.Join(root, "scratch")
	for _, dir := range []string{moduleCache, scratch} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	goMod := []byte("module example.test/captured\n\ngo 1.27.0\n\nreplace example.test/dep => ../dep\n")
	source := []byte("package captured\nfunc Value() int { return 1 }\n")
	request := LoadRequest{
		SnapshotRoot: root, ModuleRoot: ".", Patterns: []string{"."},
		CapturedFiles: map[string][]byte{"go.mod": goMod, "main.go": source},
		CaptureHashes: map[string]model.Hash{"go.mod": hashBytes(goMod), "main.go": hashBytes(source)},
		Profile: LoadProfile{
			GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ModuleRoots: []string{"."},
			ToolchainDigest: hashBytes([]byte("toolchain")), ProviderDigest: hashBytes([]byte("provider")),
			ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoBinary: filepath.Join(root, "missing-go"),
			GoVersion: "go1.27.2", GoBinarySHA256: hashBytes([]byte("expected-tool")),
			ModuleCacheRoot: moduleCache, ScratchRoot: scratch,
		},
	}
	run, err := LoadTrustedPackages(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != LoadPartial || len(run.Packages()) != 0 || len(run.Diagnostics) != 1 || run.Diagnostics[0].Code != "local_replace_not_preflighted" {
		t.Fatalf("unapproved local replacement did not fail closed to syntax: %+v", run)
	}
	request.CapturedFiles["main.go"] = []byte("package captured\nfunc Value() int { return 2 }\n")
	if _, err := LoadTrustedPackages(context.Background(), request); !errors.Is(err, ErrCapturedSource) {
		t.Fatalf("capture hash mismatch error = %v, want ErrCapturedSource", err)
	}
}

func TestDependencyClosureDigestTracksModuleSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/dep\n\ngo 1.27.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "dep.go")
	if err := os.WriteFile(sourcePath, []byte("package dep\nconst Value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	module := []dependencyModule{{Path: "example.test/dep", Version: "v1.0.0", Dir: root}}
	first, err := hashDependencyModules(context.Background(), module)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashDependencyModules(context.Background(), module)
	if err != nil || first != second {
		t.Fatalf("unchanged dependency closure digest is unstable: first=%s second=%s err=%v", first, second, err)
	}
	if err := os.WriteFile(sourcePath, []byte("package dep\nconst Value = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := hashDependencyModules(context.Background(), module)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("dependency source mutation did not change closure digest")
	}
}

func TestDependencyRootMustBeCanonicalAndPresent(t *testing.T) {
	root := t.TempDir()
	request := LoadRequest{
		SnapshotRoot: root,
		Profile:      LoadProfile{DependencyRoots: []string{"missing-dependency"}},
	}
	_, _, complete, reason, err := preflightAllowedDependencyModules(context.Background(), request, root, root)
	if err != nil || complete || !strings.Contains(reason, "dependency root") {
		t.Fatalf("missing dependency root result: complete=%v reason=%q err=%v", complete, reason, err)
	}
}

func sha256File(t *testing.T, path string) model.Hash {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return model.Hash(hex.EncodeToString(digest[:]))
}

func hashBytes(value []byte) model.Hash {
	digest := sha256.Sum256(value)
	return model.Hash(hex.EncodeToString(digest[:]))
}

func hasFunction(file *ast.File, name string) bool {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return true
		}
	}
	return false
}

func hasCall(file *ast.File, name string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		identifier, ok := call.Fun.(*ast.Ident)
		found = found || (ok && identifier.Name == name)
		return !found
	})
	return found
}
