package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const pythonProviderID = "python-v1"

var pythonVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type pythonProfileConfig struct {
	SchemaVersion          int        `json:"schema_version"`
	Language               string     `json:"language"`
	PythonImplementation   string     `json:"python_implementation"`
	PythonVersion          string     `json:"python_version"`
	PythonExecutableDigest model.Hash `json:"python_executable_digest"`
	ProviderDigest         model.Hash `json:"provider_digest"`
	ModuleRoots            []string   `json:"module_roots"`
	AnalysisMode           string     `json:"analysis_mode"`
	AllowProjectPlugins    bool       `json:"allow_project_plugins"`
}

// validatePythonProfile keeps the language-specific effective profile closed at
// the host boundary, before an external process receives any source snapshots.
func validatePythonProfile(profile Profile, descriptor Descriptor, sourceByPath map[string][]byte) error {
	if descriptor.ID != pythonProviderID || profile.SchemaID != "python-profile" || profile.SchemaVersion != 1 {
		return fmt.Errorf("provider or profile schema identity is unsupported")
	}
	decoded, err := contracts.DecodeJSON(profile.Config)
	if err != nil {
		return fmt.Errorf("profile config is not strict JSON: %v", err)
	}
	object, ok := decoded.(map[string]any)
	if !ok || len(object) != 9 {
		return fmt.Errorf("profile config does not contain the exact PythonProfile v1 keys")
	}
	for _, key := range []string{
		"schema_version", "language", "python_implementation", "python_version", "python_executable_digest",
		"provider_digest", "module_roots", "analysis_mode", "allow_project_plugins",
	} {
		if _, exists := object[key]; !exists {
			return fmt.Errorf("profile config is missing %q", key)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(profile.Config))
	decoder.DisallowUnknownFields()
	var config pythonProfileConfig
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("profile config does not match PythonProfile v1: %v", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("profile config contains trailing JSON")
	}
	if config.SchemaVersion != 1 || config.Language != "python" || config.PythonImplementation != "CPython" ||
		!pythonVersionPattern.MatchString(config.PythonVersion) || !validHash(config.PythonExecutableDigest) ||
		config.ProviderDigest != descriptor.Digest || !validHash(config.ProviderDigest) ||
		(config.AnalysisMode != "syntax" && config.AnalysisMode != "typed_when_available") || config.AllowProjectPlugins {
		return fmt.Errorf("profile fields do not match the sealed Python provider")
	}
	if len(config.ModuleRoots) == 0 || len(config.ModuleRoots) > 64 {
		return fmt.Errorf("profile must contain 1..64 captured module roots")
	}
	roots := append([]string(nil), config.ModuleRoots...)
	sort.Strings(roots)
	for index, root := range roots {
		if err := validateRelativePath(root); err != nil {
			return fmt.Errorf("invalid module root: %v", err)
		}
		if index > 0 && roots[index-1] == root {
			return fmt.Errorf("duplicate module root %q", root)
		}
		if config.ModuleRoots[index] != root {
			return fmt.Errorf("module roots must be sorted")
		}
	}
	pythonSourceFound := false
	for sourcePath := range sourceByPath {
		isPython := strings.HasSuffix(sourcePath, ".py") || strings.HasSuffix(sourcePath, ".pyi")
		if !isPython {
			continue
		}
		covered := false
		for _, root := range roots {
			if strings.HasPrefix(sourcePath, root+"/") {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("Python source %q is outside module roots", sourcePath)
		}
		pythonSourceFound = true
	}
	if !pythonSourceFound {
		return fmt.Errorf("request contains no captured Python source under module roots")
	}
	for _, root := range roots {
		covered := false
		for sourcePath := range sourceByPath {
			if (strings.HasSuffix(sourcePath, ".py") || strings.HasSuffix(sourcePath, ".pyi")) && strings.HasPrefix(sourcePath, root+"/") {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("module root %q has no captured Python source", root)
		}
	}
	return nil
}
