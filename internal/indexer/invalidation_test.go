package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/inventory"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

func TestBuildSnapshotFromInventoryCapturesUntrackedPathsAndRejectsMembershipDrift(t *testing.T) {
	input := testManifestInput()
	observed := inventory.Snapshot{
		Head: strings.Repeat("d", 40), HeadState: inventory.HeadPresent, Branch: "main",
		Coverage:   inventory.Coverage{State: inventory.CoverageComplete, CaseSemanticsKnown: true},
		Files:      []inventory.File{{Path: "main.go", Origin: inventory.OriginTracked, State: inventory.FilePresent}},
		Boundaries: []inventory.Boundary{},
	}
	previous, err := BuildSnapshotFromInventory(InventorySnapshotInput{
		Manifest: input, Observed: observed,
		Captures: map[string]snapshot.FileInput{"main.go": {Path: "main.go", Status: snapshot.FileCaptured, Content: []byte("package main")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	withUntracked := observed
	withUntracked.Files = append(append([]inventory.File(nil), observed.Files...), inventory.File{
		Path: "new.go", Origin: inventory.OriginUntracked, State: inventory.FilePresent,
	})
	current, err := BuildSnapshotFromInventory(InventorySnapshotInput{
		Manifest: input, Observed: withUntracked,
		Captures: map[string]snapshot.FileInput{
			"main.go": {Path: "main.go", Status: snapshot.FileCaptured, Content: []byte("package main")},
			"new.go":  {Path: "new.go", Status: snapshot.FileCaptured, Content: []byte("package main\nfunc Added() {}")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := CompareSnapshots(&previous, &current)
	if err != nil {
		t.Fatal(err)
	}
	if current.Manifest.SnapshotDigest == previous.Manifest.SnapshotDigest || plan.Mode != InvalidationFullRebuild || len(plan.AddedPaths) != 1 || plan.AddedPaths[0] != "new.go" {
		t.Fatalf("new untracked membership was not bound and invalidated: snapshot=%+v plan=%+v", current.Manifest, plan)
	}

	incomplete, err := BuildSnapshotFromInventory(InventorySnapshotInput{Manifest: input, Observed: withUntracked, Captures: map[string]snapshot.FileInput{
		"main.go": {Path: "main.go", Status: snapshot.FileCaptured, Content: []byte("package main")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = CompareSnapshots(&current, &incomplete)
	if err != nil {
		t.Fatal(err)
	}
	if incomplete.Scope != snapshot.ScopePartial || incomplete.Manifest.Files[1].Status != snapshot.FileMetadataOnly || plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonScopeIncomplete) {
		t.Fatalf("capture gap was not retained as partial/full-rebuild: snapshot=%+v plan=%+v", incomplete, plan)
	}
	if _, err := BuildSnapshotFromInventory(InventorySnapshotInput{Manifest: input, Observed: observed, Captures: map[string]snapshot.FileInput{
		"unobserved.go": {Path: "unobserved.go", Status: snapshot.FileCaptured, Content: []byte("package main")},
	}}); err == nil {
		t.Fatal("capture outside inventory was silently accepted")
	}
}

func TestBuildSnapshotFromInventoryHashesAssumeUnchangedBytesWithSameMtime(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("Git is unavailable: %v", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	runGitFixture(t, root, "init", "--quiet", "--initial-branch=main")
	runGitFixture(t, root, "config", "user.name", "Snapshot Fixture")
	runGitFixture(t, root, "config", "user.email", "snapshot@example.invalid")
	filePath := filepath.Join(root, "main.go")
	beforeBytes := []byte("package x\nvar value = 1\n")
	afterBytes := []byte("package x\nvar value = 2\n")
	if len(beforeBytes) != len(afterBytes) {
		t.Fatal("mutation fixture must preserve file size")
	}
	if err := os.WriteFile(filePath, beforeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, root, "add", "--", "main.go")
	runGitFixture(t, root, "commit", "--quiet", "-m", "fixture")
	runGitFixture(t, root, "update-index", "--assume-unchanged", "--", "main.go")
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()
	previous := scanAndCaptureFixture(t, root, gitPath)
	if err := os.WriteFile(filePath, afterBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filePath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if status := runGitFixture(t, root, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("assume-unchanged fixture should appear clean to Git, got %q", status)
	}
	current := scanAndCaptureFixture(t, root, gitPath)
	if previous.Manifest.SnapshotDigest == current.Manifest.SnapshotDigest || previous.Manifest.Files[0].ContentHash == nil || current.Manifest.Files[0].ContentHash == nil || *previous.Manifest.Files[0].ContentHash == *current.Manifest.Files[0].ContentHash {
		t.Fatal("snapshot digest trusted Git status or same size/mtime instead of hashing file bytes")
	}
}

func TestCompareSnapshotsUsesFullContentAndDoesNotFuzzyReuseRenames(t *testing.T) {
	previous := testSnapshot(t, map[string][]byte{"old.go": []byte("package old")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	current := testSnapshot(t, map[string][]byte{"new.go": []byte("package old")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	plan, err := CompareSnapshots(&previous, &current)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonFileAdded) || !containsReason(plan, ReasonFileRemoved) {
		t.Fatalf("rename must invalidate as delete plus add: %+v", plan)
	}
	if len(plan.AddedPaths) != 1 || plan.AddedPaths[0] != "new.go" || len(plan.RemovedPaths) != 1 || plan.RemovedPaths[0] != "old.go" {
		t.Fatalf("rename paths were fuzzy-matched: %+v", plan)
	}

	oldBytes := []byte("package x // A")
	newBytes := []byte("package x // B")
	if len(oldBytes) != len(newBytes) {
		t.Fatal("same-size mutation fixture is invalid")
	}
	before := testSnapshot(t, map[string][]byte{"main.go": oldBytes}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	after := testSnapshot(t, map[string][]byte{"main.go": newBytes}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	plan, err = CompareSnapshots(&before, &after)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || len(plan.ChangedPaths) != 1 || plan.ChangedPaths[0] != "main.go" {
		t.Fatalf("same-size content mutation was not detected: %+v", plan)
	}
}

func TestCompareSnapshotsInvalidatesModuleConfigDependenciesAndGitIdentity(t *testing.T) {
	base := testSnapshot(t, map[string][]byte{"go.mod": []byte("module example.invalid/a\n"), "api.go": []byte("package a\ntype I interface{ A() }\n")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	config := testSnapshot(t, map[string][]byte{"go.mod": []byte("module example.invalid/b\n"), "api.go": []byte("package a\ntype I interface{ A() }\n")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	plan, err := CompareSnapshots(&base, &config)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonModuleConfigChange) {
		t.Fatalf("go.mod change must invalidate semantic consumers: %+v", plan)
	}

	previousAPI := testSnapshot(t, map[string][]byte{"api.go": []byte("package a\ntype I interface{ A() }\n")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	changedAPI := testSnapshot(t, map[string][]byte{"api.go": []byte("package a\ntype I interface{ A(); B() }\n")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	plan, err = CompareSnapshots(&previousAPI, &changedAPI)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonFileChanged) || len(plan.ChangedPaths) != 1 || plan.ChangedPaths[0] != "api.go" {
		t.Fatalf("exported interface change must rebuild consumers rather than retain stale edges: %+v", plan)
	}

	dependency := base
	dependency.Manifest.DependencyInputDigests = []model.Hash{model.Hash(strings.Repeat("e", 64))}
	dependency.Manifest.SnapshotDigest, err = dependency.Manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	plan, err = CompareSnapshots(&base, &dependency)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonDependencyChanged) {
		t.Fatalf("dependency input change must force full rebuild: %+v", plan)
	}

	branch := base
	branch.Git = snapshot.GitState{HeadState: snapshot.HeadUnborn, Branch: "main"}
	plan, err = CompareSnapshots(&base, &branch)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonRepositoryState) {
		t.Fatalf("HEAD/branch transition must be represented: %+v", plan)
	}
}

func TestCompareSnapshotsNeverReturnsNoopForIncompleteScope(t *testing.T) {
	complete := testSnapshot(t, map[string][]byte{"main.go": []byte("package main")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	partial := testSnapshot(t, map[string][]byte{"main.go": []byte("package main")}, snapshot.ScopePartial, "d", snapshot.HeadPresent)
	unknown := testSnapshot(t, map[string][]byte{"main.go": []byte("package main")}, snapshot.ScopeUnknown, "d", snapshot.HeadPresent)
	for _, current := range []snapshot.Snapshot{partial, unknown} {
		plan, err := CompareSnapshots(&complete, &current)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Mode != InvalidationFullRebuild || plan.InputScopeComplete || !containsReason(plan, ReasonScopeIncomplete) {
			t.Fatalf("incomplete input was treated as a no-op: %+v", plan)
		}
	}
}

func TestCompareSnapshotsNoopRequiresIdenticalCompleteIdentity(t *testing.T) {
	previous := testSnapshot(t, map[string][]byte{"main.go": []byte("package main")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	current := testSnapshot(t, map[string][]byte{"main.go": []byte("package main")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	plan, err := CompareSnapshots(&previous, &current)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationNoop || len(plan.Reasons) != 0 {
		t.Fatalf("identical complete snapshot should be a no-op: %+v", plan)
	}
	current.Manifest.ProfileID = model.Hash(strings.Repeat("f", 64))
	current.Manifest.SnapshotDigest, err = current.Manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	plan, err = CompareSnapshots(&previous, &current)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonProfileChanged) {
		t.Fatalf("profile identity change was treated as a no-op: %+v", plan)
	}
}

func TestCompareSnapshotsInvalidatesBranchSwitchWithIdenticalContent(t *testing.T) {
	previous := testSnapshot(t, map[string][]byte{"main.go": []byte("package main")}, snapshot.ScopeComplete, "d", snapshot.HeadPresent)
	current := previous
	current.Git = snapshot.GitState{Head: previous.Git.Head, HeadState: snapshot.HeadPresent, Branch: "other"}
	plan, err := CompareSnapshots(&previous, &current)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != InvalidationFullRebuild || !containsReason(plan, ReasonRepositoryState) || previous.Manifest.SnapshotDigest != current.Manifest.SnapshotDigest {
		t.Fatalf("branch switch with identical content was not retained as repository provenance: %+v", plan)
	}
}

func testSnapshot(t *testing.T, files map[string][]byte, scope snapshot.ScopeStatus, headSuffix string, headState snapshot.HeadState) snapshot.Snapshot {
	t.Helper()
	inputs := make([]snapshot.FileInput, 0, len(files))
	for path, content := range files {
		inputs = append(inputs, snapshot.FileInput{Path: path, Status: snapshot.FileCaptured, Content: content})
	}
	state := snapshot.GitState{Head: strings.Repeat(headSuffix, 64), HeadState: headState, Branch: "main"}
	if headState == snapshot.HeadUnborn {
		state.Head = ""
	} else if headState == snapshot.HeadDetached {
		state.Branch = ""
	}
	result, err := snapshot.Build(snapshot.Input{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: model.Hash(strings.Repeat("a", 64)), PolicyDigest: model.Hash(strings.Repeat("b", 64)),
		ProviderDigest: model.Hash(strings.Repeat("c", 64)), DependencyInputDigests: []model.Hash{},
		Files: inputs, Git: state, Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func containsReason(plan InvalidationPlan, want InvalidationReason) bool {
	for _, reason := range plan.Reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func testManifestInput() snapshot.Input {
	return snapshot.Input{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: model.Hash(strings.Repeat("a", 64)), PolicyDigest: model.Hash(strings.Repeat("b", 64)),
		ProviderDigest: model.Hash(strings.Repeat("c", 64)), DependencyInputDigests: []model.Hash{},
	}
}

func scanAndCaptureFixture(t *testing.T, root, gitPath string) snapshot.Snapshot {
	t.Helper()
	caseMode := paths.CaseSensitive
	if runtime.GOOS == "windows" {
		caseMode = paths.CaseInsensitive
	}
	pathPolicy, err := policy.New(nil, caseMode)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := inventory.Scan(context.Background(), inventory.Options{
		Root: root, GitExecutable: gitPath, Policy: pathPolicy, CaseMode: caseMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	captures := make(map[string]snapshot.FileInput, len(observed.Files))
	for _, file := range observed.Files {
		if file.State != inventory.FilePresent {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		captures[file.Path] = snapshot.FileInput{Path: file.Path, Status: snapshot.FileCaptured, Content: content}
	}
	result, err := BuildSnapshotFromInventory(InventorySnapshotInput{
		Manifest: testManifestInput(), Observed: observed, Captures: captures,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func runGitFixture(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q failed: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
