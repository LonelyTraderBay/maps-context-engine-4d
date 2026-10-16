//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"golang.org/x/sys/unix"
)

const (
	linuxExt4Magic  = 0xEF53
	linuxTmpfsMagic = 0x01021994
)

func OpenRoot(path string) (*Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrOpenFailed
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, ErrOpenFailed
	}
	handle, err := unix.Open(resolved, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrOpenFailed
	}
	var info unix.Stat_t
	if err := unix.Fstat(handle, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(handle)
		return nil, ErrNotDirectory
	}
	return &Root{handle: uintptr(handle), finalPath: filepath.Clean(resolved)}, nil
}

func duplicateRootHandle(handle uintptr) (uintptr, error) {
	duplicate, err := unix.Dup(int(handle))
	if err != nil {
		return 0, err
	}
	unix.CloseOnExec(duplicate)
	return uintptr(duplicate), nil
}

func closeRootHandle(handle uintptr) error {
	if handle == 0 {
		return nil
	}
	return unix.Close(int(handle))
}

func caseModeForHandle(handle uintptr) (paths.CaseMode, error) {
	var fileSystem unix.Statfs_t
	if err := unix.Fstatfs(int(handle), &fileSystem); err != nil {
		return paths.CaseUnknown, ErrUnsupportedOS
	}
	switch uint64(fileSystem.Type) {
	case linuxTmpfsMagic:
		return paths.CaseSensitive, nil
	case linuxExt4Magic:
		flags, err := unix.IoctlGetInt(int(handle), unix.FS_IOC_GETFLAGS)
		if err != nil {
			return paths.CaseUnknown, ErrUnsupportedOS
		}
		const ext4CasefoldFlag = 0x40000000
		if flags&ext4CasefoldFlag != 0 {
			return paths.CaseInsensitive, nil
		}
		return paths.CaseSensitive, nil
	default:
		return paths.CaseUnknown, ErrUnsupportedOS
	}
}

func openRelative(rootHandle uintptr, rootPath, relative string) (*os.File, func() error, error) {
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	handle, err := unix.Openat2(int(rootHandle), relative, how)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
			return nil, nil, ErrUnsupportedOS
		}
		return nil, nil, ErrOpenFailed
	}
	var info unix.Stat_t
	if err := unix.Fstat(handle, &info); err != nil {
		_ = unix.Close(handle)
		return nil, nil, ErrOpenFailed
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(handle)
		return nil, nil, ErrNotRegularFile
	}
	actualPath, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/self/fd/%d", handle))
	if err != nil || info.Nlink == 0 {
		_ = unix.Close(handle)
		return nil, nil, ErrPathEscape
	}
	within, err := filepath.Rel(rootPath, actualPath)
	if err != nil || within == ".." || filepath.IsAbs(within) || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		_ = unix.Close(handle)
		return nil, nil, ErrPathEscape
	}
	file := os.NewFile(uintptr(handle), "")
	if file == nil {
		_ = unix.Close(handle)
		return nil, nil, ErrOpenFailed
	}
	return file, func() error { return nil }, nil
}
