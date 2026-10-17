package golang

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	controlledexec "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/exec"
)

var (
	ErrInvalidLoadRequest = errors.New("invalid Go package load request")
	ErrCapturedSource     = errors.New("Go package source differs from captured snapshot")
)

type LoadStatus string

const (
	LoadComplete LoadStatus = "complete"
	LoadPartial  LoadStatus = "partial"
)

type LoadDiagnostic struct {
	Code    string
	Message string
}

// LoadProfile is the closed GoProfile plus local executable/cache locations.
// Local paths are operational inputs; only the EffectiveProfile fields enter
// ProfileID. GoBinarySHA256 binds the exact executable used by go/packages.
type LoadProfile struct {
	GOOS            string
	GOARCH          string
	BuildTags       []string
	CGOEnabled      bool
	TestsEnabled    bool
	ModuleRoots     []string
	ToolchainDigest model.Hash
	ProviderDigest  model.Hash
	ModuleMode      string
	AnalysisMode    string
	DependencyRoots []string
	GoBinary        string
	GoVersion       string
	GoBinarySHA256  model.Hash
	ModuleCacheRoot string
	ScratchRoot     string
}

type LoadRequest struct {
	SnapshotRoot  string
	ModuleRoot    string
	Patterns      []string
	CapturedFiles map[string][]byte
	CaptureHashes map[string]model.Hash
	Profile       LoadProfile
}

// LoadRun owns one go/packages type registry. Types and types.Info returned by
// Packages are valid only for this run; callers must normalize identities
// before joining facts across another LoadRun.
type LoadRun struct {
	ID               model.UUID
	ProfileID        model.Hash
	DependencyDigest model.Hash
	Status           LoadStatus
	Diagnostics      []LoadDiagnostic
	packages         []*packages.Package
	typesRegistry    map[*types.Package]struct{}
}

func (run *LoadRun) Packages() []*packages.Package {
	if run == nil {
		return nil
	}
	return append([]*packages.Package(nil), run.packages...)
}

