//go:build !windows && !linux

package runtime

import (
	"os"
)

func tryFileLock(*os.File, LockMode) (func() error, error) {
	return nil, ErrUnsupportedOS
}
