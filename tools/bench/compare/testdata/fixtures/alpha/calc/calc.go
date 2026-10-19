package calc

import "strings"

func Add(left, right int) int { return left + right }

func Clamp(value, low, high int) int {
	if value < low { return low }
	if value > high { return high }
	return value
}

func NormalizeCode(value string) string { return strings.TrimSpace(strings.ToUpper(value)) }