// LoadTrustedPackages performs an offline, module-aware packages.Load over an
// owned captured snapshot. It never invokes tests, a package driver, generator,
// plugin, or arbitrary shell command. Any unavailable or inconsistent typed
// input returns a partial run so the caller can keep syntax facts only.
func LoadTrustedPackages(ctx context.Context, request LoadRequest) (*LoadRun, error) {
	profileID, err := validateLoadRequest(request)
	if err != nil {
		return nil, err
	}
	runID, err := newLoadRunID()
	if err != nil {
		return nil, fmt.Errorf("create Go load run identity: %w", err)
	}
	run := &LoadRun{ID: runID, ProfileID: profileID, Status: LoadPartial, typesRegistry: make(map[*types.Package]struct{})}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidLoadRequest)
	}
	if request.Profile.AnalysisMode == "syntax" {
		run.Diagnostics = []LoadDiagnostic{{Code: "typed_analysis_disabled", Message: "profile chỉ cho phép phân tích cú pháp"}}
		return run, nil
	}
	if request.Profile.CGOEnabled {
		run.Diagnostics = []LoadDiagnostic{{Code: "cgo_profile_unsupported", Message: "profile CGO chưa được xác minh; giữ kết quả cú pháp một phần"}}
		return run, nil
	}
	moduleRoot, err := resolveModuleRoot(request)
	if err != nil {
		return nil, err
	}
	modulePath := "go.mod"
	if request.ModuleRoot != "." {
		modulePath = filepath.ToSlash(filepath.Join(request.ModuleRoot, "go.mod"))
	}
	moduleBytes, ok := request.CapturedFiles[modulePath]
	if !ok {
		run.Diagnostics = []LoadDiagnostic{{Code: "module_file_not_captured", Message: "go.mod không nằm trong snapshot được chụp"}}
		return run, nil
	}
	moduleFile, err := modfile.Parse(filepath.Join(moduleRoot, "go.mod"), moduleBytes, nil)
	if err != nil || moduleFile.Module == nil || moduleFile.Module.Mod.Path == "" {
		run.Diagnostics = []LoadDiagnostic{{Code: "invalid_module_metadata", Message: "go.mod đã chụp không hợp lệ"}}
		return run, nil
	}
	if len(moduleFile.Replace) != 0 {
		run.Diagnostics = []LoadDiagnostic{{Code: "local_replace_not_preflighted", Message: "go.mod có replace; dependency closure chưa được allowlist đầy đủ"}}
		return run, nil
	}
	complete, reason, err := preflightCapturedModuleClosure(ctx, request, moduleRoot)
	if err != nil {
		return nil, err
	}
	if !complete {
		run.Diagnostics = []LoadDiagnostic{{Code: "package_closure_incomplete", Message: reason}}
		return run, nil
	}
	if err := verifyGoExecutable(ctx, request.Profile); err != nil {
		run.Diagnostics = []LoadDiagnostic{{Code: "toolchain_unavailable", Message: conciseLoadError(err)}}
		return run, nil
	}
	if err := validateCanonicalDirectory(request.Profile.ModuleCacheRoot); err != nil {
		run.Diagnostics = []LoadDiagnostic{{Code: "offline_module_cache_unavailable", Message: "module cache được allowlist không khả dụng; không truy cập mạng"}}
		return run, nil
	}
	if err := validateCanonicalDirectory(request.Profile.ScratchRoot); err != nil {
		run.Diagnostics = []LoadDiagnostic{{Code: "scratch_unavailable", Message: "không tạo được scratch đã cấu hình cho Go toolchain"}}
		return run, nil
	}
	if request.Profile.ModuleMode == "vendor_explicit" {
		run.Diagnostics = []LoadDiagnostic{{Code: "vendor_closure_not_preflighted", Message: "vendor_explicit chưa có preflight source closure; giữ kết quả cú pháp một phần"}}
		return run, nil
	}
	cacheRoot, err := os.MkdirTemp(request.Profile.ScratchRoot, "mce4d-go-load-")
	if err != nil {
		run.Diagnostics = []LoadDiagnostic{{Code: "scratch_unavailable", Message: "không cấp phát được cache riêng cho lượt load"}}
		return run, nil
	}
	defer os.RemoveAll(cacheRoot)
	dependencyModules, dependencyDigest, complete, reason, err := preflightAllowedDependencyModules(ctx, request, moduleRoot, cacheRoot)
	if err != nil {
		return nil, err
	}
	if !complete {
		run.Diagnostics = []LoadDiagnostic{{Code: "dependency_closure_not_preflighted", Message: reason}}
		return run, nil
	}
	run.DependencyDigest = dependencyDigest

	overlay := make(map[string][]byte, len(request.CapturedFiles))
	captured := make(map[string][]byte, len(request.CapturedFiles))
	for relative, source := range request.CapturedFiles {
		absolute := filepath.Join(request.SnapshotRoot, filepath.FromSlash(relative))
		if !isWithin(request.SnapshotRoot, absolute) {
			return nil, fmt.Errorf("%w: captured path escapes snapshot root", ErrInvalidLoadRequest)
		}
		canonical := filepath.Clean(absolute)
		overlay[canonical] = append([]byte(nil), source...)
		captured[canonical] = source
	}
	goEnv := environmentList(goEnvironment(request.Profile, cacheRoot))
	buildFlags := []string{}
	if len(request.Profile.BuildTags) > 0 {
		buildFlags = append(buildFlags, "-tags="+strings.Join(request.Profile.BuildTags, ","))
	}
	config := &packages.Config{
		Context: ctx,
		Dir:     moduleRoot,
		Env:     goEnv,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedTypesSizes,
		Tests:      request.Profile.TestsEnabled,
		BuildFlags: buildFlags,
		Overlay:    overlay,
		Fset:       token.NewFileSet(),
		ParseFile:  capturedParseFile(captured),
		Logf:       func(string, ...any) {},
	}
	loaded, loadErr := packages.Load(config, request.Patterns...)
	if loadErr != nil {
		addLoadDiagnostic(run, "package_load_failed", conciseLoadError(loadErr))
	}
	if len(loaded) == 0 {
		if len(run.Diagnostics) == 0 {
			addLoadDiagnostic(run, "package_not_found", "go/packages không trả về package trong scope đã chọn")
		}
		return run, nil
	}
	run.packages = loaded
	modulePrefix := moduleFile.Module.Mod.Path
	moduleMismatch := false
	for _, pkg := range loaded {
		if pkg == nil || pkg.Module == nil || pkg.Module.Main {
			continue
		}
		key := pkg.Module.Path + "@" + pkg.Module.Version
		approved, ok := dependencyModules[key]
		if !ok || !samePath(approved, pkg.Module.Dir) {
			moduleMismatch = true
		}
	}
	if moduleMismatch {
		addLoadDiagnostic(run, "package_outside_preflighted_closure", "go/packages trả package ngoài dependency closure đã duyệt")
		clearLoadTypes(loaded)
	}
	for _, pkg := range loaded {
		if pkg == nil {
			addLoadDiagnostic(run, "nil_package", "go/packages trả về package rỗng")
			continue
		}
		if pkg.Module != nil && pkg.Module.Path == modulePrefix {
			for _, sourcePath := range pkg.CompiledGoFiles {
				if _, exists := captured[filepath.Clean(sourcePath)]; !exists {
					addLoadDiagnostic(run, "package_closure_incomplete", "package chứa file ngoài snapshot; type facts bị hạ xuống partial")
					pkg.Types = nil
					pkg.TypesInfo = nil
					break
				}
			}
		}
		for _, packageError := range pkg.Errors {
			addLoadDiagnostic(run, "package_typecheck_incomplete", sanitizePackageError(packageError.Msg))
			pkg.Types = nil
			pkg.TypesInfo = nil
			break
		}
	}
	afterDigest, digestErr := hashAllowedDependencyModules(ctx, dependencyModules)
	if digestErr != nil || afterDigest != dependencyDigest {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		addLoadDiagnostic(run, "dependency_closure_changed_during_load", "dependency closure đổi trong lúc load; semantic facts bị hạ xuống partial")
		clearLoadTypes(loaded)
	}
	seenPackages := make(map[*packages.Package]struct{})
	for _, pkg := range loaded {
		registerLoadPackageTree(run, pkg, seenPackages)
	}
	if len(run.Diagnostics) == 0 {
		run.Status = LoadComplete
	} else {
		run.Status = LoadPartial
	}
	return run, nil
}

