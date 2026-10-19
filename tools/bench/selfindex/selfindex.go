// Package selfindex contains a bounded harness for comparing the engine's
// syntax facts with independent Go-tool output. It is benchmark/acceptance
// code, not a product indexing API.
package selfindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/analysis/golang"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/indexer"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/inventory"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
)

const AnalyzerPackage = "./internal/analysis/golang"

var ErrUnsupportedBoundary = errors.New("self-index harness boundary is unsupported")

type Declaration struct {
	Path             string          `json:"path"`
	Name             string          `json:"name"`
	Kind             string          `json:"kind"`
	NameRange        model.ByteRange `json:"name_range"`
	DeclarationRange model.ByteRange `json:"declaration_range"`
}

type File struct {
	Path        string     `json:"path"`
	ContentHash model.Hash `json:"content_hash"`
	ByteLength  int64      `json:"byte_length"`
}

type EngineResult struct {
	PackageImportPath         string        `json:"package_import_path"`
	InventoryHead             string        `json:"inventory_head"`
	InventoryFileCount        int           `json:"inventory_file_count"`
	InventoryCoverage         string        `json:"inventory_coverage"`
	Files                     []File        `json:"files"`
	EngineDeclarations        []Declaration `json:"engine_declarations"`
	OracleDeclarations        []Declaration `json:"oracle_declarations"`
	SourceBytes               int64         `json:"source_bytes"`
	SyntaxCacheKeyV1          model.Hash    `json:"syntax_cache_key_provider_v1"`
	SyntaxCacheKeyV2          model.Hash    `json:"syntax_cache_key_provider_v2"`
	ProviderChangeInvalidates bool          `json:"provider_change_invalidates_cache_key"`
	OperationBoundary         string        `json:"operation_boundary"`
}

type FallbackResult struct {
	Tool               string        `json:"tool"`
	EngineInvoked      bool          `json:"engine_invoked"`
	PackageImportPath  string        `json:"package_import_path"`
	Files              []File        `json:"files"`
	OracleDeclarations []Declaration `json:"oracle_declarations"`
	SourceBytes        int64         `json:"source_bytes"`
}

type goListPackage struct {
	Dir        string   `json:"Dir"`
	ImportPath string   `json:"ImportPath"`
	GoFiles    []string `json:"GoFiles"`
}

