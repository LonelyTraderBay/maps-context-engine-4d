package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if result.ExitCode != 0 || strings.Contains(string(result.Stdout), "must-not-reach-child") || strings.Contains(string(result.Stderr), "must-not-reach-child") {
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
}

func TestRunHelperProcess(t *testing.T) {
	switch os.Getenv("GOOS") {
	case "MCE4D_EXEC_HELPER_ENV":
		_, _ = os.Stdout.Write([]byte(os.Getenv("LEAK")))
		_, _ = os.Stderr.Write([]byte(os.Getenv("LEAK")))
	case "MCE4D_EXEC_HELPER_OUTPUT":
		_, _ = os.Stdout.Write([]byte("0123456789"))
		_, _ = os.Stderr.Write([]byte("9876543210"))
	}
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