func registerLoadPackageTree(run *LoadRun, pkg *packages.Package, seen map[*packages.Package]struct{}) {
	if pkg == nil {
		return
	}
	if _, ok := seen[pkg]; ok {
		return
	}
	seen[pkg] = struct{}{}
	if pkg.Types != nil {
		run.typesRegistry[pkg.Types] = struct{}{}
	}
	for _, dependency := range pkg.Imports {
		registerLoadPackageTree(run, dependency, seen)
	}
}

func validateLoadRequest(request LoadRequest) (model.Hash, error) {
	profile := request.Profile
	if request.SnapshotRoot == "" || request.ModuleRoot == "" || len(request.Patterns) == 0 || len(request.CapturedFiles) == 0 || len(request.CapturedFiles) != len(request.CaptureHashes) {
		return "", fmt.Errorf("%w: snapshot, module, patterns, and capture hashes are required", ErrInvalidLoadRequest)
	}
	if !filepath.IsAbs(request.SnapshotRoot) || !filepath.IsAbs(profile.GoBinary) || !filepath.IsAbs(profile.ModuleCacheRoot) || !filepath.IsAbs(profile.ScratchRoot) {
		return "", fmt.Errorf("%w: operational roots and Go binary must be absolute", ErrInvalidLoadRequest)
	}
	if profile.GoVersion == "" || profile.GOOS == "" || profile.GOARCH == "" ||
		len(profile.GOOS) > 32 || len(profile.GOARCH) > 32 || len(profile.GoVersion) > 32 ||
		!validDigest(profile.ToolchainDigest) || !validDigest(profile.ProviderDigest) || !validDigest(profile.GoBinarySHA256) ||
		(profile.ModuleMode != "readonly" && profile.ModuleMode != "vendor_explicit") || (profile.AnalysisMode != "typed_when_available" && profile.AnalysisMode != "syntax") {
		return "", fmt.Errorf("%w: unsupported or incomplete effective Go profile", ErrInvalidLoadRequest)
	}
	if _, err := model.ParseHash(string(profile.ToolchainDigest)); err != nil {
		return "", fmt.Errorf("%w: invalid toolchain digest", ErrInvalidLoadRequest)
	}
	if _, err := model.ParseHash(string(profile.ProviderDigest)); err != nil {
		return "", fmt.Errorf("%w: invalid provider digest", ErrInvalidLoadRequest)
	}
	if len(profile.ModuleRoots) == 0 || len(profile.ModuleRoots) > 64 || len(profile.BuildTags) > 128 || len(profile.DependencyRoots) > 128 {
		return "", fmt.Errorf("%w: profile array outside contract bounds", ErrInvalidLoadRequest)
	}
	for _, moduleRoot := range profile.ModuleRoots {
		if len(moduleRoot) > 4096 || (moduleRoot != "." && model.ValidateRelativePath(moduleRoot) != nil) {
			return "", fmt.Errorf("%w: invalid effective module root", ErrInvalidLoadRequest)
		}
	}
	for _, dependencyRoot := range profile.DependencyRoots {
		if dependencyRoot == "" || len(dependencyRoot) > 4096 || strings.ContainsRune(dependencyRoot, 0) {
			return "", fmt.Errorf("%w: invalid effective dependency root", ErrInvalidLoadRequest)
		}
	}
	if !containsString(profile.ModuleRoots, request.ModuleRoot) {
		return "", fmt.Errorf("%w: selected module root is not in the effective profile", ErrInvalidLoadRequest)
	}
	if request.ModuleRoot != "." && model.ValidateRelativePath(request.ModuleRoot) != nil {
		return "", fmt.Errorf("%w: module root must be a normalized relative path", ErrInvalidLoadRequest)
	}
	for _, tag := range profile.BuildTags {
		if !buildTagPattern.MatchString(tag) {
			return "", fmt.Errorf("%w: invalid build tag", ErrInvalidLoadRequest)
		}
	}
	if !uniqueSortedStrings(profile.BuildTags) || !uniqueSortedStrings(profile.ModuleRoots) || !uniqueSortedStrings(profile.DependencyRoots) {
		return "", fmt.Errorf("%w: profile arrays must be sorted and unique", ErrInvalidLoadRequest)
	}
	for _, pattern := range request.Patterns {
		if pattern == "." || pattern == "./..." {
			continue
		}
		if !strings.HasPrefix(pattern, "./") || strings.Contains(pattern, "...") || strings.ContainsAny(pattern, "@\\\x00") || model.ValidateRelativePath(strings.TrimPrefix(pattern, "./")) != nil {
			return "", fmt.Errorf("%w: package pattern must be scoped below its module root", ErrInvalidLoadRequest)
		}
	}
	root, err := filepath.Abs(request.SnapshotRoot)
	if err != nil {
		return "", fmt.Errorf("%w: invalid snapshot root", ErrInvalidLoadRequest)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: snapshot root must be an existing non-symlink directory", ErrInvalidLoadRequest)
	}
	for relative, source := range request.CapturedFiles {
		if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") || model.ValidateRelativePath(relative) != nil {
			return "", fmt.Errorf("%w: captured file path is not portable", ErrInvalidLoadRequest)
		}
		hash, ok := request.CaptureHashes[relative]
		if !ok || !validDigest(hash) {
			return "", fmt.Errorf("%w: captured file is missing a valid content hash", ErrInvalidLoadRequest)
		}
		actual := sha256.Sum256(source)
		if hex.EncodeToString(actual[:]) != string(hash) {
			return "", fmt.Errorf("%w: captured bytes do not match manifest hash", ErrCapturedSource)
		}
	}
	for path := range request.CaptureHashes {
		if _, ok := request.CapturedFiles[path]; !ok {
			return "", fmt.Errorf("%w: hash map contains an uncaptured file", ErrInvalidLoadRequest)
		}
	}
	profileID, err := effectiveGoProfileID(profile)
	if err != nil {
		return "", err
	}
	return profileID, nil
}

