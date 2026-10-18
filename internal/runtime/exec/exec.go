// Package exec runs explicitly approved local tools without a shell or inherited
// environment. It is not a sandbox for the executable being started.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrInvalidCommand  = errors.New("invalid controlled command")
	ErrOutputLimit     = errors.New("controlled command output limit exceeded")
	ErrResourceLimit   = errors.New("controlled command process-tree resource limit exceeded")
	ErrResourceMonitor = errors.New("controlled command process-tree resource measurement unavailable")
	ErrStorageBudget   = errors.New("controlled command storage budget not admitted")
)

const (
	maxArguments = 4096
	maxEnvVars   = 64
	// Keep these fallback values synchronized with config.DefaultRuntimeConfig.
	defaultRSSBudgetBytes  = 8_589_934_592
	defaultMonitorInterval = 250 * time.Millisecond
)

var allowedEnv = map[string]struct{}{
	"PATH": {}, "GOOS": {}, "GOARCH": {}, "CGO_ENABLED": {}, "GOTOOLCHAIN": {},
	"GOPROXY": {}, "GOSUMDB": {}, "GOPACKAGESDRIVER": {}, "GOENV": {},
	"GOWORK": {}, "GOFLAGS": {}, "GOMODCACHE": {}, "GOCACHE": {},
	"TMPDIR": {}, "TEMP": {}, "TMP": {}, "SYSTEMROOT": {}, "WINDIR": {},
}

type Spec struct {
	Path            string
	Args            []string
	Dir             string
	Env             map[string]string
	MaxStdoutBytes  int
	MaxStderrBytes  int
	RSSBudgetBytes  int64
	MonitorInterval time.Duration
	StoragePlan     *StoragePlan
}

type StoragePlan struct {
	Policy       ResourceAdmissionPolicy
	Destinations []StorageDestination
}

type Result struct {
	Stdout            []byte
	Stderr            []byte
	ExitCode          int
	ExitCodeKnown     bool
	TerminationReason string
	PeakRSSBytes      int64
	CleanupStatus     string
	Warnings          []string
	StoragePreflight  string
	StorageAdmission  []ResourceAdmission
}

// Run starts one exact executable path. It never resolves a command through
// PATH, invokes a shell, or inherits the caller's environment implicitly.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if ctx == nil || !filepath.IsAbs(spec.Path) || !filepath.IsAbs(spec.Dir) ||
		len(spec.Args) > maxArguments || spec.MaxStdoutBytes < 0 || spec.MaxStderrBytes < 0 {
		return Result{}, ErrInvalidCommand
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if strings.EqualFold(filepath.Ext(spec.Path), ".bat") || strings.EqualFold(filepath.Ext(spec.Path), ".cmd") {
		return Result{}, fmt.Errorf("%w: shell script executables are not allowed", ErrInvalidCommand)
	}
	info, err := os.Lstat(spec.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Result{}, fmt.Errorf("%w: executable must be a regular non-symlink file", ErrInvalidCommand)
	}
	dirInfo, err := os.Stat(spec.Dir)
	if err != nil || !dirInfo.IsDir() {
		return Result{}, fmt.Errorf("%w: working directory must exist", ErrInvalidCommand)
	}
	if len(spec.Env) > maxEnvVars {
		return Result{}, fmt.Errorf("%w: too many environment variables", ErrInvalidCommand)
	}
	envKeys := make([]string, 0, len(spec.Env))
	for key, value := range spec.Env {
		if _, ok := allowedEnv[key]; !ok || key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return Result{}, fmt.Errorf("%w: environment variable %q is not allowed", ErrInvalidCommand, key)
		}
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	env := make([]string, 0, len(envKeys))
	for _, key := range envKeys {
		env = append(env, key+"="+spec.Env[key])
	}
	for _, arg := range spec.Args {
		if strings.ContainsRune(arg, 0) {
			return Result{}, fmt.Errorf("%w: argument contains NUL", ErrInvalidCommand)
		}
	}

	if spec.RSSBudgetBytes == 0 && spec.MonitorInterval == 0 {
		spec.RSSBudgetBytes = defaultRSSBudgetBytes
		spec.MonitorInterval = defaultMonitorInterval
	}
	if spec.RSSBudgetBytes <= 0 || spec.MonitorInterval <= 0 {
		return Result{}, fmt.Errorf("%w: positive RSS budget and monitor interval are required", ErrInvalidCommand)
	}
	storageStatus, warnings, storageAdmission, err := preflightStorage(spec.StoragePlan)
	if err != nil {
		return Result{TerminationReason: "resource_preflight", CleanupStatus: "not_required", Warnings: warnings,
			StoragePreflight: storageStatus, StorageAdmission: storageAdmission}, err
	}

	cmd := osexec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = env
	processTree, err := newProcessTreeGuard(cmd)
	if err != nil {
		return Result{}, err
	}
	var terminationMu sync.Mutex
	terminationReason := ""
	var terminationOnce sync.Once
	terminated := make(chan struct{})
	terminate := func(reason string) {
		terminationOnce.Do(func() {
			terminationMu.Lock()
			terminationReason = reason
			terminationMu.Unlock()
			close(terminated)
		})
	}
	stdout := &boundedBuffer{limit: spec.MaxStdoutBytes, onExceeded: func() { terminate("output_limit") }}
	stderr := &boundedBuffer{limit: spec.MaxStderrBytes, onExceeded: func() { terminate("output_limit") }}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		closeErr := processTree.Close()
		return Result{CleanupStatus: cleanupStatus(closeErr), Warnings: warnings, StoragePreflight: storageStatus,
			StorageAdmission: storageAdmission}, errors.Join(fmt.Errorf("start controlled tool: %w", err), closeErr)
	}
	if err := processTree.Attach(cmd); err != nil {
		processErr := cmd.Process.Kill()
		treeErr := processTree.Kill()
		waitErr := cmd.Wait()
		closeErr := processTree.Close()
		cleanupErr := errors.Join(processErr, treeErr, closeErr)
		return Result{ExitCode: exitCode(cmd.ProcessState), ExitCodeKnown: cmd.ProcessState != nil, CleanupStatus: cleanupStatus(cleanupErr),
				Warnings: warnings, StoragePreflight: storageStatus, StorageAdmission: storageAdmission},
			errors.Join(fmt.Errorf("attach controlled process tree: %w", err), waitErr, cleanupErr)
	}

	waitDone := make(chan struct{})
	killDone := make(chan struct{})
	var killOnce sync.Once
	var killErr error
	killTree := func() {
		killOnce.Do(func() { killErr = processTree.Kill() })
	}
	go func() {
		defer close(killDone)
		select {
		case <-terminated:
			killTree()
		case <-waitDone:
		}
	}()
	contextDone := make(chan struct{})
	go func() {
		defer close(contextDone)
		select {
		case <-ctx.Done():
			terminate("context")
		case <-waitDone:
		}
	}()
	monitorDone := make(chan struct{})
	var peakRSS atomic.Int64
	var monitorErr error
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- cmd.Wait()
		close(waitDone)
	}()
	go func() {
		defer close(monitorDone)
		monitorErr = monitorProcessTree(processTree, waitDone, spec.RSSBudgetBytes, spec.MonitorInterval, func(bytes int64) {
			for current := peakRSS.Load(); bytes > current; current = peakRSS.Load() {
				if peakRSS.CompareAndSwap(current, bytes) {
					break
				}
			}
		}, terminate, killTree)
	}()

	err = <-waitResult
	terminationMu.Lock()
	if terminationReason != "" {
		killTree()
	}
	terminationMu.Unlock()
	<-killDone
	<-contextDone
	<-monitorDone
	closeErr := processTree.Close()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode(cmd.ProcessState), ExitCodeKnown: cmd.ProcessState != nil, PeakRSSBytes: peakRSS.Load(), CleanupStatus: cleanupStatus(errors.Join(killErr, closeErr)), Warnings: warnings, StoragePreflight: storageStatus, StorageAdmission: storageAdmission}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	terminationMu.Lock()
	result.TerminationReason = terminationReason
	terminationMu.Unlock()
	var runErr error
	switch result.TerminationReason {
	case "output_limit":
		runErr = ErrOutputLimit
	case "rss_limit":
		runErr = ErrResourceLimit
	case "rss_measurement":
		runErr = errors.Join(ErrResourceMonitor, monitorErr)
	case "context":
		runErr = ctx.Err()
		if runErr == nil {
			runErr = context.Canceled
		}
	default:
		runErr = err
	}
	if runErr != nil && result.ExitCodeKnown && result.ExitCode != 0 && result.TerminationReason == "" {
		runErr = fmt.Errorf("run controlled tool (exit %d): %w", result.ExitCode, runErr)
	}
	if killErr != nil || closeErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("finalize controlled process tree: %w", errors.Join(killErr, closeErr)))
	}
	if monitorErr != nil && result.TerminationReason != "rss_measurement" {
		runErr = errors.Join(runErr, monitorErr)
	}
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

