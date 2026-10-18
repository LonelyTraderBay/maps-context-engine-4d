//go:build linux

package exec

import (
	"fmt"
	"math"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func measureStoragePath(path string) (string, int64, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", 0, "", ErrResourceAdmission
	}
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	handle, err := unix.Openat2(unix.AT_FDCWD, path, how)
	if err != nil {
		return "", 0, "", err
	}
	defer unix.Close(handle)
	var identity unix.Stat_t
	if err := unix.Fstat(handle, &identity); err != nil || identity.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", 0, "", ErrResourceAdmission
	}
	var usage unix.Statfs_t
	if err := unix.Fstatfs(handle, &usage); err != nil {
		return "", 0, "", err
	}
	availableBlocks := uint64(usage.Bavail)
	blockSize := uint64(usage.Bsize)
	if usage.Bavail < 0 || usage.Bsize <= 0 || blockSize > math.MaxInt64 || availableBlocks > uint64(math.MaxInt64)/blockSize {
		return "", 0, "", ErrResourceAdmission
	}
	volumeID := fmt.Sprintf("linux-dev:%d", identity.Dev)
	return volumeID, int64(availableBlocks * blockSize), "filesystem-available-bytes; user-quota-visibility-unknown", nil
}
