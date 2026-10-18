//go:build !windows

package lsp

import (
	"os"
)

func isReparsePoint(path string) bool {
	info, err := os.Lstat(path)
	return err != nil || info.Mode()&os.ModeSymlink != 0
}
