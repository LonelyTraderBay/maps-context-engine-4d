package snapshot

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestBuildHashesFullBytesAndCanonicalizesInputOrder(t *testing.T) {
	first := validInput()
	first.Files = []FileInput{
		{Path: "z.go", Status: FileCaptured, Content: []byte("same-size-a")},
		{Path: "a.go", Status: FileCaptured, Content: []byte("package a")},
	}
	left, err := Build(first)
	if err != nil {
		t.Fatal(err)
	}

	second := validInput()
	second.Files = []FileInput{
		{Path: "a.go", Status: FileCaptured, Content: []byte("package a")},
		{Path: "z.go", Status: FileCaptured, Content: []byte("same-size-b")},
	}
	right, err := Build(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.Manifest.SnapshotDigest == right.Manifest.SnapshotDigest {
		t.Fatal("same-length byte change did not change the full-content digest")
	}
	if bytes.Equal([]byte("same-size-a"), []byte("same-size-b")) {
		t.Fatal("fixture must change bytes while retaining its length")
	}
	if len(left.Manifest.Files) != 2 || left.Manifest.Files[0].Path != "a.go" || left.Manifest.Files[1].Path != "z.go" {
		t.Fatalf("manifest file order is not canonical: %+v", left.Manifest.Files)
	}
	if left.Manifest.Files[1].ContentHash == nil || *left.Manifest.Files[1].ContentHash != model.SHA256([]byte("same-size-a")) {
		t.Fatalf("manifest does not bind the exact raw file bytes: %+v", left.Manifest.Files[1])
	}
}

func TestBuildRejectsAmbiguousOrUnsafeCaptureInputs(t *testing.T) {
	tests := []struct {
		name  string
		files []FileInput
	}{
		{name: "duplicate path", files: []FileInput{{Path: "a.go", Status: FileCaptured, Content: []byte("a")}, {Path: "a.go", Status: FileCaptured, Content: []byte("b")}}},
		{name: "captured without bytes", files: []FileInput{{Path: "a.go", Status: FileCaptured}}},
		{name: "excluded bytes", files: []FileInput{{Path: "a.go", Status: FileExcluded, Content: []byte("must not persist")}}},
		{name: "traversal path", files: []FileInput{{Path: "../a.go", Status: FileCaptured, Content: []byte("a")}}},
		{name: "unknown status", files: []FileInput{{Path: "a.go", Status: "success", Content: []byte("a")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput()
			input.Files = test.files
			if _, err := Build(input); err == nil {
				t.Fatal("invalid capture input was accepted")
			}
		})
	}
}

func TestGitStateValidatesUnbornAndDetachedWithoutAssumingHEAD(t *testing.T) {
	input := validInput()
	input.Git = GitState{HeadState: HeadUnborn, Branch: "main"}
	if _, err := Build(input); err != nil {
		t.Fatalf("unborn repository with empty HEAD failed: %v", err)
	}
	input.Git = GitState{Head: strings.Repeat("a", 64), HeadState: HeadDetached}
	if _, err := Build(input); err != nil {
		t.Fatalf("detached repository failed: %v", err)
	}
	input.Git = GitState{HeadState: HeadPresent, Branch: "main"}
	if _, err := Build(input); err == nil {
		t.Fatal("present HEAD without an object ID was accepted")
	}
}

func validInput() Input {
	return Input{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: model.Hash(strings.Repeat("a", 64)), PolicyDigest: model.Hash(strings.Repeat("b", 64)),
		ProviderDigest: model.Hash(strings.Repeat("c", 64)), Files: []FileInput{},
		DependencyInputDigests: []model.Hash{}, Git: GitState{
			Head: strings.Repeat("d", 64), HeadState: HeadPresent, Branch: "main",
		}, Scope: ScopeComplete,
	}
}
