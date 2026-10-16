//go:build !windows

package paths

func validatePlatformSegment(string) error { return nil }
