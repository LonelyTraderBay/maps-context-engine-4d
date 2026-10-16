//go:build windows

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"golang.org/x/sys/windows"
)

type caseSensitiveInfo struct {
	Flags uint32
}

func OpenRoot(path string) (*Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrOpenFailed
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, ErrOpenFailed
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, ErrOpenFailed
	}
	info, err := fileInfo(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, ErrOpenFailed
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, ErrReparsePoint
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(handle)
		return nil, ErrNotDirectory
	}
	finalPath, err := finalPathForHandle(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, ErrOpenFailed
	}
	return &Root{handle: uintptr(handle), finalPath: finalPath}, nil
}

func duplicateRootHandle(handle uintptr) (uintptr, error) {
	var duplicate windows.Handle
	err := windows.DuplicateHandle(
		windows.CurrentProcess(), windows.Handle(handle), windows.CurrentProcess(),
		&duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS,
	)
	return uintptr(duplicate), err
}

func closeRootHandle(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	return windows.CloseHandle(windows.Handle(handle))
}

func caseModeForHandle(handle uintptr) (paths.CaseMode, error) {
	var info caseSensitiveInfo
	err := windows.GetFileInformationByHandleEx(
		windows.Handle(handle), windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	)
	if err != nil || info.Flags&^uint32(windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR) != 0 {
		return paths.CaseUnknown, ErrUnsupportedOS
	}
	if info.Flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0 {
		return paths.CaseSensitive, nil
	}
	return paths.CaseInsensitive, nil
}

func openRelative(rootHandle uintptr, rootPath, relative string) (*os.File, func() error, error) {
	segments := strings.Split(relative, "/")
	parent := windows.Handle(rootHandle)
	ownedDirectories := make([]windows.Handle, 0, len(segments))
	cleanup := func() error {
		var firstErr error
		for index := len(ownedDirectories) - 1; index >= 0; index-- {
			if err := windows.CloseHandle(ownedDirectories[index]); firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	for index, segment := range segments {
		caseMode, err := caseModeForHandle(uintptr(parent))
		if err != nil {
			_ = cleanup()
			return nil, nil, ErrUnsupportedOS
		}
		attributes := uint32(0)
		if caseMode == paths.CaseInsensitive {
			attributes = windows.OBJ_CASE_INSENSITIVE
		}
		name, err := windows.NewNTUnicodeString(segment)
		if err != nil {
			_ = cleanup()
			return nil, nil, ErrOpenFailed
		}
		object := &windows.OBJECT_ATTRIBUTES{
			Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
			RootDirectory: parent,
			ObjectName:    name,
			Attributes:    attributes,
		}
		access := uint32(windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
		options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
		if index < len(segments)-1 {
			access |= windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE
			options |= windows.FILE_DIRECTORY_FILE
		} else {
			access |= windows.FILE_READ_DATA
			options |= windows.FILE_NON_DIRECTORY_FILE
		}
		var opened windows.Handle
		var status windows.IO_STATUS_BLOCK
		err = windows.NtCreateFile(
			&opened, access, object, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ, windows.FILE_OPEN, options, 0, 0,
		)
		if err != nil {
			_ = cleanup()
			return nil, nil, ErrOpenFailed
		}
		info, err := fileInfo(opened)
		if err != nil {
			_ = windows.CloseHandle(opened)
			_ = cleanup()
			return nil, nil, ErrOpenFailed
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			_ = windows.CloseHandle(opened)
			_ = cleanup()
			return nil, nil, ErrReparsePoint
		}
		if index < len(segments)-1 {
			if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
				_ = windows.CloseHandle(opened)
				_ = cleanup()
				return nil, nil, ErrNotDirectory
			}
			ownedDirectories = append(ownedDirectories, opened)
			parent = opened
			continue
		}
		if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 {
			_ = windows.CloseHandle(opened)
			_ = cleanup()
			return nil, nil, ErrNotRegularFile
		}
		actualPath, err := finalPathForHandle(opened)
		if err != nil || !withinRoot(rootPath, actualPath) {
			_ = windows.CloseHandle(opened)
			_ = cleanup()
			return nil, nil, ErrPathEscape
		}
		file := os.NewFile(uintptr(opened), "")
		if file == nil {
			_ = windows.CloseHandle(opened)
			_ = cleanup()
			return nil, nil, ErrOpenFailed
		}
		return file, cleanup, nil
	}
	_ = cleanup()
	return nil, nil, errors.New("empty secure path")
}

func fileInfo(handle windows.Handle) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(handle, &info)
	return info, err
}

func finalPathForHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil || length == 0 || length >= uint32(len(buffer)) {
		return "", errors.New("final path unavailable")
	}
	return windows.UTF16ToString(buffer[:length]), nil
}

func withinRoot(root, candidate string) bool {
	root = strings.TrimRight(root, `\`)
	if len(candidate) <= len(root) || !strings.EqualFold(candidate[:len(root)], root) {
		return false
	}
	return candidate[len(root)] == '\\'
}
