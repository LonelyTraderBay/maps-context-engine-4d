package contract_test

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

//go:embed c14n-vectors.v1.json
var canonicalVectors []byte

//go:embed capture-manifest-json1.v1.json
var captureManifestVectors []byte

func TestSharedC14NAndRawSourceVectors(t *testing.T) {
	value, err := contracts.DecodeJSON(canonicalVectors)
	if err != nil {
		t.Fatal(err)
	}
	fixture := value.(map[string]any)
	vectors := fixture["vectors"].([]any)
	if got := len(vectors) + len(fixture["raw_source_vectors"].([]any)); got != 20 {
		t.Fatalf("golden vector count=%d, want 20", got)
	}
	for _, item := range vectors {
		vector := item.(map[string]any)
		name := vector["name"].(string)
		encoded, err := model.CanonicalEncode(vector["value"])
		if err != nil {
			t.Errorf("%s encode: %v", name, err)
			continue
		}
		if got, want := hex.EncodeToString(encoded), vector["encoding_hex"].(string); got != want {
			t.Errorf("%s encoding=%s, want %s", name, got, want)
		}
		digest, err := model.HashValue(vector["domain"].(string), vector["value"])
		if err != nil {
			t.Errorf("%s hash: %v", name, err)
			continue
		}
		if got, want := string(digest), vector["digest"].(string); got != want {
			t.Errorf("%s digest=%s, want %s", name, got, want)
		}
	}
	for _, item := range fixture["raw_source_vectors"].([]any) {
		vector := item.(map[string]any)
		raw, err := hex.DecodeString(vector["bytes_hex"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(model.SHA256(raw)), vector["sha256"].(string); got != want {
			t.Errorf("source %s SHA=%s, want %s", vector["name"], got, want)
		}
	}
}

func TestSharedUTF8SourceRangeAndRawDigestVectors(t *testing.T) {
	var fixture struct {
		SourceRangeVectors []struct {
			Name             string            `json:"name"`
			Text             string            `json:"text"`
			BytesHex         string            `json:"bytes_hex"`
			SHA256           string            `json:"sha256"`
			ValidRanges      []model.ByteRange `json:"valid_ranges"`
			InvalidRanges    []model.ByteRange `json:"invalid_ranges"`
			MutationBytesHex string            `json:"mutation_bytes_hex"`
			MutationText     string            `json:"mutation_text"`
			MutationSHA256   string            `json:"mutation_sha256"`
		} `json:"source_range_vectors"`
	}
	if err := json.Unmarshal(canonicalVectors, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.SourceRangeVectors) == 0 {
		t.Fatal("shared source-range vectors are missing")
	}
	for _, vector := range fixture.SourceRangeVectors {
		t.Run(vector.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(vector.BytesHex)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != vector.Text {
				t.Fatalf("source bytes do not match readable vector text %q", vector.Text)
			}
			if got := string(model.SHA256(raw)); got != vector.SHA256 {
				t.Fatalf("raw source SHA-256=%s, want %s", got, vector.SHA256)
			}
			ref := model.SourceRef{
				WorktreeID:   "11111111-1111-4111-8111-111111111111",
				ProfileID:    model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
				GenerationID: "22222222-2222-4222-8222-222222222222",
				Path:         "src/multilingual.txt", ContentHash: model.Hash(vector.SHA256),
			}
			for _, byteRange := range vector.ValidRanges {
				ref.Range = byteRange
				if err := ref.ValidateAgainst(raw); err != nil {
					t.Errorf("valid range %+v: %v", byteRange, err)
				}
			}
			for _, byteRange := range vector.InvalidRanges {
				ref.Range = byteRange
				if err := ref.ValidateAgainst(raw); err == nil {
					t.Errorf("invalid range %+v unexpectedly passed", byteRange)
				}
			}
			mutated, err := hex.DecodeString(vector.MutationBytesHex)
			if err != nil {
				t.Fatal(err)
			}
			if len(mutated) != len(raw) {
				t.Fatalf("same-length mutation has %d bytes, want %d", len(mutated), len(raw))
			}
			if string(mutated) != vector.MutationText {
				t.Fatalf("mutation bytes do not match readable vector text %q", vector.MutationText)
			}
			if got := string(model.SHA256(mutated)); got != vector.MutationSHA256 || got == vector.SHA256 {
				t.Fatalf("mutation SHA-256=%s, expected distinct %s", got, vector.MutationSHA256)
			}
			if err := ref.ValidateAgainst(mutated); err == nil {
				t.Fatal("same-length source mutation unexpectedly passed the original raw digest")
			}
		})
	}
}

func TestManifestJSON1FullAndUnicodeGoldenBytes(t *testing.T) {
	value, err := contracts.DecodeJSON(canonicalVectors)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range value.(map[string]any)["manifest_json_vectors"].([]any) {
		vector := item.(map[string]any)
		name := vector["name"].(string)
		encoded, err := model.CanonicalJSON1(vector["value"])
		if err != nil {
			t.Errorf("%s encode: %v", name, err)
			continue
		}
		want, err := hex.DecodeString(vector["canonical_hex"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != string(want) {
			t.Errorf("%s JSON1 bytes differ: got %s, want %s", name, fmt.Sprintf("%x", encoded), fmt.Sprintf("%x", want))
		}
		if got := string(model.SHA256(encoded)); got != vector["sha256"].(string) {
			t.Errorf("%s JSON1 SHA=%s, want %s", name, got, vector["sha256"])
		}
	}
}

func TestCaptureManifestJSON1GoldenBytesAndSchema(t *testing.T) {
	value, err := contracts.DecodeJSON(captureManifestVectors)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	vectors := value.(map[string]any)["vectors"].([]any)
	for _, item := range vectors {
		vector := item.(map[string]any)
		name := vector["name"].(string)
		raw, err := hex.DecodeString(vector["canonical_hex"].(string))
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := validator.ValidateCaptureManifest(raw)
		if err != nil {
			t.Errorf("%s validate: %v", name, err)
			continue
		}
		if got, want := string(manifest.SnapshotDigest), vector["manifest"].(map[string]any)["snapshot_digest"].(string); got != want {
			t.Errorf("%s snapshot digest=%s, want %s", name, got, want)
		}
		if name == "full" && len(manifest.Files) > 1 {
			manifest.Files[0], manifest.Files[1] = manifest.Files[1], manifest.Files[0]
		}
		if name == "full" && len(manifest.DependencyInputDigests) > 1 {
			manifest.DependencyInputDigests[0], manifest.DependencyInputDigests[1] = manifest.DependencyInputDigests[1], manifest.DependencyInputDigests[0]
		}
		originalFiles := make([]model.ManifestFile, len(manifest.Files))
		copy(originalFiles, manifest.Files)
		originalDependencies := make([]model.Hash, len(manifest.DependencyInputDigests))
		copy(originalDependencies, manifest.DependencyInputDigests)
		var output bytes.Buffer
		if err := model.WriteCaptureManifestJSON1(&output, manifest); err != nil {
			t.Errorf("%s write JSON1: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(manifest.Files, originalFiles) || !reflect.DeepEqual(manifest.DependencyInputDigests, originalDependencies) {
			t.Errorf("%s JSON1 writer mutated caller-owned manifest arrays", name)
		}
		if !bytes.Equal(output.Bytes(), raw) {
			t.Errorf("%s canonical manifest bytes differ: got=%q want=%q", name, output.Bytes(), raw)
		}
		if got, want := string(model.SHA256(raw)), vector["sha256"].(string); got != want {
			t.Errorf("%s raw SHA-256=%s, want %s", name, got, want)
		}

		if name == "full" {
			unordered, err := json.Marshal(vector["manifest"])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validator.ValidateCaptureManifest(unordered); err == nil {
				t.Errorf("%s accepted noncanonical array order", name)
			}
		}
	}
}

func TestCaptureManifestRejectsParseProjectionFields(t *testing.T) {
	value, err := contracts.DecodeJSON(captureManifestVectors)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	vector := value.(map[string]any)["vectors"].([]any)[0].(map[string]any)
	raw, err := hex.DecodeString(vector["canonical_hex"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"parse_error", "created_at"} {
		projected := append([]byte(nil), raw[:len(raw)-1]...)
		projected = append(projected, []byte(`,"`+field+`":"projection"}`)...)
		if _, err := validator.ValidateCaptureManifest(projected); err == nil {
			t.Errorf("capture manifest accepted parse-result field %q", field)
		}
	}
}

func TestInvalidIdentityLexemesReject(t *testing.T) {
	value, err := contracts.DecodeJSON(canonicalVectors)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range value.(map[string]any)["negative_vectors"].([]any) {
		vector := item.(map[string]any)
		name := vector["name"].(string)
		raw := []byte(vector["raw_json"].(string))
		decoded, decodeErr := contracts.DecodeJSON(raw)
		if decodeErr == nil {
			_, decodeErr = model.HashValue(vector["domain"].(string), decoded)
		}
		if decodeErr == nil {
			t.Errorf("%s unexpectedly accepted", name)
		}
	}
}
