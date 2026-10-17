package model

import (
	"bytes"
	"errors"
	"sort"
	"testing"
)

func TestStreamingCaptureManifestMatchesCanonicalRecipeAndJSON(t *testing.T) {
	first := Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	second := Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	third := Hash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	length := int64(7)
	manifest := CaptureManifest{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: first, PolicyDigest: second, ProviderDigest: third,
		Files: []ManifestFile{
			{Path: "z/β.go", Status: "captured", ContentHash: &second, ByteLength: &length},
			{Path: "a.go", Status: "missing"},
		},
		DependencyInputDigests: []Hash{third, first, first},
	}
	got, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	independent, err := HashValue("capture-v1", map[string]any{
		"schema_version": int64(1), "worktree_id": manifest.WorktreeID, "profile_id": first,
		"policy_digest": second, "provider_digest": third,
		"files": []any{
			map[string]any{"path": "a.go", "status": "missing", "content_hash": nil, "byte_length": nil, "reason_code": nil},
			map[string]any{"path": "z/β.go", "status": "captured", "content_hash": second, "byte_length": length, "reason_code": nil},
		},
		"dependency_input_digests": []any{first, first, third},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != independent {
		t.Fatalf("stream digest %s differs from CT-04 digest %s", got, independent)
	}
	manifest.SnapshotDigest = got
	var expected bytes.Buffer
	if err := WriteCaptureManifestJSON1(&expected, manifest); err != nil {
		t.Fatal(err)
	}
	stream := CaptureManifestSliceSources(manifest)
	// Spool sources must already be normalized and sorted.
	sortManifestSources(&stream)
	var actual bytes.Buffer
	summary, err := WriteCaptureManifestJSON1Stream(&actual, stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Bytes(), expected.Bytes()) {
		t.Fatalf("streamed MCE-MANIFEST-JSON1 differs\n got: %s\nwant: %s", actual.Bytes(), expected.Bytes())
	}
	if summary.ByteLength != int64(actual.Len()) || summary.ArtifactSHA256 != SHA256(actual.Bytes()) || summary.SnapshotDigest != got || summary.FileCount != 2 || summary.DependencyCount != 3 {
		t.Fatalf("stream summary does not bind bytes and counts: %+v", summary)
	}
}

func TestStreamingCaptureManifestRejectsOrderingAndCountDrift(t *testing.T) {
	manifest := validStreamManifest(t)
	files := &sliceManifestFiles{values: []ManifestFile{
		{Path: "z.go", Status: "missing"}, {Path: "a.go", Status: "missing"},
	}}
	stream := manifest
	stream.Files = files
	if _, err := ComputeCaptureManifestSnapshotDigest(stream); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unsorted files error=%v, want ErrInvalidValue", err)
	}

	stream = validStreamManifest(t)
	stream.FileCount++
	if _, err := ComputeCaptureManifestSnapshotDigest(stream); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("declared file count error=%v, want ErrInvalidValue", err)
	}
}

func TestStreamingCaptureManifestRejectsSnapshotDigestAndPreservesDependencyMultiplicity(t *testing.T) {
	stream := validStreamManifest(t)
	var output bytes.Buffer
	if _, err := WriteCaptureManifestJSON1Stream(&output, stream); err != nil {
		t.Fatalf("valid snapshot digest failed: %v", err)
	}
	if output.Len() == 0 {
		t.Fatal("valid manifest emitted no bytes")
	}
	stream = validStreamManifest(t)
	stream.Header.SnapshotDigest = Hash("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	output.Reset()
	if _, err := WriteCaptureManifestJSON1Stream(&output, stream); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("wrong snapshot digest error=%v, want ErrInvalidValue", err)
	}
	if output.Len() != 0 {
		t.Fatalf("writer emitted bytes before digest verification: %d", output.Len())
	}
	stream = validStreamManifest(t)
	stream.Dependencies = &sliceManifestHashes{values: []Hash{Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}
	stream.DependencyCount = 2
	if _, err := ComputeCaptureManifestSnapshotDigest(stream); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unsorted dependencies error=%v, want ErrInvalidValue", err)
	}
}

func validStreamManifest(t *testing.T) CaptureManifestStream {
	t.Helper()
	manifest := CaptureManifest{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID:              Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		PolicyDigest:           Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		ProviderDigest:         Hash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		Files:                  []ManifestFile{{Path: "a.go", Status: "missing"}},
		DependencyInputDigests: []Hash{},
	}
	digest, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.SnapshotDigest = digest
	return CaptureManifestSliceSources(manifest)
}

func sortManifestSources(stream *CaptureManifestStream) {
	files := append([]ManifestFile(nil), stream.Files.(*sliceManifestFiles).values...)
	sort.Slice(files, func(i, j int) bool { return bytes.Compare([]byte(files[i].Path), []byte(files[j].Path)) < 0 })
	dependencies := append([]Hash(nil), stream.Dependencies.(*sliceManifestHashes).values...)
	sort.Slice(dependencies, func(i, j int) bool { return bytes.Compare([]byte(dependencies[i]), []byte(dependencies[j])) < 0 })
	stream.Files = &sliceManifestFiles{values: files}
	stream.Dependencies = &sliceManifestHashes{values: dependencies}
}
