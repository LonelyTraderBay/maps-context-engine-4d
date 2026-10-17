package contracts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type testManifestLease struct {
	root       string
	reserved   int64
	released   bool
	reserveErr error
}

func (lease *testManifestLease) Root() string { return lease.root }
func (lease *testManifestLease) ReserveGrowth(_ context.Context, value int64) error {
	if lease.reserveErr != nil {
		return lease.reserveErr
	}
	if value < 0 {
		return errors.New("negative reservation")
	}
	lease.reserved += value
	return nil
}
func (lease *testManifestLease) Release() error { lease.released = true; return nil }

func TestNormalizeCaptureManifestJSON1CanonicalizesWithAdmittedSpools(t *testing.T) {
	manifest := testManifest(t)
	canonical := canonicalManifest(t, manifest)
	input := fmt.Sprintf(`{"worktree_id":%q,"files":[{"status":"captured","reason_code":null,"byte_length":7,"content_hash":%q,"path":"z/\u03b2.go"},{"reason_code":null,"content_hash":null,"byte_length":null,"status":"missing","path":"a.go"}],"schema_version":1,"provider_digest":%q,"dependency_input_digests":[%q,%q,%q],"snapshot_digest":%q,"policy_digest":%q,"profile_id":%q}`,
		manifest.WorktreeID, *manifest.Files[0].ContentHash,
		manifest.ProviderDigest, manifest.DependencyInputDigests[0], manifest.DependencyInputDigests[1], manifest.DependencyInputDigests[2],
		manifest.SnapshotDigest, manifest.PolicyDigest, manifest.ProfileID)
	lease := &testManifestLease{root: t.TempDir()}
	var output bytes.Buffer
	summary, err := NormalizeCaptureManifestJSON1(context.Background(), strings.NewReader(input), &output, lease, CaptureManifestStreamOptions{RequireCanonical: false})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), canonical) {
		t.Fatalf("normalized bytes differ\n got: %s\nwant: %s", output.Bytes(), canonical)
	}
	if summary.ByteLength != int64(len(canonical)) || summary.ArtifactSHA256 != hashManifestBytes(canonical) || summary.FileCount != 2 || summary.DependencyCount != 3 {
		t.Fatalf("stream summary does not bind normalized artifact: %+v", summary)
	}
	if lease.reserved <= int64(len(input)) || !lease.released {
		t.Fatalf("spool growth was not admitted/released: reserved=%d released=%v", lease.reserved, lease.released)
	}
}

func TestNormalizeCaptureManifestJSON1RequiresCanonicalPersistedBytes(t *testing.T) {
	manifest := testManifest(t)
	canonical := canonicalManifest(t, manifest)
	lease := &testManifestLease{root: t.TempDir()}
	var accepted bytes.Buffer
	if _, err := NormalizeCaptureManifestJSON1(context.Background(), bytes.NewReader(canonical), &accepted, lease, CaptureManifestStreamOptions{RequireCanonical: true}); err != nil {
		t.Fatalf("canonical artifact rejected: %v", err)
	}
	if !bytes.Equal(accepted.Bytes(), canonical) {
		t.Fatal("canonical artifact changed during validation")
	}
	lease = &testManifestLease{root: t.TempDir()}
	var rejected bytes.Buffer
	if _, err := NormalizeCaptureManifestJSON1(context.Background(), bytes.NewReader(append([]byte(" "), canonical...)), &rejected, lease, CaptureManifestStreamOptions{RequireCanonical: true}); !errors.Is(err, ErrManifestNotCanonical) {
		t.Fatalf("noncanonical persisted artifact error=%v, want ErrManifestNotCanonical", err)
	}
	if rejected.Len() != 0 {
		t.Fatalf("rejected artifact leaked %d output bytes", rejected.Len())
	}
}

func TestNormalizeCaptureManifestJSON1RejectsMalformedAndDuplicateData(t *testing.T) {
	manifest := testManifest(t)
	canonical := canonicalManifest(t, manifest)
	invalidInputs := map[string][]byte{
		"duplicate root key": []byte(`{"schema_version":1,"schema_version":1}`),
		"unpaired surrogate": bytes.Replace(canonical, []byte("a.go"), []byte(`\uD800.go`), 1),
		"fractional integer": bytes.Replace(canonical, []byte(`"byte_length":7`), []byte(`"byte_length":7e0`), 1),
		"duplicate file key": bytes.Replace(canonical, []byte(`"path":"a.go"`), []byte(`"path":"a.go","path":"b.go"`), 1),
		"unknown root key":   bytes.Replace(canonical, []byte(`"schema_version":1`), []byte(`"extra":null,"schema_version":1`), 1),
	}
	invalidUTF8 := append([]byte(nil), canonical...)
	pathIndex := bytes.Index(invalidUTF8, []byte("a.go"))
	invalidUTF8[pathIndex] = 0xff
	invalidInputs["invalid UTF-8"] = invalidUTF8
	for name, raw := range invalidInputs {
		t.Run(name, func(t *testing.T) {
			lease := &testManifestLease{root: t.TempDir()}
			var output bytes.Buffer
			if _, err := NormalizeCaptureManifestJSON1(context.Background(), bytes.NewReader(raw), &output, lease, CaptureManifestStreamOptions{}); err == nil {
				t.Fatalf("invalid input accepted: %q", raw)
			}
			if output.Len() != 0 {
				t.Fatalf("invalid input produced %d output bytes", output.Len())
			}
		})
	}
}

