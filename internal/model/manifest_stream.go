package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

const maxCaptureManifestEntries int64 = 1_000_000

type ManifestFileSource interface {
	Reset() error
	Next() (ManifestFile, bool, error)
}

type ManifestHashSource interface {
	Reset() error
	Next() (Hash, bool, error)
}

type CaptureManifestHeader struct {
	SchemaVersion  int64
	WorktreeID     UUID
	ProfileID      Hash
	PolicyDigest   Hash
	ProviderDigest Hash
	SnapshotDigest Hash
}

// CaptureManifestStream reads files and dependencies from rewindable sources.
// Callers use owned disk spools for large inputs; no complete array is required
// in memory. Both sources must be sorted by raw UTF-8 bytes before encoding.
type CaptureManifestStream struct {
	Header          CaptureManifestHeader
	FileCount       int64
	Files           ManifestFileSource
	DependencyCount int64
	Dependencies    ManifestHashSource
}

type ManifestWriteSummary struct {
	ByteLength      int64
	ArtifactSHA256  Hash
	SnapshotDigest  Hash
	FileCount       int64
	DependencyCount int64
	Header          CaptureManifestHeader
}

// CaptureManifestSliceSources adapts the small in-memory model to the same
// streaming verifier used by spool-backed manifests.
func CaptureManifestSliceSources(manifest CaptureManifest) CaptureManifestStream {
	return CaptureManifestStream{
		Header: CaptureManifestHeader{
			SchemaVersion: manifest.SchemaVersion, WorktreeID: manifest.WorktreeID,
			ProfileID: manifest.ProfileID, PolicyDigest: manifest.PolicyDigest,
			ProviderDigest: manifest.ProviderDigest, SnapshotDigest: manifest.SnapshotDigest,
		},
		FileCount: int64(len(manifest.Files)), Files: &sliceManifestFiles{values: manifest.Files},
		DependencyCount: int64(len(manifest.DependencyInputDigests)),
		Dependencies:    &sliceManifestHashes{values: manifest.DependencyInputDigests},
	}
}

func ComputeCaptureManifestSnapshotDigest(stream CaptureManifestStream) (Hash, error) {
	if err := validateManifestStreamHeader(stream); err != nil {
		return "", err
	}
	if err := stream.Files.Reset(); err != nil {
		return "", err
	}
	if err := stream.Dependencies.Reset(); err != nil {
		return "", err
	}
	digest := sha256.New()
	write := func(value string) error {
		_, err := io.WriteString(digest, value)
		return err
	}
	if err := write("MCE4D\x00C14N1\x00s10:capture-v1o7:"); err != nil {
		return "", err
	}
	if err := writeC14NString(digest, "dependency_input_digests"); err != nil {
		return "", err
	}
	if _, err := io.WriteString(digest, "a"+strconv.FormatInt(stream.DependencyCount, 10)+":"); err != nil {
		return "", err
	}
	var previousDependency string
	for index := int64(0); index < stream.DependencyCount; index++ {
		value, ok, err := stream.Dependencies.Next()
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: dependency spool ended before declared count", ErrInvalidValue)
		}
		if _, err := ParseHash(string(value)); err != nil {
			return "", err
		}
		if index > 0 && bytes.Compare([]byte(previousDependency), []byte(value)) > 0 {
			return "", fmt.Errorf("%w: dependency spool is not sorted", ErrInvalidValue)
		}
		previousDependency = string(value)
		if err := writeC14NString(digest, string(value)); err != nil {
			return "", err
		}
	}
	if err := ensureHashesDone(stream.Dependencies); err != nil {
		return "", err
	}
	if err := writeC14NString(digest, "files"); err != nil {
		return "", err
	}
	if _, err := io.WriteString(digest, "a"+strconv.FormatInt(stream.FileCount, 10)+":"); err != nil {
		return "", err
	}
	previousPath := ""
	for index := int64(0); index < stream.FileCount; index++ {
		file, ok, err := stream.Files.Next()
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: file spool ended before declared count", ErrInvalidValue)
		}
		if err := ValidateCaptureManifestFile(file); err != nil {
			return "", err
		}
		if index > 0 && bytes.Compare([]byte(previousPath), []byte(file.Path)) >= 0 {
			return "", fmt.Errorf("%w: file spool must be sorted and contain unique paths", ErrInvalidValue)
		}
		previousPath = file.Path
		if err := writeC14NManifestFile(digest, file); err != nil {
			return "", err
		}
	}
	if err := ensureFilesDone(stream.Files); err != nil {
		return "", err
	}
	for _, key := range []string{"policy_digest", "profile_id", "provider_digest"} {
		if err := writeC14NString(digest, key); err != nil {
			return "", err
		}
		var value string
		switch key {
		case "policy_digest":
			value = string(stream.Header.PolicyDigest)
		case "profile_id":
			value = string(stream.Header.ProfileID)
		case "provider_digest":
			value = string(stream.Header.ProviderDigest)
		}
		if err := writeC14NString(digest, value); err != nil {
			return "", err
		}
	}
	if err := writeC14NString(digest, "schema_version"); err != nil {
		return "", err
	}
	if _, err := io.WriteString(digest, "i"+strconv.FormatInt(stream.Header.SchemaVersion, 10)+";"); err != nil {
		return "", err
	}
	if err := writeC14NString(digest, "worktree_id"); err != nil {
		return "", err
	}
	if err := writeC14NString(digest, string(stream.Header.WorktreeID)); err != nil {
		return "", err
	}
	return Hash(hex.EncodeToString(digest.Sum(nil))), nil
}

