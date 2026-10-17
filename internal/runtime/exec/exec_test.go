package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRunUsesExplicitEnvironmentAndBoundsOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture executable is a POSIX shell script")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s' \"$PATH\"\nprintf '%s' \"$LEAK\" >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Spec{
		Path: tool, Dir: dir,
		Env:            map[string]string{"PATH": "", "TMPDIR": dir},
		MaxStdoutBytes: 16, MaxStderrBytes: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "" || string(result.Stderr) != "" {
		t.Fatalf("runner inherited an undeclared variable: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	_, err = Run(context.Background(), Spec{Path: tool, Dir: dir, Env: map[string]string{"PATH": "0123456789"}, MaxStdoutBytes: 4, MaxStderrBytes: 4})
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("oversized output error = %v, want ErrOutputLimit", err)
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
