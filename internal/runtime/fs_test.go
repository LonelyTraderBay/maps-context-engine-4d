package runtime

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenRegularFileCannotBeRedirectedByDirectoryReplacement(t *testing.T) {
	rootPath := t.TempDir()
	outsidePath := t.TempDir()
	pivotPath := filepath.Join(rootPath, "pivot")
	if err := os.Mkdir(pivotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	insideBytes := []byte("opened while contained\n")
	if err := os.WriteFile(filepath.Join(pivotPath, "entry.go"), insideBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsidePath, "outside.go"), []byte("outside bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	opened, err := root.openRegularFile("pivot/entry.go")
	if err != nil {
		t.Fatalf("open contained file: %v", err)
	}
	defer opened.Close()

	movedPath := filepath.Join(rootPath, "pivot-moved")
	renameErr := os.Rename(pivotPath, movedPath)
	if runtime.GOOS == "windows" && renameErr == nil {
		t.Fatal("rename succeeded while the no-delete directory handle was retained")
	}
	if runtime.GOOS != "windows" && renameErr != nil {
		t.Fatalf("rename synthetic directory on Linux: %v", renameErr)
	}
	if renameErr == nil {
		if err := os.Symlink(outsidePath, pivotPath); err != nil {
			t.Fatalf("replace directory with symbolic link: %v", err)
		}
		defer os.Remove(pivotPath)
		if _, err := root.openRegularFile("pivot/outside.go"); err == nil {
			t.Fatal("replacement symbolic link was followed")
		}
	}

	data, err := io.ReadAll(opened)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(insideBytes) {
		t.Fatalf("open handle read %q after path replacement", data)
	}
}
