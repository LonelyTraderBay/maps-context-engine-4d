package exec

import (
	"context"
	"errors"
	"math"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

type runOutcome struct {
	result Result
	err    error
}

func TestRunUsesExplicitEnvironmentAndBoundsOutput(t *testing.T) {
	t.Setenv("LEAK", "must-not-reach-child")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	result, err := Run(context.Background(), Spec{
		Path: executable, Args: []string{"-test.run=^TestRunHelperProcess$"}, Dir: dir,
		Env:            map[string]string{"GOOS": "MCE4D_EXEC_HELPER_ENV", "PATH": "", "TMPDIR": dir},
		MaxStdoutBytes: 16, MaxStderrBytes: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || strings.Contains(string(result.Stdout), "must-not-reach-child") || strings.Contains(string(result.Stderr), "must-not-reach-child") || result.StoragePreflight != "not_requested" {
		t.Fatalf("runner inherited an undeclared variable or helper failed: exit=%d stdout=%q stderr=%q", result.ExitCode, result.Stdout, result.Stderr)
	}
	result, err = Run(context.Background(), Spec{
		Path: executable, Args: []string{"-test.run=^TestRunHelperProcess$"}, Dir: dir,
		Env:            map[string]string{"GOOS": "MCE4D_EXEC_HELPER_OUTPUT", "PATH": ""},
		MaxStdoutBytes: 4, MaxStderrBytes: 4,
	})
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("oversized output error = %v, want ErrOutputLimit (exit=%d stdout=%q stderr=%q)", err, result.ExitCode, result.Stdout, result.Stderr)
	}
	if len(result.Stdout) > 4 || len(result.Stderr) > 4 || result.TerminationReason != "output_limit" {
		t.Fatalf("output limit did not stop capture at the configured cap: %+v", result)
	}
}

func TestRunHelperProcess(t *testing.T) {
	switch os.Getenv("GOOS") {
	case "MCE4D_EXEC_HELPER_ENV":
		_, _ = os.Stdout.Write([]byte(os.Getenv("LEAK")))
		_, _ = os.Stderr.Write([]byte(os.Getenv("LEAK")))
	case "MCE4D_EXEC_HELPER_OUTPUT":
		_, _ = os.Stdout.Write([]byte("0123456789"))
		_, _ = os.Stderr.Write([]byte("9876543210"))
	case "MCE4D_EXEC_HELPER_TREE_PARENT":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		root := helperTempRoot()
		child := osexec.Command(executable, "-test.run=^TestRunHelperProcess$")
		child.Env = []string{
			"GOOS=MCE4D_EXEC_HELPER_TREE_CHILD", "PATH=" + os.Getenv("PATH"),
			"TMPDIR=" + os.Getenv("TMPDIR"), "TEMP=" + os.Getenv("TEMP"), "TMP=" + os.Getenv("TMP"),
			"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "WINDIR=" + os.Getenv("WINDIR"),
		}
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			_ = os.WriteFile(filepath.Join(root, "mce4d-exec-child.error"), []byte(err.Error()), 0o600)
			os.Exit(3)
		}
		if err := os.WriteFile(filepath.Join(root, "mce4d-exec-child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(filepath.Join(root, "mce4d-exec-parent.ready"), []byte("ready"), 0o600); err != nil {
			os.Exit(5)
		}
		waitForever()
	case "MCE4D_EXEC_HELPER_SUCCESS_TREE_PARENT":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		root := helperTempRoot()
		child := osexec.Command(executable, "-test.run=^TestRunHelperProcess$")
		child.Env = []string{
			"GOOS=MCE4D_EXEC_HELPER_SUCCESS_TREE_CHILD", "PATH=" + os.Getenv("PATH"),
			"TMPDIR=" + root, "TEMP=" + root, "TMP=" + root,
			"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "WINDIR=" + os.Getenv("WINDIR"),
		}
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			_ = os.WriteFile(filepath.Join(root, "mce4d-exec-child.error"), []byte(err.Error()), 0o600)
			os.Exit(3)
		}
		if err := os.WriteFile(filepath.Join(root, "mce4d-exec-child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(4)
		}
		waitForFile(t, filepath.Join(root, "mce4d-exec-child.ready"))
	case "MCE4D_EXEC_HELPER_SUCCESS_TREE_CHILD":
		root := helperTempRoot()
		if err := os.WriteFile(filepath.Join(root, "mce4d-exec-child.ready"), []byte("ready"), 0o600); err != nil {
			_ = os.WriteFile(filepath.Join(root, "mce4d-exec-child.error"), []byte(err.Error()), 0o600)
			os.Exit(6)
		}
		waitForever()
	case "MCE4D_EXEC_HELPER_TREE_CHILD":
		root := helperTempRoot()
		if err := os.WriteFile(filepath.Join(root, "mce4d-exec-child.ready"), []byte("ready"), 0o600); err != nil {
			_ = os.WriteFile(filepath.Join(root, "mce4d-exec-child.error"), []byte(err.Error()), 0o600)
			os.Exit(6)
		}
		waitForever()
	case "MCE4D_EXEC_HELPER_BLOCK":
		waitForever()
	}
}

func waitForever() {
	for {
		time.Sleep(time.Hour)
	}
}

func helperTempRoot() string {
	if value := os.Getenv("TMPDIR"); value != "" {
		return value
	}
	if value := os.Getenv("TEMP"); value != "" {
		return value
	}
	return os.TempDir()
}

func TestRunRejectsUnknownEnvironmentAndShellWrapper(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("tool"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Spec{Path: tool, Dir: dir, Env: map[string]string{"PATH": "", "GOFLAGS": "-mod=readonly", "SECRET_TOKEN": "x"}}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("unknown environment error = %v, want ErrInvalidCommand", err)
	}
	if _, err := Run(context.Background(), Spec{Path: filepath.Join(dir, "tool.cmd"), Dir: dir}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("shell wrapper error = %v, want ErrInvalidCommand", err)
	}
}

func TestRunCancelsOwnedProcessTreeAndSettlesChild(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan runOutcome, 1)
	go func() {
		result, runErr := Run(ctx, Spec{
			Path: executable, Args: []string{"-test.run=^TestRunHelperProcess$"}, Dir: dir,
			Env:            map[string]string{"GOOS": "MCE4D_EXEC_HELPER_TREE_PARENT", "PATH": "", "TMPDIR": dir, "TEMP": dir, "TMP": dir, "SYSTEMROOT": os.Getenv("SYSTEMROOT"), "WINDIR": os.Getenv("WINDIR")},
			MaxStdoutBytes: 1024, MaxStderrBytes: 1024,
		})
		resultCh <- runOutcome{result: result, err: runErr}
	}()
	waitForFileOrRun(t, filepath.Join(dir, "mce4d-exec-child.ready"), resultCh)
	waitForFile(t, filepath.Join(dir, "mce4d-exec-parent.ready"))
	childPIDBytes, err := os.ReadFile(filepath.Join(dir, "mce4d-exec-child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid fixture child PID %q: %v", childPIDBytes, err)
	}
	cancel()
	select {
	case outcome := <-resultCh:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("controlled command result=%+v error=%v, want context cancellation", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controlled command did not settle after cancellation")
	}
	waitForProcessExit(t, childPID)
}

func TestRunSettlesDescendantsAfterParentSuccess(t *testing.T) {
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resultCh := make(chan runOutcome, 1)
	go func() {
		result, runErr := Run(ctx, Spec{
			Path: executable, Args: []string{"-test.run=^TestRunHelperProcess$"}, Dir: dir,
			Env:            map[string]string{"GOOS": "MCE4D_EXEC_HELPER_SUCCESS_TREE_PARENT", "PATH": "", "TMPDIR": dir, "TEMP": dir, "TMP": dir, "SYSTEMROOT": os.Getenv("SYSTEMROOT"), "WINDIR": os.Getenv("WINDIR")},
			MaxStdoutBytes: 1024, MaxStderrBytes: 1024,
		})
		resultCh <- runOutcome{result: result, err: runErr}
	}()
	select {
	case outcome := <-resultCh:
		if outcome.err != nil || outcome.result.ExitCode != 0 || outcome.result.TerminationReason != "" || outcome.result.CleanupStatus != "pass" {
			t.Fatalf("successful parent did not finalize cleanly: result=%+v err=%v", outcome.result, outcome.err)
		}
	case <-ctx.Done():
		if pidBytes, readErr := os.ReadFile(filepath.Join(dir, "mce4d-exec-child.pid")); readErr == nil {
			if childPID, parseErr := strconv.Atoi(string(pidBytes)); parseErr == nil {
				if child, findErr := os.FindProcess(childPID); findErr == nil {
					_ = child.Kill()
				}
			}
		}
		<-resultCh
		t.Fatal("controlled command left an inherited writer open after its parent exited")
	}
	childPIDBytes, err := os.ReadFile(filepath.Join(dir, "mce4d-exec-child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(string(childPIDBytes))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid fixture child PID %q: %v", childPIDBytes, err)
	}
	waitForProcessExit(t, childPID)
}

func TestRunCancelsWhenProcessTreeRSSExceedsBudget(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Spec{
		Path: executable, Args: []string{"-test.run=^TestRunHelperProcess$"}, Dir: t.TempDir(),
		Env:            map[string]string{"GOOS": "MCE4D_EXEC_HELPER_BLOCK", "PATH": ""},
		MaxStdoutBytes: 1024, MaxStderrBytes: 1024, RSSBudgetBytes: 1, MonitorInterval: time.Millisecond,
	})
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("RSS budget error = %v, want ErrResourceLimit (result=%+v)", err, result)
	}
	if result.TerminationReason != "rss_limit" || result.PeakRSSBytes <= 1 || result.CleanupStatus != "pass" {
		t.Fatalf("RSS termination evidence is incomplete: %+v", result)
	}
}

