//go:build windows

package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE | windows.FILE_GENERIC_WRITE)
	handle, err := windows.CreateFile(
		name,
		access,
		// Root-level atomic files need directory updates while the verified
		// handle remains open. Keep delete sharing disabled so the root cannot
		// be renamed or removed until Close.
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		handle, err = windows.CreateFile(
			name,
			windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
	}
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
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN, options, 0, 0,
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
		if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 || info.NumberOfLinks != 1 {
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

func identityForHandle(handle uintptr) (string, string, error) {
	info, err := fileInfo(windows.Handle(handle))
	if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", "", ErrOpenFailed
	}
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fmt.Sprintf("windows-volume-%08x", info.VolumeSerialNumber),
		fmt.Sprintf("windows-home-%08x-%016x", info.VolumeSerialNumber, fileIndex), nil
}

func availableBytesForHandle(handle uintptr, finalPath string) (int64, error) {
	actualPath, err := finalPathForHandle(windows.Handle(handle))
	if err != nil || !strings.EqualFold(actualPath, finalPath) {
		return 0, ErrPathEscape
	}
	name, err := windows.UTF16PtrFromString(actualPath)
	if err != nil {
		return 0, ErrOpenFailed
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil || available > uint64(^uint64(0)>>1) {
		return 0, ErrUnsupportedOS
	}
	return int64(available), nil
}

func makeRelativeDirs(rootHandle uintptr, rootPath, relative string) error {
	segments := strings.Split(relative, "/")
	parent := windows.Handle(rootHandle)
	owned := make([]windows.Handle, 0, len(segments))
	cleanup := func() {
		for index := len(owned) - 1; index >= 0; index-- {
			_ = windows.CloseHandle(owned[index])
		}
	}
	for _, segment := range segments {
		caseMode, err := caseModeForHandle(uintptr(parent))
		if err != nil {
			cleanup()
			return ErrUnsupportedOS
		}
		attributes := uint32(0)
		if caseMode == paths.CaseInsensitive {
			attributes = windows.OBJ_CASE_INSENSITIVE
		}
		name, err := windows.NewNTUnicodeString(segment)
		if err != nil {
			cleanup()
			return ErrOpenFailed
		}
		object := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: name, Attributes: attributes}
		var opened windows.Handle
		var status windows.IO_STATUS_BLOCK
		err = windows.NtCreateFile(&opened,
			windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			object, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN_IF,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
		if err != nil {
			cleanup()
			return fmt.Errorf("open private directory: %w", ErrOpenFailed)
		}
		info, err := fileInfo(opened)
		actualPath, pathErr := finalPathForHandle(opened)
		if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
			info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || pathErr != nil || !withinRoot(rootPath, actualPath) {
			_ = windows.CloseHandle(opened)
			cleanup()
			return ErrPathEscape
		}
		owned = append(owned, opened)
		parent = opened
	}
	cleanup()
	return nil
}

func openRelativeLock(rootHandle uintptr, rootPath, relative string) (*os.File, error) {
	segments := strings.Split(relative, "/")
	parent := windows.Handle(rootHandle)
	ownedDirectories := make([]windows.Handle, 0, len(segments))
	cleanup := func() {
		for index := len(ownedDirectories) - 1; index >= 0; index-- {
			_ = windows.CloseHandle(ownedDirectories[index])
		}
	}
	for index, segment := range segments {
		caseMode, err := caseModeForHandle(uintptr(parent))
		if err != nil {
			cleanup()
			return nil, ErrUnsupportedOS
		}
		attributes := uint32(0)
		if caseMode == paths.CaseInsensitive {
			attributes = windows.OBJ_CASE_INSENSITIVE
		}
		name, err := windows.NewNTUnicodeString(segment)
		if err != nil {
			cleanup()
			return nil, ErrOpenFailed
		}
		object := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: name, Attributes: attributes}
		var opened windows.Handle
		var status windows.IO_STATUS_BLOCK
		access := uint32(windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
		options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
		createDisposition := uint32(windows.FILE_OPEN)
		if index < len(segments)-1 {
			access |= windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE
			options |= windows.FILE_DIRECTORY_FILE
		} else {
			access |= windows.FILE_READ_DATA | windows.FILE_WRITE_DATA
			options |= windows.FILE_NON_DIRECTORY_FILE
			createDisposition = windows.FILE_OPEN_IF
		}
		err = windows.NtCreateFile(&opened, access, object, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, createDisposition, options, 0, 0)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("open private OS lockfile: %w", ErrOpenFailed)
		}
		info, err := fileInfo(opened)
		actualPath, pathErr := finalPathForHandle(opened)
		if err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || pathErr != nil || !withinRoot(rootPath, actualPath) {
			_ = windows.CloseHandle(opened)
			cleanup()
			return nil, ErrPathEscape
		}
		if index < len(segments)-1 {
			if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
				_ = windows.CloseHandle(opened)
				cleanup()
				return nil, ErrNotDirectory
			}
			ownedDirectories = append(ownedDirectories, opened)
			parent = opened
			continue
		}
		if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 || info.NumberOfLinks != 1 {
			_ = windows.CloseHandle(opened)
			cleanup()
			return nil, ErrNotRegularFile
		}
		file := os.NewFile(uintptr(opened), relative)
		if file == nil {
			_ = windows.CloseHandle(opened)
			cleanup()
			return nil, ErrOpenFailed
		}
		cleanup()
		return file, nil
	}
	cleanup()
	return nil, ErrOpenFailed
}

type atomicRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

type atomicDispositionInformation struct {
	DeleteFile uint8
}

func writeAtomicRelative(rootHandle uintptr, rootPath, relative string, value []byte, _ os.FileMode, replace bool, result *AtomicPublishResult) (retErr error) {
	segments := strings.Split(relative, "/")
	parent := windows.Handle(rootHandle)
	ownedDirectories := make([]windows.Handle, 0, len(segments)-1)
	defer func() {
		for index := len(ownedDirectories) - 1; index >= 0; index-- {
			if err := windows.CloseHandle(ownedDirectories[index]); retErr == nil {
				retErr = err
			}
		}
	}()
	for _, segment := range segments[:len(segments)-1] {
		caseMode, err := caseModeForHandle(uintptr(parent))
		if err != nil {
			return ErrUnsupportedOS
		}
		attributes := uint32(0)
		if caseMode == paths.CaseInsensitive {
			attributes = windows.OBJ_CASE_INSENSITIVE
		}
		name, err := windows.NewNTUnicodeString(segment)
		if err != nil {
			return ErrOpenFailed
		}
		object := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: name, Attributes: attributes}
		var opened windows.Handle
		var status windows.IO_STATUS_BLOCK
		err = windows.NtCreateFile(&opened,
			windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.FILE_GENERIC_WRITE|windows.SYNCHRONIZE,
			object, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
		if err != nil {
			return fmt.Errorf("open atomic file parent: %w", ErrPathEscape)
		}
		info, infoErr := fileInfo(opened)
		actualPath, pathErr := finalPathForHandle(opened)
		if infoErr != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
			info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || pathErr != nil || !withinRoot(rootPath, actualPath) {
			_ = windows.CloseHandle(opened)
			return ErrPathEscape
		}
		ownedDirectories = append(ownedDirectories, opened)
		parent = opened
	}
	target := segments[len(segments)-1]
	targetName, err := windows.NewNTUnicodeString(target)
	if err != nil {
		return ErrPathDenied
	}
	targetCaseMode, err := caseModeForHandle(uintptr(parent))
	if err != nil {
		return ErrUnsupportedOS
	}
	targetAttributes := uint32(0)
	if targetCaseMode == paths.CaseInsensitive {
		targetAttributes = windows.OBJ_CASE_INSENSITIVE
	}
	targetObject := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: targetName, Attributes: targetAttributes}
	var existing windows.Handle
	var existingStatus windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&existing,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		targetObject, &existingStatus, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err == nil {
		info, infoErr := fileInfo(existing)
		actualPath, pathErr := finalPathForHandle(existing)
		closeErr := windows.CloseHandle(existing)
		if infoErr != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DEVICE) != 0 ||
			info.NumberOfLinks != 1 || pathErr != nil || !withinRoot(rootPath, actualPath) || closeErr != nil {
			return ErrPathEscape
		}
		if !replace {
			return ErrAlreadyPublished
		}
	} else if !errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) && !errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) {
		return fmt.Errorf("inspect existing atomic file target: %w", err)
	}

	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporaryName := ".mce4d-atomic-" + hex.EncodeToString(suffix[:]) + ".tmp"
	name, err := windows.NewNTUnicodeString(temporaryName)
	if err != nil {
		return ErrOpenFailed
	}
	object := &windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: name}
	var temporaryHandle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&temporaryHandle,
		windows.DELETE|windows.FILE_WRITE_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		object, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_CREATE,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return fmt.Errorf("create same-directory atomic file: %w", err)
	}
	file := os.NewFile(uintptr(temporaryHandle), temporaryName)
	if file == nil {
		_ = windows.CloseHandle(temporaryHandle)
		return ErrOpenFailed
	}
	renamed := false
	fileClosed := false
	defer func() {
		if !renamed {
			var disposition atomicDispositionInformation
			disposition.DeleteFile = 1
			var iosb windows.IO_STATUS_BLOCK
			_ = windows.NtSetInformationFile(temporaryHandle, &iosb, (*byte)(unsafe.Pointer(&disposition)), uint32(unsafe.Sizeof(disposition)), windows.FileDispositionInformation)
		}
		if !fileClosed {
			if err := file.Close(); retErr == nil {
				retErr = err
			}
		}
	}()
	info, err := fileInfo(temporaryHandle)
	if err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DEVICE) != 0 || info.NumberOfLinks != 1 {
		return ErrPathEscape
	}
	written, err := file.Write(value)
	if err == nil && written != len(value) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if result != nil {
		result.FileFlushed = true
	}
	targetUTF16, err := windows.UTF16FromString(target)
	if err != nil {
		return ErrPathDenied
	}
	fileNameBytes := (len(targetUTF16) - 1) * 2
	var sample atomicRenameInformation
	buffer := make([]byte, int(unsafe.Offsetof(sample.FileName))+fileNameBytes)
	rename := (*atomicRenameInformation)(unsafe.Pointer(&buffer[0]))
	if replace {
		rename.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	} else {
		rename.ReplaceIfExists = 0
	}
	rename.RootDirectory = parent
	rename.FileNameLength = uint32(fileNameBytes)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&rename.FileName[0]))[:fileNameBytes/2:fileNameBytes/2], targetUTF16)
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtSetInformationFile(temporaryHandle, &iosb, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation); err != nil {
		if !replace {
			var appeared windows.Handle
			var appearedStatus windows.IO_STATUS_BLOCK
			inspectErr := windows.NtCreateFile(&appeared, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
				targetObject, &appearedStatus, nil, windows.FILE_ATTRIBUTE_NORMAL,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
				windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
			if inspectErr == nil {
				closeErr := windows.CloseHandle(appeared)
				if closeErr != nil {
					return errors.Join(ErrAlreadyPublished, closeErr)
				}
				return ErrAlreadyPublished
			}
			if !errors.Is(inspectErr, windows.STATUS_OBJECT_NAME_NOT_FOUND) && !errors.Is(inspectErr, windows.STATUS_OBJECT_PATH_NOT_FOUND) {
				return errors.Join(fmt.Errorf("publish atomic file: %w", err), fmt.Errorf("inspect no-replace target: %w", inspectErr))
			}
		}
		return fmt.Errorf("atomically replace home-relative file: %w", err)
	}
	renamed = true
	if result != nil {
		result.Published = true
	}
	if err := file.Close(); err != nil {
		fileClosed = true
		return fmt.Errorf("close published atomic file: %w", err)
	}
	fileClosed = true
	if result != nil {
		if err := windows.FlushFileBuffers(parent); err != nil {
			return fmt.Errorf("flush published atomic file directory: %w: %v", ErrDirectoryFlushUnsupported, err)
		}
		result.DirectoryFlushed = true
	}
	return nil
}
