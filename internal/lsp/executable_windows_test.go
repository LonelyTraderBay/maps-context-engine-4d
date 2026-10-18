//go:build windows

package lsp

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCanonicalDirectoryAcceptsShortNamePath(t *testing.T) {
	longPath := t.TempDir()
	longName, err := windows.UTF16PtrFromString(longPath)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetShortPathName(longName, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 || length >= uint32(len(buffer)) {
		t.Skip("Windows short-name aliases are unavailable")
	}
	shortPath := windows.UTF16ToString(buffer[:length])
	if filepath.Clean(shortPath) == filepath.Clean(longPath) {
		t.Skip("temporary path has no short-name alias")
	}

	want, err := filepath.EvalSymlinks(longPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalDirectory(shortPath)
	if err != nil {
		t.Fatalf("canonicalDirectory rejected a short-name alias: %v", err)
	}
	if !samePath(got, want) {
		t.Fatalf("canonical directory=%q, want %q", got, want)
	}
}
