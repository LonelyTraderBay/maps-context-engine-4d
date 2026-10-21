package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	appconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/config"
	appexplorer "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/explorer"
	apprecords "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/records"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/history"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/ports"
	mceruntime "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

const version = "1.6.13"

var (
	errUsage         = errors.New("invalid command or arguments")
	errInputTooLarge = errors.New("input exceeds the 1 MiB limit")
	errInvalidHome   = errors.New("invalid MCE4D home path")
)

type optionSpec struct {
	value      bool
	repeatable bool
}

func Help() string {
	return `mce4d — Maps Context Engine 4D 1.6.13

Commands:
  mce4d version --json
  mce4d doctor --json
  mce4d config show [--json] [--alternate-json <file>] [--override <key=value>...]
  mce4d config validate --input-json <file-or-dash>
  mce4d config set --input-json <file-or-dash> --expected-digest <sha256-or-absent>
  mce4d store backup --target worktree --worktree-id <uuid> --output <owned-backup-dir> --json
  mce4d store gc --target worktree --worktree-id <uuid> --keep-per-profile <count> --json [--apply]
  mce4d store restore --manifest <approved-local-file> --confirm --wait-ms <bounded> --json
  mce4d store recover --target worktree --worktree-id <uuid> --report-only --json
  mce4d store recover --target worktree --worktree-id <uuid> --apply --confirm [--wait-ms <bounded>] --json
	mce4d records list --worktree-id <uuid> --input-json <file-or-dash> --json
	mce4d records read --worktree-id <uuid> --input-json <file-or-dash> --json
  mce4d group get|list|put|remove --input-json <file-or-dash>
  mce4d explorer --worktree-id <uuid> --profile-id <sha256> --listen 127.0.0.1:<port>

Config show layers the canonical home config, an explicit alternate preview,
and process overrides in that order. Validate and set use the complete candidate
with built-in defaults; set requires the current canonical source digest.
Use --input-json - to read candidate JSON from stdin. Diagnostics go to stderr.
Exit codes: 0 success, 2 arguments/input, 3 invalid config or stale precondition,
4 lock timeout/resource busy, 5 storage/internal error, 6 partial result.

Other task commands are enabled only after their typed runtime capabilities are registered.
`
}

// Run executes the supported local CLI commands. It returns the S13 process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(stdout, Help())
		if err != nil {
			return writeFailure(stderr, err)
		}
		return 0
	}
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		_, err := io.WriteString(stdout, Help())
		if err != nil {
			return writeFailure(stderr, err)
		}
		return 0
	}
	switch args[0] {
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(ctx, args[1:], stdout, stderr)
	case "config":
		return runConfig(ctx, args[1:], stdin, stdout, stderr)
	case "store":
		return runStore(ctx, args[1:], stdout, stderr)
	case "records":
		return runRecords(ctx, args[1:], stdin, stdout, stderr)
	case "group":
		return runGroups(ctx, args[1:], stdin, stdout, stderr)
	case "explorer":
		return runExplorer(ctx, args[1:], stdout, stderr)
	default:
		return writeFailure(stderr, fmt.Errorf("%w: %q", errUsage, args[0]))
	}
}

func runExplorer(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, map[string]optionSpec{
		"worktree-id": {value: true}, "profile-id": {value: true}, "listen": {value: true},
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	for _, required := range []string{"worktree-id", "profile-id", "listen"} {
		if !has(options, required) {
			return writeFailure(stderr, fmt.Errorf("%w: explorer requires --%s", errUsage, required))
		}
	}
	worktreeID, err := model.ParseUUID(first(options, "worktree-id"))
	if err != nil {
		return writeFailure(stderr, fmt.Errorf("%w: explorer worktree ID is invalid", errUsage))
	}
	profileID, err := model.ParseHash(first(options, "profile-id"))
	if err != nil {
		return writeFailure(stderr, fmt.Errorf("%w: explorer profile ID must be a canonical SHA-256", errUsage))
	}
	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	err = appexplorer.Run(ctx, appexplorer.Options{HomePath: homePath, WorktreeID: worktreeID, ProfileID: profileID}, first(options, "listen"), stdout)
	if err != nil {
		return writeFailure(stderr, err)
	}
	return 0
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	options, err := parseOptions(args, map[string]optionSpec{"json": {}})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "json") {
		return writeFailure(stderr, fmt.Errorf("%w: version requires --json", errUsage))
	}
	return writeJSON(stdout, stderr, map[string]any{
		"name": "maps-context-engine-4d", "version": version,
		"go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
	})
}

