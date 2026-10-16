package inventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
)

const (
	maxInventoryEntries = 1_000_000
	maxGitOutputBytes   = 64 << 20
	maxGitStderrBytes   = 64 << 10
)

var (
	// ErrInvalidOptions indicates a missing or invalid root, Git executable, or policy.
	ErrInvalidOptions = errors.New("invalid inventory options")
	// ErrNotWorktree indicates the requested root is nested inside another checkout.
	ErrNotWorktree = errors.New("inventory root is not the Git worktree root")
	// ErrGitCommand marks a failed read-only Git metadata query.
	ErrGitCommand = errors.New("read-only Git metadata command failed")
	// ErrResourceLimit indicates a Git output or inventory entry cap was reached.
	ErrResourceLimit = errors.New("inventory metadata limit exceeded")
	// ErrCaseFoldCollision indicates distinct paths compare equal on an insensitive filesystem.
	ErrCaseFoldCollision = errors.New("case-fold path collision")
)

// HeadState describes the checkout's current HEAD state.
type HeadState string

const (
	HeadPresent  HeadState = "present"
	HeadUnborn   HeadState = "unborn"
	HeadDetached HeadState = "detached"
)

// Origin states whether Git's index or the worktree supplied a file path.
type Origin string

const (
	OriginTracked   Origin = "tracked"
	OriginUntracked Origin = "untracked"
)

// FileState describes path metadata without implying that content was captured.
type FileState string

const (
	FilePresent       FileState = "present"
	FileMissing       FileState = "missing"
	FileUnmerged      FileState = "unmerged"
	FileSparseOmitted FileState = "sparse_omitted"
	FileInaccessible  FileState = "inaccessible"
)

// BoundaryReason explains why inventory did not descend into or expose content at a path.
type BoundaryReason string

const (
	BoundaryDeniedDirectory BoundaryReason = "denied_directory"
	BoundaryNestedGit       BoundaryReason = "nested_git_root"
	BoundarySubmodule       BoundaryReason = "submodule"
	BoundarySparse          BoundaryReason = "sparse_directory"
	BoundarySymlink         BoundaryReason = "symlink"
	BoundaryTypeChanged     BoundaryReason = "type_changed"
	BoundaryInaccessible    BoundaryReason = "inaccessible"
)

// CoverageState distinguishes complete metadata coverage from a known partial inventory.
type CoverageState string

const (
	CoverageComplete CoverageState = "complete"
	CoveragePartial  CoverageState = "partial"
)

// Options binds one scan to a caller-authorized worktree root, a pinned Git
// executable, and the already-compiled path policy. Registration authorization
// belongs to the caller; this scanner never resolves Git through a shell.
type Options struct {
	Root          string
	GitExecutable string
	Policy        *policy.Policy
	CaseMode      paths.CaseMode
}

// File contains path and filesystem/index metadata only. It never contains source bytes.
type File struct {
	Path               string    `json:"path"`
	Origin             Origin    `json:"origin"`
	State              FileState `json:"state"`
	Kind               string    `json:"kind"`
	GitMode            uint32    `json:"git_mode,omitempty"`
	SizeBytes          int64     `json:"size_bytes,omitempty"`
	Sparse             bool      `json:"sparse"`
	Language           string    `json:"language,omitempty"`
	TextSearchEligible bool      `json:"text_search_eligible"`
	GeneratedCandidate bool      `json:"generated_candidate"`
}

// Boundary marks a path or subtree that inventory deliberately did not traverse.
type Boundary struct {
	Path   string         `json:"path,omitempty"`
	Reason BoundaryReason `json:"reason"`
}

// Coverage counts included, excluded, missing, and explicitly unsupported paths.
type Coverage struct {
	State                   CoverageState `json:"state"`
	CaseSemanticsKnown      bool          `json:"case_semantics_known"`
	Tracked                 int           `json:"tracked"`
	Untracked               int           `json:"untracked"`
	Ignored                 int           `json:"ignored"`
	ExcludedByPolicy        int           `json:"excluded_by_policy"`
	Sparse                  int           `json:"sparse"`
	Unmerged                int           `json:"unmerged"`
	Missing                 int           `json:"missing"`
	Inaccessible            int           `json:"inaccessible"`
	Symlinks                int           `json:"symlinks"`
	NestedRepositories      int           `json:"nested_repositories"`
	UnsupportedPath         int           `json:"unsupported_path"`
	UnsupportedPathEncoding int           `json:"unsupported_path_encoding"`
}

