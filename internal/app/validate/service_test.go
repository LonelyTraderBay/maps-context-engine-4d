package validate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/inventory"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	fsruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

func TestValidateRequiresTwoStableContentPasses(t *testing.T) {
	manifest, vector, oldHash := appValidationFixture(t)
	newHash := model.SHA256([]byte("package demo\n// new\n"))
	workspace := &fakeWorkspace{
		inventories: []inventory.Snapshot{fakeInventory("one", "main.go")},
		hashes:      map[string][]model.Hash{"main.go": {newHash, oldHash, newHash, newHash}},
	}
	service := newFakeService(t, workspace, fakeScopeReaderFor(manifest, vector, vector))
	result, err := service.Validate(context.Background(), appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessChanged || len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "main.go" {
		t.Fatalf("stable changed content was not returned after retry: %+v", result)
	}
	if workspace.hashCalls != 4 {
		t.Fatalf("hash calls = %d; expected two complete passes after one unstable attempt", workspace.hashCalls)
	}
}

func TestValidateStopsAsUnknownAfterBoundedWorkspaceInstability(t *testing.T) {
	manifest, vector, _ := appValidationFixture(t)
	first := model.SHA256([]byte("package demo\n// old\n"))
	second := model.SHA256([]byte("package demo\n// new\n"))
	workspace := &fakeWorkspace{
		inventories: []inventory.Snapshot{fakeInventory("one", "main.go")},
		hashes:      map[string][]model.Hash{"main.go": {first, second, first, second, first, second}},
	}
	service := newFakeService(t, workspace, fakeScopeReaderFor(manifest, vector, vector, vector, vector, vector, vector))
	result, err := service.Validate(context.Background(), appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessUnknown || result.EditPreconditionValid || result.ReasonCode != "WORKSPACE_CHANGED_DURING_VALIDATION" {
		t.Fatalf("continually changing workspace was not blocked as unknown: %+v", result)
	}
	if workspace.hashCalls != 6 {
		t.Fatalf("hash calls = %d; stability retries were not bounded to two", workspace.hashCalls)
	}
}

func TestValidateRechecksScopeVectorAndInvalidatesRecordEpoch(t *testing.T) {
	manifest, vector, oldHash := appValidationFixture(t)
	currentVector := vector
	currentVector.Members = append([]model.ScopeMember(nil), vector.Members...)
	currentVector.Members[0].RecordEpoch++
	setAppVectorDigest(t, &currentVector)
	workspace := &fakeWorkspace{
		inventories: []inventory.Snapshot{fakeInventory("one", "main.go")},
		hashes:      map[string][]model.Hash{"main.go": {oldHash, oldHash, oldHash, oldHash}},
	}
	service := newFakeService(t, workspace, fakeScopeReaderFor(manifest, vector, currentVector, currentVector, currentVector))
	result, err := service.Validate(context.Background(), appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessChanged || !result.RecordSetChanged || result.EditPreconditionValid {
		t.Fatalf("stable record revision drift was not invalidated: %+v", result)
	}
	if workspace.hashCalls != 4 {
		t.Fatalf("scope-vector drift during capture was not retried once: hash calls=%d", workspace.hashCalls)
	}
}

func TestValidateReturnsUnknownWhenInventoryIsPartial(t *testing.T) {
	manifest, vector, oldHash := appValidationFixture(t)
	observed := fakeInventory("partial", "main.go")
	observed.Coverage.State = inventory.CoveragePartial
	workspace := &fakeWorkspace{
		inventories: []inventory.Snapshot{observed},
		hashes:      map[string][]model.Hash{"main.go": {oldHash}},
	}
	service := newFakeService(t, workspace, fakeScopeReaderFor(manifest, vector))
	result, err := service.Validate(context.Background(), appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessUnknown || result.EditPreconditionValid || result.ReasonCode != "WORKSPACE_COVERAGE_INCOMPLETE" {
		t.Fatalf("partial inventory was treated as freshness proof: %+v", result)
	}
	if workspace.hashCalls != 0 {
		t.Fatalf("partial workspace should not be presented as fully checked; hash calls=%d", workspace.hashCalls)
	}
}

func TestValidateDeadlineReturnsUnknownWithoutStartingWorkspaceReads(t *testing.T) {
	manifest, vector, _ := appValidationFixture(t)
	workspace := &fakeWorkspace{inventories: []inventory.Snapshot{fakeInventory("one", "main.go")}, hashes: map[string][]model.Hash{}}
	scope := fakeScopeReaderFor(manifest, vector)
	service := newFakeService(t, workspace, scope)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := service.Validate(ctx, appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessUnknown || result.ReasonCode != "FRESHNESS_CANCELLED" || result.EditPreconditionValid {
		t.Fatalf("canceled validation was not explicit: %+v", result)
	}
	if workspace.scans != 0 || scope.reads != 0 {
		t.Fatalf("canceled request started I/O: scans=%d scope_reads=%d", workspace.scans, scope.reads)
	}
}

func TestLocalWorkspaceHashesSameSizeAssumeUnchangedAndFindsNewFile(t *testing.T) {
	rootPath, gitPath := newValidationRepository(t)
	oldBytes := "package demo\n// old\n"
	newBytes := "package demo\n// new\n"
	writeValidationFile(t, rootPath, "main.go", oldBytes)
	validationGit(t, rootPath, "add", "--", "main.go")
	validationGit(t, rootPath, "commit", "-m", "fixture")
	root, err := fsruntime.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	caseMode, err := root.CaseMode()
	if err != nil {
		t.Fatal(err)
	}
	pathPolicy, err := policy.New(nil, caseMode)
	if err != nil {
		t.Fatal(err)
	}
	localWorkspace, err := NewLocalWorkspace(root, inventory.Options{
		Root: rootPath, GitExecutable: gitPath, Policy: pathPolicy, CaseMode: caseMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, vector := manifestForWorkspace(t, oldBytes)
	service := newFakeService(t, localWorkspace, fakeScopeReaderFor(manifest, vector, vector, vector, vector, vector, vector))
	filePath := filepath.Join(rootPath, "main.go")
	fixedTime := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filePath, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	writeValidationFile(t, rootPath, "main.go", newBytes)
	if err := os.Chtimes(filePath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	afterWrite, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != afterWrite.Size() || !before.ModTime().Equal(afterWrite.ModTime()) {
		t.Fatalf("fixture failed to preserve size/mtime: before=%d/%s after=%d/%s", before.Size(), before.ModTime(), afterWrite.Size(), afterWrite.ModTime())
	}
	validationGit(t, rootPath, "update-index", "--assume-unchanged", "--", "main.go")
	status := validationGit(t, rootPath, "status", "--porcelain", "--", "main.go")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("fixture did not establish an assume-unchanged working tree: %q", status)
	}
	request := Request{
		Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector,
		SourcePreconditions: []snapshot.SourcePrecondition{{Path: "main.go", ContentHash: model.SHA256([]byte(oldBytes))}},
	}
	result, err := service.Validate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessChanged || result.EditPreconditionValid || len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "main.go" {
		t.Fatalf("same-size assume-unchanged bytes were not detected: %+v", result)
	}

	writeValidationFile(t, rootPath, "new.go", "package demo\nfunc Added() {}\n")
	result, err = service.Validate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessChanged || !result.MembershipChanged || len(result.ChangedPaths) != 2 {
		t.Fatalf("new untracked member was not detected alongside source drift: %+v", result)
	}
}

func TestValidateRetriesWhenSameBytesAreReplacedByDifferentFileIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("verified Windows directory handles prevent atomic file replacement in this fixture")
	}
	rootPath, gitPath := newValidationRepository(t)
	contents := "package demo\n// old\n"
	writeValidationFile(t, rootPath, "main.go", contents)
	validationGit(t, rootPath, "add", "--", "main.go")
	validationGit(t, rootPath, "commit", "-m", "fixture")
	root, err := fsruntime.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	caseMode, err := root.CaseMode()
	if err != nil {
		t.Fatal(err)
	}
	pathPolicy, err := policy.New(nil, caseMode)
	if err != nil {
		t.Fatal(err)
	}
	localWorkspace, err := NewLocalWorkspace(root, inventory.Options{
		Root: rootPath, GitExecutable: gitPath, Policy: pathPolicy, CaseMode: caseMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, vector := manifestForWorkspace(t, contents)
	rotating := &replaceAfterFirstCapture{Workspace: localWorkspace, path: filepath.Join(rootPath, "main.go"), contents: []byte(contents)}
	service := newFakeService(t, rotating, fakeScopeReaderFor(manifest, vector, vector, vector, vector))
	result, err := service.Validate(context.Background(), appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessObservedCurrent || !result.EditPreconditionValid {
		t.Fatalf("replacement with identical bytes did not settle as current: %+v; fixture error=%v", result, rotating.failure)
	}
	if rotating.captures != 4 {
		t.Fatalf("replacement was not detected and retried: captures=%d", rotating.captures)
	}
}

func TestValidateMarksMovedTrackedSourceStale(t *testing.T) {
	rootPath, gitPath := newValidationRepository(t)
	contents := "package demo\n// old\n"
	writeValidationFile(t, rootPath, "main.go", contents)
	validationGit(t, rootPath, "add", "--", "main.go")
	validationGit(t, rootPath, "commit", "-m", "fixture")
	if err := os.Rename(filepath.Join(rootPath, "main.go"), filepath.Join(rootPath, "moved.go")); err != nil {
		t.Fatal(err)
	}
	manifest, vector := manifestForWorkspace(t, contents)
	workspace, root := openLocalValidationWorkspace(t, rootPath, gitPath)
	defer root.Close()
	service := newFakeService(t, workspace, fakeScopeReaderFor(manifest, vector, vector))
	result, err := service.Validate(context.Background(), appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessChanged || !result.MembershipChanged || len(result.ChangedPaths) != 2 || result.ChangedPaths[0] != "main.go" || result.ChangedPaths[1] != "moved.go" {
		t.Fatalf("moved source was not reported as stale: %+v", result)
	}
}

func TestValidateMarksDeletedTrackedSourceStale(t *testing.T) {
	rootPath, gitPath := newValidationRepository(t)
	contents := "package demo\n// old\n"
	writeValidationFile(t, rootPath, "main.go", contents)
	validationGit(t, rootPath, "add", "--", "main.go")
	validationGit(t, rootPath, "commit", "-m", "fixture")
	if err := os.Remove(filepath.Join(rootPath, "main.go")); err != nil {
		t.Fatal(err)
	}
	manifest, vector := manifestForWorkspace(t, contents)
	workspace, root := openLocalValidationWorkspace(t, rootPath, gitPath)
	defer root.Close()
	service := newFakeService(t, workspace, fakeScopeReaderFor(manifest, vector, vector))
	result, err := service.Validate(context.Background(), appValidationRequest(manifest, vector))
	if err != nil {
		t.Fatal(err)
	}
	if result.Freshness.Status != snapshot.FreshnessChanged || !result.MembershipChanged || len(result.ChangedPaths) != 1 || result.ChangedPaths[0] != "main.go" {
		t.Fatalf("deleted source was not reported as stale: %+v", result)
	}
}

type fakeWorkspace struct {
	inventories []inventory.Snapshot
	hashes      map[string][]model.Hash
	scans       int
	hashCalls   int
}

type replaceAfterFirstCapture struct {
	Workspace
	path     string
	contents []byte
	captures int
	failure  error
}

func (workspace *replaceAfterFirstCapture) Scan(ctx context.Context) (inventory.Snapshot, error) {
	observed, err := workspace.Workspace.Scan(ctx)
	if err != nil {
		workspace.failure = err
	}
	return observed, err
}

func (workspace *replaceAfterFirstCapture) CaptureFile(ctx context.Context, relative string) (FileCapture, error) {
	captured, err := workspace.Workspace.CaptureFile(ctx, relative)
	if err != nil {
		workspace.failure = err
		return FileCapture{}, err
	}
	workspace.captures++
	if workspace.captures == 1 {
		info, err := os.Stat(workspace.path)
		if err != nil {
			workspace.failure = err
			return FileCapture{}, err
		}
		backup := workspace.path + ".replacement"
		if err := os.Rename(workspace.path, backup); err != nil {
			workspace.failure = err
			return FileCapture{}, err
		}
		if err := os.WriteFile(workspace.path, workspace.contents, 0o600); err != nil {
			workspace.failure = err
			return FileCapture{}, err
		}
		if err := os.Chtimes(workspace.path, info.ModTime(), info.ModTime()); err != nil {
			workspace.failure = err
			return FileCapture{}, err
		}
		if err := os.Remove(backup); err != nil {
			workspace.failure = err
			return FileCapture{}, err
		}
	}
	return captured, nil
}

func (workspace *fakeWorkspace) Scan(context.Context) (inventory.Snapshot, error) {
	index := workspace.scans
	workspace.scans++
	if index >= len(workspace.inventories) {
		index = len(workspace.inventories) - 1
	}
	return workspace.inventories[index], nil
}

func (workspace *fakeWorkspace) CaptureFile(_ context.Context, path string) (FileCapture, error) {
	values := workspace.hashes[path]
	if len(values) == 0 {
		return FileCapture{}, os.ErrNotExist
	}
	index := workspace.hashCalls
	workspace.hashCalls++
	if index >= len(values) {
		index = len(values) - 1
	}
	return FileCapture{ContentHash: values[index], ByteLength: int64(len("package demo\n// old\n"))}, nil
}

type fakeScopeReader struct {
	observations []snapshot.ScopeObservation
	reads        int
}

func (reader *fakeScopeReader) ReadScopeObservation(context.Context) (snapshot.ScopeObservation, error) {
	index := reader.reads
	reader.reads++
	if index >= len(reader.observations) {
		index = len(reader.observations) - 1
	}
	return reader.observations[index], nil
}

func fakeScopeReaderFor(manifest model.CaptureManifest, vectors ...model.ScopeVector) *fakeScopeReader {
	observations := make([]snapshot.ScopeObservation, len(vectors))
	for index, vector := range vectors {
		observations[index] = snapshot.ScopeObservation{
			Vector: vector, ProviderDigest: manifest.ProviderDigest,
			DependencyInputDigests: []model.Hash{}, Complete: true,
		}
	}
	return &fakeScopeReader{observations: observations}
}

func newFakeService(t *testing.T, workspace Workspace, scope ScopeReader) *Service {
	t.Helper()
	service, err := NewService(workspace, scope)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func appValidationRequest(manifest model.CaptureManifest, vector model.ScopeVector) Request {
	return Request{Expected: manifest, ExpectedComplete: true, ExpectedScopeVector: vector}
}

func fakeInventory(digest string, names ...string) inventory.Snapshot {
	files := make([]inventory.File, 0, len(names))
	for _, name := range names {
		files = append(files, inventory.File{
			Path: name, Origin: inventory.OriginTracked, State: inventory.FilePresent,
			Kind: "text", TextSearchEligible: true,
		})
	}
	return inventory.Snapshot{
		MembershipDigest: digest, Files: files, Boundaries: []inventory.Boundary{},
		Coverage: inventory.Coverage{State: inventory.CoverageComplete, CaseSemanticsKnown: true},
	}
}

func appValidationFixture(t *testing.T) (model.CaptureManifest, model.ScopeVector, model.Hash) {
	t.Helper()
	content := []byte("package demo\n// old\n")
	manifest, vector := manifestForWorkspace(t, string(content))
	return manifest, vector, model.SHA256(content)
}

func manifestForWorkspace(t *testing.T, content string) (model.CaptureManifest, model.ScopeVector) {
	t.Helper()
	worktreeID, _ := model.ParseUUID("11111111-1111-4111-8111-111111111111")
	profileID := model.SHA256([]byte("profile"))
	policyDigest := model.SHA256([]byte("policy"))
	providerDigest := model.SHA256([]byte("provider"))
	captured, err := snapshot.Build(snapshot.Input{
		SchemaVersion: 1, WorktreeID: worktreeID, ProfileID: profileID,
		PolicyDigest: policyDigest, ProviderDigest: providerDigest,
		DependencyInputDigests: []model.Hash{},
		Files:                  []snapshot.FileInput{{Path: "main.go", Status: snapshot.FileCaptured, Content: []byte(content)}},
		Git:                    snapshot.GitState{HeadState: snapshot.HeadUnborn}, Scope: snapshot.ScopeComplete,
	})
	if err != nil {
		t.Fatal(err)
	}
	generationID, _ := model.ParseUUID("22222222-2222-4222-8222-222222222222")
	indexIncarnation, _ := model.ParseUUID("44444444-4444-4444-8444-444444444444")
	recordsIncarnation, _ := model.ParseUUID("55555555-5555-4555-8555-555555555555")
	vector := model.ScopeVector{
		Kind: "single", Members: []model.ScopeMember{{
			WorktreeID: worktreeID, ProfileID: profileID, GenerationID: generationID,
			SnapshotDigest: captured.Manifest.SnapshotDigest, PolicyDigest: policyDigest,
			PolicyEpoch: 1, BindingRevision: 1, RecordEpoch: 0,
			IndexIncarnation: indexIncarnation, RecordsIncarnation: recordsIncarnation,
		}}, GroupID: nil, GroupRevision: nil, CatalogIncarnation: indexIncarnation,
		UnavailableMembers: []model.UnavailableMember{},
	}
	setAppVectorDigest(t, &vector)
	return captured.Manifest, vector
}

func setAppVectorDigest(t *testing.T, vector *model.ScopeVector) {
	t.Helper()
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	vector.VectorDigest = digest
}

func newValidationRepository(t *testing.T) (string, string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("Git unavailable: %v", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	validationGit(t, root, "init", "--quiet", "--initial-branch=main")
	validationGit(t, root, "config", "user.name", "Validation Fixture")
	validationGit(t, root, "config", "user.email", "validation@example.invalid")
	return root, gitPath
}

func openLocalValidationWorkspace(t *testing.T, rootPath, gitPath string) (*LocalWorkspace, *fsruntime.Root) {
	t.Helper()
	root, err := fsruntime.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	caseMode, err := root.CaseMode()
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	pathPolicy, err := policy.New(nil, caseMode)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	workspace, err := NewLocalWorkspace(root, inventory.Options{
		Root: rootPath, GitExecutable: gitPath, Policy: pathPolicy, CaseMode: caseMode,
	})
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	return workspace, root
}

func validationGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q failed: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeValidationFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
