package contextpack

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var ErrFragmentBudgetTooSmall = errors.New("source fragment budget cannot fit the next UTF-8 code point")

// SourceFragment is one exact contiguous slice of a validated parent SourceRef.
// NextOffset is the only continuation progress and Complete is true only at the
// parent range end.
type SourceFragment struct {
	ItemID     model.Hash
	ParentRef  model.SourceRef
	SourceRef  model.SourceRef
	Content    []byte
	NextOffset int64
	Complete   bool
}

// NextSourceFragment returns one bounded fragment without materializing the
// remaining parent range. Offsets are absolute UTF-8 byte offsets in source.
func NextSourceFragment(parent model.SourceRef, source []byte, offset, maxBytes int64) (SourceFragment, error) {
	if err := parent.ValidateAgainst(source); err != nil {
		return SourceFragment{}, fmt.Errorf("validate source fragment parent: %w", err)
	}
	if offset < parent.Range.StartByte || offset >= parent.Range.EndByte || maxBytes < 1 ||
		offset > int64(len(source)) || (offset < int64(len(source)) && source[offset]&0xc0 == 0x80) {
		return SourceFragment{}, model.ErrInvalidRange
	}
	end := parent.Range.EndByte
	if end-offset > maxBytes {
		end = offset + maxBytes
		// Prefer the last complete line ending that fits. Including LF also
		// retains a preceding CR, so CRLF bytes are never normalized.
		if relative := bytes.LastIndexByte(source[offset:end], '\n'); relative >= 0 {
			end = offset + int64(relative) + 1
		} else {
			for end > offset && end < parent.Range.EndByte && source[end]&0xc0 == 0x80 {
				end--
			}
		}
	}
	if end <= offset || end-offset > maxBytes || end < parent.Range.EndByte && source[end]&0xc0 == 0x80 {
		return SourceFragment{}, ErrFragmentBudgetTooSmall
	}
	content := append([]byte(nil), source[offset:end]...)
	if !utf8.Valid(content) {
		return SourceFragment{}, model.ErrInvalidRange
	}
	fragmentRef := parent
	fragmentRef.Range = model.ByteRange{StartByte: offset, EndByte: end}
	itemID, err := model.HashValue("source-fragment-v1", map[string]any{
		"parent_ref": sourceRefValue(parent),
		"range":      map[string]any{"start_byte": offset, "end_byte": end},
	})
	if err != nil {
		return SourceFragment{}, fmt.Errorf("hash source fragment identity: %w", err)
	}
	return SourceFragment{
		ItemID: itemID, ParentRef: parent, SourceRef: fragmentRef,
		Content: content, NextOffset: end, Complete: end == parent.Range.EndByte,
	}, nil
}

func sourceRefValue(ref model.SourceRef) map[string]any {
	return map[string]any{
		"worktree_id":   ref.WorktreeID,
		"profile_id":    ref.ProfileID,
		"generation_id": ref.GenerationID,
		"path":          ref.Path,
		"content_hash":  ref.ContentHash,
		"range":         map[string]any{"start_byte": ref.Range.StartByte, "end_byte": ref.Range.EndByte},
	}
}