func AnalyzePinnedPackage(ctx context.Context, root string) (EngineResult, error) {
	if ctx == nil || root == "" {
		return EngineResult{}, fmt.Errorf("self-index request requires context and root: %w", ErrUnsupportedBoundary)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return EngineResult{}, fmt.Errorf("resolve target root: %w", err)
	}
	goPackage, err := listAnalyzerPackage(ctx, absoluteRoot)
	if err != nil {
		return EngineResult{}, err
	}
	if err := validatePackageDirectory(absoluteRoot, goPackage.Dir); err != nil {
		return EngineResult{}, err
	}
	gitBinary, err := osexec.LookPath("git")
	if err != nil {
		return EngineResult{}, fmt.Errorf("locate Git for read-only inventory: %w", err)
	}
	gitBinary, err = filepath.Abs(gitBinary)
	if err != nil {
		return EngineResult{}, fmt.Errorf("resolve Git executable: %w", err)
	}
	rootHandle, err := mceruntime.OpenRoot(absoluteRoot)
	if err != nil {
		return EngineResult{}, fmt.Errorf("open pinned target root: %w", err)
	}
	defer rootHandle.Close()
	caseMode, err := rootHandle.CaseMode()
	if err != nil {
		return EngineResult{}, fmt.Errorf("measure target case semantics: %w", err)
	}
	if caseMode == paths.CaseUnknown {
		return EngineResult{}, fmt.Errorf("target case semantics are unknown: %w", ErrUnsupportedBoundary)
	}
	rules, err := policy.New(nil, caseMode)
	if err != nil {
		return EngineResult{}, fmt.Errorf("compile self-index read policy: %w", err)
	}
	snapshot, err := inventory.Scan(ctx, inventory.Options{
		Root: absoluteRoot, GitExecutable: gitBinary, Policy: rules, CaseMode: caseMode,
	})
	if err != nil {
		return EngineResult{}, fmt.Errorf("scan target inventory: %w", err)
	}
	if snapshot.Coverage.State != inventory.CoverageComplete {
		return EngineResult{}, fmt.Errorf("target inventory coverage is %s: %w", snapshot.Coverage.State, ErrUnsupportedBoundary)
	}
	included := make(map[string]inventory.File, len(snapshot.Files))
	for _, file := range snapshot.Files {
		included[file.Path] = file
	}
	captured := make([]golang.CapturedFile, 0, len(goPackage.GoFiles))
	files := make([]File, 0, len(goPackage.GoFiles))
	oracle := make([]Declaration, 0)
	var sourceBytes int64
	for _, name := range goPackage.GoFiles {
		relative := filepath.ToSlash(filepath.Join("internal/analysis/golang", name))
		if _, ok := included[relative]; !ok {
			return EngineResult{}, fmt.Errorf("go list source %q is absent from inventory: %w", relative, ErrUnsupportedBoundary)
		}
		read, err := mceruntime.ReadAllowed(rootHandle, relative, rules)
		if err != nil {
			return EngineResult{}, fmt.Errorf("capture analyzer source %q: %w", relative, err)
		}
		captured = append(captured, golang.CapturedFile{Path: relative, Source: read.Bytes})
		files = append(files, File{Path: relative, ContentHash: model.SHA256(read.Bytes), ByteLength: int64(len(read.Bytes))})
		sourceBytes += int64(len(read.Bytes))
		declarations, err := OracleDeclarations(relative, read.Bytes)
		if err != nil {
			return EngineResult{}, err
		}
		oracle = append(oracle, declarations...)
	}
	sortFiles(files)
	sortDeclarations(oracle)
	profile := model.SHA256([]byte("mce4d-self-index-syntax-profile-v1"))
	facts, err := golang.AnalyzePackage(golang.PackageInput{
		GenerationID: model.UUID("11111111-1111-4111-8111-111111111111"),
		ProfileID:    profile, ImportPath: goPackage.ImportPath, ModuleKey: "maps-context-engine-4d",
		Variant: golang.VariantProduction, MaxFileBytes: golang.DefaultMaxFileBytes, Files: captured,
	})
	if err != nil {
		return EngineResult{}, fmt.Errorf("analyze pinned self-index package: %w", err)
	}
	packageScope := model.Hash("")
	for _, scope := range facts.Scopes {
		if scope.Kind == "package" {
			packageScope = scope.ID
			break
		}
	}
	if packageScope == "" {
		return EngineResult{}, errors.New("Go analyzer omitted package scope")
	}
	engineDeclarations := make([]Declaration, 0)
	for _, declaration := range facts.Declarations {
		if declaration.ScopeID != packageScope {
			continue
		}
		engineDeclarations = append(engineDeclarations, Declaration{
			Path: declaration.Path, Name: declaration.Name, Kind: declaration.Kind,
			NameRange: declaration.NameRange, DeclarationRange: declaration.DeclarationRange,
		})
	}
	sortDeclarations(engineDeclarations)
	providerV1 := model.SHA256([]byte("mce4d-go-provider-revision-1"))
	providerV2 := model.SHA256([]byte("mce4d-go-provider-revision-2"))
	cacheInput := indexer.SyntaxCacheInput{
		Path: files[0].Path, ContentDigest: files[0].ContentHash, Language: model.Identifier("go"),
		ProviderDigest: providerV1, CompilerConfigDigest: model.SHA256([]byte("self-index-default-build-profile")),
		InterpretationVersion: 1,
	}
	keyV1, err := indexer.SyntaxCacheKey(cacheInput)
	if err != nil {
		return EngineResult{}, fmt.Errorf("compute provider v1 cache key: %w", err)
	}
	cacheInput.ProviderDigest = providerV2
	keyV2, err := indexer.SyntaxCacheKey(cacheInput)
	if err != nil {
		return EngineResult{}, fmt.Errorf("compute provider v2 cache key: %w", err)
	}
	return EngineResult{
		PackageImportPath: goPackage.ImportPath, InventoryHead: snapshot.Head,
		InventoryFileCount: len(snapshot.Files), InventoryCoverage: string(snapshot.Coverage.State),
		Files: files, EngineDeclarations: engineDeclarations, OracleDeclarations: oracle,
		SourceBytes: sourceBytes, SyntaxCacheKeyV1: keyV1, SyntaxCacheKeyV2: keyV2,
		ProviderChangeInvalidates: keyV1 != keyV2,
		OperationBoundary:         "direct internal acceptance API; no CLI index command is claimed",
	}, nil
}

