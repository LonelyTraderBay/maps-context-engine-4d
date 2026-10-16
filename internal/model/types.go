// Package model contains pure, validated domain values shared across adapters.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxSafeInteger int64 = 9007199254740991

var (
	ErrInvalidValue = errors.New("invalid model value")
	ErrInvalidRange = errors.New("invalid byte range")
)

type UUID string
type Hash string
type Identifier string

func ParseUUID(value string) (UUID, error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", fmt.Errorf("%w: UUID is not canonical", ErrInvalidValue)
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return "", fmt.Errorf("%w: UUID is not lowercase hexadecimal", ErrInvalidValue)
		}
	}
	return UUID(value), nil
}

func ParseHash(value string) (Hash, error) {
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("%w: hash must contain 64 lowercase hexadecimal bytes", ErrInvalidValue)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return "", fmt.Errorf("%w: hash is not lowercase hexadecimal", ErrInvalidValue)
	}
	return Hash(value), nil
}

func SHA256(raw []byte) Hash {
	digest := sha256.Sum256(raw)
	return Hash(hex.EncodeToString(digest[:]))
}

type ByteRange struct {
	StartByte int64 `json:"start_byte"`
	EndByte   int64 `json:"end_byte"`
}

func (r ByteRange) ValidateAgainst(source []byte) error {
	if !utf8.Valid(source) || r.StartByte < 0 || r.EndByte < r.StartByte || r.EndByte > int64(len(source)) {
		return ErrInvalidRange
	}
	for _, offset := range []int64{r.StartByte, r.EndByte} {
		if offset < int64(len(source)) && source[offset]&0xc0 == 0x80 {
			return fmt.Errorf("%w: offset %d is inside a UTF-8 code point", ErrInvalidRange, offset)
		}
	}
	return nil
}

type SourceRef struct {
	WorktreeID   UUID      `json:"worktree_id"`
	ProfileID    Hash      `json:"profile_id"`
	GenerationID UUID      `json:"generation_id"`
	Path         string    `json:"path"`
	ContentHash  Hash      `json:"content_hash"`
	Range        ByteRange `json:"range"`
}

func (ref SourceRef) ValidateAgainst(source []byte) error {
	if _, err := ParseUUID(string(ref.WorktreeID)); err != nil {
		return err
	}
	if _, err := ParseUUID(string(ref.GenerationID)); err != nil {
		return err
	}
	if _, err := ParseHash(string(ref.ProfileID)); err != nil {
		return err
	}
	if _, err := ParseHash(string(ref.ContentHash)); err != nil {
		return err
	}
	if err := ValidateRelativePath(ref.Path); err != nil {
		return err
	}
	if SHA256(source) != ref.ContentHash {
		return fmt.Errorf("%w: source content hash mismatch", ErrInvalidValue)
	}
	return ref.Range.ValidateAgainst(source)
}

func ValidateRelativePath(value string) error {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") {
		return fmt.Errorf("%w: path must be nonempty relative UTF-8 using '/'", ErrInvalidValue)
	}
	if len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' {
		return fmt.Errorf("%w: drive-qualified path is forbidden", ErrInvalidValue)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\u0085' || r == '\u2028' || r == '\u2029' {
			return fmt.Errorf("%w: control character in path", ErrInvalidValue)
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: path contains an empty or traversal segment", ErrInvalidValue)
		}
	}
	if path.IsAbs(value) || fs.ValidPath(value) == false {
		return fmt.Errorf("%w: path is not a valid relative locator", ErrInvalidValue)
	}
	return nil
}

type ScopeMember struct {
	WorktreeID         UUID        `json:"worktree_id"`
	ProfileID          Hash        `json:"profile_id"`
	GenerationID       UUID        `json:"generation_id"`
	SnapshotDigest     Hash        `json:"snapshot_digest"`
	PolicyDigest       Hash        `json:"policy_digest"`
	PolicyEpoch        int64       `json:"policy_epoch"`
	BindingRevision    int64       `json:"binding_revision"`
	RecordEpoch        int64       `json:"record_epoch"`
	ServiceID          *Identifier `json:"service_id"`
	IndexIncarnation   UUID        `json:"index_incarnation"`
	RecordsIncarnation UUID        `json:"records_incarnation"`
}