// WriteCaptureManifestJSON1Stream verifies the logical snapshot digest before
// emitting canonical physical JSON. It performs two bounded passes over each
// rewindable source and hashes the raw artifact as it writes it.
func WriteCaptureManifestJSON1Stream(dst io.Writer, stream CaptureManifestStream) (ManifestWriteSummary, error) {
	if dst == nil {
		return ManifestWriteSummary{}, fmt.Errorf("%w: nil manifest writer", ErrInvalidCanonicalValue)
	}
	if stream.Header.SchemaVersion != 1 {
		return ManifestWriteSummary{}, fmt.Errorf("%w: unsupported capture manifest schema version", ErrInvalidValue)
	}
	digest, err := ComputeCaptureManifestSnapshotDigest(stream)
	if err != nil {
		return ManifestWriteSummary{}, fmt.Errorf("%w: invalid streaming capture manifest: %v", ErrInvalidCanonicalValue, err)
	}
	if digest != stream.Header.SnapshotDigest {
		return ManifestWriteSummary{}, fmt.Errorf("%w: capture snapshot digest mismatch", ErrInvalidValue)
	}
	if err := stream.Dependencies.Reset(); err != nil {
		return ManifestWriteSummary{}, err
	}
	if err := stream.Files.Reset(); err != nil {
		return ManifestWriteSummary{}, err
	}
	artifactHash := sha256.New()
	counter := &countingWriter{writer: io.MultiWriter(dst, artifactHash)}
	if err := writeCanonicalBytes(counter, []byte("{\"dependency_input_digests\":")); err != nil {
		return ManifestWriteSummary{}, err
	}
	if err := writeStreamArray(counter, stream.DependencyCount, func() error {
		value, ok, err := stream.Dependencies.Next()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: dependency spool ended during write", ErrInvalidValue)
		}
		return writeJSON1Value(counter, value)
	}); err != nil {
		return ManifestWriteSummary{}, err
	}
	if err := ensureHashesDone(stream.Dependencies); err != nil {
		return ManifestWriteSummary{}, err
	}
	if err := writeCanonicalBytes(counter, []byte(",\"files\":")); err != nil {
		return ManifestWriteSummary{}, err
	}
	if err := writeStreamArray(counter, stream.FileCount, func() error {
		file, ok, err := stream.Files.Next()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: file spool ended during write", ErrInvalidValue)
		}
		return writeJSON1Value(counter, map[string]any{
			"path": file.Path, "status": file.Status, "content_hash": optionalHash(file.ContentHash),
			"byte_length": optionalInt(file.ByteLength), "reason_code": optionalIdentifier(file.ReasonCode),
		})
	}); err != nil {
		return ManifestWriteSummary{}, err
	}
	if err := ensureFilesDone(stream.Files); err != nil {
		return ManifestWriteSummary{}, err
	}
	for _, field := range []struct {
		name  string
		value any
	}{
		{name: "policy_digest", value: stream.Header.PolicyDigest},
		{name: "profile_id", value: stream.Header.ProfileID},
		{name: "provider_digest", value: stream.Header.ProviderDigest},
		{name: "schema_version", value: stream.Header.SchemaVersion},
		{name: "snapshot_digest", value: stream.Header.SnapshotDigest},
		{name: "worktree_id", value: stream.Header.WorktreeID},
	} {
		if err := writeCanonicalBytes(counter, []byte(",\""+field.name+"\":")); err != nil {
			return ManifestWriteSummary{}, err
		}
		if err := writeJSON1Value(counter, field.value); err != nil {
			return ManifestWriteSummary{}, err
		}
	}
	if err := writeCanonicalBytes(counter, []byte("}")); err != nil {
		return ManifestWriteSummary{}, err
	}
	return ManifestWriteSummary{
		ByteLength: counter.count, ArtifactSHA256: Hash(hex.EncodeToString(artifactHash.Sum(nil))),
		SnapshotDigest: digest, FileCount: stream.FileCount, DependencyCount: stream.DependencyCount, Header: stream.Header,
	}, nil
}

