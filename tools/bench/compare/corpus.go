// Package compare validates the synthetic A/B/C benchmark corpus and imports
// measurements supplied by the AI host. It never invokes an LLM or API.
package compare

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	FormatVersion       = 1
	MinimumTasks        = 30
	MinimumRepositories = 3
	RecommendedRepeats  = 3
	MaxRepeatCount      = 100
	MaxUsageLineBytes   = 4 << 20
)

var (
	ErrInvalidSuite = errors.New("invalid benchmark suite")
	ErrInvalidUsage = errors.New("invalid benchmark usage")
)

type Suite struct {
	FormatVersion   int          `json:"format_version"`
	SuiteID         string       `json:"suite_id"`
	Synthetic       bool         `json:"synthetic"`
	RecommendedRuns int          `json:"recommended_repeats"`
	Repositories    []Repository `json:"repositories"`
	Tasks           []Task       `json:"tasks"`
	SourceSHA256    string       `json:"-"`
}

type Repository struct {
	ID               string   `json:"id"`
	Module           string   `json:"module"`
	FixturePath      string   `json:"fixture_path"`
	FixtureRevision  string   `json:"fixture_revision"`
	FixtureSHA256    string   `json:"fixture_sha256"`
	DependencySHA256 string   `json:"dependency_sha256"`
	DependencyFiles  []string `json:"dependency_files"`
}

type Task struct {
	ID                    string    `json:"id"`
	RepositoryID          string    `json:"repository_id"`
	Scenario              string    `json:"scenario"`
	Request               string    `json:"request"`
	ExpectedChangedPaths  []string  `json:"expected_changed_paths"`
	ForbiddenChangedPaths []string  `json:"forbidden_changed_paths"`
	RequiredInvariants    []string  `json:"required_invariants"`
	Oracle                string    `json:"oracle"`
	Setup                 TaskSetup `json:"setup"`
}

type TaskSetup struct {
	Files  map[string]string `json:"files"`
	SHA256 string            `json:"sha256"`
}

var requiredScenarios = []string{
	"local_rename", "interface_change", "field_change", "type_change",
	"behavior_bug", "contract_preservation", "dirty_worktree",
	"missing_dependency", "large_fanout",
}

func LoadSuite(path string) (Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, fmt.Errorf("read suite: %w", err)
	}
	var suite Suite
	if err := decodeStrictJSON(data, &suite); err != nil {
		return Suite{}, fmt.Errorf("decode suite: %w", err)
	}
	digest := sha256.Sum256(normalizeLineEndings(data))
	suite.SourceSHA256 = hex.EncodeToString(digest[:])
	if err := ValidateSuite(suite, filepath.Dir(path)); err != nil {
		return Suite{}, err
	}
	return suite, nil
}

func ValidateSuite(suite Suite, suiteRoot string) error {
	if suite.FormatVersion != FormatVersion || strings.TrimSpace(suite.SuiteID) == "" ||
		!suite.Synthetic || suite.RecommendedRuns < RecommendedRepeats ||
		len(suite.Repositories) < MinimumRepositories || len(suite.Tasks) < MinimumTasks {
		return fmt.Errorf("header, synthetic flag, or minimum corpus size: %w", ErrInvalidSuite)
	}
	root, err := filepath.Abs(suiteRoot)
	if err != nil {
		return fmt.Errorf("resolve suite root: %w", err)
	}
	repositories := make(map[string]Repository, len(suite.Repositories))
	modules := make(map[string]struct{}, len(suite.Repositories))
	for _, repo := range suite.Repositories {
		if !validIdentifier(repo.ID) || strings.TrimSpace(repo.Module) == "" ||
			!safeRelativePath(repo.FixturePath) || !validSHA256(repo.FixtureSHA256) ||
			repo.FixtureRevision != repo.FixtureSHA256 || !validSHA256(repo.DependencySHA256) ||
			len(repo.DependencyFiles) == 0 {
			return fmt.Errorf("repository %q metadata: %w", repo.ID, ErrInvalidSuite)
		}
		if _, exists := repositories[repo.ID]; exists {
			return fmt.Errorf("duplicate repository %q: %w", repo.ID, ErrInvalidSuite)
		}
		if _, exists := modules[repo.Module]; exists {
			return fmt.Errorf("duplicate module %q: %w", repo.Module, ErrInvalidSuite)
		}
		modules[repo.Module] = struct{}{}
		fixtureRoot, err := resolveUnder(root, repo.FixturePath)
		if err != nil {
			return fmt.Errorf("repository %q fixture path: %w", repo.ID, err)
		}
		fixtureDigest, err := fixtureSHA256(fixtureRoot)
		if err != nil {
			return fmt.Errorf("repository %q fixture: %w", repo.ID, err)
		}
		if fixtureDigest != repo.FixtureSHA256 {
			return fmt.Errorf("repository %q fixture SHA mismatch: got %s want %s: %w", repo.ID, fixtureDigest, repo.FixtureSHA256, ErrInvalidSuite)
		}
		dependencyDigest, err := dependencySHA256(fixtureRoot, repo.DependencyFiles)
		if err != nil {
			return fmt.Errorf("repository %q dependency files: %w", repo.ID, err)
		}
		if dependencyDigest != repo.DependencySHA256 {
			return fmt.Errorf("repository %q dependency SHA mismatch: got %s want %s: %w", repo.ID, dependencyDigest, repo.DependencySHA256, ErrInvalidSuite)
		}
		repositories[repo.ID] = repo
	}

	tasks := make(map[string]struct{}, len(suite.Tasks))
	scenarios := make(map[string]int, len(requiredScenarios))
	for _, task := range suite.Tasks {
		if !validIdentifier(task.ID) || strings.TrimSpace(task.Request) == "" ||
			strings.TrimSpace(task.Oracle) == "" || len(task.RequiredInvariants) == 0 {
			return fmt.Errorf("task %q metadata: %w", task.ID, ErrInvalidSuite)
		}
		if _, exists := tasks[task.ID]; exists {
			return fmt.Errorf("duplicate task %q: %w", task.ID, ErrInvalidSuite)
		}
		tasks[task.ID] = struct{}{}
		if _, ok := repositories[task.RepositoryID]; !ok {
			return fmt.Errorf("task %q references unknown repository %q: %w", task.ID, task.RepositoryID, ErrInvalidSuite)
		}
		if !contains(requiredScenarios, task.Scenario) {
			return fmt.Errorf("task %q has unknown scenario %q: %w", task.ID, task.Scenario, ErrInvalidSuite)
		}
		scenarios[task.Scenario]++
		if !validPathSet(task.ExpectedChangedPaths) || !validPathSet(task.ForbiddenChangedPaths) ||
			!validStringList(task.RequiredInvariants) || !validSetup(task.Setup) {
			return fmt.Errorf("task %q path/setup fields: %w", task.ID, ErrInvalidSuite)
		}
		for _, path := range task.ExpectedChangedPaths {
			if contains(task.ForbiddenChangedPaths, path) {
				return fmt.Errorf("task %q path %q is both expected and forbidden: %w", task.ID, path, ErrInvalidSuite)
			}
		}
	}
	for _, scenario := range requiredScenarios {
		if scenarios[scenario] == 0 {
			return fmt.Errorf("scenario %q is missing: %w", scenario, ErrInvalidSuite)
		}
	}
	return nil
}

