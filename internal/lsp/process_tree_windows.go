//go:build windows

package lsp

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessTree struct {
	job  windows.Handle
	once sync.Once
	err  error
}

func newProcessTreeGuard(command *exec.Cmd) (processTreeGuard, error) {
	if command == nil {
		return nil, ErrLSPProtocol
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, ErrExecutableUnavailable
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, ErrExecutableUnavailable
	}
	return &windowsProcessTree{job: job}, nil
}

func (tree *windowsProcessTree) Attach(command *exec.Cmd) error {
	if tree == nil || tree.job == 0 || command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return ErrLSPProtocol
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(command.Process.Pid))
	if err != nil {
		return ErrExecutableUnavailable
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(tree.job, process); err != nil {
		return ErrExecutableUnavailable
	}
	return nil
}

func (tree *windowsProcessTree) Kill() error {
	return tree.Close()
}

func (tree *windowsProcessTree) Close() error {
	if tree == nil {
		return nil
	}
	tree.once.Do(func() {
		if tree.job != 0 {
			tree.err = windows.CloseHandle(tree.job)
			tree.job = 0
		}
	})
	return tree.err
}