func preflightStorage(plan *StoragePlan) (string, []string, []ResourceAdmission, error) {
	if plan == nil {
		return "not_requested", nil, nil, nil
	}
	destinations, observations, err := measureStorageDestinations(plan.Destinations)
	if err != nil {
		return "unknown", nil, nil, err
	}
	decisions, err := EvaluateResourceAdmission(destinations, observations, plan.Policy)
	if err != nil {
		return "unknown", nil, nil, err
	}
	warnings := make([]string, 0)
	var blockers []error
	for _, decision := range decisions {
		if decision.Warning {
			warnings = append(warnings, fmt.Sprintf("volume %s headroom is %d bytes, below warning threshold", decision.VolumeID, decision.AvailableBytes))
		}
		if !decision.Admitted {
			blockers = append(blockers, fmt.Errorf("volume %s has %d bytes available, requires %d", decision.VolumeID,
				decision.AvailableBytes, decision.RequiredBytes))
		}
	}
	if len(blockers) > 0 {
		return "blocked", warnings, decisions, errors.Join(ErrStorageBudget, errors.Join(blockers...))
	}
	return "admitted", warnings, decisions, nil
}

func exitCode(state *os.ProcessState) int {
	if state == nil {
		return 0
	}
	return state.ExitCode()
}

func cleanupStatus(cleanupErr error) string {
	if cleanupErr != nil {
		return "failed"
	}
	return "pass"
}

type boundedBuffer struct {
	buffer     bytes.Buffer
	limit      int
	exceeded   bool
	onExceeded func()
	once       sync.Once
}

func (buffer *boundedBuffer) Bytes() []byte {
	return buffer.buffer.Bytes()
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = buffer.exceeded || len(value) > 0
		if len(value) > 0 {
			buffer.overflow()
		}
		return len(value), nil
	}
	if len(value) > remaining {
		buffer.exceeded = true
		_, _ = buffer.buffer.Write(value[:remaining])
		buffer.overflow()
		return len(value), nil
	}
	_, _ = buffer.buffer.Write(value)
	return len(value), nil
}

func (buffer *boundedBuffer) overflow() {
	if buffer.onExceeded != nil {
		buffer.once.Do(buffer.onExceeded)
	}
}