// Snapshot is a deterministic metadata view of one Git index/worktree observation.
type Snapshot struct {
	Head             string     `json:"head,omitempty"`
	HeadState        HeadState  `json:"head_state"`
	Branch           string     `json:"branch,omitempty"`
	GitIgnoreMode    string     `json:"git_ignore_mode"`
	MembershipDigest string     `json:"membership_digest"`
	Files            []File     `json:"files"`
	Boundaries       []Boundary `json:"boundaries"`
	Coverage         Coverage   `json:"coverage"`
}

// ChangeSet compares exact path identities between two metadata snapshots.
type ChangeSet struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

type indexEntry struct {
	path   string
	mode   uint32
	stage  int
	sparse bool
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if len(value) > b.limit-b.Len() {
		b.overflow = true
		return 0, ErrResourceLimit
	}
	return b.Buffer.Write(value)
}

// Scan enumerates the current index and permitted untracked paths. Git and the
// filesystem are queried afresh on every call so callers can detect membership changes.
func Scan(ctx context.Context, options Options) (Snapshot, error) {
	if ctx == nil || options.Policy == nil || options.Root == "" || options.GitExecutable == "" {
		return Snapshot{}, ErrInvalidOptions
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve inventory root: %w", ErrInvalidOptions)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve inventory root: %w", ErrInvalidOptions)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve inventory root: %w", ErrInvalidOptions)
	}
	if !rootInfo.IsDir() {
		return Snapshot{}, fmt.Errorf("inventory root must be a directory: %w", ErrInvalidOptions)
	}
	gitExecutable, err := filepath.Abs(options.GitExecutable)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve Git executable: %w", ErrInvalidOptions)
	}
	gitInfo, err := os.Stat(gitExecutable)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve Git executable: %w", ErrInvalidOptions)
	}
	if !gitInfo.Mode().IsRegular() {
		return Snapshot{}, fmt.Errorf("Git executable must be an existing file: %w", ErrInvalidOptions)
	}

	scanner := gitScanner{ctx: ctx, root: root, executable: gitExecutable}
	worktreeRoot, err := scanner.text("rev-parse", "--show-toplevel")
	if err != nil {
		return Snapshot{}, err
	}
	if !samePath(root, strings.TrimSpace(worktreeRoot)) {
		return Snapshot{}, ErrNotWorktree
	}

	result := Snapshot{
		HeadState:     HeadUnborn,
		GitIgnoreMode: "repository .gitignore and .git/info/exclude; global/system and core.excludesFile disabled",
		Coverage:      Coverage{State: CoverageComplete, CaseSemanticsKnown: options.CaseMode != paths.CaseUnknown},
	}
	if !result.Coverage.CaseSemanticsKnown {
		result.Coverage.State = CoveragePartial
	}
	if head, code, headErr := scanner.textAllowExitOne("rev-parse", "--verify", "--quiet", "HEAD"); headErr != nil {
		return Snapshot{}, headErr
	} else if code == 0 {
		result.Head = head
		result.HeadState = HeadPresent
	}
	if branch, code, branchErr := scanner.textAllowExitOne("symbolic-ref", "--quiet", "--short", "HEAD"); branchErr != nil {
		return Snapshot{}, branchErr
	} else if code == 0 {
		result.Branch = branch
	} else if result.HeadState == HeadPresent {
		result.HeadState = HeadDetached
	}

	index, err := scanner.indexEntries()
	if err != nil {
		return Snapshot{}, err
	}
	tags, err := scanner.indexTags()
	if err != nil {
		return Snapshot{}, err
	}
	trackedPaths := make(map[string]struct{}, len(index))
	unmergedPaths := make(map[string]struct{})
	for _, entry := range index {
		if entry.stage != 0 {
			unmergedPaths[entry.path] = struct{}{}
		}
	}
	processedTracked := make(map[string]struct{}, len(index))
	files := make([]File, 0, len(index))
	boundaries := make([]Boundary, 0)
	for _, entry := range index {
		path := entry.path
		trackedPaths[path] = struct{}{}
		if _, duplicate := processedTracked[path]; duplicate {
			continue
		}
		processedTracked[path] = struct{}{}
		if pathErr := paths.ValidateRelative(path); pathErr != nil {
			if errors.Is(pathErr, paths.ErrUnsupportedEncoding) {
				result.Coverage.UnsupportedPathEncoding++
			} else {
				result.Coverage.UnsupportedPath++
			}
			result.Coverage.State = CoveragePartial
			continue
		}
		if entry.mode == 0o160000 || entry.mode == 0o040000 {
			denied, policyErr := deniedDirectory(options.Policy, path)
			if policyErr != nil {
				return Snapshot{}, policyErr
			}
			if denied {
				result.Coverage.ExcludedByPolicy++
				continue
			}
		}
		if entry.mode == 0o160000 {
			result.Coverage.NestedRepositories++
			boundaries = append(boundaries, Boundary{Path: path, Reason: BoundarySubmodule})
			result.Coverage.State = CoveragePartial
			continue
		}
		if entry.mode == 0o040000 {
			result.Coverage.Sparse++
			boundaries = append(boundaries, Boundary{Path: strings.TrimSuffix(path, "/"), Reason: BoundarySparse})
			result.Coverage.State = CoveragePartial
			continue
		}
		denied, policyErr := deniedPath(options.Policy, path, false)
		if policyErr != nil {
			return Snapshot{}, policyErr
		}
		if denied {
			result.Coverage.ExcludedByPolicy++
			continue
		}
		if entry.mode == 0o120000 {
			result.Coverage.Symlinks++
			result.Coverage.State = CoveragePartial
			boundaries = append(boundaries, Boundary{Path: path, Reason: BoundarySymlink})
			if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); errors.Is(statErr, fs.ErrNotExist) {
				result.Coverage.Missing++
			} else if statErr != nil {
				result.Coverage.Inaccessible++
			}
			continue
		}
		if entry.mode != 0o100644 && entry.mode != 0o100755 {
			boundaries = append(boundaries, Boundary{Path: path, Reason: BoundaryTypeChanged})
			result.Coverage.State = CoveragePartial
			continue
		}
		sparse := tags[path] == 'S' || entry.sparse
		if sparse {
			result.Coverage.Sparse++
			result.Coverage.State = CoveragePartial
		}
		if info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); statErr != nil {
			state := FileMissing
			if errors.Is(statErr, fs.ErrPermission) {
				state = FileInaccessible
				result.Coverage.Inaccessible++
				result.Coverage.State = CoveragePartial
			} else if sparse {
				state = FileSparseOmitted
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				state = FileInaccessible
				result.Coverage.Inaccessible++
				result.Coverage.State = CoveragePartial
			}
			if state == FileMissing {
				result.Coverage.Missing++
				result.Coverage.State = CoveragePartial
			}
			if state == FileSparseOmitted {
				boundaries = append(boundaries, Boundary{Path: path, Reason: BoundarySparse})
			}
			if _, unmerged := unmergedPaths[path]; unmerged {
				state = FileUnmerged
				result.Coverage.Unmerged++
				result.Coverage.State = CoveragePartial
			}
			files = append(files, mappedFile(path, OriginTracked, state, entry.mode, 0, sparse, options.Policy))
			continue
		} else if info.Mode()&os.ModeSymlink != 0 {
			result.Coverage.Symlinks++
			boundaries = append(boundaries, Boundary{Path: path, Reason: BoundarySymlink})
			result.Coverage.State = CoveragePartial
			continue
		} else if !info.Mode().IsRegular() {
			boundaries = append(boundaries, Boundary{Path: path, Reason: BoundaryTypeChanged})
			result.Coverage.State = CoveragePartial
			continue
		} else {
			state := FilePresent
			if _, unmerged := unmergedPaths[path]; unmerged {
				state = FileUnmerged
				result.Coverage.Unmerged++
				result.Coverage.State = CoveragePartial
			}
			files = append(files, mappedFile(path, OriginTracked, state, entry.mode, info.Size(), sparse, options.Policy))
		}
	}
	result.Coverage.Tracked = len(trackedPaths)

	candidates, walkedBoundaries, walkCoverage, err := walkUntracked(ctx, root, options.Policy, trackedPaths)
	if err != nil {
		return Snapshot{}, err
	}
	result.Coverage.ExcludedByPolicy += walkCoverage.ExcludedByPolicy
	result.Coverage.Symlinks += walkCoverage.Symlinks
	result.Coverage.NestedRepositories += walkCoverage.NestedRepositories
	result.Coverage.UnsupportedPath += walkCoverage.UnsupportedPath
	result.Coverage.UnsupportedPathEncoding += walkCoverage.UnsupportedPathEncoding
	result.Coverage.Inaccessible += walkCoverage.Inaccessible
	if walkCoverage.State == CoveragePartial {
		result.Coverage.State = CoveragePartial
	}
	boundaries = append(boundaries, walkedBoundaries...)
	ignored, err := scanner.ignoredPaths(candidates)
	if err != nil {
		return Snapshot{}, err
	}
	for _, candidate := range candidates {
		if _, excluded := ignored[candidate.path]; excluded {
			result.Coverage.Ignored++
			continue
		}
		denied, policyErr := deniedPath(options.Policy, candidate.path, false)
		if policyErr != nil {
			return Snapshot{}, policyErr
		}
		if denied {
			result.Coverage.ExcludedByPolicy++
			continue
		}
		info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(candidate.path)))
		if statErr != nil {
			result.Coverage.Inaccessible++
			result.Coverage.State = CoveragePartial
			files = append(files, mappedFile(candidate.path, OriginUntracked, FileInaccessible, 0, 0, false, options.Policy))
			result.Coverage.Untracked++
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			result.Coverage.Symlinks++
			boundaries = append(boundaries, Boundary{Path: candidate.path, Reason: BoundarySymlink})
			result.Coverage.State = CoveragePartial
			continue
		}
		if !info.Mode().IsRegular() {
			result.Coverage.State = CoveragePartial
			boundaries = append(boundaries, Boundary{Path: candidate.path, Reason: BoundaryTypeChanged})
			continue
		}
		files = append(files, mappedFile(candidate.path, OriginUntracked, FilePresent, 0, info.Size(), false, options.Policy))
		result.Coverage.Untracked++
	}
	if result.Coverage.Sparse > 0 || result.Coverage.Unmerged > 0 || result.Coverage.Inaccessible > 0 || result.Coverage.UnsupportedPath > 0 || result.Coverage.UnsupportedPathEncoding > 0 {
		result.Coverage.State = CoveragePartial
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Slice(boundaries, func(i, j int) bool {
		if boundaries[i].Path == boundaries[j].Path {
			return boundaries[i].Reason < boundaries[j].Reason
		}
		return boundaries[i].Path < boundaries[j].Path
	})
	if options.CaseMode == paths.CaseInsensitive {
		pathsToCheck := make([]string, 0, len(files)+len(boundaries))
		for _, file := range files {
			pathsToCheck = append(pathsToCheck, file.Path)
		}
		for _, boundary := range boundaries {
			if boundary.Path != "" {
				pathsToCheck = append(pathsToCheck, boundary.Path)
			}
		}
		collisions, collisionErr := paths.FindCaseFoldCollisions(pathsToCheck)
		if collisionErr != nil {
			return Snapshot{}, collisionErr
		}
		if len(collisions) > 0 {
			return Snapshot{}, fmt.Errorf("%w: %q and %q", ErrCaseFoldCollision, collisions[0].First, collisions[0].Second)
		}
	}
	result.Files = files
	result.Boundaries = boundaries
	result.MembershipDigest = membershipDigest(result)
	return result, nil
}