func TestNormalizeCaptureManifestJSON1EnforcesByteLimitBeforeOutput(t *testing.T) {
	lease := &testManifestLease{root: t.TempDir()}
	var output bytes.Buffer
	_, err := NormalizeCaptureManifestJSON1(context.Background(), strings.NewReader(strings.Repeat(" ", 128)), &output, lease, CaptureManifestStreamOptions{MaxBytes: 16})
	if !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("oversized input error=%v, want ErrManifestTooLarge", err)
	}
	if output.Len() != 0 {
		t.Fatalf("oversized input produced %d output bytes", output.Len())
	}
	if !lease.released {
		t.Fatal("oversized input did not release spool reservation")
	}
}

func TestNormalizeCaptureManifestJSON1CapsExpandedCanonicalSpool(t *testing.T) {
	manifest := testManifest(t)
	reason := model.Identifier(strings.Repeat("\u2028", 50))
	manifest.Files = append(manifest.Files, model.ManifestFile{Path: "expanded.go", Status: "missing", ReasonCode: &reason})
	digest, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.SnapshotDigest = digest
	canonical := canonicalManifest(t, manifest)
	input := bytes.ReplaceAll(canonical, []byte(`\u2028`), []byte("\u2028"))
	if len(input) >= len(canonical) {
		t.Fatal("fixture must expand during canonical JSON encoding")
	}
	lease := &testManifestLease{root: t.TempDir()}
	var output bytes.Buffer
	_, err = NormalizeCaptureManifestJSON1(context.Background(), bytes.NewReader(input), &output, lease, CaptureManifestStreamOptions{MaxBytes: int64(len(input))})
	if !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("expanded output error=%v, want ErrManifestTooLarge", err)
	}
	if output.Len() != 0 || !lease.released {
		t.Fatalf("over-limit canonical spool leaked output or admission: output=%d released=%v", output.Len(), lease.released)
	}
}

func TestNormalizeCaptureManifestJSON1RejectsNestingAboveSchemaLimit(t *testing.T) {
	input := `{"unknown":` + strings.Repeat("[", 32) + `0` + strings.Repeat("]", 32) + `}`
	lease := &testManifestLease{root: t.TempDir()}
	var output bytes.Buffer
	_, err := NormalizeCaptureManifestJSON1(context.Background(), strings.NewReader(input), &output, lease, CaptureManifestStreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "nesting exceeds 32") {
		t.Fatalf("nesting limit error=%v, want a depth rejection", err)
	}
	if output.Len() != 0 || !lease.released {
		t.Fatalf("rejected deep input wrote output or leaked admission: output=%d released=%v", output.Len(), lease.released)
	}
}

