//go:build windows

package paths

import (
	"strings"
	"unicode/utf8"
)

func validatePlatformSegment(segment string) error {
	if strings.ContainsAny(segment, `:*?"<>|`) || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") || isWindowsDeviceName(segment) {
		return ErrInvalidRelativePath
	}
	if utf8.RuneCountInString(segment) > MaxSegmentRunes {
		return ErrInvalidRelativePath
	}
	return nil
}

func isWindowsDeviceName(segment string) bool {
	base := strings.TrimRight(segment, ". ")
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	base = strings.ToLower(base)
	switch base {
	case "con", "prn", "aux", "nul", "conin$", "conout$", "clock$":
		return true
	}
	runes := []rune(base)
	if len(runes) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) {
		last := runes[3]
		return last >= '1' && last <= '9' || last == '¹' || last == '²' || last == '³'
	}
	return false
}