func ValidateCaptureManifestFile(file ManifestFile) error {
	if !utf8.ValidString(file.Path) || utf8.RuneCountInString(file.Path) > 4096 {
		return fmt.Errorf("%w: capture-manifest path exceeds UTF-8 schema bounds", ErrInvalidValue)
	}
	if err := ValidateRelativePath(file.Path); err != nil {
		return err
	}
	if !oneOf(file.Status, "captured", "metadata_only", "excluded", "missing", "read_error") {
		return fmt.Errorf("%w: unknown capture-manifest file status", ErrInvalidValue)
	}
	if file.ContentHash != nil {
		if _, err := ParseHash(string(*file.ContentHash)); err != nil {
			return err
		}
	}
	if file.ByteLength != nil && (*file.ByteLength < 0 || *file.ByteLength > MaxSafeInteger) {
		return fmt.Errorf("%w: capture-manifest byte length outside safe range", ErrInvalidValue)
	}
	if file.ReasonCode != nil && !validIdentifier(*file.ReasonCode) {
		return fmt.Errorf("%w: invalid capture-manifest reason code", ErrInvalidValue)
	}
	if file.Status == "captured" && (file.ContentHash == nil || file.ByteLength == nil) {
		return fmt.Errorf("%w: captured file needs hash and safe byte length", ErrInvalidValue)
	}
	return nil
}

func validateManifestStreamHeader(stream CaptureManifestStream) error {
	if stream.FileCount < 0 || stream.DependencyCount < 0 || stream.FileCount > maxCaptureManifestEntries || stream.DependencyCount > maxCaptureManifestEntries || stream.Files == nil || stream.Dependencies == nil {
		return fmt.Errorf("%w: streaming capture manifest arrays are required and capped at 1000000", ErrInvalidValue)
	}
	if stream.Header.SchemaVersion < 0 || stream.Header.SchemaVersion > MaxSafeInteger {
		return fmt.Errorf("%w: capture manifest schema version outside safe range", ErrInvalidValue)
	}
	if _, err := ParseUUID(string(stream.Header.WorktreeID)); err != nil {
		return err
	}
	for _, value := range []Hash{stream.Header.ProfileID, stream.Header.PolicyDigest, stream.Header.ProviderDigest} {
		if _, err := ParseHash(string(value)); err != nil {
			return err
		}
	}
	if stream.Header.SnapshotDigest != "" {
		if _, err := ParseHash(string(stream.Header.SnapshotDigest)); err != nil {
			return err
		}
	}
	return nil
}

