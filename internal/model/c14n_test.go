package model

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCanonicalEncodeUsesSortedUTF8KeysAndPreservesArrays(t *testing.T) {
	encoded, err := CanonicalEncode(map[string]any{"é": 1, "a": nil})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), "o2:s1:ans2:éi1;"; got != want {
		t.Fatalf("encoded=%q, want %q", got, want)
	}
	left, err := HashValue("test", []any{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := HashValue("test", []any{"second", "first"})
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("array order must remain part of the identity")
	}
}

func TestCanonicalNumberRejectsFractionExponentAndOverflow(t *testing.T) {
	for _, value := range []json.Number{"1.0", "1e0", "9007199254740992", "01", "+1"} {
		if _, err := CanonicalEncode(value); !errors.Is(err, ErrInvalidCanonicalValue) {
			t.Errorf("CanonicalEncode(%q) error=%v, want ErrInvalidCanonicalValue", value, err)
		}
	}
	encoded, err := CanonicalEncode(json.Number("-0"))
	if err != nil || string(encoded) != "i0;" {
		t.Fatalf("-0 encoding=%q error=%v", encoded, err)
	}
	if _, err := CanonicalEncode(string([]byte{0xff})); !errors.Is(err, ErrInvalidCanonicalValue) {
		t.Fatalf("invalid UTF-8 string error=%v", err)
	}
}

func TestAdmissionConfigDigestUsesSharedFieldsOnly(t *testing.T) {
	config := AdmissionConfig{
		SchemaVersion: 1, MaxWorktreeCacheBytes: 1000000,
		VolumeReserveBytes: 100000, VolumeWarningBytes: 200000,
	}
	digest, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 {
		t.Fatalf("digest length=%d", len(digest))
	}
	config.VolumeWarningBytes++
	changed, err := config.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest == changed {
		t.Fatal("shared admission change must change the digest")
	}
	config.VolumeReserveBytes = config.VolumeWarningBytes + 1
	if _, err := config.Digest(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("warning below reserve error=%v", err)
	}
}

func TestScopeVectorDigestNormalizesMembersAndChecksPinnedIdentity(t *testing.T) {
	member := ScopeMember{
		WorktreeID:     "11111111-1111-4111-8111-111111111111",
		ProfileID:      Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		GenerationID:   "22222222-2222-4222-8222-222222222222",
		SnapshotDigest: Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		PolicyDigest:   Hash("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		PolicyEpoch:    1, BindingRevision: 2, RecordEpoch: 0,
		IndexIncarnation:   "33333333-3333-4333-8333-333333333333",
		RecordsIncarnation: "44444444-4444-4444-8444-444444444444",
	}
	vector := ScopeVector{
		Kind: "single", Members: []ScopeMember{member},
		CatalogIncarnation: "55555555-5555-4555-8555-555555555555",
		UnavailableMembers: []UnavailableMember{},
	}
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	vector.VectorDigest = digest
	if err := vector.Validate(); err != nil {
		t.Fatal(err)
	}
	vector.Members[0].BindingRevision++
	if err := vector.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("changed member must invalidate digest, got %v", err)
	}
}

func TestScopeVectorRejectsMalformedGroupAndOutOfSchemaMembers(t *testing.T) {
	vector := ScopeVector{
		Kind: "federated", Members: []ScopeMember{}, UnavailableMembers: []UnavailableMember{},
		GroupID: ptr(UUID("not-a-uuid")), GroupRevision: ptr(int64(1)),
		CatalogIncarnation: "55555555-5555-4555-8555-555555555555",
	}
	digest, err := vector.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	vector.VectorDigest = digest
	if err := vector.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("malformed group ID error=%v", err)
	}

	vector.GroupID = ptr(UUID("22222222-2222-4222-8222-222222222222"))
	vector.Members = make([]ScopeMember, 33)
	if err := vector.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("member cap error=%v", err)
	}
}

func TestSourceRefUsesRawBytesAndUTF8Boundaries(t *testing.T) {
	source := []byte("aéz")
	ref := SourceRef{
		WorktreeID:   "11111111-1111-4111-8111-111111111111",
		ProfileID:    Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		GenerationID: "22222222-2222-4222-8222-222222222222",
		Path:         "src/main.go", ContentHash: SHA256(source),
		Range: ByteRange{StartByte: 1, EndByte: 3},
	}
	if err := ref.ValidateAgainst(source); err != nil {
		t.Fatal(err)
	}
	ref.Range = ByteRange{StartByte: 2, EndByte: 3}
	if err := ref.ValidateAgainst(source); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("mid-codepoint range error=%v", err)
	}
	ref.Range = ByteRange{StartByte: 0, EndByte: 3}
	if err := ref.ValidateAgainst([]byte("changed")); err == nil {
		t.Fatal("changed raw source must fail its content hash")
	}
}

func TestCaptureManifestDigestSortsFilesButRetainsDependencyMultiplicity(t *testing.T) {
	first := Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	second := Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	length := int64(3)
	manifest := CaptureManifest{
		SchemaVersion: 1,
		WorktreeID:    "11111111-1111-4111-8111-111111111111",
		ProfileID:     first, PolicyDigest: second, ProviderDigest: first,
		Files: []ManifestFile{{Path: "b.go", Status: "captured", ContentHash: &second, ByteLength: &length},
			{Path: "a.go", Status: "captured", ContentHash: &first, ByteLength: &length}},
		DependencyInputDigests: []Hash{second, first, first},
	}
	digest, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.SnapshotDigest = digest
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.Files[0], manifest.Files[1] = manifest.Files[1], manifest.Files[0]
	if err := manifest.Validate(); err != nil {
		t.Fatalf("file order must not change the normalized digest: %v", err)
	}
	manifest.DependencyInputDigests = append(manifest.DependencyInputDigests, first)
	if err := manifest.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("dependency multiplicity is part of the digest, got %v", err)
	}
}

func TestCaptureManifestRejectsSchemaDriftAndUnsafeFields(t *testing.T) {
	hash := Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	length := int64(1)
	manifest := CaptureManifest{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: hash, PolicyDigest: hash, ProviderDigest: hash,
		Files:                  []ManifestFile{{Path: "src/a.go", Status: "captured", ContentHash: &hash, ByteLength: &length}},
		DependencyInputDigests: []Hash{},
	}
	digest, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.SnapshotDigest = digest
	if err := manifest.Validate(); err != nil {
		t.Fatalf("valid manifest error=%v", err)
	}

	manifest.Files[0].Status = "invented"
	if err := manifest.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unknown status error=%v", err)
	}
	manifest.Files[0].Status = "captured"
	manifest.Files[0].ByteLength = ptr(int64(MaxSafeInteger + 1))
	if err := manifest.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unsafe byte length error=%v", err)
	}
	manifest.Files[0].ByteLength = &length
	manifest.Files = nil
	if err := manifest.Validate(); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("null files array error=%v", err)
	}
}

func ptr[T any](value T) *T {
	return &value
}
