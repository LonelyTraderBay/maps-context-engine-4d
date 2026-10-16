//go:build windows

package runtime

import (
	"errors"
	"testing"
)

func TestAvailableBytesRejectsPathNotBoundToRootHandle(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	finalPath, err := root.FinalPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := availableBytesForHandle(root.handle, finalPath+`\different`); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("headroom lookup for a path unrelated to the open root returned %v; want ErrPathEscape", err)
	}
}
