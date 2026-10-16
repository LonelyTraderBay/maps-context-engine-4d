package policy

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
)

const (
	MaxGlobBytes = 4096
	maxMatchOps  = 8_000_000
)

var (
	ErrInvalidGlob          = errors.New("invalid deny glob")
	ErrCaseSemanticsUnknown = errors.New("filesystem case semantics are unknown")
	ErrMatcherBudget        = errors.New("glob matcher budget exceeded")
)

type globSegment struct {
	value      []rune
	doublestar bool
}

// Pattern is immutable after compilation and uses only a bounded, non-regex matcher.
type Pattern struct {
	segments  []globSegment
	caseMode  paths.CaseMode
	asciiOnly bool
}

func Compile(pattern string, mode paths.CaseMode) (Pattern, error) {
	if mode == paths.CaseUnknown {
		return Pattern{}, ErrCaseSemanticsUnknown
	}
	return compile(pattern, mode, false)
}

func compileBuiltin(pattern string) (Pattern, error) {
	return compile(pattern, paths.CaseInsensitive, true)
}

func compile(pattern string, mode paths.CaseMode, asciiOnly bool) (Pattern, error) {
	if len(pattern) == 0 || len(pattern) > MaxGlobBytes || !utf8.ValidString(pattern) || strings.HasPrefix(pattern, "/") || strings.HasSuffix(pattern, "/") {
		return Pattern{}, ErrInvalidGlob
	}
	if strings.ContainsAny(pattern, `\\:[]{}!"<>|`) {
		return Pattern{}, ErrInvalidGlob
	}
	rawSegments := strings.Split(pattern, "/")
	if len(rawSegments) > paths.MaxPathSegments {
		return Pattern{}, ErrInvalidGlob
	}
	segments := make([]globSegment, 0, len(rawSegments))
	for _, raw := range rawSegments {
		if raw == "" || raw == "." || raw == ".." || utf8.RuneCountInString(raw) > paths.MaxSegmentRunes {
			return Pattern{}, ErrInvalidGlob
		}
		if strings.Contains(raw, "**") && raw != "**" {
			return Pattern{}, ErrInvalidGlob
		}
		for _, r := range raw {
			if unicode.IsControl(r) {
				return Pattern{}, ErrInvalidGlob
			}
		}
		segments = append(segments, globSegment{value: []rune(raw), doublestar: raw == "**"})
	}
	return Pattern{segments: segments, caseMode: mode, asciiOnly: asciiOnly}, nil
}

func (p Pattern) Match(value string) (bool, error) {
	if err := paths.ValidateRelative(value); err != nil {
		return false, err
	}
	pathSegments := strings.Split(value, "/")
	budget := 0
	previous := make([]bool, len(pathSegments)+1)
	previous[0] = true
	for _, patternSegment := range p.segments {
		next := make([]bool, len(pathSegments)+1)
		if patternSegment.doublestar {
			next[0] = previous[0]
			for pathIndex := 1; pathIndex <= len(pathSegments); pathIndex++ {
				budget++
				if budget > maxMatchOps {
					return false, ErrMatcherBudget
				}
				next[pathIndex] = previous[pathIndex] || next[pathIndex-1]
			}
			previous = next
			continue
		}
		for pathIndex := 1; pathIndex <= len(pathSegments); pathIndex++ {
			if !previous[pathIndex-1] {
				continue
			}
			matched, err := p.matchSegment(patternSegment.value, []rune(pathSegments[pathIndex-1]), &budget)
			if err != nil {
				return false, err
			}
			next[pathIndex] = matched
		}
		previous = next
	}
	return previous[len(pathSegments)], nil
}

func (p Pattern) matchSegment(pattern, value []rune, budget *int) (bool, error) {
	previous := make([]bool, len(value)+1)
	previous[0] = true
	for _, token := range pattern {
		next := make([]bool, len(value)+1)
		if token == '*' {
			next[0] = previous[0]
			for index := 1; index <= len(value); index++ {
				*budget++
				if *budget > maxMatchOps {
					return false, ErrMatcherBudget
				}
				next[index] = previous[index] || next[index-1]
			}
		} else {
			for index, char := range value {
				*budget++
				if *budget > maxMatchOps {
					return false, ErrMatcherBudget
				}
				equal := token == '?' || p.equalRune(token, char)
				next[index+1] = previous[index] && equal
			}
		}
		previous = next
	}
	return previous[len(value)], nil
}

func (p Pattern) equalRune(left, right rune) bool {
	if left == right {
		return true
	}
	if p.asciiOnly {
		if left >= 'A' && left <= 'Z' {
			left += 'a' - 'A'
		}
		if right >= 'A' && right <= 'Z' {
			right += 'a' - 'A'
		}
		return left == right
	}
	return p.caseMode == paths.CaseInsensitive && paths.EqualFoldRune(left, right)
}
