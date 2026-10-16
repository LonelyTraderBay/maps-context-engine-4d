//go:build windows

package runtime

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryFileLock(file *os.File, mode LockMode) (func() error, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if mode == LockExclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, overlapped)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, ErrLockBusy
		}
		return nil, err
	}
	handle := windows.Handle(file.Fd())
	return func() error { return windows.UnlockFileEx(handle, 0, 1, 0, overlapped) }, nil
}
