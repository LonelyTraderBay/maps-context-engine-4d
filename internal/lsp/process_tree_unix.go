//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package lsp

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

type unixProcessTree struct {
	mu   sync.Mutex
	pid  int
	once sync.Once
	err  error
}

func newProcessTreeGuard(command *exec.Cmd) (processTreeGuard, error) {
	if command == nil {
		return nil, ErrLSPProtocol
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &unixProcessTree{}, nil
}

func (tree *unixProcessTree) Attach(command *exec.Cmd) error {
	if tree == nil || command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return ErrLSPProtocol
	}
	tree.mu.Lock()
	tree.pid = command.Process.Pid
	tree.mu.Unlock()
	return nil
}

func (tree *unixProcessTree) Kill() error {
	if tree == nil {
		return nil
	}
	tree.once.Do(func() {
		tree.mu.Lock()
		pid := tree.pid
		tree.mu.Unlock()
		if pid == 0 {
			return
		}
		err := syscall.Kill(-pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			err = nil
		}
		tree.err = err
	})
	return tree.err
}

func (tree *unixProcessTree) Close() error {
	return tree.Kill()
}