func validSetup(setup TaskSetup) bool {
	if !validSHA256(setup.SHA256) {
		return false
	}
	for path := range setup.Files {
		if !safeRelativePath(path) {
			return false
		}
	}
	return setupSHA256(setup.Files) == setup.SHA256
}

func setupSHA256(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	h := sha256.New()
	for _, key := range keys {
		content := sha256.Sum256([]byte(files[key]))
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(hex.EncodeToString(content[:])))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fixtureSHA256(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("fixture root must be a real directory: %w", ErrInvalidSuite)
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture contains symlink %q: %w", path, ErrInvalidSuite)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fixture contains non-regular file %q: %w", path, ErrInvalidSuite)
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("fixture has no files: %w", ErrInvalidSuite)
	}
	sort.Slice(files, func(i, j int) bool {
		iRel, _ := filepath.Rel(root, files[i])
		jRel, _ := filepath.Rel(root, files[j])
		return filepath.ToSlash(iRel) < filepath.ToSlash(jRel)
	})
	h := sha256.New()
	for _, path := range files {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		contentSHA := sha256.Sum256(normalizeLineEndings(content))
		_, _ = h.Write([]byte(filepath.ToSlash(relative)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(hex.EncodeToString(contentSHA[:])))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func dependencySHA256(root string, files []string) (string, error) {
	seen := make(map[string]struct{}, len(files))
	paths := append([]string(nil), files...)
	sort.Strings(paths)
	if len(paths) == 0 {
		return "", fmt.Errorf("dependency file list empty: %w", ErrInvalidSuite)
	}
	h := sha256.New()
	for _, relative := range paths {
		if !safeRelativePath(relative) || (relative != "go.mod" && relative != "go.sum") {
			return "", fmt.Errorf("unsupported dependency file %q: %w", relative, ErrInvalidSuite)
		}
		if _, exists := seen[relative]; exists {
			return "", fmt.Errorf("duplicate dependency file %q: %w", relative, ErrInvalidSuite)
		}
		seen[relative] = struct{}{}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return "", err
		}
		if len(paths) == 1 {
			digest := sha256.Sum256(normalizeLineEndings(data))
			return hex.EncodeToString(digest[:]), nil
		}
		fileSHA := sha256.Sum256(normalizeLineEndings(data))
		_, _ = h.Write([]byte(relative))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(hex.EncodeToString(fileSHA[:])))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// normalizeLineEndings keeps fixture fingerprints stable when Git checks out
// the same UTF-8 text with CRLF on Windows and LF on Unix.
func normalizeLineEndings(data []byte) []byte {
	if !utf8.Valid(data) || bytes.Contains(data, []byte{0}) {
		return data
	}
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

func decodeStrictJSON(data []byte, target any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		if err != nil {
			return err
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func safeRelativePath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || strings.HasPrefix(path, "/") ||
		filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return false
	}
	if len(path) >= 2 && path[1] == ':' {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func resolveUnder(root, relative string) (string, error) {
	if !safeRelativePath(relative) {
		return "", fmt.Errorf("unsafe relative path %q: %w", relative, ErrInvalidSuite)
	}
	joined := filepath.Join(root, filepath.FromSlash(relative))
	resolved, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes suite root: %w", ErrInvalidSuite)
	}
	return resolved, nil
}

func validPathSet(paths []string) bool {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if !safeRelativePath(path) {
			return false
		}
		if _, exists := seen[path]; exists {
			return false
		}
		seen[path] = struct{}{}
	}
	return true
}

func validStringList(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func validIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 80 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' {
			return false
		}
	}
	return value[0] != '-'
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
