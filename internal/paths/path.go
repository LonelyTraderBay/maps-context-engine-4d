package paths

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxRelativePathBytes = 32 * 1024
	MaxPathSegments      = 128
	MaxSegmentRunes      = 255
)

var (
	ErrInvalidRelativePath = errors.New("invalid relative path")
	ErrUnsupportedEncoding = errors.New("unsupported path encoding")
)

// CaseMode records filesystem semantics that have been observed, not guessed.
type CaseMode uint8

const (
	CaseUnknown CaseMode = iota
	CaseSensitive
	CaseInsensitive
)

// ValidateRelative accepts the canonical slash-separated path form used in manifests.
func ValidateRelative(value string) error {
	if len(value) == 0 || len(value) > MaxRelativePathBytes {
		return ErrInvalidRelativePath
	}
	if !utf8.ValidString(value) {
		return ErrUnsupportedEncoding
	}
	if strings.HasPrefix(value, "/") || strings.Contains(value, `\`) {
		return ErrInvalidRelativePath
	}
	segments := strings.Split(value, "/")
	if len(segments) > MaxPathSegments {
		return ErrInvalidRelativePath
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || utf8.RuneCountInString(segment) > MaxSegmentRunes {
			return ErrInvalidRelativePath
		}
		if err := validatePlatformSegment(segment); err != nil {
			return err
		}
		for _, r := range segment {
			if unicode.IsControl(r) {
				return ErrInvalidRelativePath
			}
		}
	}
	return nil
}

type Collision struct {
	First  string
	Second string
}

// FindCaseFoldCollisions rejects distinct paths that collapse under Unicode simple folding.
func FindCaseFoldCollisions(values []string) ([]Collision, error) {
	seen := make(map[string]string, len(values))
	collisions := make([]Collision, 0)
	for _, value := range values {
		if err := ValidateRelative(value); err != nil {
			return nil, err
		}
		key := foldKey(value)
		if previous, ok := seen[key]; ok && previous != value {
			collisions = append(collisions, Collision{First: previous, Second: value})
			continue
		}
		seen[key] = value
	}
	return collisions, nil
}

func EqualFoldRune(left, right rune) bool {
	if left == right {
		return true
	}
	for folded := unicode.SimpleFold(left); folded != left; folded = unicode.SimpleFold(folded) {
		if folded == right {
			return true
		}
	}
	return false
}

func foldKey(value string) string {
	var out strings.Builder
	for _, r := range value {
		lowest := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < lowest {
				lowest = folded
			}
		}
		out.WriteRune(lowest)
	}
	return out.String()
}
