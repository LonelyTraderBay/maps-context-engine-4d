//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package lsp

import "os/exec"

func newProcessTreeGuard(*exec.Cmd) (processTreeGuard, error) {
	return nil, ErrExecutableUnavailable
}