// CompareMembership reports exact path additions/removals/state changes. A move
// remains one removal and one addition; no path similarity is used as identity.
func CompareMembership(previous, current Snapshot) ChangeSet {
	before := membershipItems(previous)
	after := membershipItems(current)
	change := ChangeSet{Added: make([]string, 0), Removed: make([]string, 0), Changed: make([]string, 0)}
	for path, fingerprint := range after {
		old, exists := before[path]
		if !exists {
			change.Added = append(change.Added, path)
		} else if old != fingerprint {
			change.Changed = append(change.Changed, path)
		}
	}
	for path := range before {
		if _, exists := after[path]; !exists {
			change.Removed = append(change.Removed, path)
		}
	}
	sort.Strings(change.Added)
	sort.Strings(change.Removed)
	sort.Strings(change.Changed)
	return change
}

type walkCounts struct {
	Coverage
}

type untrackedCandidate struct{ path string }

func walkUntracked(ctx context.Context, root string, pathPolicy *policy.Policy, tracked map[string]struct{}) ([]untrackedCandidate, []Boundary, walkCounts, error) {
	candidates := make([]untrackedCandidate, 0)
	boundaries := make([]Boundary, 0)
	counts := walkCounts{Coverage: Coverage{State: CoverageComplete}}
	pathBytes := 0
	err := filepath.WalkDir(root, func(fullPath string, entry os.DirEntry, walkErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if walkErr != nil {
			if fullPath == root {
				return walkErr
			}
			relative, relErr := filepath.Rel(root, fullPath)
			if relErr == nil {
				path := filepath.ToSlash(relative)
				if paths.ValidateRelative(path) == nil {
					denied, policyErr := deniedPath(pathPolicy, path, false)
					if policyErr != nil {
						return policyErr
					}
					if !denied {
						counts.Inaccessible++
						counts.State = CoveragePartial
						boundaries = append(boundaries, Boundary{Path: path, Reason: BoundaryInaccessible})
					}
				}
			}
			return nil
		}
		if fullPath == root {
			return nil
		}
		relative, relErr := filepath.Rel(root, fullPath)
		if relErr != nil {
			return relErr
		}
		path := filepath.ToSlash(relative)
		if path == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if pathErr := paths.ValidateRelative(path); pathErr != nil {
			if errors.Is(pathErr, paths.ErrUnsupportedEncoding) {
				counts.UnsupportedPathEncoding++
			} else {
				counts.UnsupportedPath++
			}
			counts.State = CoveragePartial
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			denied, policyErr := deniedDirectory(pathPolicy, path)
			if policyErr != nil {
				return policyErr
			}
			if denied {
				counts.ExcludedByPolicy++
				boundaries = append(boundaries, Boundary{Reason: BoundaryDeniedDirectory})
				return filepath.SkipDir
			}
			if _, tracked := tracked[path]; tracked {
				return filepath.SkipDir
			}
			hasGit, markerErr := hasGitMarker(fullPath)
			if markerErr != nil {
				counts.Inaccessible++
				counts.State = CoveragePartial
				boundaries = append(boundaries, Boundary{Path: path, Reason: BoundaryInaccessible})
				return filepath.SkipDir
			}
			if hasGit {
				counts.NestedRepositories++
				counts.State = CoveragePartial
				boundaries = append(boundaries, Boundary{Path: path, Reason: BoundaryNestedGit})
				return filepath.SkipDir
			}
			return nil
		}
		if _, alreadyTracked := tracked[path]; alreadyTracked {
			return nil
		}
		denied, policyErr := deniedPath(pathPolicy, path, false)
		if policyErr != nil {
			return policyErr
		}
		if denied {
			counts.ExcludedByPolicy++
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			counts.Symlinks++
			counts.State = CoveragePartial
			boundaries = append(boundaries, Boundary{Path: path, Reason: BoundarySymlink})
			return nil
		}
		if entry.Type()&os.ModeType != 0 {
			counts.State = CoveragePartial
			boundaries = append(boundaries, Boundary{Path: path, Reason: BoundaryTypeChanged})
			return nil
		}
		candidates = append(candidates, untrackedCandidate{path: path})
		pathBytes += len(path) + 1
		if len(candidates) > maxInventoryEntries || pathBytes > maxGitOutputBytes {
			return ErrResourceLimit
		}
		return nil
	})
	if err != nil {
		return nil, nil, counts, err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].path < candidates[j].path })
	return candidates, boundaries, counts, nil
}