type UnavailableMember struct {
	ServiceID  *Identifier `json:"service_id"`
	WorktreeID *UUID       `json:"worktree_id"`
	ProfileID  *Hash       `json:"profile_id"`
	Status     string      `json:"status"`
	ReasonCode Identifier  `json:"reason_code"`
}

type ScopeVector struct {
	Kind               string              `json:"kind"`
	Members            []ScopeMember       `json:"members"`
	VectorDigest       Hash                `json:"vector_digest"`
	GroupID            *UUID               `json:"group_id"`
	GroupRevision      *int64              `json:"group_revision"`
	CatalogIncarnation UUID                `json:"catalog_incarnation"`
	UnavailableMembers []UnavailableMember `json:"unavailable_members"`
}

type ManifestFile struct {
	Path        string      `json:"path"`
	Status      string      `json:"status"`
	ContentHash *Hash       `json:"content_hash"`
	ByteLength  *int64      `json:"byte_length"`
	ReasonCode  *Identifier `json:"reason_code"`
}

type CaptureManifest struct {
	SchemaVersion          int64          `json:"schema_version"`
	WorktreeID             UUID           `json:"worktree_id"`
	ProfileID              Hash           `json:"profile_id"`
	PolicyDigest           Hash           `json:"policy_digest"`
	ProviderDigest         Hash           `json:"provider_digest"`
	Files                  []ManifestFile `json:"files"`
	DependencyInputDigests []Hash         `json:"dependency_input_digests"`
	SnapshotDigest         Hash           `json:"snapshot_digest"`
}

type StoredCaptureManifestRef struct {
	FormatVersion  int64  `json:"format_version"`
	Kind           string `json:"kind"`
	Path           string `json:"path"`
	ByteLength     int64  `json:"byte_length"`
	ArtifactSHA256 Hash   `json:"artifact_sha256"`
	SnapshotDigest Hash   `json:"snapshot_digest"`
}

func (ref StoredCaptureManifestRef) Validate() error {
	if ref.FormatVersion != 1 || ref.Kind != "external_capture_manifest" || ref.ByteLength < 1 || ref.ByteLength > 34359738368 {
		return fmt.Errorf("%w: invalid stored capture manifest reference", ErrInvalidValue)
	}
	if _, err := ParseHash(string(ref.ArtifactSHA256)); err != nil {
		return err
	}
	if _, err := ParseHash(string(ref.SnapshotDigest)); err != nil {
		return err
	}
	if err := ValidateRelativePath(ref.Path); err != nil {
		return err
	}
	if ref.Path != "manifests/"+string(ref.ArtifactSHA256)+".json" {
		return fmt.Errorf("%w: manifest locator is not bound to raw artifact SHA-256", ErrInvalidValue)
	}
	return nil
}

type AdmissionConfig struct {
	SchemaVersion         int64 `json:"schema_version"`
	MaxWorktreeCacheBytes int64 `json:"max_worktree_cache_bytes"`
	VolumeReserveBytes    int64 `json:"volume_reserve_bytes"`
	VolumeWarningBytes    int64 `json:"volume_warning_bytes"`
}

func (c AdmissionConfig) Validate() error {
	if c.SchemaVersion != 1 || c.MaxWorktreeCacheBytes < 1 || c.VolumeReserveBytes < 1 || c.VolumeWarningBytes < c.VolumeReserveBytes {
		return fmt.Errorf("%w: admission capacities must be positive and warning >= reserve", ErrInvalidValue)
	}
	for _, value := range []int64{c.MaxWorktreeCacheBytes, c.VolumeReserveBytes, c.VolumeWarningBytes} {
		if value > MaxSafeInteger {
			return fmt.Errorf("%w: admission capacity exceeds safe integer", ErrInvalidValue)
		}
	}
	return nil
}

func (c AdmissionConfig) Digest() (Hash, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	return HashValue("admission-config-v1", map[string]any{
		"schema_version":           c.SchemaVersion,
		"max_worktree_cache_bytes": c.MaxWorktreeCacheBytes,
		"volume_reserve_bytes":     c.VolumeReserveBytes,
		"volume_warning_bytes":     c.VolumeWarningBytes,
	})
}

type AdmissionOwnedPath string

