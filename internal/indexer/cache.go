package indexer

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/snapshot"
)

// IncrementalMode describes whether an index can skip semantic work. A content
// only result is limited to identical, complete snapshots; any semantic input
// change keeps the conservative full rebuild until differential proof exists.
type IncrementalMode string

const (
	IncrementalContentOnly IncrementalMode = "content_only"
	IncrementalFull        IncrementalMode = "full"
)

// SyntaxCacheInput identifies parser output only. The key is path-specific
// because source positions carry a repository-relative file identity.
type SyntaxCacheInput struct {
	Path                  string
	ContentDigest         model.Hash
	Language              model.Identifier
	ProviderDigest        model.Hash
	CompilerConfigDigest  model.Hash
	InterpretationVersion int64
}

// SyntaxCacheKey returns a deterministic key for syntax facts. It deliberately
// does not imply that semantic facts or live freshness can be reused.
func SyntaxCacheKey(input SyntaxCacheInput) (model.Hash, error) {
	if err := model.ValidateRelativePath(input.Path); err != nil {
		return "", fmt.Errorf("syntax cache path: %w", err)
	}
	if err := validateCacheIdentifier(input.Language, "language"); err != nil {
		return "", err
	}
	for _, item := range []struct {
		name  string
		value model.Hash
	}{
		{name: "content digest", value: input.ContentDigest},
		{name: "provider digest", value: input.ProviderDigest},
		{name: "compiler config digest", value: input.CompilerConfigDigest},
	} {
		if _, err := model.ParseHash(string(item.value)); err != nil {
			return "", fmt.Errorf("syntax cache %s: %w", item.name, err)
		}
	}
	if input.InterpretationVersion < 1 || input.InterpretationVersion > model.MaxSafeInteger {
		return "", fmt.Errorf("syntax cache interpretation version: %w", model.ErrInvalidValue)
	}
	return model.HashValue("syntax-cache-v1", map[string]any{
		"path": input.Path, "content_digest": input.ContentDigest,
		"language": input.Language, "provider_digest": input.ProviderDigest,
		"compiler_config_digest": input.CompilerConfigDigest,
		"interpretation_version": input.InterpretationVersion,
	})
}

// SemanticCacheInput includes every effective input that can change semantic
// facts, including package membership for negative lookups and dependency
// exports for import resolution. Generation-scoped IDs prevent cross-generation
// facts from escaping their source binding.
type SemanticCacheInput struct {
	WorktreeID              model.UUID
	GenerationID            model.UUID
	PartitionID             model.Hash
	InputDigest             model.Hash
	ProfileDigest           model.Hash
	PolicyDigest            model.Hash
	ProviderDigest          model.Hash
	CompilerConfigDigest    model.Hash
	MembershipDigest        model.Hash
	DependencyExportDigests []model.Hash
	InterpretationVersion   int64
}

// SemanticCacheKey is safe only for a consumer that also binds the resulting
// facts to the exact generation and profile in this input.
func SemanticCacheKey(input SemanticCacheInput) (model.Hash, error) {
	for _, item := range []struct {
		name  string
		value model.UUID
	}{
		{name: "worktree ID", value: input.WorktreeID},
		{name: "generation ID", value: input.GenerationID},
	} {
		if _, err := model.ParseUUID(string(item.value)); err != nil {
			return "", fmt.Errorf("semantic cache %s: %w", item.name, err)
		}
	}
	for _, item := range []struct {
		name  string
		value model.Hash
	}{
		{name: "partition ID", value: input.PartitionID},
		{name: "input digest", value: input.InputDigest},
		{name: "profile digest", value: input.ProfileDigest},
		{name: "policy digest", value: input.PolicyDigest},
		{name: "provider digest", value: input.ProviderDigest},
		{name: "compiler config digest", value: input.CompilerConfigDigest},
		{name: "membership digest", value: input.MembershipDigest},
	} {
		if _, err := model.ParseHash(string(item.value)); err != nil {
			return "", fmt.Errorf("semantic cache %s: %w", item.name, err)
		}
	}
	if input.InterpretationVersion < 1 || input.InterpretationVersion > model.MaxSafeInteger {
		return "", fmt.Errorf("semantic cache interpretation version: %w", model.ErrInvalidValue)
	}
	dependencies := append([]model.Hash(nil), input.DependencyExportDigests...)
	slices.Sort(dependencies)
	dependencyValues := make([]any, len(dependencies))
	for index, digest := range dependencies {
		if _, err := model.ParseHash(string(digest)); err != nil {
			return "", fmt.Errorf("semantic cache dependency export: %w", err)
		}
		if index > 0 && dependencies[index-1] == digest {
			return "", fmt.Errorf("semantic cache dependency exports must be unique: %w", model.ErrInvalidValue)
		}
		dependencyValues[index] = digest
	}
	return model.HashValue("semantic-cache-v1", map[string]any{
		"worktree_id": input.WorktreeID, "generation_id": input.GenerationID,
		"partition_id": input.PartitionID, "input_digest": input.InputDigest,
		"profile_digest": input.ProfileDigest, "policy_digest": input.PolicyDigest,
		"provider_digest": input.ProviderDigest, "compiler_config_digest": input.CompilerConfigDigest,
		"membership_digest": input.MembershipDigest, "dependency_export_digests": dependencyValues,
		"interpretation_version": input.InterpretationVersion,
	})
}

