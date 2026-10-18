//go:build linux

package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
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
		if errors.Is(err, unix.ENOENT) {
			return nil, nil, errors.Join(ErrOpenFailed, os.ErrNotExist)
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
	if err != nil || info.Nlink != 1 {
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

func identityForHandle(handle uintptr) (string, string, error) {
	var info unix.Stat_t
	if err := unix.Fstat(int(handle), &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", "", ErrOpenFailed
	}
	volumeID := fmt.Sprintf("linux-volume-%x", uint64(info.Dev))
	homeIdentity := fmt.Sprintf("linux-home-%x-%x", uint64(info.Dev), uint64(info.Ino))
	return volumeID, homeIdentity, nil
}

func availableBytesForHandle(handle uintptr, _ string) (int64, error) {
	var stats unix.Statfs_t
	if err := unix.Fstatfs(int(handle), &stats); err != nil || stats.Bsize <= 0 {
		return 0, ErrUnsupportedOS
	}
	available := uint64(stats.Bavail)
	blockSize := uint64(stats.Bsize)
	if blockSize == 0 || available > uint64(math.MaxInt64)/blockSize {
		return 0, fmt.Errorf("filesystem available bytes overflow: %w", ErrOpenFailed)
	}
	return int64(available * blockSize), nil
}

func makeRelativeDirs(rootHandle uintptr, rootPath, relative string) error {
	if err := paths.ValidateRelative(relative); err != nil {
		return ErrPathDenied
	}
	var rootInfo unix.Stat_t
	if err := unix.Fstat(int(rootHandle), &rootInfo); err != nil {
		return ErrOpenFailed
	}
	current, err := unix.Dup(int(rootHandle))
	if err != nil {
		return ErrOpenFailed
	}
	defer func() {
		if current >= 0 {
			_ = unix.Close(current)
		}
	}()
	for _, segment := range strings.Split(relative, "/") {
		if err := unix.Mkdirat(current, segment, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create private directory: %w", err)
		}
		how := &unix.OpenHow{
			Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY | unix.O_NOFOLLOW),
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
		}
		next, err := unix.Openat2(current, segment, how)
		if err != nil {
			return fmt.Errorf("open private directory securely: %w", ErrPathEscape)
		}
		var info unix.Stat_t
		if err := unix.Fstat(next, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Dev != rootInfo.Dev {
			_ = unix.Close(next)
			return ErrPathEscape
		}
		if info.Uid != uint32(os.Geteuid()) || info.Mode&0o077 != 0 {
			_ = unix.Close(next)
			return fmt.Errorf("private directory ownership or permissions invalid: %w", ErrPathDenied)
		}
		_ = unix.Close(current)
		current = next
	}
	_ = rootPath
	return nil
}

func openRelativeLock(rootHandle uintptr, _ string, relative string, create bool) (*os.File, error) {
	var rootInfo unix.Stat_t
	if err := unix.Fstat(int(rootHandle), &rootInfo); err != nil {
		return nil, ErrOpenFailed
	}
	flags := uint64(unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK)
	mode := uint64(0)
	if create {
		flags |= unix.O_CREAT
		mode = 0o600
	}
	how := &unix.OpenHow{
		Flags:   flags,
		Mode:    mode,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	handle, err := unix.Openat2(int(rootHandle), relative, how)
	if err != nil {
		return nil, fmt.Errorf("open private OS lockfile: %w", ErrOpenFailed)
	}
	var info unix.Stat_t
	if err := unix.Fstat(handle, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Dev != rootInfo.Dev || info.Nlink != 1 {
		_ = unix.Close(handle)
		return nil, ErrNotRegularFile
	}
	if info.Uid != uint32(os.Geteuid()) || info.Mode&0o077 != 0 {
		_ = unix.Close(handle)
		return nil, fmt.Errorf("private lockfile ownership or permissions invalid: %w", ErrPathDenied)
	}
	file := os.NewFile(uintptr(handle), relative)
	if file == nil {
		_ = unix.Close(handle)
		return nil, ErrOpenFailed
	}
	return file, nil
}

func writeAtomicRelative(rootHandle uintptr, _ string, relative string, writeValue func(io.Writer) error, mode os.FileMode, replace bool, result *AtomicPublishResult) (retErr error) {
	segments := strings.Split(relative, "/")
	parent := int(rootHandle)
	ownedParent := -1
	var rootInfo unix.Stat_t
	if err := unix.Fstat(parent, &rootInfo); err != nil || rootInfo.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrOpenFailed
	}
	for _, segment := range segments[:len(segments)-1] {
		how := &unix.OpenHow{
			Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY | unix.O_NOFOLLOW),
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
		}
		next, err := unix.Openat2(parent, segment, how)
		if err != nil {
			if ownedParent >= 0 {
				_ = unix.Close(ownedParent)
			}
			return fmt.Errorf("open atomic file parent securely: %w", ErrPathEscape)
		}
		var info unix.Stat_t
		if err := unix.Fstat(next, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Dev != rootInfo.Dev {
			_ = unix.Close(next)
			if ownedParent >= 0 {
				_ = unix.Close(ownedParent)
			}
			return ErrPathEscape
		}
		if ownedParent >= 0 {
			_ = unix.Close(ownedParent)
		}
		ownedParent, parent = next, next
	}
	if ownedParent >= 0 {
		defer func() {
			if err := unix.Close(ownedParent); retErr == nil {
				retErr = err
			}
		}()
	}
	target := segments[len(segments)-1]
	if handle, err := openRegularAt(parent, target, rootInfo.Dev); err == nil {
		if closeErr := unix.Close(handle); closeErr != nil {
			return closeErr
		}
		if !replace {
			return ErrAlreadyPublished
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}

	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporary := ".mce4d-atomic-" + hex.EncodeToString(suffix[:]) + ".tmp"
	fd, err := unix.Openat(parent, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return fmt.Errorf("create same-directory atomic file: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = unix.Unlinkat(parent, temporary, 0)
		}
	}()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Dev != rootInfo.Dev || info.Nlink != 1 {
		_ = unix.Close(fd)
		return ErrPathEscape
	}
	file := os.NewFile(uintptr(fd), temporary)
	if file == nil {
		_ = unix.Close(fd)
		return ErrOpenFailed
	}
	if err := writeValue(file); err != nil {
		_ = file.Close()
		return err
	}
	if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if result != nil {
		result.FileFlushed = true
	}
	if err := file.Close(); err != nil {
		return err
	}
	var renameErr error
	if replace {
		renameErr = unix.Renameat(parent, temporary, parent, target)
	} else {
		renameErr = unix.Renameat2(parent, temporary, parent, target, unix.RENAME_NOREPLACE)
	}
	if errors.Is(renameErr, unix.EEXIST) && !replace {
		return ErrAlreadyPublished
	}
	if renameErr != nil {
		return fmt.Errorf("atomically publish home-relative file: %w", renameErr)
	}
	renamed = true
	if result != nil {
		result.Published = true
	}
	if err := unix.Fsync(parent); err != nil {
		if result != nil {
			return fmt.Errorf("flush atomic file directory: %w", errors.Join(ErrDirectoryFlushUnsupported, err))
		}
		return fmt.Errorf("flush atomic file directory: %w", err)
	}
	if result != nil {
		result.DirectoryFlushed = true
	}
	return nil
}

func openRegularAt(parent int, name string, device uint64) (int, error) {
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	handle, err := unix.Openat2(parent, name, how)
	if err != nil {
		return -1, err
	}
	var info unix.Stat_t
	if err := unix.Fstat(handle, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Dev != device || info.Nlink != 1 {
		_ = unix.Close(handle)
		return -1, ErrPathEscape
	}
	return handle, nil
}

func removeRegularRelative(rootHandle uintptr, _ string, relative string) (retErr error) {
	segments := strings.Split(relative, "/")
	parent := int(rootHandle)
	ownedParent := -1
	var rootInfo unix.Stat_t
	if err := unix.Fstat(parent, &rootInfo); err != nil || rootInfo.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrOpenFailed
	}
	for _, segment := range segments[:len(segments)-1] {
		how := &unix.OpenHow{
			Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY | unix.O_NOFOLLOW),
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
		}
		next, err := unix.Openat2(parent, segment, how)
		if err != nil {
			if ownedParent >= 0 {
				_ = unix.Close(ownedParent)
			}
			return fmt.Errorf("open file removal parent securely: %w", ErrPathEscape)
		}
		var info unix.Stat_t
		if err := unix.Fstat(next, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Dev != rootInfo.Dev {
			_ = unix.Close(next)
			if ownedParent >= 0 {
				_ = unix.Close(ownedParent)
			}
			return ErrPathEscape
		}
		if ownedParent >= 0 {
			if err := unix.Close(ownedParent); err != nil {
				_ = unix.Close(next)
				return err
			}
		}
		ownedParent, parent = next, next
	}
	if ownedParent >= 0 {
		defer func() {
			if err := unix.Close(ownedParent); retErr == nil {
				retErr = err
			}
		}()
	}
	target := segments[len(segments)-1]
	handle, err := openRegularAt(parent, target, uint64(rootInfo.Dev))
	if err != nil {
		return fmt.Errorf("open regular file selected for removal: %w", ErrNotRegularFile)
	}
	defer func() {
		if err := unix.Close(handle); retErr == nil {
			retErr = err
		}
	}()
	var openedInfo, pathInfo unix.Stat_t
	if err := unix.Fstat(handle, &openedInfo); err != nil {
		return ErrOpenFailed
	}
	if err := unix.Fstatat(parent, target, &pathInfo, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("recheck file before removal: %w", ErrPathEscape)
	}
	if pathInfo.Mode&unix.S_IFMT != unix.S_IFREG || pathInfo.Dev != openedInfo.Dev || pathInfo.Ino != openedInfo.Ino || pathInfo.Nlink != 1 {
		return ErrPathEscape
	}
	if err := unix.Unlinkat(parent, target, 0); err != nil {
		return fmt.Errorf("remove verified regular file: %w", err)
	}
	if err := unix.Fsync(parent); err != nil {
		return fmt.Errorf("flush file removal directory: %w", errors.Join(ErrDirectoryFlushUnsupported, err))
	}
	return nil
}