// DirectToolFallback reads the same pinned package with go list and the Go
// standard-library parser. It does not call MCE4D inventory or analysis code.
func DirectToolFallback(ctx context.Context, root string) (FallbackResult, error) {
	if ctx == nil || root == "" {
		return FallbackResult{}, fmt.Errorf("fallback request requires context and root: %w", ErrUnsupportedBoundary)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return FallbackResult{}, fmt.Errorf("resolve fallback target root: %w", err)
	}
	goPackage, err := listAnalyzerPackage(ctx, absoluteRoot)
	if err != nil {
		return FallbackResult{}, err
	}
	if err := validatePackageDirectory(absoluteRoot, goPackage.Dir); err != nil {
		return FallbackResult{}, err
	}
	files := make([]File, 0, len(goPackage.GoFiles))
	oracle := make([]Declaration, 0)
	var sourceBytes int64
	for _, name := range goPackage.GoFiles {
		if filepath.Base(name) != name || filepath.Ext(name) != ".go" {
			return FallbackResult{}, fmt.Errorf("go list returned unsafe source name %q: %w", name, ErrUnsupportedBoundary)
		}
		relative := filepath.ToSlash(filepath.Join("internal/analysis/golang", name))
		path := filepath.Join(absoluteRoot, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil {
			return FallbackResult{}, fmt.Errorf("inspect direct-tool source %q: %w", relative, err)
		}
		if !info.Mode().IsRegular() {
			return FallbackResult{}, fmt.Errorf("direct-tool source %q is not a regular file: %w", relative, ErrUnsupportedBoundary)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return FallbackResult{}, fmt.Errorf("read direct-tool source %q: %w", relative, err)
		}
		files = append(files, File{Path: relative, ContentHash: model.SHA256(data), ByteLength: int64(len(data))})
		sourceBytes += int64(len(data))
		declarations, err := OracleDeclarations(relative, data)
		if err != nil {
			return FallbackResult{}, err
		}
		oracle = append(oracle, declarations...)
	}
	sortFiles(files)
	sortDeclarations(oracle)
	return FallbackResult{
		Tool: "go list + go/parser", EngineInvoked: false, PackageImportPath: goPackage.ImportPath,
		Files: files, OracleDeclarations: oracle, SourceBytes: sourceBytes,
	}, nil
}

func OracleDeclarations(path string, source []byte) ([]Declaration, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("independent Go parser rejected %q: %w", path, err)
	}
	result := make([]Declaration, 0)
	appendDeclaration := func(name *ast.Ident, node ast.Node, kind string) {
		if name == nil || name.Name == "_" || node == nil {
			return
		}
		result = append(result, Declaration{
			Path: path, Name: name.Name, Kind: kind,
			NameRange: model.ByteRange{
				StartByte: int64(fset.PositionFor(name.Pos(), false).Offset),
				EndByte:   int64(fset.PositionFor(name.End(), false).Offset),
			},
			DeclarationRange: model.ByteRange{
				StartByte: int64(fset.PositionFor(node.Pos(), false).Offset),
				EndByte:   int64(fset.PositionFor(node.End(), false).Offset),
			},
		})
	}
	for _, declaration := range file.Decls {
		switch node := declaration.(type) {
		case *ast.FuncDecl:
			kind := "function"
			if node.Recv != nil {
				kind = "method"
			}
			appendDeclaration(node.Name, node, kind)
		case *ast.GenDecl:
			for _, item := range node.Specs {
				switch spec := item.(type) {
				case *ast.TypeSpec:
					appendDeclaration(spec.Name, spec, "type")
				case *ast.ValueSpec:
					kind := "var"
					if node.Tok == token.CONST {
						kind = "const"
					} else if node.Tok != token.VAR {
						continue
					}
					for _, name := range spec.Names {
						appendDeclaration(name, spec, kind)
					}
				}
			}
		}
	}
	sortDeclarations(result)
	return result, nil
}

