//go:build !linux && !windows

package exec

import (
	"os/exec"
)

func newProcessTreeGuard(*exec.Cmd) (processTreeGuard, error) {
	return nil, ErrResourceMonitor
}
