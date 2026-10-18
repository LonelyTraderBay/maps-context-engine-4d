//go:build windows

package exec

import (
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type resourceFileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

func measureStoragePath(path string) (string, int64, string, error) {
	if !filepath.IsAbs(path) {
		return "", 0, "", ErrResourceAdmission
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, "", err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", 0, "", err
	}
	defer windows.CloseHandle(handle)
	var tag resourceFileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return "", 0, "", err
	}
	if tag.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || tag.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", 0, "", ErrResourceAdmission
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return "", 0, "", err
	}
	finalPath, err := finalPathForResourceHandle(handle)
	if err != nil {
		return "", 0, "", err
	}
	finalPath = strings.TrimPrefix(finalPath, "\\\\?\\")
	finalName, err := windows.UTF16PtrFromString(finalPath)
	if err != nil {
		return "", 0, "", err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(finalName, &available, &total, &free); err != nil {
		return "", 0, "", err
	}
	if available > uint64(^uint64(0)>>1) {
		return "", 0, "", ErrResourceAdmission
	}
	volumeID := fmt.Sprintf("windows-volume-serial:%08x", information.VolumeSerialNumber)
	return volumeID, int64(available), "caller-available-bytes-for-opened-volume", nil
}

func finalPathForResourceHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", err
	}
	if length == 0 || int(length) >= len(buffer) {
		return "", ErrResourceAdmission
	}
	return windows.UTF16ToString(buffer[:length]), nil
}
