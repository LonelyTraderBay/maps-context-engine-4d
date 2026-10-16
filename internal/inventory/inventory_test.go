package inventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
)

func TestScanHonorsGitIgnoreAndSecurityBoundaries(t *testing.T) {
	root, gitPath := newRepository(t)
	writeFile(t, root, ".gitignore", "ignored.txt\nignored-dir/\n")
	writeFile(t, root, "main.go", "source-secret-sentinel")
	writeFile(t, root, "vendor/dependency.go", "vendored source")
	writeFile(t, root, "generated/zz_generated.go", "generated source")
	gitCommand(t, root, "add", "--", ".gitignore", "main.go", "vendor/dependency.go", "generated/zz_generated.go")
	gitCommand(t, root, "commit", "-m", "fixture")
	writeFile(t, root, "new.go", "new source")
	writeFile(t, root, "ignored.txt", "ignored text")
	writeFile(t, root, "ignored-dir/hidden.go", "ignored source")
	writeFile(t, root, "vendor/new.go", "vendored untracked")
	writeFile(t, root, ".env", "credential-shaped fixture")
	writeFile(t, root, ".git/info/exclude", "info-excluded.go\n")
	writeFile(t, root, "info-excluded.go", "repository info exclude")
	indexBefore := sha256.Sum256(mustRead(t, filepath.Join(root, ".git", "index")))

	snapshot := scan(t, root, gitPath)
	indexAfter := sha256.Sum256(mustRead(t, filepath.Join(root, ".git", "index")))
	if indexBefore != indexAfter {
		t.Fatal("read-only inventory mutated the Git index")
	}
	pathsInMap := filePaths(snapshot)
	for _, path := range []string{".gitignore", "main.go", "generated/zz_generated.go", "new.go"} {
		if !slices.Contains(pathsInMap, path) {
			t.Errorf("file map omitted permitted path %q: %v", path, pathsInMap)
		}
	}
	for _, path := range []string{"vendor/dependency.go", "vendor/new.go", ".env", "ignored.txt", "ignored-dir/hidden.go", "info-excluded.go"} {
		if slices.Contains(pathsInMap, path) {
			t.Errorf("file map exposed excluded path %q", path)
		}
	}
	if snapshot.Coverage.Tracked != 4 || snapshot.Coverage.Untracked != 1 || snapshot.Coverage.Ignored != 3 {
		t.Fatalf("unexpected inventory coverage: %+v", snapshot.Coverage)
	}
	if snapshot.Coverage.ExcludedByPolicy != 3 {
		t.Fatalf("security policy exclusions were not counted at their boundary: %+v", snapshot.Coverage)
	}
	generated := fileByPath(t, snapshot, "generated/zz_generated.go")
	if !generated.GeneratedCandidate || generated.Language != "go" || !generated.TextSearchEligible {
		t.Fatalf("generated candidate metadata is incomplete: %+v", generated)
	}
	serialized, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "source-secret-sentinel") {
		t.Fatal("inventory serialized source bytes")
	}
}