func runConfig(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeFailure(stderr, fmt.Errorf("%w: config requires show, validate, or set", errUsage))
	}
	optionsSchema := map[string]optionSpec{"json": {}}
	switch args[0] {
	case "show":
		optionsSchema["alternate-json"] = optionSpec{value: true}
		optionsSchema["override"] = optionSpec{value: true, repeatable: true}
	case "validate":
		optionsSchema["input-json"] = optionSpec{value: true}
	case "set":
		optionsSchema["input-json"] = optionSpec{value: true}
		optionsSchema["expected-digest"] = optionSpec{value: true}
	default:
		return writeFailure(stderr, fmt.Errorf("%w: config action %q is unavailable", errUsage, args[0]))
	}
	options, err := parseOptions(args[1:], optionsSchema)
	if err != nil {
		return writeFailure(stderr, err)
	}
	if (args[0] == "validate" || args[0] == "set") && !has(options, "input-json") {
		return writeFailure(stderr, fmt.Errorf("%w: %s requires --input-json", errUsage, args[0]))
	}
	if args[0] == "set" && !has(options, "expected-digest") {
		return writeFailure(stderr, fmt.Errorf("%w: set requires --expected-digest", errUsage))
	}
	if args[0] == "show" && has(options, "input-json") || args[0] != "show" && (has(options, "alternate-json") || has(options, "override")) {
		return writeFailure(stderr, fmt.Errorf("%w: flags do not apply to this config action", errUsage))
	}

	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	pipeline, err := runtimeconfig.NewPipeline()
	if err != nil {
		return writeFailure(stderr, err)
	}
	store, err := newHomeConfigStore(homePath, pipeline)
	if err != nil {
		return writeFailure(stderr, err)
	}
	active, err := pipeline.Resolve(runtimeconfig.Source{}, nil, nil, nil, runtime.NumCPU(), runtimeconfig.ModeShow)
	if err != nil {
		return writeFailure(stderr, err)
	}
	service, err := appconfig.NewService(store, pipeline, active.ConfigDigest)
	if err != nil {
		return writeFailure(stderr, err)
	}

	var data runtimeconfig.RuntimeConfigData
	switch args[0] {
	case "show":
		var alternate []byte
		alternatePresent := has(options, "alternate-json")
		if alternatePresent {
			alternate, err = readInput(first(options, "alternate-json"), stdin)
			if err != nil {
				return writeFailure(stderr, err)
			}
		}
		overrides, parseErr := parseOverrides(options["override"])
		if parseErr != nil {
			return writeFailure(stderr, parseErr)
		}
		data, err = service.Show(ctx, alternate, alternatePresent, overrides, runtime.NumCPU())
	case "validate":
		var candidate []byte
		candidate, err = readInput(first(options, "input-json"), stdin)
		if err == nil {
			data, err = service.Validate(ctx, candidate, false, nil, runtime.NumCPU())
		}
	case "set":
		var candidate []byte
		candidate, err = readInput(first(options, "input-json"), stdin)
		var expected *model.Hash
		if err == nil {
			expected, err = parseExpectedDigest(first(options, "expected-digest"))
		}
		if err == nil {
			data, err = service.Set(ctx, candidate, false, expected, nil, runtime.NumCPU())
		}
	}
	if err != nil {
		return writeFailure(stderr, err)
	}
	return writeJSON(stdout, stderr, data)
}

func parseExpectedDigest(value string) (*model.Hash, error) {
	if value == "absent" {
		return nil, nil
	}
	digest, err := model.ParseHash(value)
	if err != nil {
		return nil, fmt.Errorf("%w: expected digest must be a canonical SHA-256 value or absent", errUsage)
	}
	return &digest, nil
}

func parseOverrides(values []string) (map[string]int64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]int64, len(values))
	for _, value := range values {
		key, rendered, found := strings.Cut(value, "=")
		if !found || strings.TrimSpace(key) != key || key == "" || rendered == "" {
			return nil, fmt.Errorf("%w: override must use key=value", errUsage)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("%w: override %q is repeated", errUsage, key)
		}
		parsed, err := strconv.ParseInt(rendered, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: override %q must be an integer", errUsage, key)
		}
		result[key] = parsed
	}
	return result, nil
}

