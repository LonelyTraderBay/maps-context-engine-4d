//go:build !linux && !windows

package exec

import "errors"

func measureStoragePath(string) (string, int64, string, error) {
	return "", 0, "", errors.New("storage destination measurement is unsupported on this operating system")
}
