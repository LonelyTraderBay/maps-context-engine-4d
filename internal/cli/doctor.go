package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	appconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/config"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
	_ "modernc.org/sqlite"
)

type doctorReport struct {
	Operation     string            `json:"operation"`
	Version       string            `json:"version"`
	Platform      platformInfo      `json:"platform"`
	Executables   []executableInfo  `json:"executables"`
	Home          homeInfo          `json:"home"`
	SQLiteFTS5    capabilityInfo    `json:"sqlite_fts5"`
	RuntimeConfig runtimeConfigInfo `json:"runtime_config"`
}

type platformInfo struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type executableInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Issue     string `json:"issue,omitempty"`
}

type homeInfo struct {
	Path             string `json:"path,omitempty"`
	DirectoryExists  bool   `json:"directory_exists"`
	FilesystemUsable bool   `json:"filesystem_usable"`
	Issue            string `json:"issue,omitempty"`
}

type capabilityInfo struct {
	Available bool   `json:"available"`
	Issue     string `json:"issue,omitempty"`
}

type runtimeConfigInfo struct {
	Valid            bool     `json:"valid"`
	SourceKind       string   `json:"source_kind,omitempty"`
	SourceDigest     any      `json:"source_digest"`
	EffectiveSources []string `json:"effective_sources,omitempty"`
	Issue            string   `json:"issue,omitempty"`
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, map[string]optionSpec{"json": {}})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "json") {
		return writeFailure(stderr, fmt.Errorf("%w: doctor requires --json", errUsage))
	}
	if err := ctx.Err(); err != nil {
		return writeFailure(stderr, err)
	}

	report := doctorReport{
		Operation: "doctor", Version: version,
		Platform: platformInfo{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Executables: []executableInfo{
			probeExecutable(ctx, "git"), probeExecutable(ctx, "codex"), probeExecutable(ctx, "go"),
		},
	}
	report.SQLiteFTS5 = probeFTS5(ctx)
	homePath, pathErr := resolveHomePath()
	if pathErr != nil {
		report.Home.Issue = "home_path_invalid"
		report.RuntimeConfig = runtimeConfigInfo{SourceDigest: nil, Issue: "home_path_invalid"}
	} else {
		report.Home = inspectHome(homePath)
		report.RuntimeConfig = inspectRuntimeConfig(ctx, homePath)
	}
	if err := ctx.Err(); err != nil {
		return writeFailure(stderr, err)
	}
	return writeJSON(stdout, stderr, report)
}

func probeExecutable(ctx context.Context, name string) executableInfo {
	result := executableInfo{Name: name}
	path, err := exec.LookPath(name)
	if err != nil {
		result.Issue = "not_found"
		return result
	}
	result.Available = true
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, path, "--version")
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = 4096, 4096
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			result.Issue = "version_probe_timeout"
		} else if errors.Is(err, errOutputLimit) {
			result.Issue = "version_output_exceeded_limit"
		} else {
			result.Issue = "version_probe_failed"
		}
		return result
	}
	result.Version = strings.TrimSpace(stdout.String())
	if result.Version == "" {
		result.Version = strings.TrimSpace(stderr.String())
	}
	if result.Version == "" {
		result.Issue = "version_unreported"
	}
	return result
}

var errOutputLimit = errors.New("subprocess output exceeded diagnostic limit")

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		return 0, errOutputLimit
	}
	if len(value) > remaining {
		written, _ := buffer.Buffer.Write(value[:remaining])
		return written, errOutputLimit
	}
	return buffer.Buffer.Write(value)
}

func probeFTS5(ctx context.Context) capabilityInfo {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return capabilityInfo{Issue: "sqlite_open_failed"}
	}
	defer database.Close()
	if _, err := database.ExecContext(probeCtx, "CREATE VIRTUAL TABLE mce4d_fts_probe USING fts5(content)"); err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return capabilityInfo{Issue: "probe_timeout"}
		}
		return capabilityInfo{Issue: "fts5_unavailable"}
	}
	return capabilityInfo{Available: true}
}

func inspectHome(path string) homeInfo {
	result := homeInfo{Path: path}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			parent := filepath.Dir(path)
			if _, parentErr := os.Stat(parent); parentErr == nil {
				result.FilesystemUsable = true
				result.Issue = "home_directory_not_created"
			} else {
				result.Issue = "home_parent_unavailable"
			}
			return result
		}
		result.Issue = "home_inspection_failed"
		return result
	}
	result.DirectoryExists = info.IsDir()
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		result.Issue = "home_not_a_verified_directory"
		return result
	}
	root, err := mceruntime.OpenRoot(path)
	if err != nil {
		result.Issue = "home_root_verification_failed"
		return result
	}
	_, _, identityErr := root.Identity()
	closeErr := root.Close()
	if identityErr != nil || closeErr != nil {
		result.Issue = "home_filesystem_probe_failed"
		return result
	}
	result.FilesystemUsable = true
	return result
}

func inspectRuntimeConfig(ctx context.Context, homePath string) runtimeConfigInfo {
	pipeline, err := runtimeconfig.NewPipeline()
	if err != nil {
		return runtimeConfigInfo{SourceDigest: nil, Issue: "config_validator_unavailable"}
	}
	store, err := newHomeConfigStore(homePath, pipeline)
	if err != nil {
		return runtimeConfigInfo{SourceDigest: nil, Issue: "home_store_unavailable"}
	}
	active, err := pipeline.Resolve(runtimeconfig.Source{}, nil, nil, nil, runtime.NumCPU(), runtimeconfig.ModeShow)
	if err != nil {
		return runtimeConfigInfo{SourceDigest: nil, Issue: "built_in_config_invalid"}
	}
	service, err := appconfig.NewService(store, pipeline, active.ConfigDigest)
	if err != nil {
		return runtimeConfigInfo{SourceDigest: nil, Issue: "config_service_unavailable"}
	}
	data, err := service.Show(ctx, nil, false, nil, runtime.NumCPU())
	if err != nil {
		return runtimeConfigInfo{SourceDigest: nil, Issue: "canonical_runtime_config_invalid_or_unreadable"}
	}
	return runtimeConfigInfo{
		Valid: true, SourceKind: data.SourceKind, SourceDigest: data.SourceDigest,
		EffectiveSources: data.EffectiveSources,
	}
}
