//go:build linux

package runtime

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryFileLock(file *os.File, mode LockMode) (func() error, error) {
	flags := unix.LOCK_NB
	if mode == LockExclusive {
		flags |= unix.LOCK_EX
	} else {
		flags |= unix.LOCK_SH
	}
	if err := unix.Flock(int(file.Fd()), flags); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			return nil, ErrLockBusy
		}
		return nil, err
	}
	fd := int(file.Fd())
	return func() error { return unix.Flock(fd, unix.LOCK_UN) }, nil
}