func parseOptions(args []string, schema map[string]optionSpec) (map[string][]string, error) {
	options := make(map[string][]string)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") || len(arg) == 2 {
			return nil, fmt.Errorf("%w: unexpected argument %q", errUsage, arg)
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		spec, ok := schema[name]
		if !ok {
			return nil, fmt.Errorf("%w: unknown flag --%s", errUsage, name)
		}
		if spec.value {
			if !hasValue {
				index++
				if index >= len(args) || strings.HasPrefix(args[index], "--") {
					return nil, fmt.Errorf("%w: --%s requires a value", errUsage, name)
				}
				value = args[index]
			}
			if value == "" {
				return nil, fmt.Errorf("%w: --%s cannot be empty", errUsage, name)
			}
		} else if hasValue {
			return nil, fmt.Errorf("%w: --%s does not take a value", errUsage, name)
		}
		if len(options[name]) > 0 && !spec.repeatable {
			return nil, fmt.Errorf("%w: --%s is repeated", errUsage, name)
		}
		options[name] = append(options[name], value)
	}
	return options, nil
}

func has(options map[string][]string, name string) bool {
	return len(options[name]) > 0
}

func first(options map[string][]string, name string) string {
	if len(options[name]) == 0 {
		return ""
	}
	return options[name][0]
}

func readInput(path string, stdin io.Reader) ([]byte, error) {
	var reader io.Reader
	var file *os.File
	if path == "-" {
		if stdin == nil {
			return nil, fmt.Errorf("%w: stdin is unavailable", errUsage)
		}
		reader = stdin
	} else {
		opened, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("%w: open input JSON: %v", errUsage, err)
		}
		file = opened
		info, err := file.Stat()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("read input JSON metadata: %w", err), file.Close())
		}
		if !info.Mode().IsRegular() {
			return nil, errors.Join(fmt.Errorf("%w: input JSON must be a regular file", errUsage), file.Close())
		}
		if info.Size() > maxRuntimeConfigBytes {
			return nil, errors.Join(errInputTooLarge, file.Close())
		}
		reader = file
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxRuntimeConfigBytes+1))
	var closeErr error
	if file != nil {
		closeErr = file.Close()
	}
	if len(data) > maxRuntimeConfigBytes {
		return nil, errors.Join(errInputTooLarge, readErr, closeErr)
	}
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	return data, nil
}

func resolveHomePath() (string, error) {
	if override := os.Getenv("MCE4D_HOME"); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%w: MCE4D_HOME must be an absolute local path", errInvalidHome)
		}
		return filepath.Clean(override), nil
	}
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" || !filepath.IsAbs(localAppData) {
			return "", fmt.Errorf("%w: LOCALAPPDATA must be set to an absolute path", errInvalidHome)
		}
		return filepath.Join(localAppData, "mce4d"), nil
	}
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		if !filepath.IsAbs(stateHome) {
			return "", fmt.Errorf("%w: XDG_STATE_HOME must be an absolute path", errInvalidHome)
		}
		return filepath.Join(stateHome, "mce4d"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	if home == "" {
		return "", fmt.Errorf("resolve user home directory: path is unavailable")
	}
	return filepath.Join(home, ".local", "state", "mce4d"), nil
}

func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return writeFailure(stderr, err)
	}
	return 0
}

func writeFailure(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintln(stderr, err)
	switch {
	case errors.Is(err, errUsage), errors.Is(err, errInputTooLarge), errors.Is(err, apprecords.ErrInvalidRequest), errors.Is(err, history.ErrInvalidRecord):
		return 2
	case errors.Is(err, errInvalidHome), errors.Is(err, appconfig.ErrConflict), errors.Is(err, runtimeconfig.ErrInvalidConfig), errors.Is(err, appconfig.ErrInvalidRequest), errors.Is(err, apprecords.ErrStalePage), errors.Is(err, apprecords.ErrSourceNotCurrent), errors.Is(err, history.ErrRecordNotFound), errors.Is(err, ports.ErrGroupConflict), errors.Is(err, ports.ErrGroupNotFound), errors.Is(err, ports.ErrGroupStalePage):
		return 3
	case errors.Is(err, context.DeadlineExceeded):
		return 4
	case errors.Is(err, context.Canceled):
		return 4
	case errors.Is(err, mceruntime.ErrLockBusy), errors.Is(err, mceruntime.ErrLockTimeout):
		return 4
	default:
		return 5
	}
}
