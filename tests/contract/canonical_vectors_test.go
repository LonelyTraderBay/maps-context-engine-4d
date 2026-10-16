package contract_test

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		var output bytes.Buffer
		if err := model.WriteCaptureManifestJSON1(&output, manifest); err != nil {
			t.Errorf("%s write JSON1: %v", name, err)
			continue
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