func listAnalyzerPackage(ctx context.Context, root string) (goListPackage, error) {
	goBinary, err := osexec.LookPath("go")
	if err != nil {
		return goListPackage{}, fmt.Errorf("locate pinned Go tool: %w", err)
	}
	goBinary, err = filepath.Abs(goBinary)
	if err != nil {
		return goListPackage{}, fmt.Errorf("resolve Go tool: %w", err)
	}
	command := osexec.CommandContext(ctx, goBinary, "list", "-mod=readonly", "-json", AnalyzerPackage)
	command.Dir = root
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
	})
	output, err := command.Output()
	if err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			return goListPackage{}, fmt.Errorf("go list failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return goListPackage{}, fmt.Errorf("run go list: %w", err)
	}
	var result goListPackage
	if err := json.Unmarshal(output, &result); err != nil {
		return goListPackage{}, fmt.Errorf("decode go list package: %w", err)
	}
	if result.Dir == "" || result.ImportPath == "" || len(result.GoFiles) == 0 {
		return goListPackage{}, fmt.Errorf("go list returned incomplete analyzer package: %w", ErrUnsupportedBoundary)
	}
	sort.Strings(result.GoFiles)
	return result, nil
}

func validatePackageDirectory(root, packageDir string) error {
	expected := filepath.Join(root, filepath.FromSlash(AnalyzerPackage[2:]))
	actual, err := filepath.Abs(packageDir)
	if err != nil {
		return fmt.Errorf("resolve analyzer package directory: %w", err)
	}
	if !samePath(actual, expected) {
		return fmt.Errorf("go list resolved analyzer package outside pinned target: %q: %w", actual, ErrUnsupportedBoundary)
	}
	info, err := os.Lstat(expected)
	if err != nil {
		return fmt.Errorf("inspect analyzer package directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("analyzer package directory is not a real directory: %w", ErrUnsupportedBoundary)
	}
	return nil
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtimeCaseInsensitive() {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func runtimeCaseInsensitive() bool {
	return os.PathSeparator == '\\'
}

func replaceEnvironment(base []string, updates map[string]string) []string {
	result := make([]string, 0, len(base)+len(updates))
	for _, item := range base {
		key, _, found := strings.Cut(item, "=")
		if !found {
			result = append(result, item)
			continue
		}
		if _, replaced := updates[strings.ToUpper(key)]; replaced {
			continue
		}
		result = append(result, item)
	}
	for key, value := range updates {
		result = append(result, key+"="+value)
	}
	return result
}

func sortFiles(items []File) {
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
}

func sortDeclarations(items []Declaration) {
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i], items[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.NameRange.StartByte != right.NameRange.StartByte {
			return left.NameRange.StartByte < right.NameRange.StartByte
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Kind < right.Kind
	})
}