func TestScanDisablesHostGlobalIgnoreConfigAndReportsEffectiveMode(t *testing.T) {
	root, gitPath := newRepository(t)
	globalIgnore := filepath.Join(t.TempDir(), "global-ignore")
	globalConfig := filepath.Join(t.TempDir(), "global-config")
	if err := os.WriteFile(globalIgnore, []byte("global-only.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "[core]\n\texcludesFile = " + filepath.ToSlash(globalIgnore) + "\n"
	if err := os.WriteFile(globalConfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	writeFile(t, root, "global-only.go", "included because the host global ignore is disabled")

	snapshot := scan(t, root, gitPath)
	if !slices.Contains(filePaths(snapshot), "global-only.go") {
		t.Fatal("host-global ignore configuration changed the pinned inventory")
	}
	if !strings.Contains(snapshot.GitIgnoreMode, "global/system and core.excludesFile disabled") {
		t.Fatalf("effective Git ignore mode was not reported: %q", snapshot.GitIgnoreMode)
	}
}

func TestScanRefreshesMembershipAndRepresentsMoveAsDeleteAndAdd(t *testing.T) {
	root, gitPath := newRepository(t)
	writeFile(t, root, "old.go", "old")
	gitCommand(t, root, "add", "--", "old.go")
	gitCommand(t, root, "commit", "-m", "fixture")
	first := scan(t, root, gitPath)

	writeFile(t, root, "added.go", "added after the first inventory")
	second := scan(t, root, gitPath)
	change := CompareMembership(first, second)
	if !slices.Equal(change.Added, []string{"added.go"}) || len(change.Removed) != 0 {
		t.Fatalf("new untracked membership was not observed: %+v", change)
	}
	if first.MembershipDigest == second.MembershipDigest {
		t.Fatal("membership digest did not change after a file was added")
	}

	gitCommand(t, root, "mv", "--", "old.go", "renamed.go")
	third := scan(t, root, gitPath)
	change = CompareMembership(second, third)
	if !slices.Equal(change.Added, []string{"renamed.go"}) || !slices.Equal(change.Removed, []string{"old.go"}) {
		t.Fatalf("move must be represented without fuzzy identity reuse: %+v", change)
	}

	if err := os.Remove(filepath.Join(root, "renamed.go")); err != nil {
		t.Fatal(err)
	}
	fourth := scan(t, root, gitPath)
	if fileByPath(t, fourth, "renamed.go").State != FileMissing {
		t.Fatalf("deleted tracked file was not retained as missing metadata: %+v", fileByPath(t, fourth, "renamed.go"))
	}
}

func TestScanHandlesUnbornBranchAndDetachedHead(t *testing.T) {
	root, gitPath := newRepository(t)
	unborn := scan(t, root, gitPath)
	if unborn.HeadState != HeadUnborn || unborn.Head != "" || unborn.Branch != "main" {
		t.Fatalf("unborn repository state is incorrect: %+v", unborn)
	}
	writeFile(t, root, "loose.go", "loose")
	if !slices.Contains(filePaths(scan(t, root, gitPath)), "loose.go") {
		t.Fatal("unborn repository inventory did not include allowed file")
	}
	gitCommand(t, root, "add", "--", "loose.go")
	gitCommand(t, root, "commit", "-m", "fixture")
	gitCommand(t, root, "switch", "-c", "other")
	branch := scan(t, root, gitPath)
	if branch.HeadState != HeadPresent || branch.Branch != "other" || branch.Head == "" {
		t.Fatalf("branch switch was not observed: %+v", branch)
	}
	gitCommand(t, root, "checkout", "--detach", "HEAD")
	detached := scan(t, root, gitPath)
	if detached.HeadState != HeadDetached || detached.Branch != "" || detached.Head == "" {
		t.Fatalf("detached HEAD was not reported explicitly: %+v", detached)
	}
}

func TestScanStopsAtNestedRepositoriesAndSubmodules(t *testing.T) {
	root, gitPath := newRepository(t)
	writeFile(t, root, "root.go", "root")
	gitCommand(t, root, "add", "--", "root.go")
	gitCommand(t, root, "commit", "-m", "fixture")
	writeFile(t, root, "nested/secret.go", "nested")
	gitCommand(t, root, "-C", filepath.Join(root, "nested"), "init", "--quiet", "--initial-branch=main")
	gitCommand(t, root, "-C", filepath.Join(root, "nested"), "config", "user.name", "Inventory Fixture")
	gitCommand(t, root, "-C", filepath.Join(root, "nested"), "config", "user.email", "inventory@example.invalid")
	gitCommand(t, root, "-C", filepath.Join(root, "nested"), "add", "--", "secret.go")
	gitCommand(t, root, "-C", filepath.Join(root, "nested"), "commit", "-m", "nested fixture")

	head := strings.TrimSpace(gitCommand(t, root, "rev-parse", "HEAD"))
	if err := os.MkdirAll(filepath.Join(root, "submodule"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, root, "-C", filepath.Join(root, "submodule"), "init", "--quiet", "--initial-branch=main")
	gitCommand(t, root, "update-index", "--add", "--cacheinfo", "160000,"+head+",submodule")
	gitCommand(t, root, "init", "--bare", "--quiet", filepath.Join(root, "bare-repo"))

	snapshot := scan(t, root, gitPath)
	if slices.Contains(filePaths(snapshot), "nested/secret.go") || slices.Contains(filePaths(snapshot), "submodule/secret.go") || slices.Contains(filePaths(snapshot), "bare-repo/HEAD") {
		t.Fatal("inventory traversed into a nested repository")
	}
	if !hasBoundary(snapshot, "nested", BoundaryNestedGit) || !hasBoundary(snapshot, "bare-repo", BoundaryNestedGit) || !hasBoundary(snapshot, "submodule", BoundarySubmodule) {
		t.Fatalf("nested repository boundaries were not represented: %+v", snapshot.Boundaries)
	}
}

func TestScanReportsUnmergedIndexState(t *testing.T) {
	root, gitPath := newRepository(t)
	writeFile(t, root, "conflict.go", "base\n")
	gitCommand(t, root, "add", "--", "conflict.go")
	gitCommand(t, root, "commit", "-m", "base")
	gitCommand(t, root, "switch", "-c", "topic")
	writeFile(t, root, "conflict.go", "topic\n")
	gitCommand(t, root, "commit", "-am", "topic")
	gitCommand(t, root, "switch", "main")
	writeFile(t, root, "conflict.go", "main\n")
	gitCommand(t, root, "commit", "-am", "main")
	command := exec.Command(gitPath, "-C", root, "merge", "topic")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("expected fixture merge conflict, got success: %s", output)
	}

	snapshot := scan(t, root, gitPath)
	if got := fileByPath(t, snapshot, "conflict.go").State; got != FileUnmerged {
		t.Fatalf("unmerged path state = %q, want %q", got, FileUnmerged)
	}
	if snapshot.Coverage.Unmerged != 1 || snapshot.Coverage.State != CoveragePartial {
		t.Fatalf("unmerged coverage was not explicit: %+v", snapshot.Coverage)
	}
}

func TestScanReportsSparseDirectoryWithoutWalkingIt(t *testing.T) {
	root, gitPath := newRepository(t)
	writeFile(t, root, "keep/visible.go", "visible")
	writeFile(t, root, "omit/hidden.go", "sparse")
	gitCommand(t, root, "add", "--", "keep/visible.go", "omit/hidden.go")
	gitCommand(t, root, "commit", "-m", "fixture")
	gitCommand(t, root, "sparse-checkout", "init", "--cone")
	gitCommand(t, root, "sparse-checkout", "set", "keep")

	snapshot := scan(t, root, gitPath)
	if got := fileByPath(t, snapshot, "omit/hidden.go").State; got != FileSparseOmitted {
		t.Fatalf("sparse omitted path state = %q, want %q", got, FileSparseOmitted)
	}
	if snapshot.Coverage.Sparse == 0 || snapshot.Coverage.State != CoveragePartial || !hasBoundary(snapshot, "omit/hidden.go", BoundarySparse) {
		t.Fatalf("sparse boundary was not visible in coverage: %+v %+v", snapshot.Coverage, snapshot.Boundaries)
	}
}

func TestScanDoesNotFollowSymlinkTargets(t *testing.T) {
	root, gitPath := newRepository(t)
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("outside-target-sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.go")); err != nil {
		t.Skipf("symlink fixture is unavailable: %v", err)
	}
	snapshot := scan(t, root, gitPath)
	if slices.Contains(filePaths(snapshot), "link.go") || !hasBoundary(snapshot, "link.go", BoundarySymlink) {
		t.Fatalf("symlink boundary was not explicit: %+v %+v", snapshot.Files, snapshot.Boundaries)
	}
	serialized, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "outside-target-sentinel") {
		t.Fatal("inventory read or serialized symlink target bytes")
	}
}

func TestScanTreatsIndexSymlinkModeAsBoundaryEvenWhenWorktreeFileIsAbsent(t *testing.T) {
	root, gitPath := newRepository(t)
	command := exec.Command("git", "-C", root, "hash-object", "-w", "--stdin")
	command.Stdin = bytes.NewBufferString("target.go")
	objectID, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	gitCommand(t, root, "update-index", "--add", "--cacheinfo", "120000,"+strings.TrimSpace(string(objectID))+",tracked-link.go")

	snapshot := scan(t, root, gitPath)
	if slices.Contains(filePaths(snapshot), "tracked-link.go") || !hasBoundary(snapshot, "tracked-link.go", BoundarySymlink) {
		t.Fatalf("index symlink was mapped as source file: %+v %+v", snapshot.Files, snapshot.Boundaries)
	}
	if snapshot.Coverage.Symlinks != 1 || snapshot.Coverage.Missing != 1 {
		t.Fatalf("tracked symlink coverage was not explicit: %+v", snapshot.Coverage)
	}
}

func TestScanUsesNULDelimitedPaths(t *testing.T) {
	root, gitPath := newRepository(t)
	writeFile(t, root, "-leading.go", "leading")
	if runtime.GOOS != "windows" {
		writeFile(t, root, "line\nbreak.go", "newline")
		gitCommand(t, root, "add", "--", "line\nbreak.go")
		gitCommand(t, root, "commit", "-m", "newline path fixture")
		writeFile(t, root, ":(glob)*.go", "literal pathspec-like name")
	}
	snapshot := scan(t, root, gitPath)
	if !slices.Contains(filePaths(snapshot), "-leading.go") {
		t.Fatal("path beginning with dash was not inventoried")
	}
	if runtime.GOOS != "windows" {
		pathsInMap := filePaths(snapshot)
		if snapshot.Coverage.UnsupportedPath != 1 || slices.Contains(pathsInMap, "line\nbreak.go") {
			t.Fatalf("control-character path was not retained as unsupported coverage: %+v", snapshot.Coverage)
		}
		if slices.Contains(pathsInMap, "line") || slices.Contains(pathsInMap, "break.go") {
			t.Fatalf("newline path leaked as split path identities: %v", pathsInMap)
		}
	}
	if runtime.GOOS != "windows" && !slices.Contains(filePaths(snapshot), ":(glob)*.go") {
		t.Fatal("pathspec-like filename was not passed to check-ignore literally")
	}
}

func TestScanMarksNonUTF8UntrackedPathAsUnsupported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot contain arbitrary non-UTF-8 bytes")
	}
	root, gitPath := newRepository(t)
	path := string([]byte{0xff, '.', 'g', 'o'})
	if err := os.WriteFile(filepath.Join(root, path), []byte("unsupported path fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := scan(t, root, gitPath)
	if snapshot.Coverage.UnsupportedPathEncoding != 1 || snapshot.Coverage.State != CoveragePartial {
		t.Fatalf("non-UTF-8 coverage was silently dropped: %+v", snapshot.Coverage)
	}
	if len(snapshot.Files) != 0 {
		t.Fatalf("non-UTF-8 path leaked into file map: %+v", snapshot.Files)
	}
}

func TestScanRejectsCaseFoldCollisionWhenFilesystemIsInsensitive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows test filesystem cannot store case-only distinct names")
	}
	root, gitPath := newRepository(t)
	writeFile(t, root, "Upper.go", "upper")
	writeFile(t, root, "upper.go", "lower")
	gitCommand(t, root, "add", "--", "Upper.go", "upper.go")
	gitCommand(t, root, "commit", "-m", "fixture")
	options := testOptions(root, gitPath)
	options.CaseMode = paths.CaseInsensitive
	if _, err := Scan(context.Background(), options); !errors.Is(err, ErrCaseFoldCollision) {
		t.Fatalf("case-fold collision error = %v", err)
	}
}

