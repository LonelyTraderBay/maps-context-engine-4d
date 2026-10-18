//go:build windows

package exec

import (
	"encoding/binary"
	"errors"
	"fmt"
	osexec "os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessTree struct {
	mu     sync.Mutex
	job    windows.Handle
	parent windows.Handle
}

type processMemoryCounters struct {
	Size                    uint32
	PageFaultCount          uint32
	PeakWorkingSetSize      uintptr
	WorkingSetSize          uintptr
	QuotaPeakPagedPoolUsage uintptr
	QuotaPagedPoolUsage     uintptr
	QuotaPeakNonPagedUsage  uintptr
	QuotaNonPagedPoolUsage  uintptr
	PagefileUsage           uintptr
	PeakPagefileUsage       uintptr
}

type jobProcessIDListHeader struct {
	Assigned uint32
	Count    uint32
}

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

func newProcessTreeGuard(command *osexec.Cmd) (processTreeGuard, error) {
	if command == nil {
		return nil, ErrInvalidCommand
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process-tree job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure process-tree job: %w", err)
	}
	return &windowsProcessTree{job: job}, nil
}

func (tree *windowsProcessTree) Attach(command *osexec.Cmd) error {
	if tree == nil || command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return ErrInvalidCommand
	}
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return ErrResourceMonitor
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ,
		false, uint32(command.Process.Pid))
	if err != nil {
		return fmt.Errorf("open controlled process for job assignment: %w", err)
	}
	defer windows.CloseHandle(process)
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(command.Process.Pid))
	if err != nil {
		return fmt.Errorf("open controlled process for exit observation: %w", err)
	}
	if err := windows.AssignProcessToJobObject(tree.job, process); err != nil {
		_ = windows.CloseHandle(parent)
		return fmt.Errorf("assign controlled process tree: %w", err)
	}
	tree.parent = parent
	return nil
}

func (tree *windowsProcessTree) ParentExited() (bool, error) {
	if tree == nil {
		return true, nil
	}
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.parent == 0 {
		return false, ErrResourceMonitor
	}
	result, err := windows.WaitForSingleObject(tree.parent, 0)
	if err != nil {
		return false, fmt.Errorf("observe controlled parent exit: %w", err)
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, fmt.Errorf("observe controlled parent exit returned %#x", result)
	}
}

func (tree *windowsProcessTree) RSSBytes() (int64, error) {
	if tree == nil {
		return 0, ErrResourceMonitor
	}
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return 0, ErrResourceMonitor
	}
	processIDs, err := tree.processIDsLocked()
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, pid := range processIDs {
		process, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			continue // Process ended between the job snapshot and opening its handle.
		}
		if err != nil {
			return 0, fmt.Errorf("open job process for RSS measurement: %w", err)
		}
		counters := processMemoryCounters{Size: uint32(unsafe.Sizeof(processMemoryCounters{}))}
		result, _, callErr := getProcessMemoryInfo.Call(uintptr(process), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
		closeErr := windows.CloseHandle(process)
		if result == 0 {
			if callErr == nil || callErr == windows.ERROR_SUCCESS {
				callErr = windows.GetLastError()
			}
			return 0, fmt.Errorf("measure job process RSS: %w", callErr)
		}
		if closeErr != nil {
			return 0, fmt.Errorf("close measured process handle: %w", closeErr)
		}
		if uint64(counters.WorkingSetSize) > ^uint64(0)-total {
			return 0, ErrResourceMonitor
		}
		total += uint64(counters.WorkingSetSize)
	}
	if total > uint64(^uint64(0)>>1) {
		return 0, ErrResourceMonitor
	}
	return int64(total), nil
}

func (tree *windowsProcessTree) processIDsLocked() ([]uintptr, error) {
	capacity := 16
	const maxProcessTreeEntries = 4096
	for capacity <= maxProcessTreeEntries {
		pointerSize := int(unsafe.Sizeof(uintptr(0)))
		buffer := make([]byte, 8+capacity*pointerSize)
		binary.LittleEndian.PutUint32(buffer[4:8], uint32(capacity))
		var returned uint32
		err := windows.QueryInformationJobObject(tree.job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buffer[0])), uint32(len(buffer)), &returned)
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			capacity *= 2
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list controlled process-tree members: %w", err)
		}
		count := int(binary.LittleEndian.Uint32(buffer[4:8]))
		if count < 0 || count > capacity {
			return nil, ErrResourceMonitor
		}
		processIDs := make([]uintptr, 0, count)
		for index := 0; index < count; index++ {
			offset := 8 + index*pointerSize
			if pointerSize == 8 {
				processIDs = append(processIDs, uintptr(binary.LittleEndian.Uint64(buffer[offset:offset+8])))
			} else {
				processIDs = append(processIDs, uintptr(binary.LittleEndian.Uint32(buffer[offset:offset+4])))
			}
		}
		return processIDs, nil
	}
	return nil, fmt.Errorf("process tree exceeds %d members: %w", maxProcessTreeEntries, ErrResourceMonitor)
}

func (tree *windowsProcessTree) Kill() error {
	if tree == nil {
		return nil
	}
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.job == 0 {
		return nil
	}
	processIDs, err := tree.processIDsLocked()
	if err != nil {
		return err
	}
	handles := make([]windows.Handle, 0, len(processIDs))
	defer func() {
		for _, handle := range handles {
			_ = windows.CloseHandle(handle)
		}
	}()
	for _, pid := range processIDs {
		process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open process-tree member for settlement: %w", err)
		}
		handles = append(handles, process)
	}
	if len(handles) == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(tree.job, 1); err != nil {
		return fmt.Errorf("terminate owned process tree: %w", err)
	}
	for _, process := range handles {
		result, err := windows.WaitForSingleObject(process, windows.INFINITE)
		if err != nil {
			return fmt.Errorf("wait for owned process-tree member: %w", err)
		}
		if result != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("wait for owned process-tree member returned %#x", result)
		}
	}
	return nil
}

func (tree *windowsProcessTree) Close() error {
	if tree == nil {
		return nil
	}
	killErr := tree.Kill()
	tree.mu.Lock()
	defer tree.mu.Unlock()
	var closeErr, parentErr error
	if tree.job != 0 {
		closeErr = windows.CloseHandle(tree.job)
		tree.job = 0
	}
	if tree.parent != 0 {
		parentErr = windows.CloseHandle(tree.parent)
		tree.parent = 0
	}
	return errors.Join(killErr, closeErr, parentErr)
}
