package contextpack

import (
	"bytes"
	"errors"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestSourceFragmentsReassembleExactUTF8LaoEmojiAndCRLF(t *testing.T) {
	source := []byte("ພາສາລາວ\r\nA😀B\r\nlong-line-0123456789")
	parent := model.SourceRef{
		WorktreeID:   "11111111-1111-4111-8111-111111111111",
		ProfileID:    model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		GenerationID: "22222222-2222-4222-8222-222222222222",
		Path:         "src/ข้อความ.go", ContentHash: model.SHA256(source),
		Range: model.ByteRange{StartByte: 0, EndByte: int64(len(source))},
	}
	const capBytes int64 = 9
	var offset int64
	var joined []byte
	var ids = make(map[model.Hash]bool)
	for {
		fragment, err := NextSourceFragment(parent, source, offset, capBytes)
		if err != nil {
			t.Fatal(err)
		}
		if fragment.SourceRef.Range.StartByte != offset || fragment.SourceRef.Range.EndByte != fragment.NextOffset || int64(len(fragment.Content)) > capBytes {
			t.Fatalf("fragment does not preserve bounded contiguous range: %+v", fragment)
		}
		if ids[fragment.ItemID] {
			t.Fatalf("fragment identity repeated at offset %d", offset)
		}
		ids[fragment.ItemID] = true
		if err := fragment.SourceRef.ValidateAgainst(source); err != nil {
			t.Fatalf("fragment source reference failed validation: %v", err)
		}
		joined = append(joined, fragment.Content...)
		offset = fragment.NextOffset
		if fragment.Complete {
			break
		}
	}
	if !bytes.Equal(joined, source) || offset != int64(len(source)) {
		t.Fatalf("fragments did not reassemble exact source: got %x want %x", joined, source)
	}
}

func TestSourceFragmentRejectsInvalidBoundaryAndTooSmallUTF8Budget(t *testing.T) {
	source := []byte("a😀z")
	parent := model.SourceRef{
		WorktreeID:   "11111111-1111-4111-8111-111111111111",
		ProfileID:    model.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		GenerationID: "22222222-2222-4222-8222-222222222222",
		Path:         "src/main.go", ContentHash: model.SHA256(source),
		Range: model.ByteRange{StartByte: 0, EndByte: int64(len(source))},
	}
	if _, err := NextSourceFragment(parent, source, 2, 4); !errors.Is(err, model.ErrInvalidRange) {
		t.Fatalf("mid-codepoint continuation error=%v", err)
	}
	if _, err := NextSourceFragment(parent, source, 1, 3); !errors.Is(err, ErrFragmentBudgetTooSmall) {
		t.Fatalf("too-small emoji fragment budget error=%v", err)
	}
	parent.ContentHash = model.Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := NextSourceFragment(parent, source, 0, 8); !errors.Is(err, model.ErrInvalidValue) {
		t.Fatalf("changed parent content hash error=%v", err)
	}
}
