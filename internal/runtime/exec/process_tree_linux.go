//go:build linux

package exec

import (
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type linuxProcessTree struct {
	mu         sync.Mutex
	pgid       int
	parentPID  int
	startTicks uint64
}

func newProcessTreeGuard(command *osexec.Cmd) (processTreeGuard, error) {
	if command == nil {
		return nil, ErrInvalidCommand
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &linuxProcessTree{}, nil
}

func (tree *linuxProcessTree) Attach(command *osexec.Cmd) error {
	if tree == nil || command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return ErrInvalidCommand
	}
	_, group, startTicks, err := linuxProcessDetails(command.Process.Pid)
	if err != nil || group != command.Process.Pid {
		return errors.Join(ErrResourceMonitor, err)
	}
	tree.mu.Lock()
	tree.pgid = command.Process.Pid
	tree.parentPID = command.Process.Pid
	tree.startTicks = startTicks
	tree.mu.Unlock()
	return nil
}

func (tree *linuxProcessTree) ParentExited() (bool, error) {
	if tree == nil {
		return true, nil
	}
	tree.mu.Lock()
	pid, startTicks := tree.parentPID, tree.startTicks
	tree.mu.Unlock()
	if pid <= 0 || startTicks == 0 {
		return false, ErrResourceMonitor
	}
	state, _, currentStart, err := linuxProcessDetails(pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return currentStart != startTicks || state == 'Z' || state == 'X', nil
}

func linuxProcessDetails(pid int) (byte, int, uint64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, 0, err
	}
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 || closeParen+1 >= len(data) {
		return 0, 0, 0, ErrResourceMonitor
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return 0, 0, 0, ErrResourceMonitor
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, 0, ErrResourceMonitor
	}
	startTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTicks == 0 {
		return 0, 0, 0, ErrResourceMonitor
	}
	return fields[0][0], group, startTicks, nil
}

func (tree *linuxProcessTree) RSSBytes() (int64, error) {
	tree.mu.Lock()
	pgid := tree.pgid
	tree.mu.Unlock()
	if pgid <= 0 {
		return 0, ErrResourceMonitor
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read Linux process table: %w", err)
	}
	var total int64
	pageSize := int64(os.Getpagesize())
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read Linux process identity: %w", err)
		}
		closeParen := strings.LastIndexByte(string(data), ')')
		if closeParen < 0 || closeParen+1 >= len(data) {
			return 0, ErrResourceMonitor
		}
		fields := strings.Fields(string(data[closeParen+1:]))
		if len(fields) <= 21 {
			return 0, ErrResourceMonitor
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return 0, ErrResourceMonitor
		}
		if group != pgid {
			continue
		}
		pages, err := strconv.ParseInt(fields[21], 10, 64)
		if err != nil || pages < 0 || pageSize <= 0 || pages > (int64(^uint64(0)>>1)-total)/pageSize {
			return 0, ErrResourceMonitor
		}
		total += pages * pageSize
	}
	return total, nil
}

func (tree *linuxProcessTree) Kill() error {
	if tree == nil {
		return nil
	}
	tree.mu.Lock()
	pgid := tree.pgid
	tree.mu.Unlock()
	if pgid <= 0 {
		return nil
	}
	err := unix.Kill(-pgid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func (tree *linuxProcessTree) Close() error {
	if tree == nil {
		return nil
	}
	bytes, err := tree.RSSBytes()
	if err != nil {
		return err
	}
	if bytes == 0 {
		return nil
	}
	return tree.Kill()
}