func (s gitScanner) indexEntries() ([]indexEntry, error) {
	output, err := s.run("ls-files", "--stage", "--sparse", "-z", "--")
	if err != nil {
		return nil, err
	}
	entries := make([]indexEntry, 0)
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		metadata, rawPath, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("%w: malformed ls-files index record", ErrGitCommand)
		}
		fields := strings.Fields(string(metadata))
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: malformed ls-files stage metadata", ErrGitCommand)
		}
		mode, modeErr := strconv.ParseUint(fields[0], 8, 32)
		stage, stageErr := strconv.Atoi(fields[2])
		if modeErr != nil || stageErr != nil || stage < 0 || stage > 3 {
			return nil, fmt.Errorf("%w: invalid ls-files index metadata", ErrGitCommand)
		}
		path := string(rawPath)
		if mode == 0o040000 {
			path = strings.TrimSuffix(path, "/")
		}
		entries = append(entries, indexEntry{path: path, mode: uint32(mode), stage: stage, sparse: mode == 0o040000})
		if len(entries) > maxInventoryEntries {
			return nil, ErrResourceLimit
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].path == entries[j].path {
			return entries[i].stage < entries[j].stage
		}
		return entries[i].path < entries[j].path
	})
	return entries, nil
}

func (s gitScanner) indexTags() (map[string]byte, error) {
	output, err := s.run("ls-files", "-t", "--sparse", "-z", "--")
	if err != nil {
		return nil, err
	}
	tags := make(map[string]byte)
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if len(record) < 3 || record[1] != ' ' {
			return nil, fmt.Errorf("%w: malformed ls-files tag record", ErrGitCommand)
		}
		path := string(record[2:])
		if strings.HasSuffix(path, "/") {
			path = strings.TrimSuffix(path, "/")
		}
		tags[path] = record[0]
		if len(tags) > maxInventoryEntries {
			return nil, ErrResourceLimit
		}
	}
	return tags, nil
}

