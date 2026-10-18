package lsp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestPinnedGoplsRenamePreviewAndImmutableContinuation(t *testing.T) {
	goPath := os.Getenv("MCE4D_GO_BINARY")
	goplsPath := os.Getenv("MCE4D_GOPLS_BINARY")
	moduleCache := os.Getenv("MCE4D_GOMODCACHE")
	if goPath == "" || goplsPath == "" || moduleCache == "" {
		t.Skip("set MCE4D_GO_BINARY, MCE4D_GOPLS_BINARY, and MCE4D_GOMODCACHE for the pinned gopls integration proof")
	}
	goVersion := os.Getenv("MCE4D_GO_VERSION")
	if goVersion == "" {
		goVersion = "go1.27.2"
	}
	privateRoot := t.TempDir()
	workspaceRoot := filepath.Join(privateRoot, "workspace")
	cacheRoot := filepath.Join(privateRoot, "cache")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"go.mod":    []byte("module example.com/rename-fixture\n\ngo 1.27.0\n"),
		"rename.go": []byte("package fixture\n\nvar oldName = 1\nfunc Read() int { return oldName }\n"),
	}
	snapshotFiles := make([]SnapshotFile, 0, len(files))
	for relative, content := range files {
		if err := os.WriteFile(filepath.Join(workspaceRoot, relative), content, 0o600); err != nil {
			t.Fatal(err)
		}
		snapshotFiles = append(snapshotFiles, SnapshotFile{Path: relative, ContentHash: model.SHA256(content)})
	}
	sort.Slice(snapshotFiles, func(i, j int) bool { return snapshotFiles[i].Path < snapshotFiles[j].Path })
	identity := resultTestIdentity(t)
	goPin := ExecutablePin{Path: goPath, Version: goVersion, SHA256: fileHash(t, goPath)}
	goplsPin := ExecutablePin{Path: goplsPath, Version: "v0.24.0", SHA256: fileHash(t, goplsPath)}
	policy := ExecutionPolicy{
		AllowedRoots: []string{workspaceRoot}, DependencyRoots: []string{moduleCache},
		AllowedExecutables: []AllowedExecutable{{Path: goPath, SHA256: goPin.SHA256}, {Path: goplsPath, SHA256: goplsPin.SHA256}},
		MaxFileBytes:       2 * 1024 * 1024, MaxSourceBytes: 32 * 1024 * 1024 * 1024,
	}
	profile := GoProfile{
		GOOS: "windows", GOARCH: "amd64", TestsEnabled: true, ModuleRoots: []string{"."}, DependencyRoots: []string{moduleCache},
		ToolchainDigest: model.SHA256([]byte("integration toolchain")), ProviderDigest: model.SHA256([]byte("integration provider")),
		ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoVersion: goPin.Version, GoBinarySHA256: goPin.SHA256,
		Go: goPin, Gopls: goplsPin,
		ModuleCacheRoot: moduleCache, PrivateRoot: privateRoot, CacheRoot: cacheRoot,
		MaxFileBytes: policy.MaxFileBytes, MaxSourceBytes: policy.MaxSourceBytes,
	}
	profileID, err := effectiveGoProfileID(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID = profileID
	identity = identityWithProfile(t, identity, profile.ID)
	content := files["rename.go"]
	selectorStart := int64(strings.Index(string(content), "oldName"))
	source := model.SourceRef{
		WorktreeID: identity.Scope.Members[0].WorktreeID, ProfileID: profileID,
		GenerationID: identity.Scope.Members[0].GenerationID, Path: "rename.go",
		ContentHash: model.SHA256(content), Range: model.ByteRange{StartByte: selectorStart, EndByte: selectorStart + int64(len("oldName"))},
	}
	spool := &memorySpool{}
	results, err := NewResultStore(spool)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cursor.NewCursorCodec([]byte(strings.Repeat("p", 32)), 5*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewRenameProvider(results, codec)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := provider.Preview(context.Background(), RenameRequest{
		Identity: identity, Snapshot: WorkspaceSnapshot{Root: workspaceRoot, Complete: true, Scope: identity.Scope, Files: snapshotFiles},
		Profile: profile, Policy: policy, Source: source, NewName: "newName", PageSize: 1,
		StartupTimeout: 45 * time.Second, RenameTimeout: 45 * time.Second, ShutdownTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.ValidationRequired || preview.Applied || preview.SourceMutationCount != 0 || !preview.HasMore || len(preview.Edits) != 1 || preview.NextCursor == "" {
		t.Fatalf("unexpected first preview page: %+v", preview)
	}
	if !containsWarning(preview.Warnings, "RUNTIME_STRING_LOOKUP_REVIEW_REQUIRED") {
		t.Fatalf("rename review boundary warning is missing: %v", preview.Warnings)
	}
	var firstEdit RenameEdit
	if err := json.Unmarshal(preview.Edits[0], &firstEdit); err != nil || firstEdit.Path != "rename.go" || firstEdit.NewText != "newName" || firstEdit.Version != 1 {
		t.Fatalf("first preview edit=%+v err=%v", firstEdit, err)
	}
	continued, err := provider.Continue(context.Background(), identity, preview.ResultRef, preview.NextCursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if continued.HasMore || len(continued.Edits) != 1 || continued.Applied || continued.SourceMutationCount != 0 {
		t.Fatalf("unexpected continuation page: %+v", continued)
	}
	var secondEdit RenameEdit
	if err := json.Unmarshal(continued.Edits[0], &secondEdit); err != nil || secondEdit.Path != "rename.go" || secondEdit.NewText != "newName" || secondEdit.Version != 1 {
		t.Fatalf("continuation edit=%+v err=%v", secondEdit, err)
	}
	for relative, before := range files {
		after, err := os.ReadFile(filepath.Join(workspaceRoot, relative))
		if err != nil || model.SHA256(after) != model.SHA256(before) {
			t.Fatalf("snapshot source mutated: %s err=%v", relative, err)
		}
	}
}

func identityWithProfile(t *testing.T, identity ResultIdentity, profileID model.Hash) ResultIdentity {
	t.Helper()
	identity.Scope.Members = append([]model.ScopeMember(nil), identity.Scope.Members...)
	identity.Scope.Members[0].ProfileID = profileID
	digest, err := identity.Scope.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	identity.Scope.VectorDigest = digest
	if err := identity.Scope.Validate(); err != nil {
		t.Fatal(err)
	}
	return identity
}

func containsWarning(warnings []string, expected string) bool {
	for _, warning := range warnings {
		if warning == expected {
			return true
		}
	}
	return false
}

func fileHash(t *testing.T, path string) model.Hash {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return model.Hash(hex.EncodeToString(digest[:]))
}