var buildTagPattern = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)

func effectiveGoProfileID(profile LoadProfile) (model.Hash, error) {
	tags := make([]any, len(profile.BuildTags))
	for index, tag := range profile.BuildTags {
		tags[index] = tag
	}
	moduleRoots := make([]any, len(profile.ModuleRoots))
	for index, root := range profile.ModuleRoots {
		moduleRoots[index] = root
	}
	dependencyRoots := make([]any, len(profile.DependencyRoots))
	for index, root := range profile.DependencyRoots {
		dependencyRoots[index] = root
	}
	return model.HashValue("profile-v1", map[string]any{
		"schema_version": int64(1), "language": "go", "goos": profile.GOOS, "goarch": profile.GOARCH,
		"build_tags": tags, "cgo_enabled": profile.CGOEnabled, "tests_enabled": profile.TestsEnabled,
		"module_roots": moduleRoots, "toolchain_digest": profile.ToolchainDigest,
		"provider_digest": profile.ProviderDigest, "module_mode": profile.ModuleMode,
		"analysis_mode": profile.AnalysisMode, "dependency_roots": dependencyRoots,
	})
}

func verifyGoExecutable(ctx context.Context, profile LoadProfile) error {
	binary, err := filepath.Abs(profile.GoBinary)
	if err != nil {
		return err
	}
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Go executable is unavailable or is a symbolic link")
	}
	file, err := os.Open(binary)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), string(profile.GoBinarySHA256)) {
		return errors.New("Go executable digest differs from the pinned tool")
	}
	resolved, err := osexec.LookPath("go")
	if err != nil || !samePath(resolved, binary) {
		return errors.New("Go executable resolved by go/packages does not match the pinned path")
	}
	env := goEnvironment(profile, filepath.Dir(binary))
	result, err := controlledexec.Run(ctx, controlledexec.Spec{
		Path: binary, Args: []string{"version"}, Dir: filepath.Dir(binary), Env: env,
		MaxStdoutBytes: 512, MaxStderrBytes: 512,
	})
	if err != nil {
		return err
	}
	if !bytes.Contains(result.Stdout, []byte(profile.GoVersion)) {
		return errors.New("Go executable version differs from the pinned profile")
	}
	return nil
}