type VolumeAdmissionEntry struct {
	OperationID                  UUID                 `json:"operation_id"`
	ScopeKind                    string               `json:"scope_kind"`
	WorktreeID                   *UUID                `json:"worktree_id"`
	OwnerIdentity                Identifier           `json:"owner_identity"`
	OwnershipRef                 AdmissionOwnedPath   `json:"ownership_ref"`
	OperationKind                string               `json:"operation_kind"`
	AllowedPaths                 []AdmissionOwnedPath `json:"allowed_paths"`
	VolumeReservedRemainingBytes int64                `json:"volume_reserved_remaining_bytes"`
	CacheReservedRemainingBytes  int64                `json:"cache_reserved_remaining_bytes"`
	CreatedAt                    string               `json:"created_at"`
}

type VolumeAdmissionJournal struct {
	FormatVersion         int64                  `json:"format_version"`
	VolumeID              Identifier             `json:"volume_id"`
	HomeIdentity          Identifier             `json:"home_identity"`
	AdmissionConfigDigest Hash                   `json:"admission_config_digest"`
	Entries               []VolumeAdmissionEntry `json:"entries"`
}

func (p AdmissionOwnedPath) Validate() error {
	if len(string(p)) > 256 {
		return fmt.Errorf("%w: owned path exceeds 256 UTF-8 bytes", ErrInvalidValue)
	}
	return ValidateRelativePath(string(p))
}

func (entry VolumeAdmissionEntry) Validate() error {
	if _, err := ParseUUID(string(entry.OperationID)); err != nil {
		return err
	}
	if entry.ScopeKind != "home" && entry.ScopeKind != "worktree" {
		return fmt.Errorf("%w: unknown admission scope kind", ErrInvalidValue)
	}
	if entry.VolumeReservedRemainingBytes < 0 || entry.CacheReservedRemainingBytes < 0 || entry.VolumeReservedRemainingBytes > MaxSafeInteger || entry.CacheReservedRemainingBytes > MaxSafeInteger || entry.VolumeReservedRemainingBytes < entry.CacheReservedRemainingBytes {
		return fmt.Errorf("%w: volume reservation must cover cache reservation", ErrInvalidValue)
	}
	if err := entry.OwnershipRef.Validate(); err != nil {
		return err
	}
	if len(entry.AllowedPaths) < 1 || len(entry.AllowedPaths) > 8 {
		return fmt.Errorf("%w: allowed paths count outside 1..8", ErrInvalidValue)
	}
	seen := make(map[AdmissionOwnedPath]struct{}, len(entry.AllowedPaths))
	for _, ownedPath := range entry.AllowedPaths {
		if err := ownedPath.Validate(); err != nil {
			return err
		}
		if _, exists := seen[ownedPath]; exists {
			return fmt.Errorf("%w: duplicate allowed path", ErrInvalidValue)
		}
		seen[ownedPath] = struct{}{}
	}
	if entry.ScopeKind == "home" {
		if entry.WorktreeID != nil || entry.CacheReservedRemainingBytes != 0 || !oneOf(entry.OperationKind, "catalog_write", "config_write", "maintenance", "backup") {
			return fmt.Errorf("%w: invalid home admission ownership", ErrInvalidValue)
		}
	} else if entry.WorktreeID == nil || !oneOf(entry.OperationKind, "index", "map_import", "rename_result", "partition", "maintenance", "record_write", "evidence_import", "backup", "log_write") {
		return fmt.Errorf("%w: invalid worktree admission ownership", ErrInvalidValue)
	}
	if entry.WorktreeID != nil {
		if _, err := ParseUUID(string(*entry.WorktreeID)); err != nil {
			return err
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, entry.CreatedAt); err != nil {
		return fmt.Errorf("%w: invalid admission creation time", ErrInvalidValue)
	}
	return nil
}

func (journal VolumeAdmissionJournal) Validate() error {
	if journal.FormatVersion != 1 || len(journal.Entries) > 256 {
		return fmt.Errorf("%w: journal header or entry count invalid", ErrInvalidValue)
	}
	if _, err := ParseHash(string(journal.AdmissionConfigDigest)); err != nil {
		return err
	}
	seen := make(map[UUID]struct{}, len(journal.Entries))
	for _, entry := range journal.Entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if _, exists := seen[entry.OperationID]; exists {
			return fmt.Errorf("%w: duplicate admission operation ID", ErrInvalidValue)
		}
		seen[entry.OperationID] = struct{}{}
	}
	return nil
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}
