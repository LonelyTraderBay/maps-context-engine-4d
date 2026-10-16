package catalog

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
)

const (
	SecretRulesVersion     = "mce4d-secret-v1"
	DefaultPolicyMaxFile   = int64(2_097_152)
	DefaultPolicyMaxSource = int64(34_359_738_368)
	DefaultPolicyMaxOutput = int64(24_576)
)

func DefaultPolicy(root, gitExecutablePath string, gitExecutableSHA model.Hash) (PolicyConfig, error) {
	if !validPathValue(root) || !validPathValue(gitExecutablePath) {
		return PolicyConfig{}, fmt.Errorf("%w: verified root and Git executable path are required", model.ErrInvalidValue)
	}
	if _, err := model.ParseHash(string(gitExecutableSHA)); err != nil {
		return PolicyConfig{}, err
	}
	config := PolicyConfig{
		SchemaVersion:        1,
		AllowedRoots:         []string{root},
		DeniedGlobs:          policy.BuiltinDenyGlobs(),
		TrustedRulePaths:     []string{},
		DependencyRoots:      []string{},
		AllowedExecutables:   []AllowedExecutable{{Path: gitExecutablePath, SHA256: gitExecutableSHA}},
		AllowTargetExecution: false,
		AllowRuntimeDownload: false,
		MaxFileBytes:         DefaultPolicyMaxFile,
		MaxSourceBytes:       DefaultPolicyMaxSource,
		MaxOutputBytes:       DefaultPolicyMaxOutput,
		SecretRulesVersion:   SecretRulesVersion,
	}
	if err := config.Validate(); err != nil {
		return PolicyConfig{}, err
	}
	return config, nil
}

func (config PolicyConfig) Validate() error {
	if config.SchemaVersion != 1 || len(config.AllowedRoots) < 1 || len(config.AllowedRoots) > 32 || len(config.DeniedGlobs) > 256 || len(config.TrustedRulePaths) > 128 || len(config.DependencyRoots) > 128 || len(config.AllowedExecutables) > 32 {
		return fmt.Errorf("%w: policy collection or schema limit is invalid", model.ErrInvalidValue)
	}
	if config.AllowTargetExecution || config.AllowRuntimeDownload || config.MaxFileBytes < 1 || config.MaxSourceBytes < 1 || config.MaxOutputBytes < 4096 || config.MaxOutputBytes > 131072 || config.SecretRulesVersion != SecretRulesVersion {
		return fmt.Errorf("%w: policy may not enable execution/download or exceed fixed caps", model.ErrInvalidValue)
	}
	if err := validateUniqueStrings(config.AllowedRoots, 4096); err != nil {
		return fmt.Errorf("%w: allowed roots: %v", model.ErrInvalidValue, err)
	}
	if err := validateUniqueStrings(config.DependencyRoots, 4096); err != nil {
		return fmt.Errorf("%w: dependency roots: %v", model.ErrInvalidValue, err)
	}
	if err := validateUniqueStrings(config.DeniedGlobs, 512); err != nil {
		return fmt.Errorf("%w: denied globs: %v", model.ErrInvalidValue, err)
	}
	if err := validateUniqueStrings(config.TrustedRulePaths, 4096); err != nil {
		return fmt.Errorf("%w: trusted rule paths: %v", model.ErrInvalidValue, err)
	}
	seenExecutables := make(map[string]struct{}, len(config.AllowedExecutables))
	for _, executable := range config.AllowedExecutables {
		if !validPathValue(executable.Path) {
			return fmt.Errorf("%w: executable path is empty or malformed", model.ErrInvalidValue)
		}
		if _, err := model.ParseHash(string(executable.SHA256)); err != nil {
			return err
		}
		if _, exists := seenExecutables[executable.Path]; exists {
			return fmt.Errorf("%w: duplicate executable path", model.ErrInvalidValue)
		}
		seenExecutables[executable.Path] = struct{}{}
	}
	return nil
}

func (config PolicyConfig) Digest() (model.Hash, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	allowedRoots := sortedUnique(config.AllowedRoots)
	dependencyRoots := sortedUnique(config.DependencyRoots)
	deniedGlobs := sortedUnique(config.DeniedGlobs)
	executables := append([]AllowedExecutable(nil), config.AllowedExecutables...)
	sort.Slice(executables, func(i, j int) bool {
		if executables[i].Path != executables[j].Path {
			return strings.Compare(executables[i].Path, executables[j].Path) < 0
		}
		return strings.Compare(string(executables[i].SHA256), string(executables[j].SHA256)) < 0
	})
	executableValues := make([]any, 0, len(executables))
	for _, executable := range executables {
		executableValues = append(executableValues, map[string]any{"path": executable.Path, "sha256": executable.SHA256})
	}
	return model.HashValue("policy-v1", map[string]any{
		"schema_version":         config.SchemaVersion,
		"allowed_roots":          stringValues(allowedRoots),
		"denied_globs":           stringValues(deniedGlobs),
		"trusted_rule_paths":     stringValues(config.TrustedRulePaths),
		"dependency_roots":       stringValues(dependencyRoots),
		"allowed_executables":    executableValues,
		"allow_target_execution": config.AllowTargetExecution,
		"allow_runtime_download": config.AllowRuntimeDownload,
		"max_file_bytes":         config.MaxFileBytes,
		"max_source_bytes":       config.MaxSourceBytes,
		"max_output_bytes":       config.MaxOutputBytes,
		"secret_rules_version":   config.SecretRulesVersion,
	})
}

func validateUniqueStrings(values []string, maxBytes int) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validPathValue(value) || len(value) > maxBytes {
			return fmt.Errorf("value is empty, oversized, or malformed")
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("duplicate value")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validPathValue(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f || char == '\u0085' || char == '\u2028' || char == '\u2029' {
			return false
		}
	}
	return true
}

func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func stringValues(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