func TestScanRejectsInvalidOptionsAndHonorsCancellation(t *testing.T) {
	root, gitPath := newRepository(t)
	if _, err := Scan(context.Background(), Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid options error = %v", err)
	}
	unknownCase := testOptions(root, gitPath)
	unknownCase.CaseMode = paths.CaseUnknown
	unknownSnapshot, err := Scan(context.Background(), unknownCase)
	if err != nil {
		t.Fatal(err)
	}
	if unknownSnapshot.Coverage.CaseSemanticsKnown || unknownSnapshot.Coverage.State != CoveragePartial {
		t.Fatalf("unknown filesystem case semantics were claimed complete: %+v", unknownSnapshot.Coverage)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, testOptions(root, gitPath)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan error = %v", err)
	}
}

func newRepository(t *testing.T) (string, string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("Git is unavailable: %v", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	gitCommand(t, root, "init", "--quiet", "--initial-branch=main")
	gitCommand(t, root, "config", "user.name", "Inventory Fixture")
	gitCommand(t, root, "config", "user.email", "inventory@example.invalid")
	return root, gitPath
}

func testOptions(root, gitPath string) Options {
	caseMode := paths.CaseSensitive
	if runtime.GOOS == "windows" {
		caseMode = paths.CaseInsensitive
	}
	pathPolicy, err := policy.New(nil, caseMode)
	if err != nil {
		panic(err)
	}
	return Options{Root: root, GitExecutable: gitPath, Policy: pathPolicy, CaseMode: caseMode}
}

func scan(t *testing.T, root, gitPath string) Snapshot {
	t.Helper()
	snapshot, err := Scan(context.Background(), testOptions(root, gitPath))
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	return snapshot
}

func gitCommand(t *testing.T, root string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	command := exec.Command("git", commandArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q failed: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, root, path, content string) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func filePaths(snapshot Snapshot) []string {
	pathsInMap := make([]string, 0, len(snapshot.Files))
	for _, file := range snapshot.Files {
		pathsInMap = append(pathsInMap, file.Path)
	}
	return pathsInMap
}

func fileByPath(t *testing.T, snapshot Snapshot, path string) File {
	t.Helper()
	for _, file := range snapshot.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("file map omitted %q: %+v", path, snapshot.Files)
	return File{}
}

func hasBoundary(snapshot Snapshot, path string, reason BoundaryReason) bool {
	for _, boundary := range snapshot.Boundaries {
		if boundary.Path == path && boundary.Reason == reason {
			return true
		}
	}
	return false
}
