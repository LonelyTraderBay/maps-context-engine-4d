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
)

var (
	ErrInvalidCommand = errors.New("invalid controlled command")
	ErrOutputLimit    = errors.New("controlled command output limit exceeded")
)

const (
	maxArguments = 4096
	maxEnvVars   = 64
)

var allowedEnv = map[string]struct{}{
	"PATH": {}, "GOOS": {}, "GOARCH": {}, "CGO_ENABLED": {}, "GOTOOLCHAIN": {},
	"GOPROXY": {}, "GOSUMDB": {}, "GOPACKAGESDRIVER": {}, "GOENV": {},
	"GOWORK": {}, "GOFLAGS": {}, "GOMODCACHE": {}, "GOCACHE": {},
	"TMPDIR": {}, "TEMP": {}, "TMP": {}, "SYSTEMROOT": {}, "WINDIR": {},
}

type Spec struct {
	Path           string
	Args           []string
	Dir            string
	Env            map[string]string
	MaxStdoutBytes int
	MaxStderrBytes int
}

type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Run starts one exact executable path. It never resolves a command through
// PATH, invokes a shell, or inherits the caller's environment implicitly.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if ctx == nil || !filepath.IsAbs(spec.Path) || !filepath.IsAbs(spec.Dir) ||
		len(spec.Args) > maxArguments || spec.MaxStdoutBytes < 0 || spec.MaxStderrBytes < 0 {
		return Result{}, ErrInvalidCommand
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

	cmd := osexec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = env
	stdout := &boundedBuffer{limit: spec.MaxStdoutBytes}
	stderr := &boundedBuffer{limit: spec.MaxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if stdout.exceeded || stderr.exceeded {
		return result, ErrOutputLimit
	}
	if err != nil {
		return result, fmt.Errorf("run controlled tool: %w", err)
	}
	return result, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = buffer.exceeded || len(value) > 0
		return len(value), nil
	}
	if len(value) > remaining {
		buffer.exceeded = true
		_, _ = buffer.Buffer.Write(value[:remaining])
		return len(value), nil
	}
	_, _ = buffer.Buffer.Write(value)
	return len(value), nil
}