func writeC14NManifestFile(dst io.Writer, file ManifestFile) error {
	if _, err := io.WriteString(dst, "o5:"); err != nil {
		return err
	}
	if err := writeC14NString(dst, "byte_length"); err != nil {
		return err
	}
	if file.ByteLength == nil {
		if _, err := io.WriteString(dst, "n"); err != nil {
			return err
		}
	} else if _, err := io.WriteString(dst, "i"+strconv.FormatInt(*file.ByteLength, 10)+";"); err != nil {
		return err
	}
	if err := writeC14NString(dst, "content_hash"); err != nil {
		return err
	}
	if err := writeC14NOptionalString(dst, optionalHash(file.ContentHash)); err != nil {
		return err
	}
	if err := writeC14NString(dst, "path"); err != nil {
		return err
	}
	if err := writeC14NString(dst, file.Path); err != nil {
		return err
	}
	if err := writeC14NString(dst, "reason_code"); err != nil {
		return err
	}
	if err := writeC14NOptionalString(dst, optionalIdentifier(file.ReasonCode)); err != nil {
		return err
	}
	if err := writeC14NString(dst, "status"); err != nil {
		return err
	}
	return writeC14NString(dst, file.Status)
}

func writeC14NOptionalString(dst io.Writer, value any) error {
	if value == nil {
		_, err := io.WriteString(dst, "n")
		return err
	}
	return writeC14NString(dst, fmt.Sprint(value))
}

func writeC14NString(dst io.Writer, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: string is not valid UTF-8", ErrInvalidCanonicalValue)
	}
	if _, err := io.WriteString(dst, "s"+strconv.Itoa(len(value))+":"); err != nil {
		return err
	}
	_, err := io.WriteString(dst, value)
	return err
}

func writeStreamArray(dst io.Writer, count int64, writeNext func() error) error {
	if err := writeCanonicalBytes(dst, []byte("[")); err != nil {
		return err
	}
	for index := int64(0); index < count; index++ {
		if index > 0 {
			if err := writeCanonicalBytes(dst, []byte(",")); err != nil {
				return err
			}
		}
		if err := writeNext(); err != nil {
			return err
		}
	}
	return writeCanonicalBytes(dst, []byte("]"))
}

func ensureFilesDone(source ManifestFileSource) error {
	_, ok, err := source.Next()
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("%w: file spool has more entries than declared", ErrInvalidValue)
	}
	return nil
}

func ensureHashesDone(source ManifestHashSource) error {
	_, ok, err := source.Next()
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("%w: dependency spool has more entries than declared", ErrInvalidValue)
	}
	return nil
}

type sliceManifestFiles struct {
	values []ManifestFile
	index  int
}

func (source *sliceManifestFiles) Reset() error { source.index = 0; return nil }
func (source *sliceManifestFiles) Next() (ManifestFile, bool, error) {
	if source.index >= len(source.values) {
		return ManifestFile{}, false, nil
	}
	value := source.values[source.index]
	source.index++
	return value, true, nil
}

type sliceManifestHashes struct {
	values []Hash
	index  int
}

func (source *sliceManifestHashes) Reset() error { source.index = 0; return nil }
func (source *sliceManifestHashes) Next() (Hash, bool, error) {
	if source.index >= len(source.values) {
		return "", false, nil
	}
	value := source.values[source.index]
	source.index++
	return value, true, nil
}

type countingWriter struct {
	writer io.Writer
	count  int64
}

func (writer *countingWriter) Write(value []byte) (int, error) {
	written, err := writer.writer.Write(value)
	writer.count += int64(written)
	if err == nil && written != len(value) {
		err = io.ErrShortWrite
	}
	return written, err
}