func (s gitScanner) ignoredPaths(candidates []untrackedCandidate) (map[string]struct{}, error) {
	ignored := make(map[string]struct{})
	if len(candidates) == 0 {
		return ignored, nil
	}
	var input bytes.Buffer
	for _, candidate := range candidates {
		// A ./ prefix keeps leading ':' names literal; Git's check-ignore command
		// rejects pathspec magic, so GIT_LITERAL_PATHSPECS is not supported here.
		input.WriteString("./")
		input.WriteString(candidate.path)
		input.WriteByte(0)
		if input.Len() > maxGitOutputBytes {
			return nil, ErrResourceLimit
		}
	}
	output, code, err := s.runAllowExitOne(input.Bytes(), "check-ignore", "-z", "--stdin")
	if err != nil {
		return nil, err
	}
	if code == 1 && len(output) != 0 {
		return nil, fmt.Errorf("%w: check-ignore exit status inconsistent", ErrGitCommand)
	}
	for _, path := range bytes.Split(output, []byte{0}) {
		if len(path) != 0 {
			value := strings.TrimPrefix(string(path), "./")
			ignored[value] = struct{}{}
		}
	}
	return ignored, nil
}

type gitScanner struct {
	ctx        context.Context
	root       string
	executable string
}

func (s gitScanner) text(args ...string) (string, error) {
	output, err := s.run(args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (s gitScanner) textAllowExitOne(args ...string) (string, int, error) {
	output, code, err := s.runAllowExitOne(nil, args...)
	return strings.TrimSpace(string(output)), code, err
}

func (s gitScanner) run(args ...string) ([]byte, error) {
	output, code, err := s.runAllowExitOne(nil, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, ErrGitCommand
	}
	return output, nil
}

func (s gitScanner) runAllowExitOne(input []byte, args ...string) ([]byte, int, error) {
	global := []string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.excludesFile=" + nullDevice(), "-c", "core.hooksPath=" + nullDevice()}
	commandArgs := append(global, args...)
	command := exec.CommandContext(s.ctx, s.executable, commandArgs...)
	command.Dir = s.root
	command.Env = sanitizedEnvironment()
	if len(input) != 0 {
		command.Stdin = bytes.NewReader(input)
	}
	stdout := &boundedBuffer{limit: maxGitOutputBytes}
	stderr := &boundedBuffer{limit: maxGitStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if s.ctx.Err() != nil {
		return nil, 0, s.ctx.Err()
	}
	if stdout.overflow || stderr.overflow || errors.Is(err, ErrResourceLimit) {
		return nil, 0, ErrResourceLimit
	}
	if err == nil {
		return stdout.Bytes(), 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if exitError.ExitCode() == 1 {
			return stdout.Bytes(), 1, nil
		}
		return nil, exitError.ExitCode(), fmt.Errorf("%w: %s (exit %d)", ErrGitCommand, args[0], exitError.ExitCode())
	}
	return nil, 0, fmt.Errorf("%w: %s", ErrGitCommand, args[0])
}

func sanitizedEnvironment() []string {
	keep := make(map[string]string)
	for _, variable := range os.Environ() {
		key, value, found := strings.Cut(variable, "=")
		if !found || strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		upper := strings.ToUpper(key)
		if upper == "PATH" || upper == "SYSTEMROOT" || upper == "WINDIR" || upper == "TEMP" || upper == "TMP" {
			keep[key] = value
		}
	}
	environment := make([]string, 0, len(keep)+7)
	for key, value := range keep {
		environment = append(environment, key+"="+value)
	}
	environment = append(environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+nullDevice(),
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
	)
	sort.Strings(environment)
	return environment
}

func nullDevice() string {
	if os.PathSeparator == '\\' {
		return "NUL"
	}
	return os.DevNull
}

func deniedPath(pathPolicy *policy.Policy, path string, isDirectory bool) (bool, error) {
	decision, err := pathPolicy.CheckPath(path, isDirectory)
	if err != nil {
		return false, err
	}
	return decision.Denied, nil
}

func deniedDirectory(pathPolicy *policy.Policy, path string) (bool, error) {
	denied, err := deniedPath(pathPolicy, path, true)
	if err != nil || denied {
		return denied, err
	}
	return deniedPath(pathPolicy, path+"/__mce4d_inventory_boundary__", true)
}

func hasGitMarker(directory string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	// Bare repositories have no .git marker; their root metadata is enough to
	// identify the boundary without opening HEAD/config or traversing objects.
	for _, name := range []string{"HEAD", "config", "objects", "refs"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

func mappedFile(path string, origin Origin, state FileState, gitMode uint32, size int64, sparse bool, pathPolicy *policy.Policy) File {
	return File{
		Path:               path,
		Origin:             origin,
		State:              state,
		Kind:               "regular_file",
		GitMode:            gitMode,
		SizeBytes:          size,
		Sparse:             sparse,
		Language:           languageForPath(path),
		TextSearchEligible: pathPolicy.AllowsText(path),
		GeneratedCandidate: generatedCandidate(path),
	}
}

func languageForPath(path string) string {
	base := strings.ToLower(filepath.Base(path))
	extension := strings.ToLower(filepath.Ext(base))
	switch extension {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".sql":
		return "sql"
	case ".md", ".txt":
		return "text"
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".mod", ".sum", ".work", ".xml", ".proto", ".tf":
		return "configuration"
	default:
		return "unknown"
	}
}

func generatedCandidate(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(base, "_generated.go") || strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, ".gen.go") || strings.HasSuffix(base, ".generated.ts") || strings.HasSuffix(base, ".generated.js") {
		return true
	}
	for _, segment := range strings.Split(strings.ToLower(path), "/") {
		if segment == "generated" || segment == "gen" {
			return true
		}
	}
	return false
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if os.PathSeparator == '\\' {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func membershipItems(snapshot Snapshot) map[string]string {
	items := make(map[string]string, len(snapshot.Files)+len(snapshot.Boundaries))
	for _, file := range snapshot.Files {
		items[file.Path] = string(file.Origin) + "\x00" + string(file.State) + "\x00" + strconv.FormatUint(uint64(file.GitMode), 10)
	}
	for _, boundary := range snapshot.Boundaries {
		if boundary.Path != "" {
			if file, exists := items[boundary.Path]; exists {
				items[boundary.Path] = file + "\x00boundary\x00" + string(boundary.Reason)
			} else {
				items[boundary.Path] = "boundary\x00" + string(boundary.Reason)
			}
		}
	}
	return items
}

func membershipDigest(snapshot Snapshot) string {
	items := membershipItems(snapshot)
	paths := make([]string, 0, len(items))
	for path := range items {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hash := sha256.New()
	var length [8]byte
	for _, path := range paths {
		binary.LittleEndian.PutUint64(length[:], uint64(len(path)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(path))
		value := items[path]
		binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