func preflightCapturedModuleClosure(ctx context.Context, request LoadRequest, moduleRoot string) (bool, string, error) {
	root, err := filepath.Abs(request.SnapshotRoot)
	if err != nil {
		return false, "snapshot root không hợp lệ", err
	}
	moduleRelative := "go.mod"
	if request.ModuleRoot != "." {
		moduleRelative = filepath.ToSlash(filepath.Join(request.ModuleRoot, "go.mod"))
	}
	if err := verifyCapturedMetadataFile(moduleRoot, filepath.Join(moduleRoot, "go.mod"), request.CapturedFiles[moduleRelative]); err != nil {
		return false, "go.mod trên snapshot không khớp bytes đã chụp", nil
	}
	goSumRelative := "go.sum"
	if request.ModuleRoot != "." {
		goSumRelative = filepath.ToSlash(filepath.Join(request.ModuleRoot, "go.sum"))
	}
	goSumPath := filepath.Join(moduleRoot, "go.sum")
	if _, err := os.Lstat(goSumPath); err == nil {
		captured, ok := request.CapturedFiles[goSumRelative]
		if !ok || verifyCapturedMetadataFile(moduleRoot, goSumPath, captured) != nil {
			return false, "go.sum không nằm trong snapshot hoặc không khớp bytes đã chụp", nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, "không xác minh được go.sum trong snapshot", nil
	}
	visited := 0
	complete := true
	reason := ""
	err = filepath.WalkDir(moduleRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			complete = false
			reason = "không duyệt được package closure trong snapshot"
			return filepath.SkipAll
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == moduleRoot {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			complete = false
			reason = "package closure có symlink hoặc reparse point"
			return filepath.SkipAll
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !isWithin(root, path) {
			complete = false
			reason = "package closure vượt khỏi snapshot root"
			return filepath.SkipAll
		}
		portable := filepath.ToSlash(relative)
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".go") {
			visited++
			if visited > 1_000_000 {
				complete = false
				reason = "package closure vượt giới hạn file đã chụp"
				return filepath.SkipAll
			}
			if _, ok := request.CapturedFiles[portable]; !ok {
				complete = false
				reason = "package closure chứa Go file ngoài capture manifest"
				return filepath.SkipAll
			}
		}
		if entry.Name() == "go.mod" && path != filepath.Join(moduleRoot, "go.mod") {
			complete = false
			reason = "package closure chứa module lồng chưa được preflight"
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return false, "không duyệt được package closure trong snapshot", nil
	}
	if err != nil {
		return false, "package closure preflight đã bị hủy", err
	}
	return complete, reason, nil
}

type dependencyModule struct {
	Path    string
	Version string
	Dir     string
}

type goListModule struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *struct {
		Path string
		Dir  string
	}
}

// preflightAllowedDependencyModules inspects only the module graph metadata
// before allowing go/packages to read dependency source. Every external module
// must resolve below a canonical profile dependency root.
func preflightAllowedDependencyModules(ctx context.Context, request LoadRequest, moduleRoot, cacheRoot string) (map[string]string, model.Hash, bool, string, error) {
	allowedRoots := make([]string, 0, len(request.Profile.DependencyRoots))
	for _, value := range request.Profile.DependencyRoots {
		root := value
		if !filepath.IsAbs(root) {
			if model.ValidateRelativePath(root) != nil {
				return nil, "", false, "dependency root không phải đường dẫn snapshot hợp lệ", nil
			}
			root = filepath.Join(request.SnapshotRoot, filepath.FromSlash(root))
		}
		absolute, err := filepath.Abs(root)
		if err != nil || validateCanonicalDirectory(absolute) != nil {
			return nil, "", false, "dependency root thiếu, không canonical hoặc là symlink", nil
		}
		allowedRoots = append(allowedRoots, absolute)
	}
	result, err := controlledexec.Run(ctx, controlledexec.Spec{
		Path: request.Profile.GoBinary, Args: []string{"list", "-m", "-json", "all"}, Dir: moduleRoot,
		Env: goEnvironment(request.Profile, cacheRoot), MaxStdoutBytes: 8 << 20, MaxStderrBytes: 1 << 20,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", false, "", ctx.Err()
		}
		return nil, "", false, "module graph chưa sẵn có offline; giữ kết quả cú pháp một phần", nil
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
	modules := make(map[string]string)
	mainFound := false
	var dependencies []dependencyModule
	for {
		var module goListModule
		if err := decoder.Decode(&module); err == io.EOF {
			break
		} else if err != nil {
			return nil, "", false, "metadata module graph không đọc được; giữ kết quả cú pháp một phần", nil
		}
		if module.Main {
			if !samePath(module.Dir, moduleRoot) || module.Path == "" {
				return nil, "", false, "module gốc không khớp snapshot đã chụp", nil
			}
			mainFound = true
			continue
		}
		if module.Path == "" || module.Version == "" || module.Dir == "" || module.Replace != nil {
			return nil, "", false, "module dependency thiếu identity/path ổn định; giữ kết quả cú pháp một phần", nil
		}
		dir, err := filepath.Abs(module.Dir)
		if err != nil || validateCanonicalDirectory(dir) != nil {
			return nil, "", false, "dependency module không có thư mục canonical; giữ kết quả cú pháp một phần", nil
		}
		approved := false
		for _, root := range allowedRoots {
			if isWithin(root, dir) {
				approved = true
				break
			}
		}
		if !approved {
			return nil, "", false, "dependency module nằm ngoài các root được profile cho phép; giữ kết quả cú pháp một phần", nil
		}
		key := module.Path + "@" + module.Version
		if _, duplicate := modules[key]; duplicate {
			return nil, "", false, "module graph có identity bị lặp; giữ kết quả cú pháp một phần", nil
		}
		modules[key] = dir
		dependencies = append(dependencies, dependencyModule{Path: module.Path, Version: module.Version, Dir: dir})
	}
	if !mainFound {
		return nil, "", false, "module graph không xác nhận module gốc; giữ kết quả cú pháp một phần", nil
	}
	digest, err := hashDependencyModules(ctx, dependencies)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", false, "", ctx.Err()
		}
		return nil, "", false, "dependency source closure không ổn định hoặc vượt giới hạn; giữ kết quả cú pháp một phần", nil
	}
	return modules, digest, true, "", nil
}