func TestRunRejectsUnadmittedStorageBeforeStartingTool(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Spec{
		Path: executable, Args: []string{"-test.run=^TestRunHelperProcess$"}, Dir: t.TempDir(),
		Env:            map[string]string{"GOOS": "MCE4D_EXEC_HELPER_OUTPUT", "PATH": ""},
		MaxStdoutBytes: 1024, MaxStderrBytes: 1024,
		StoragePlan: &StoragePlan{
			Policy:       ResourceAdmissionPolicy{BlockThresholdBytes: math.MaxInt64, WarningThresholdBytes: math.MaxInt64, MaxObservationAge: time.Second},
			Destinations: []StorageDestination{{Name: "fixture-output", Path: t.TempDir(), PeakRemainingBytes: 1}},
		},
	})
	if !errors.Is(err, ErrStorageBudget) || result.TerminationReason != "resource_preflight" || result.CleanupStatus != "not_required" ||
		result.StoragePreflight != "blocked" || len(result.StorageAdmission) != 1 || result.StorageAdmission[0].Admitted {
		t.Fatalf("storage preflight did not block before start: result=%+v err=%v", result, err)
	}
}

func TestDefaultProcessLimitsMatchRuntimeConfiguration(t *testing.T) {
	limits := config.DefaultRuntimeConfig().Core
	if defaultRSSBudgetBytes != limits.ProcessTreeRSSBudgetBytes || int64(defaultMonitorInterval/time.Millisecond) != limits.ResourceMonitorIntervalMS {
		t.Fatalf("controlled runner defaults diverged from runtime configuration: rss=%d/%d interval_ms=%d/%d",
			defaultRSSBudgetBytes, limits.ProcessTreeRSSBudgetBytes, int64(defaultMonitorInterval/time.Millisecond), limits.ResourceMonitorIntervalMS)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("fixture did not create %s", path)
}

func waitForFileOrRun(t *testing.T, path string, resultCh <-chan runOutcome) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return
		}
		select {
		case outcome := <-resultCh:
			t.Fatalf("controlled command exited before fixture readiness: result=%+v error=%v", outcome.result, outcome.err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if detail, err := os.ReadFile(filepath.Join(filepath.Dir(path), "mce4d-exec-child.error")); err == nil {
		t.Fatalf("fixture did not create %s: %s", path, detail)
	}
	t.Fatalf("fixture did not create %s", path)
}