func TestNormalizeCaptureManifestJSON1ReleasesAdmissionOnDenialAndCancellation(t *testing.T) {
	manifest := testManifest(t)
	canonical := canonicalManifest(t, manifest)
	t.Run("admission denied", func(t *testing.T) {
		lease := &testManifestLease{root: t.TempDir(), reserveErr: errors.New("quota denied")}
		var output bytes.Buffer
		_, err := NormalizeCaptureManifestJSON1(context.Background(), bytes.NewReader(canonical), &output, lease, CaptureManifestStreamOptions{})
		if err == nil || !strings.Contains(err.Error(), "quota denied") {
			t.Fatalf("admission error=%v, want quota denial", err)
		}
		if output.Len() != 0 || !lease.released {
			t.Fatalf("admission denial wrote output or leaked reservation: output=%d released=%v", output.Len(), lease.released)
		}
	})
	t.Run("context canceled", func(t *testing.T) {
		lease := &testManifestLease{root: t.TempDir()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var output bytes.Buffer
		_, err := NormalizeCaptureManifestJSON1(ctx, bytes.NewReader(canonical), &output, lease, CaptureManifestStreamOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled normalization error=%v, want context.Canceled", err)
		}
		if output.Len() != 0 || !lease.released {
			t.Fatalf("canceled normalization wrote output or leaked reservation: output=%d released=%v", output.Len(), lease.released)
		}
	})
}

func TestNormalizeCaptureManifestJSON1SortsAcrossExternalRunBoundary(t *testing.T) {
	const count = 4100
	manifest := model.CaptureManifest{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: model.Hash(strings.Repeat("a", 64)), PolicyDigest: model.Hash(strings.Repeat("b", 64)),
		ProviderDigest: model.Hash(strings.Repeat("c", 64)), Files: make([]model.ManifestFile, count), DependencyInputDigests: []model.Hash{},
	}
	for index := 0; index < count; index++ {
		manifest.Files[index] = model.ManifestFile{Path: fmt.Sprintf("file-%05d.go", index), Status: "missing"}
	}
	digest, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.SnapshotDigest = digest
	want := canonicalManifest(t, manifest)
	var input strings.Builder
	fmt.Fprintf(&input, `{"schema_version":1,"worktree_id":%q,"profile_id":%q,"policy_digest":%q,"provider_digest":%q,"snapshot_digest":%q,"dependency_input_digests":[],"files":[`, manifest.WorktreeID, manifest.ProfileID, manifest.PolicyDigest, manifest.ProviderDigest, manifest.SnapshotDigest)
	for index := count - 1; index >= 0; index-- {
		if index != count-1 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `{"path":"file-%05d.go","status":"missing","content_hash":null,"byte_length":null,"reason_code":null}`, index)
	}
	input.WriteString("]}")
	lease := &testManifestLease{root: t.TempDir()}
	var output bytes.Buffer
	summary, err := NormalizeCaptureManifestJSON1(context.Background(), strings.NewReader(input.String()), &output, lease, CaptureManifestStreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatal("external runs did not normalize to the canonical sorted manifest")
	}
	if summary.FileCount != count || summary.ByteLength != int64(len(want)) {
		t.Fatalf("wrong XL count/length summary: %+v", summary)
	}
}

func TestNormalizeCaptureManifestJSON1RejectsDuplicateAcrossRuns(t *testing.T) {
	manifest := model.CaptureManifest{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: model.Hash(strings.Repeat("a", 64)), PolicyDigest: model.Hash(strings.Repeat("b", 64)),
		ProviderDigest: model.Hash(strings.Repeat("c", 64)), Files: []model.ManifestFile{}, DependencyInputDigests: []model.Hash{},
	}
	for index := 0; index < 4096; index++ {
		manifest.Files = append(manifest.Files, model.ManifestFile{Path: fmt.Sprintf("file-%05d.go", index), Status: "missing"})
	}
	manifest.Files = append(manifest.Files, model.ManifestFile{Path: "file-00000.go", Status: "missing"})
	manifest.SnapshotDigest = model.Hash(strings.Repeat("d", 64))
	var input strings.Builder
	fmt.Fprintf(&input, `{"schema_version":1,"worktree_id":%q,"profile_id":%q,"policy_digest":%q,"provider_digest":%q,"snapshot_digest":%q,"dependency_input_digests":[],"files":[`, manifest.WorktreeID, manifest.ProfileID, manifest.PolicyDigest, manifest.ProviderDigest, manifest.SnapshotDigest)
	for index, file := range manifest.Files {
		if index != 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `{"path":%q,"status":"missing","content_hash":null,"byte_length":null,"reason_code":null}`, file.Path)
	}
	input.WriteString("]}")
	lease := &testManifestLease{root: t.TempDir()}
	var output bytes.Buffer
	if _, err := NormalizeCaptureManifestJSON1(context.Background(), strings.NewReader(input.String()), &output, lease, CaptureManifestStreamOptions{}); err == nil {
		t.Fatal("duplicate path in different external sort runs was accepted")
	}
	if output.Len() != 0 {
		t.Fatalf("duplicate manifest produced %d output bytes", output.Len())
	}
}

func testManifest(t *testing.T) model.CaptureManifest {
	t.Helper()
	first := model.Hash(strings.Repeat("a", 64))
	second := model.Hash(strings.Repeat("b", 64))
	third := model.Hash(strings.Repeat("c", 64))
	length := int64(7)
	manifest := model.CaptureManifest{
		SchemaVersion: 1, WorktreeID: "11111111-1111-4111-8111-111111111111",
		ProfileID: first, PolicyDigest: second, ProviderDigest: third,
		Files: []model.ManifestFile{
			{Path: "z/β.go", Status: "captured", ContentHash: &second, ByteLength: &length},
			{Path: "a.go", Status: "missing"},
		},
		DependencyInputDigests: []model.Hash{third, first, first},
	}
	digest, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.SnapshotDigest = digest
	return manifest
}

func hashManifestBytes(value []byte) model.Hash {
	digest := sha256.Sum256(value)
	return model.Hash(hex.EncodeToString(digest[:]))
}

func canonicalManifest(t *testing.T, manifest model.CaptureManifest) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := model.WriteCaptureManifestJSON1(&output, manifest); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
