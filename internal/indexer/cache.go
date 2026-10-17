package indexer

import (
	"fmt"

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
