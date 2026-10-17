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
	"reflect"
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
	capturedSource := []byte(`package captured
import "example.test/captured/dep"
type Inner struct { Value int }
func (i *Inner) Read() int { return i.Value }
type Box[T any] struct { *Inner; Value T }
func (b *Box[T]) Use(item T) (result T) {
	shadow := item
	{
		shadow := item
		_ = shadow
	}
	_ = b.Inner.Value
	_ = b.Read()
	result = shadow
	return
}
func Add(left, right int) int { return left + right }
func External(record dep.Record) int { _ = record.Value; return record.Read() }
`)
	diskSource := []byte("package captured\nfunc Add(left, right int) int { panic(\"disk source must not win\") }\n")
	diskDependency := []byte("package dep\ntype Record struct { Value int }\nfunc (Record) Read() int { panic(\"disk dependency must not win\") }\n")
	if err := os.Mkdir(filepath.Join(root, "dep"), 0o700); err != nil {
		t.Fatal(err)
	}
	for relative, data := range map[string][]byte{"go.mod": goMod, "main.go": diskSource, "dep/record.go": diskDependency} {
		if err := os.WriteFile(filepath.Join(root, relative), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	capturedDependency := []byte("package dep\ntype Record struct { Value int }\nfunc (Record) Read() int { return 7 }\n")
	goHash := sha256File(t, goBinary)
	request := LoadRequest{
		SnapshotRoot: root, ModuleRoot: ".", Patterns: []string{"."},
		CapturedFiles: map[string][]byte{"go.mod": goMod, "main.go": capturedSource, "dep/record.go": capturedDependency},
		CaptureHashes: map[string]model.Hash{"go.mod": hashBytes(goMod), "main.go": hashBytes(capturedSource), "dep/record.go": hashBytes(capturedDependency)},
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
	input := PackageInput{
		GenerationID: testGeneration, ProfileID: run.ProfileID, ImportPath: "example.test/captured",
		ModuleKey: "example.test/captured", Variant: VariantProduction, MaxFileBytes: DefaultMaxFileBytes,
		Files: []CapturedFile{{Path: "main.go", Source: capturedSource}},
	}
	facts, err := AnalyzeLoadedPackage(input, run, pkg)
	if err != nil {
		t.Fatal(err)
	}
	if facts.TypedStatus != TypedComplete || len(facts.Defs) == 0 || len(facts.Uses) == 0 || len(facts.Selections) < 3 {
		t.Fatalf("typed definitions, uses, and selections are incomplete: status=%s defs=%d uses=%d selections=%d diagnostics=%+v", facts.TypedStatus, len(facts.Defs), len(facts.Uses), len(facts.Selections), facts.Diagnostics)
	}
	shadowDeclarations := declarationsNamed(facts, "shadow", "short_var_candidate")
	if len(shadowDeclarations) != 2 {
		t.Fatalf("syntax identity lost one shadowed local: %+v", shadowDeclarations)
	}
	shadowIDs := map[model.Hash]struct{}{shadowDeclarations[0].SymbolID: {}, shadowDeclarations[1].SymbolID: {}}
	shadowUses := 0
	usedShadowIDs := make(map[model.Hash]struct{}, len(shadowIDs))
	for _, use := range facts.Uses {
		if use.Name == "shadow" {
			shadowUses++
			if _, ok := shadowIDs[use.SymbolID]; !ok {
				t.Fatalf("shadow use points to a different object identity: %+v", use)
			}
			usedShadowIDs[use.SymbolID] = struct{}{}
		}
	}
	if shadowUses != 2 || len(usedShadowIDs) != 2 {
		t.Fatalf("shadowed locals should retain one use per binding, uses=%d identities=%v", shadowUses, usedShadowIDs)
	}
	if len(shadowIDs) != 2 {
		t.Fatalf("both shadow identities should retain uses: %+v", shadowIDs)
	}
	declarationIDs := make(map[string]map[model.Hash]struct{})
	for _, declaration := range facts.Declarations {
		if declarationIDs[declaration.Name] == nil {
			declarationIDs[declaration.Name] = make(map[model.Hash]struct{})
		}
		declarationIDs[declaration.Name][declaration.SymbolID] = struct{}{}
	}
	allLocalIDs := make(map[model.Hash]struct{})
	for _, ids := range declarationIDs {
		for id := range ids {
			allLocalIDs[id] = struct{}{}
		}
	}
	for _, use := range facts.Uses {
		if _, local := allLocalIDs[use.SymbolID]; local {
			expected := declarationIDs[use.Name]
			if _, ok := expected[use.SymbolID]; !ok {
				t.Fatalf("local use %q does not point to its declaration identity: %+v", use.Name, use)
			}
		}
	}
	localValueSelections, externalValueSelections := 0, 0
	for _, name := range []string{"Inner", "Value", "Read"} {
		found := false
		for _, selection := range facts.Selections {
			if selection.Name == name {
				found = true
				if selection.Kind == "" || selection.SymbolID == "" {
					t.Fatalf("selection lost its kind or stable object identity: %+v", selection)
				}
			}
		}
		if !found {
			t.Errorf("missing typed selection for %q", name)
		}
	}
	for _, fact := range facts.Defs {
		if err := fact.Range.ValidateAgainst(capturedSource); err != nil {
			t.Fatalf("invalid Def range: %+v: %v", fact, err)
		}
	}
	for _, fact := range facts.Uses {
		if err := fact.Range.ValidateAgainst(capturedSource); err != nil {
			t.Fatalf("invalid Use range: %+v: %v", fact, err)
		}
	}
	for _, fact := range facts.Selections {
		if err := fact.Range.ValidateAgainst(capturedSource); err != nil {
			t.Fatalf("invalid Selection range: %+v: %v", fact, err)
		}
		if fact.Name == "Value" {
			if _, local := declarationIDs[fact.Name][fact.SymbolID]; local {
				localValueSelections++
			} else {
				externalValueSelections++
			}
		}
	}
	if len(declarationIDs["Value"]) != 2 || localValueSelections == 0 || externalValueSelections == 0 {
		t.Fatalf("local and imported fields with the same spelling lost distinct identities: local=%d external=%d declarations=%v", localValueSelections, externalValueSelections, declarationIDs["Value"])
	}
	repeated, err := AnalyzeLoadedPackage(input, run, pkg)
	if err != nil || !reflect.DeepEqual(facts, repeated) {
		t.Fatalf("typed facts are not deterministic: err=%v", err)
	}
	mismatchedInput := input
	mismatchedInput.Files = []CapturedFile{{Path: "main.go", Source: []byte(strings.Replace(string(capturedSource), "Box[T any]", "Box[U any]", 1))}}
	if _, err := AnalyzeLoadedPackage(mismatchedInput, run, pkg); !errors.Is(err, ErrCapturedSource) {
		t.Fatalf("type facts accepted bytes outside the captured load: %v", err)
	}
	mismatchedProfile := input
	mismatchedProfile.ProfileID = testProfile
	if mismatchedProfile.ProfileID == run.ProfileID {
		t.Fatal("fixture profile unexpectedly equals the loader profile")
	}
	if _, err := AnalyzeLoadedPackage(mismatchedProfile, run, pkg); !errors.Is(err, ErrLoadRunMismatch) {
		t.Fatalf("type facts accepted a different captured profile: %v", err)
	}
	if len(run.typesRegistry) < 2 {
		t.Fatalf("one load run should own root and imported type packages, got %d", len(run.typesRegistry))
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
	independentFacts, err := AnalyzeLoadedPackage(input, secondRun, secondRun.Packages()[0])
	if err != nil || !reflect.DeepEqual(facts, independentFacts) {
		t.Fatalf("identical captured input produced different typed facts across LoadRuns: err=%v", err)
	}
	if _, err := AnalyzeLoadedPackage(input, secondRun, pkg); !errors.Is(err, ErrLoadRunMismatch) {
		t.Fatalf("cross-LoadRun package was accepted: %v", err)
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
		facts, err := AnalyzeLoadedPackage(PackageInput{
			GenerationID: testGeneration, ProfileID: run.ProfileID, ImportPath: "example.test/offline",
			ModuleKey: "example.test/offline", Variant: VariantProduction, MaxFileBytes: DefaultMaxFileBytes,
			Files: []CapturedFile{{Path: "main.go", Source: source}},
		}, run, pkg)
		if err != nil || facts.TypedStatus != TypedPartial || facts.SyntaxStatus != SyntaxComplete || len(facts.Declarations) == 0 {
			t.Fatalf("offline typed failure did not preserve syntax facts: status=%s syntax=%s declarations=%d err=%v", facts.TypedStatus, facts.SyntaxStatus, len(facts.Declarations), err)
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
