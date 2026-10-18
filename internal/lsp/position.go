package lsp

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

type PositionEncoding string

const (
	PositionUTF8  PositionEncoding = "utf-8"
	PositionUTF16 PositionEncoding = "utf-16"
	PositionUTF32 PositionEncoding = "utf-32"
)

var (
	ErrInvalidPosition     = errors.New("invalid LSP position")
	ErrUnsupportedEncoding = errors.New("unsupported LSP position encoding")
)

type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

func ValidatePositionEncoding(encoding PositionEncoding) error {
	switch encoding {
	case PositionUTF8, PositionUTF16, PositionUTF32:
		return nil
	default:
		return ErrUnsupportedEncoding
	}
}

// ByteOffsetToPosition maps an exact UTF-8 byte boundary to a negotiated LSP
// position. A point between CR and LF is deliberately not representable.
func ByteOffsetToPosition(source []byte, offset int64, encoding PositionEncoding) (Position, error) {
	if ValidatePositionEncoding(encoding) != nil {
		return Position{}, ErrUnsupportedEncoding
	}
	if !utf8.Valid(source) || offset < 0 || offset > int64(len(source)) {
		return Position{}, ErrInvalidPosition
	}
	line, start := uint32(0), 0
	for index, value := range source {
		if value != '\n' {
			continue
		}
		end := index
		if end > start && source[end-1] == '\r' {
			end--
		}
		current := int(offset)
		if current <= end {
			character, err := encodedLength(source[start:current], encoding)
			if err != nil {
				return Position{}, err
			}
			return Position{Line: line, Character: character}, nil
		}
		if current <= index {
			return Position{}, ErrInvalidPosition
		}
		line++
		start = index + 1
	}
	current := int(offset)
	if current < start || current > len(source) {
		return Position{}, ErrInvalidPosition
	}
	character, err := encodedLength(source[start:current], encoding)
	if err != nil {
		return Position{}, err
	}
	return Position{Line: line, Character: character}, nil
}

// PositionToByteOffset maps an LSP position only when it lands on a complete
// UTF-8 code point; UTF-16 positions inside a surrogate pair are rejected.
func PositionToByteOffset(source []byte, position Position, encoding PositionEncoding) (int64, error) {
	if ValidatePositionEncoding(encoding) != nil {
		return 0, ErrUnsupportedEncoding
	}
	if !utf8.Valid(source) {
		return 0, ErrInvalidPosition
	}
	line, start := uint32(0), 0
	for index, value := range source {
		if value != '\n' {
			continue
		}
		end := index
		if end > start && source[end-1] == '\r' {
			end--
		}
		if line == position.Line {
			return byteOffsetInLine(source[start:end], start, position.Character, encoding)
		}
		line++
		start = index + 1
	}
	if line == position.Line {
		return byteOffsetInLine(source[start:], start, position.Character, encoding)
	}
	return 0, fmt.Errorf("line %d is outside the document: %w", position.Line, ErrInvalidPosition)
}

func encodedLength(source []byte, encoding PositionEncoding) (uint32, error) {
	if !utf8.Valid(source) {
		return 0, ErrInvalidPosition
	}
	var units uint64
	for _, value := range string(source) {
		switch encoding {
		case PositionUTF8:
			units += uint64(utf8.RuneLen(value))
		case PositionUTF16:
			if value > 0xffff {
				units += 2
			} else {
				units++
			}
		case PositionUTF32:
			units++
		default:
			return 0, ErrUnsupportedEncoding
		}
		if units > uint64(^uint32(0)) {
			return 0, ErrInvalidPosition
		}
	}
	return uint32(units), nil
}

func byteOffsetInLine(line []byte, lineStart int, character uint32, encoding PositionEncoding) (int64, error) {
	var units uint64
	for offset := 0; offset < len(line); {
		if units == uint64(character) {
			return int64(lineStart + offset), nil
		}
		value, size := utf8.DecodeRune(line[offset:])
		if value == utf8.RuneError && size == 1 {
			return 0, ErrInvalidPosition
		}
		var width uint64
		switch encoding {
		case PositionUTF8:
			width = uint64(size)
		case PositionUTF16:
			width = 1
			if value > 0xffff {
				width = 2
			}
		case PositionUTF32:
			width = 1
		default:
			return 0, ErrUnsupportedEncoding
		}
		if units+width > uint64(character) {
			return 0, ErrInvalidPosition
		}
		units += width
		offset += size
	}
	if units == uint64(character) {
		return int64(lineStart + len(line)), nil
	}
	return 0, ErrInvalidPosition
}