func validateCacheIdentifier(value model.Identifier, name string) error {
	if len(value) == 0 || !utf8.ValidString(string(value)) || strings.TrimSpace(string(value)) != string(value) {
		return fmt.Errorf("cache %s: %w", name, model.ErrInvalidValue)
	}
	return nil
}

// ReusableContent is an exact-path blob identity that a caller may resolve
// from its captured-content cache. It never authorizes reusing semantic facts.
type ReusableContent struct {
	Path        string
	ContentHash model.Hash
	ByteLength  int64
}

// IncrementalPlan separates safe captured-blob reuse from semantic
// invalidation. ReusableContent can be non-empty while IncrementalMode is full.
type IncrementalPlan struct {
	IncrementalMode IncrementalMode
	Invalidation    InvalidationPlan
	ReusableContent []ReusableContent
}

// PlanIncremental identifies raw captured content that is byte-identical at
// the same path and chooses a full semantic rebuild for every changed snapshot.
// Rename matching, partial-scope no-ops, and selective semantic reuse are not
// inferred here.
func PlanIncremental(previous, current *snapshot.Snapshot) (IncrementalPlan, error) {
	invalidation, err := CompareSnapshots(previous, current)
	if err != nil {
		return IncrementalPlan{}, fmt.Errorf("plan snapshot invalidation: %w", err)
	}

	mode := IncrementalFull
	if invalidation.Mode == InvalidationNoop {
		mode = IncrementalContentOnly
	}
	return IncrementalPlan{
		IncrementalMode: mode,
		Invalidation:    invalidation,
		ReusableContent: reusableContent(previous, current),
	}, nil
}

func reusableContent(previous, current *snapshot.Snapshot) []ReusableContent {
	if previous == nil || current == nil {
		return []ReusableContent{}
	}

	previousFiles := make(map[string]model.ManifestFile, len(previous.Manifest.Files))
	for _, file := range previous.Manifest.Files {
		previousFiles[file.Path] = file
	}

	reusable := make([]ReusableContent, 0)
	for _, currentFile := range current.Manifest.Files {
		previousFile, exists := previousFiles[currentFile.Path]
		if !exists || !sameCapturedContent(previousFile, currentFile) {
			continue
		}
		reusable = append(reusable, ReusableContent{
			Path: currentFile.Path, ContentHash: *currentFile.ContentHash, ByteLength: *currentFile.ByteLength,
		})
	}
	return reusable
}

func sameCapturedContent(previous, current model.ManifestFile) bool {
	return previous.Path == current.Path &&
		previous.Status == snapshot.FileCaptured && current.Status == snapshot.FileCaptured &&
		previous.ContentHash != nil && current.ContentHash != nil &&
		*previous.ContentHash == *current.ContentHash &&
		previous.ByteLength != nil && current.ByteLength != nil &&
		*previous.ByteLength == *current.ByteLength
}