func hashAllowedDependencyModules(ctx context.Context, modules map[string]string) (model.Hash, error) {
	dependencies := make([]dependencyModule, 0, len(modules))
	for key, dir := range modules {
		separator := strings.LastIndexByte(key, '@')
		if separator < 1 || separator == len(key)-1 {
			return "", ErrInvalidLoadRequest
		}
		dependencies = append(dependencies, dependencyModule{Path: key[:separator], Version: key[separator+1:], Dir: dir})
	}
	return hashDependencyModules(ctx, dependencies)
}

func hashDependencyModules(ctx context.Context, modules []dependencyModule) (model.Hash, error) {
	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Path != modules[j].Path {
			return modules[i].Path < modules[j].Path
		}
		return modules[i].Version < modules[j].Version
	})
	hash := sha256.New()
	_, _ = io.WriteString(hash, "MCE4D\x00go-dependency-closure-v1\x00")
	totalBytes := int64(0)
	files := 0
	for _, module := range modules {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		writeDigestField(hash, module.Path)
		writeDigestField(hash, module.Version)
		root, err := filepath.Abs(module.Dir)
		if err != nil || validateCanonicalDirectory(root) != nil {
			return "", ErrCapturedSource
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if path == root {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return ErrCapturedSource
			}
			if entry.IsDir() {
				if entry.Name() == ".git" || entry.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			name := strings.ToLower(entry.Name())
			if name != "go.mod" && name != "go.sum" && !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".s") {
				return nil
			}
			files++
			if files > 1_000_000 {
				return ErrInvalidLoadRequest
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 2<<20 {
				return ErrCapturedSource
			}
			totalBytes += info.Size()
			if totalBytes > 32<<30 {
				return ErrInvalidLoadRequest
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			openedInfo, statErr := file.Stat()
			if statErr != nil || !os.SameFile(info, openedInfo) {
				_ = file.Close()
				return ErrCapturedSource
			}
			fileHash := sha256.New()
			read, readErr := io.Copy(fileHash, io.LimitReader(file, info.Size()+1))
			finalInfo, finalStatErr := file.Stat()
			pathInfo, pathStatErr := os.Lstat(path)
			closeErr := file.Close()
			if readErr != nil || finalStatErr != nil || pathStatErr != nil || closeErr != nil || read != info.Size() || finalInfo.Size() != info.Size() || !os.SameFile(info, finalInfo) || !os.SameFile(info, pathInfo) {
				return ErrCapturedSource
			}
			relative, err := filepath.Rel(root, path)
			if err != nil || !isWithin(root, path) {
				return ErrCapturedSource
			}
			writeDigestField(hash, filepath.ToSlash(relative))
			_, _ = hash.Write(fileHash.Sum(nil))
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return model.Hash(hex.EncodeToString(hash.Sum(nil))), nil
}

func writeDigestField(dst io.Writer, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = dst.Write(size[:])
	_, _ = io.WriteString(dst, value)
}

func clearLoadTypes(loaded []*packages.Package) {
	seen := make(map[*packages.Package]struct{})
	var clear func(*packages.Package)
	clear = func(pkg *packages.Package) {
		if pkg == nil {
			return
		}
		if _, ok := seen[pkg]; ok {
			return
		}
		seen[pkg] = struct{}{}
		pkg.Types = nil
		pkg.TypesInfo = nil
		for _, dependency := range pkg.Imports {
			clear(dependency)
		}
	}
	for _, pkg := range loaded {
		clear(pkg)
	}
}

func verifyCapturedMetadataFile(root, path string, expected []byte) error {
	if expected == nil || len(expected) > 16*1024*1024 || !isWithin(root, path) {
		return ErrCapturedSource
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(len(expected)) {
		return ErrCapturedSource
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	actual, readErr := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !bytes.Equal(actual, expected) {
		return ErrCapturedSource
	}
	return nil
}

func validateCanonicalDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("directory path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("directory must exist and cannot be a link")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !samePath(resolved, path) {
		return errors.New("directory path is not canonical")
	}
	return nil
}

func goEnvironment(profile LoadProfile, cacheRoot string) map[string]string {
	values := map[string]string{
		"GOOS": profile.GOOS, "GOARCH": profile.GOARCH, "CGO_ENABLED": "0",
		"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
		"GOPACKAGESDRIVER": "off", "GOENV": "off", "GOWORK": "off",
		"GOFLAGS": "-mod=readonly", "GOMODCACHE": profile.ModuleCacheRoot,
		"GOCACHE": cacheRoot, "TMPDIR": cacheRoot, "TEMP": cacheRoot, "TMP": cacheRoot,
		"PATH": filepath.Dir(profile.GoBinary),
	}
	if len(profile.BuildTags) > 0 {
		values["GOFLAGS"] += " -tags=" + strings.Join(profile.BuildTags, ",")
	}
	if runtime.GOOS == "windows" {
		if value := os.Getenv("SystemRoot"); value != "" {
			values["SYSTEMROOT"] = value
			values["WINDIR"] = value
			values["PATH"] = filepath.Dir(profile.GoBinary) + string(os.PathListSeparator) + filepath.Join(value, "System32")
		}
	}
	return values
}

func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func newLoadRunID() (model.UUID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return model.ParseUUID(encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:])
}

func capturedParseFile(captured map[string][]byte) func(*token.FileSet, string, []byte) (*ast.File, error) {
	return func(fset *token.FileSet, filename string, supplied []byte) (*ast.File, error) {
		key := filepath.Clean(filename)
		source, ok := captured[key]
		if !ok || supplied == nil || !bytes.Equal(source, supplied) {
			return nil, fmt.Errorf("%w: %s", ErrCapturedSource, filepath.Base(filename))
		}
		return parser.ParseFile(fset, filename, source, parser.ParseComments|parser.AllErrors)
	}
}

func resolveModuleRoot(request LoadRequest) (string, error) {
	root, err := filepath.Abs(request.SnapshotRoot)
	if err != nil {
		return "", err
	}
	moduleRoot := root
	if request.ModuleRoot != "." {
		moduleRoot = filepath.Join(root, filepath.FromSlash(request.ModuleRoot))
	}
	resolved, err := filepath.EvalSymlinks(moduleRoot)
	if err != nil || !isWithin(root, resolved) || !samePath(moduleRoot, resolved) {
		return "", fmt.Errorf("%w: module root is missing, linked, or outside the snapshot", ErrInvalidLoadRequest)
	}
	return resolved, nil
}

func addLoadDiagnostic(run *LoadRun, code, message string) {
	if len(run.Diagnostics) >= 64 {
		return
	}
	if len(message) > 512 {
		message = message[:512]
	}
	run.Diagnostics = append(run.Diagnostics, LoadDiagnostic{Code: code, Message: message})
}

func sanitizePackageError(message string) string {
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func conciseLoadError(err error) string {
	message := sanitizePackageError(err.Error())
	if message == "" {
		return "go/packages thất bại; giữ kết quả cú pháp một phần"
	}
	return message
}

func validDigest(value model.Hash) bool {
	_, err := model.ParseHash(string(value))
	return err == nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func uniqueSortedStrings(values []string) bool {
	return sort.StringsAreSorted(values) && uniqueStrings(values)
}

func uniqueStrings(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return false
		}
	}
	return true
}

func isWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
