// Package snapshot builds immutable content identities for one captured worktree.
package snapshot

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type ScopeStatus string

const (
	ScopeComplete ScopeStatus = "complete"
	ScopePartial  ScopeStatus = "partial"
	ScopeUnknown  ScopeStatus = "unknown"
)

type HeadState string

const (
	HeadPresent  HeadState = "present"
	HeadDetached HeadState = "detached"
	HeadUnborn   HeadState = "unborn"
)

const (
	FileCaptured     = "captured"
	FileMetadataOnly = "metadata_only"
	FileExcluded     = "excluded"
	FileMissing      = "missing"
	FileReadError    = "read_error"
	maxManifestFiles = 1_000_000
)

// GitState is descriptive provenance. HEAD and branch do not replace content
// identity and are intentionally excluded from CaptureManifest.SnapshotDigest.
type GitState struct {
	Head      string
	HeadState HeadState
	Branch    string
}

func (state GitState) Validate() error {
	if !utf8.ValidString(state.Branch) || len(state.Branch) > 4096 || strings.ContainsAny(state.Branch, "\x00\r\n") {
		return fmt.Errorf("invalid Git branch metadata: %w", model.ErrInvalidValue)
	}
	switch state.HeadState {
	case HeadPresent:
		if !validObjectID(state.Head) {
			return fmt.Errorf("present Git HEAD is not a 40- or 64-character lowercase object ID: %w", model.ErrInvalidValue)
		}
		if state.Branch == "" {
			return fmt.Errorf("present Git HEAD needs a branch name: %w", model.ErrInvalidValue)
		}
	case HeadDetached:
		if !validObjectID(state.Head) {
			return fmt.Errorf("detached Git HEAD is not a 40- or 64-character lowercase object ID: %w", model.ErrInvalidValue)
		}
		if state.Branch != "" {
			return fmt.Errorf("detached Git HEAD cannot have a branch name: %w", model.ErrInvalidValue)
		}
	case HeadUnborn:
		if state.Head != "" {
			return fmt.Errorf("unborn Git repository cannot have a HEAD hash: %w", model.ErrInvalidValue)
		}
	default:
		return fmt.Errorf("unknown Git HEAD state %q: %w", state.HeadState, model.ErrInvalidValue)
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

// FileInput contains captured bytes only for files whose status is captured.
// It deliberately has no mtime, size hint, or Git-clean field to trust.
type FileInput struct {
	Path       string
	Status     string
	Content    []byte
	ReasonCode *model.Identifier
}

type Input struct {
	SchemaVersion          int64
	WorktreeID             model.UUID
	ProfileID              model.Hash
	PolicyDigest           model.Hash
	ProviderDigest         model.Hash
	DependencyInputDigests []model.Hash
	Files                  []FileInput
	Git                    GitState
	Scope                  ScopeStatus
}

// Snapshot holds a canonical capture manifest plus Git provenance and the
// caller's explicit statement about whether the observed scope is complete.
type Snapshot struct {
	Manifest model.CaptureManifest
	Git      GitState
	Scope    ScopeStatus
}

func Build(input Input) (Snapshot, error) {
	if input.SchemaVersion != 1 {
		return Snapshot{}, fmt.Errorf("unsupported snapshot schema version: %w", model.ErrInvalidValue)
	}
	if int64(len(input.Files)) > maxManifestFiles || int64(len(input.DependencyInputDigests)) > maxManifestFiles {
		return Snapshot{}, fmt.Errorf("snapshot input count exceeds 1000000: %w", model.ErrInvalidValue)
	}
	if err := input.Git.Validate(); err != nil {
		return Snapshot{}, err
	}
	if input.Scope != ScopeComplete && input.Scope != ScopePartial && input.Scope != ScopeUnknown {
		return Snapshot{}, fmt.Errorf("invalid snapshot scope status %q: %w", input.Scope, model.ErrInvalidValue)
	}

	files := make([]model.ManifestFile, 0, len(input.Files))
	seen := make(map[string]struct{}, len(input.Files))
	for _, file := range input.Files {
		if err := model.ValidateRelativePath(file.Path); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot file %q: %w", file.Path, err)
		}
		if _, exists := seen[file.Path]; exists {
			return Snapshot{}, fmt.Errorf("duplicate snapshot path %q: %w", file.Path, model.ErrInvalidValue)
		}
		seen[file.Path] = struct{}{}

		manifestFile := model.ManifestFile{Path: file.Path, Status: file.Status, ReasonCode: cloneIdentifier(file.ReasonCode)}
		if file.Status == FileCaptured {
			if file.Content == nil {
				return Snapshot{}, fmt.Errorf("captured file %q has no byte buffer: %w", file.Path, model.ErrInvalidValue)
			}
			hash := model.SHA256(file.Content)
			length := int64(len(file.Content))
			manifestFile.ContentHash = &hash
			manifestFile.ByteLength = &length
		} else if file.Content != nil {
			return Snapshot{}, fmt.Errorf("non-captured file %q must not retain source bytes: %w", file.Path, model.ErrInvalidValue)
		}
		files = append(files, manifestFile)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	dependencies := make([]model.Hash, len(input.DependencyInputDigests))
	copy(dependencies, input.DependencyInputDigests)
	manifest := model.CaptureManifest{
		SchemaVersion:          input.SchemaVersion,
		WorktreeID:             input.WorktreeID,
		ProfileID:              input.ProfileID,
		PolicyDigest:           input.PolicyDigest,
		ProviderDigest:         input.ProviderDigest,
		Files:                  files,
		DependencyInputDigests: dependencies,
	}
	digest, err := manifest.ComputeSnapshotDigest()
	if err != nil {
		return Snapshot{}, fmt.Errorf("compute content snapshot digest: %w", err)
	}
	manifest.SnapshotDigest = digest
	if err := manifest.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("validate content snapshot: %w", err)
	}
	return Snapshot{Manifest: manifest, Git: input.Git, Scope: input.Scope}, nil
}

func (snapshot Snapshot) Validate() error {
	if err := snapshot.Manifest.Validate(); err != nil {
		return fmt.Errorf("invalid capture manifest: %w", err)
	}
	if err := snapshot.Git.Validate(); err != nil {
		return err
	}
	if snapshot.Scope != ScopeComplete && snapshot.Scope != ScopePartial && snapshot.Scope != ScopeUnknown {
		return fmt.Errorf("invalid snapshot scope status %q: %w", snapshot.Scope, model.ErrInvalidValue)
	}
	return nil
}

func cloneIdentifier(value *model.Identifier) *model.Identifier {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
