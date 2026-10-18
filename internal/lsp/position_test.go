package lsp

import (
	"errors"
	"testing"
)

func TestPositionEncodingRoundTripsUnicodeAndCRLF(t *testing.T) {
	source := []byte("aé😀\r\nສາຍ\n")
	tests := []struct {
		name     string
		encoding PositionEncoding
		byte     int64
		position Position
	}{
		{name: "utf16 emoji end", encoding: PositionUTF16, byte: 7, position: Position{Line: 0, Character: 4}},
		{name: "utf8 emoji end", encoding: PositionUTF8, byte: 7, position: Position{Line: 0, Character: 7}},
		{name: "utf32 emoji end", encoding: PositionUTF32, byte: 7, position: Position{Line: 0, Character: 3}},
		{name: "next line after CRLF", encoding: PositionUTF16, byte: 9, position: Position{Line: 1, Character: 0}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			position, err := ByteOffsetToPosition(source, test.byte, test.encoding)
			if err != nil || position != test.position {
				t.Fatalf("ByteOffsetToPosition=%+v, %v; want %+v", position, err, test.position)
			}
			offset, err := PositionToByteOffset(source, test.position, test.encoding)
			if err != nil || offset != test.byte {
				t.Fatalf("PositionToByteOffset=%d, %v; want %d", offset, err, test.byte)
			}
		})
	}
}

func TestPositionRejectsInvalidUTF8AndUnrepresentableBoundaries(t *testing.T) {
	source := []byte("é😀\r\nx")
	if _, err := ByteOffsetToPosition(source, 1, PositionUTF16); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("UTF-8 interior byte accepted: %v", err)
	}
	if _, err := ByteOffsetToPosition(source, 7, PositionUTF16); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("CR/LF interior byte accepted: %v", err)
	}
	if _, err := PositionToByteOffset(source, Position{Line: 0, Character: 2}, PositionUTF16); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("UTF-16 surrogate interior accepted: %v", err)
	}
	if _, err := PositionToByteOffset(source, Position{Line: 0, Character: 99}, PositionUTF8); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("out-of-range character accepted: %v", err)
	}
	if _, err := ByteOffsetToPosition([]byte{0xff}, 0, PositionUTF8); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("invalid source UTF-8 accepted: %v", err)
	}
}

func TestPositionRejectsUnknownEncoding(t *testing.T) {
	if _, err := ByteOffsetToPosition([]byte("x"), 0, PositionEncoding("utf-64")); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("unknown encoding error=%v", err)
	}
}
