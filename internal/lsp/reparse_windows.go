//go:build windows

package lsp

import "golang.org/x/sys/windows"

func isReparsePoint(path string) bool {
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	return err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
