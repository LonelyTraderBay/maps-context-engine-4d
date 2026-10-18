package lsp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const pinnedGoplsVersion = "v0.24.0"

var (
	ErrExecutableUnavailable = errors.New("allowlisted language tool is unavailable")
	ErrInvalidLSPProfile     = errors.New("invalid pinned Go LSP profile")
	goValuePattern           = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,128}$`)
	buildTagPattern          = regexp.MustCompile(`^[A-Za-z0-9_.]{1,128}$`)
)

type ExecutablePin struct {
	Path    string
	Version string
	SHA256  model.Hash
}

type AllowedExecutable struct {
	Path   string
	SHA256 model.Hash
}

type ExecutionPolicy struct {
	AllowedRoots         []string
	DependencyRoots      []string
	AllowedExecutables   []AllowedExecutable
	AllowTargetExecution bool
	AllowRuntimeDownload bool
	MaxFileBytes         int64
	MaxSourceBytes       int64
}

type GoProfile struct {
	ID              model.Hash
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
	GoVersion       string
	GoBinarySHA256  model.Hash
	Go              ExecutablePin
	Gopls           ExecutablePin
	ModuleCacheRoot string
	PrivateRoot     string
	CacheRoot       string
	MaxFileBytes    int64
	MaxSourceBytes  int64
}

func validateGoProfile(profile GoProfile, policy ExecutionPolicy, snapshotRoot string) (string, map[string]string, error) {
	if _, err := model.ParseHash(string(profile.ID)); err != nil || profile.GOOS == "" || profile.GOARCH == "" || len(profile.GOOS) > 32 || len(profile.GOARCH) > 32 ||
		!goValuePattern.MatchString(profile.GOOS) || !goValuePattern.MatchString(profile.GOARCH) || profile.CGOEnabled ||
		profile.MaxFileBytes < 1 || profile.MaxFileBytes > 2*1024*1024 || profile.MaxSourceBytes < profile.MaxFileBytes || profile.MaxSourceBytes > 32*1024*1024*1024 ||
		profile.GoVersion == "" || len(profile.GoVersion) > 32 || profile.Go.Version != profile.GoVersion || profile.GoBinarySHA256 != profile.Go.SHA256 ||
		(profile.ModuleMode != "readonly" && profile.ModuleMode != "vendor_explicit") || (profile.AnalysisMode != "typed_when_available" && profile.AnalysisMode != "syntax") ||
		len(profile.ModuleRoots) == 0 || len(profile.ModuleRoots) > 64 || len(profile.BuildTags) > 128 || len(profile.DependencyRoots) > 128 {
		return "", nil, ErrInvalidLSPProfile
	}
	if _, err := model.ParseHash(string(profile.ToolchainDigest)); err != nil {
		return "", nil, ErrInvalidLSPProfile
	}
	if _, err := model.ParseHash(string(profile.ProviderDigest)); err != nil {
		return "", nil, ErrInvalidLSPProfile
	}
	if _, err := model.ParseHash(string(profile.GoBinarySHA256)); err != nil {
		return "", nil, ErrInvalidLSPProfile
	}
	for index, root := range profile.ModuleRoots {
		if len(root) > 4096 || (root != "." && model.ValidateRelativePath(root) != nil) || (index > 0 && profile.ModuleRoots[index-1] >= root) {
			return "", nil, ErrInvalidLSPProfile
		}
	}
	for index, root := range profile.DependencyRoots {
		if root == "" || len(root) > 4096 || strings.ContainsRune(root, 0) || (index > 0 && profile.DependencyRoots[index-1] >= root) {
			return "", nil, ErrInvalidLSPProfile
		}
	}
	computedProfileID, err := effectiveGoProfileID(profile)
	if err != nil || computedProfileID != profile.ID {
		return "", nil, ErrInvalidLSPProfile
	}
	if profile.Gopls.Version != pinnedGoplsVersion {
		return "", nil, ErrLSPNotReady
	}
	if profile.ModuleMode != "readonly" || profile.AnalysisMode != "typed_when_available" {
		return "", nil, ErrLSPNotReady
	}
	if validateExecutionPolicy(policy) != nil || policy.AllowRuntimeDownload || policy.AllowTargetExecution {
		return "", nil, ErrInvalidLSPProfile
	}
	if err := validateBuildTags(profile.BuildTags); err != nil {
		return "", nil, err
	}
	root, err := canonicalDirectory(snapshotRoot)
	if err != nil {
		return "", nil, ErrInvalidLSPProfile
	}
	for _, moduleRoot := range profile.ModuleRoots {
		if moduleRoot == "." {
			continue
		}
		canonical, canonicalErr := canonicalDirectory(filepath.Join(root, filepath.FromSlash(moduleRoot)))
		if canonicalErr != nil || !pathWithin(root, canonical) {
			return "", nil, ErrInvalidLSPProfile
		}
	}
	privateRoot, err := canonicalDirectory(profile.PrivateRoot)
	if err != nil || !pathWithin(privateRoot, root) {
		return "", nil, ErrInvalidLSPProfile
	}
	cacheRoot, err := canonicalDirectory(profile.CacheRoot)
	if err != nil || !pathWithin(privateRoot, cacheRoot) || pathWithin(root, cacheRoot) {
		return "", nil, ErrInvalidLSPProfile
	}
	moduleCache, err := canonicalDirectory(profile.ModuleCacheRoot)
	if err != nil || !isDependencyRoot(policy.DependencyRoots, moduleCache) || pathWithin(root, moduleCache) {
		return "", nil, ErrInvalidLSPProfile
	}
	effectiveDependencyRoots := make([]string, 0, len(profile.DependencyRoots))
	for _, configuredRoot := range profile.DependencyRoots {
		var dependencyRoot string
		if filepath.IsAbs(configuredRoot) {
			dependencyRoot, err = canonicalDirectory(configuredRoot)
			if err != nil || !isDependencyRoot(policy.DependencyRoots, dependencyRoot) {
				return "", nil, ErrInvalidLSPProfile
			}
		} else {
			if model.ValidateRelativePath(configuredRoot) != nil {
				return "", nil, ErrInvalidLSPProfile
			}
			dependencyRoot, err = canonicalDirectory(filepath.Join(root, filepath.FromSlash(configuredRoot)))
			if err != nil || !pathWithin(root, dependencyRoot) {
				return "", nil, ErrInvalidLSPProfile
			}
		}
		effectiveDependencyRoots = append(effectiveDependencyRoots, dependencyRoot)
	}
	if profile.ModuleMode == "readonly" && !isDependencyRoot(effectiveDependencyRoots, moduleCache) {
		return "", nil, ErrInvalidLSPProfile
	}
	if !isAllowedExecutable(policy.AllowedExecutables, profile.Go) || !isAllowedExecutable(policy.AllowedExecutables, profile.Gopls) {
		return "", nil, ErrExecutableUnavailable
	}
	for _, allowedRoot := range policy.AllowedRoots {
		canonical, canonicalErr := canonicalDirectory(allowedRoot)
		if canonicalErr == nil && samePath(canonical, root) {
			goto rootAllowed
		}
	}
	return "", nil, ErrInvalidLSPProfile

rootAllowed:
	if err := validateExecutablePin(profile.Go); err != nil {
		return "", nil, err
	}
	if err := validateExecutablePin(profile.Gopls); err != nil {
		return "", nil, err
	}
	environment := map[string]string{
		"GOOS": profile.GOOS, "GOARCH": profile.GOARCH, "CGO_ENABLED": "0",
		"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
		"GOPACKAGESDRIVER": "off", "GOENV": "off", "GOWORK": "off",
		"GOFLAGS": "-mod=" + profile.ModuleMode, "GOMODCACHE": moduleCache, "GOCACHE": cacheRoot,
		"TMPDIR": cacheRoot, "TEMP": cacheRoot, "TMP": cacheRoot,
		"PATH": filepath.Dir(profile.Go.Path),
	}
	if len(profile.BuildTags) > 0 {
		environment["GOFLAGS"] += " -tags=" + strings.Join(profile.BuildTags, ",")
	}
	if runtime.GOOS == "windows" {
		systemRoot := os.Getenv("SystemRoot")
		if systemRoot == "" || isReparsePoint(systemRoot) {
			return "", nil, ErrInvalidLSPProfile
		}
		environment["SYSTEMROOT"] = systemRoot
		environment["WINDIR"] = systemRoot
		environment["PATH"] = filepath.Dir(profile.Go.Path) + string(os.PathListSeparator) + filepath.Join(systemRoot, "System32")
	}
	return root, environment, nil
}

func validateBuildTags(tags []string) error {
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if !buildTagPattern.MatchString(tag) {
			return ErrInvalidLSPProfile
		}
		if _, duplicate := seen[tag]; duplicate {
			return ErrInvalidLSPProfile
		}
		seen[tag] = struct{}{}
	}
	for index := 1; index < len(tags); index++ {
		if tags[index-1] >= tags[index] {
			return ErrInvalidLSPProfile
		}
	}
	return nil
}

func effectiveGoProfileID(profile GoProfile) (model.Hash, error) {
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

func validateExecutablePin(pin ExecutablePin) error {
	if !filepath.IsAbs(pin.Path) || pin.Version == "" || !goValuePattern.MatchString(pin.Version) {
		return ErrExecutableUnavailable
	}
	if _, err := model.ParseHash(string(pin.SHA256)); err != nil {
		return ErrExecutableUnavailable
	}
	if err := executableHashMatches(pin.Path, pin.SHA256); err != nil {
		return ErrExecutableUnavailable
	}
	return nil
}

func isAllowedExecutable(allowed []AllowedExecutable, pin ExecutablePin) bool {
	if !filepath.IsAbs(pin.Path) {
		return false
	}
	for _, candidate := range allowed {
		if samePath(candidate.Path, pin.Path) && candidate.SHA256 == pin.SHA256 {
			return true
		}
	}
	return false
}

func verifyExecutableVersion(ctx context.Context, pin ExecutablePin, workingDirectory string, environment map[string]string) error {
	if ctx == nil || validateExecutablePin(pin) != nil || !filepath.IsAbs(workingDirectory) {
		return ErrExecutableUnavailable
	}
	command := exec.CommandContext(ctx, pin.Path, "version")
	command.Dir = workingDirectory
	command.Env = environmentList(environment)
	stdout, stderr := &limitedVersionOutput{}, &limitedVersionOutput{}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil || stdout.overflow || stderr.overflow || !containsExactVersion(stdout.String(), pin.Version) {
		return ErrExecutableUnavailable
	}
	if err := executableHashMatches(pin.Path, pin.SHA256); err != nil {
		return ErrExecutableUnavailable
	}
	return nil
}

type limitedVersionOutput struct {
	bytes.Buffer
	overflow bool
}

func (output *limitedVersionOutput) Write(value []byte) (int, error) {
	const limit = 1024
	if output.Len()+len(value) > limit {
		output.overflow = true
		remaining := limit - output.Len()
		if remaining > 0 {
			_, _ = output.Buffer.Write(value[:remaining])
		}
		return len(value), nil
	}
	return output.Buffer.Write(value)
}

func validateExecutionPolicy(policy ExecutionPolicy) error {
	if len(policy.AllowedRoots) < 1 || len(policy.AllowedRoots) > 32 || len(policy.DependencyRoots) > 128 ||
		len(policy.AllowedExecutables) < 1 || len(policy.AllowedExecutables) > 32 || policy.MaxFileBytes < 1 || policy.MaxSourceBytes < policy.MaxFileBytes {
		return ErrInvalidLSPProfile
	}
	for _, value := range append(append([]string(nil), policy.AllowedRoots...), policy.DependencyRoots...) {
		if !filepath.IsAbs(value) {
			return ErrInvalidLSPProfile
		}
	}
	seen := make(map[string]struct{}, len(policy.AllowedExecutables))
	for _, executable := range policy.AllowedExecutables {
		if !filepath.IsAbs(executable.Path) {
			return ErrInvalidLSPProfile
		}
		if _, err := model.ParseHash(string(executable.SHA256)); err != nil {
			return ErrInvalidLSPProfile
		}
		key := filepath.Clean(executable.Path)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, duplicate := seen[key]; duplicate {
			return ErrInvalidLSPProfile
		}
		seen[key] = struct{}{}
	}
	return nil
}

func executableHashMatches(path string, expected model.Hash) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || isReparsePoint(path) {
		return ErrExecutableUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrExecutableUnavailable
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	current, lstatErr := os.Lstat(path)
	if copyErr != nil || statErr != nil || closeErr != nil || lstatErr != nil || !os.SameFile(info, after) || !os.SameFile(after, current) ||
		info.Size() != after.Size() || info.ModTime() != after.ModTime() || current.Size() != after.Size() || current.ModTime() != after.ModTime() {
		return ErrExecutableUnavailable
	}
	if hex.EncodeToString(hash.Sum(nil)) != string(expected) {
		return ErrExecutableUnavailable
	}
	return nil
}

func containsExactVersion(output, expected string) bool {
	for _, token := range strings.Fields(output) {
		if token == expected {
			return true
		}
	}
	return false
}

func canonicalDirectory(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", ErrInvalidLSPProfile
	}
	info, err := os.Lstat(value)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || isReparsePoint(value) {
		return "", ErrInvalidLSPProfile
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || !samePath(absolute, resolved) {
		return "", ErrInvalidLSPProfile
	}
	return filepath.Clean(absolute), nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

func isDependencyRoot(roots []string, candidate string) bool {
	for _, root := range roots {
		canonical, err := canonicalDirectory(root)
		if err == nil && pathWithin(canonical, candidate) {
			return true
		}
	}
	return false
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func executableCommand(pin ExecutablePin) *exec.Cmd {
	return exec.Command(pin.Path, "serve")
}

func validateProfileSummary(profile GoProfile) error {
	if profile.ID == "" || profile.Go.Version == "" || profile.Gopls.Version == "" {
		return fmt.Errorf("tool versions are required: %w", ErrInvalidLSPProfile)
	}
	return nil
}
