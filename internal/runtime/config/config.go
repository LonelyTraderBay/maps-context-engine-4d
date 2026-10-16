// Package config resolves the immutable RuntimeConfig used by a process.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrInvalidConfig  = errors.New("invalid runtime config")
	ErrConfigConflict = errors.New("runtime config conflict")
)

type Mode uint8

const (
	ModeShow Mode = iota + 1
	ModeValidate
	ModeSet
)

type SourceKind string

const (
	SourceBuiltIn       SourceKind = "built_in"
	SourceCanonicalHome SourceKind = "canonical_home"
)

type Source struct {
	present bool
	value   map[string]any
	digest  model.Hash
	json    []byte
}

func (source Source) Present() bool { return source.present }

func (source Source) Digest() *model.Hash {
	if !source.present {
		return nil
	}
	digest := source.digest
	return &digest
}

func (source Source) CanonicalJSON() []byte { return bytes.Clone(source.json) }

type Pipeline struct {
	validator *contracts.Validator
}

func NewPipeline() (*Pipeline, error) {
	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("create runtime config validator: %w", err)
	}
	return &Pipeline{validator: validator}, nil
}

type Preview struct {
	Config           RuntimeConfig
	ConfigDigest     model.Hash
	SourceKind       SourceKind
	SourceDigest     *model.Hash
	EffectiveSources []string
}

func (preview Preview) Data(restartRequired bool) RuntimeConfigData {
	sources := append([]string(nil), preview.EffectiveSources...)
	return RuntimeConfigData{
		Operation:        "runtime_config",
		Config:           preview.Config,
		ConfigDigest:     preview.ConfigDigest,
		RestartRequired:  restartRequired,
		SourceKind:       string(preview.SourceKind),
		SourceDigest:     preview.SourceDigest,
		EffectiveSources: sources,
	}
}

type RuntimeConfigData struct {
	Operation        string        `json:"operation"`
	Config           RuntimeConfig `json:"config"`
	ConfigDigest     model.Hash    `json:"config_digest"`
	RestartRequired  bool          `json:"restart_required"`
	SourceKind       string        `json:"source_kind"`
	SourceDigest     *model.Hash   `json:"source_digest"`
	EffectiveSources []string      `json:"effective_sources"`
}

// Resolve applies exactly the layers that participate in the requested operation.
func (pipeline *Pipeline) Resolve(home Source, candidate, alternate *Source, overrides map[string]int64, cpuCount int, mode Mode) (Preview, error) {
	if pipeline == nil || pipeline.validator == nil || cpuCount < 1 {
		return Preview{}, fmt.Errorf("%w: pipeline and positive CPU count are required", ErrInvalidConfig)
	}
	if err := validateOverrides(overrides); err != nil {
		return Preview{}, err
	}
	if mode < ModeShow || mode > ModeSet {
		return Preview{}, fmt.Errorf("%w: unknown operation mode", ErrInvalidConfig)
	}
	if mode == ModeShow && candidate != nil || mode != ModeShow && (candidate == nil || alternate != nil) {
		return Preview{}, fmt.Errorf("%w: invalid source layers for operation", ErrInvalidConfig)
	}
	if candidate != nil && !candidate.present || alternate != nil && !alternate.present {
		return Preview{}, fmt.Errorf("%w: candidate and alternate sources must be validated and present", ErrInvalidConfig)
	}

	base := defaultValue()
	sourceKind := SourceBuiltIn
	sourceDigest := (*model.Hash)(nil)
	if home.present {
		base = merge(base, home.value)
		sourceKind = SourceCanonicalHome
		sourceDigest = home.Digest()
	}

	var layers []string
	if mode == ModeValidate {
		base = defaultValue()
		base = merge(base, candidate.value)
		layers = []string{"built_in", "candidate_input"}
	} else if mode == ModeSet {
		base = defaultValue()
		base = merge(base, candidate.value)
		sourceKind = SourceCanonicalHome
		sourceDigest = candidate.Digest()
		layers = []string{"built_in", "canonical_home"}
	} else {
		layers = []string{"built_in"}
		if home.present {
			layers = append(layers, "canonical_home")
		}
	}

	if mode == ModeShow && alternate != nil {
		before := cloneMap(base)
		base = merge(base, alternate.value)
		if !sameSharedFields(before, base) {
			return Preview{}, fmt.Errorf("%w: alternate config changes shared admission fields", ErrConfigConflict)
		}
		layers = append(layers, "approved_alternate")
	}
	if len(overrides) > 0 {
		applyOverrides(base, overrides)
		layers = append(layers, "cli_process")
	}

	if err := pipeline.validateValue(base); err != nil {
		return Preview{}, err
	}
	config, err := decodeConfig(base)
	if err != nil {
		return Preview{}, err
	}
	if config.Core.MaxParseWorkers == 0 {
		workers := cpuCount
		if workers > 4 {
			workers = 4
		}
		if workers < 1 {
			workers = 1
		}
		config.Core.MaxParseWorkers = int64(workers)
	}
	if err := config.ValidateEffective(); err != nil {
		return Preview{}, err
	}
	effective, err := jsonValue(config)
	if err != nil {
		return Preview{}, err
	}
	digest, err := model.HashValue("runtime-config-v1", effective)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: hash effective config: %v", ErrInvalidConfig, err)
	}
	return Preview{
		Config:           config,
		ConfigDigest:     digest,
		SourceKind:       sourceKind,
		SourceDigest:     sourceDigest,
		EffectiveSources: layers,
	}, nil
}

func (pipeline *Pipeline) CanonicalizeCandidate(raw []byte) (Source, error) {
	return pipeline.ParseSource(raw, true)
}

func (pipeline *Pipeline) ParseSource(raw []byte, present bool) (Source, error) {
	if !present {
		if len(raw) != 0 {
			return Source{}, fmt.Errorf("%w: absent source included bytes", ErrInvalidConfig)
		}
		return Source{}, nil
	}
	return parseSourceWithValidator(raw, pipeline.validator)
}

func parseSourceWithValidator(raw []byte, validator *contracts.Validator) (Source, error) {
	if validator == nil {
		return Source{}, fmt.Errorf("%w: validator is required", ErrInvalidConfig)
	}
	value, canonical, err := parseConfigWithValidator(raw, validator)
	if err != nil {
		return Source{}, err
	}
	digest, err := model.HashValue("runtime-config-source-v1", value)
	if err != nil {
		return Source{}, fmt.Errorf("%w: hash source: %v", ErrInvalidConfig, err)
	}
	return Source{present: true, value: value, digest: digest, json: canonical}, nil
}

func parseConfigWithValidator(raw []byte, validator *contracts.Validator) (map[string]any, []byte, error) {
	value, err := contracts.DecodeJSON(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: decode source: %v", ErrInvalidConfig, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("%w: config must be an object", ErrInvalidConfig)
	}
	canonical, err := model.CanonicalJSON1(object)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: canonicalize source: %v", ErrInvalidConfig, err)
	}
	request := append([]byte(`{"action":"validate","config":`), canonical...)
	request = append(request, '}')
	if err := validator.ValidateRequest("CLI config", request); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return cloneMap(object), canonical, nil
}

func (pipeline *Pipeline) validateValue(value map[string]any) error {
	canonical, err := model.CanonicalJSON1(value)
	if err != nil {
		return fmt.Errorf("%w: canonicalize config: %v", ErrInvalidConfig, err)
	}
	request := append([]byte(`{"action":"validate","config":`), canonical...)
	request = append(request, '}')
	if err := pipeline.validator.ValidateRequest("CLI config", request); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return nil
}

func validateOverrides(overrides map[string]int64) error {
	for key, value := range overrides {
		if !processOverrideAllowed[key] {
			return fmt.Errorf("%w: CLI cannot override %q", ErrInvalidConfig, key)
		}
		if value < 0 || key != "max_parse_workers" && value == 0 {
			return fmt.Errorf("%w: invalid CLI override for %q", ErrInvalidConfig, key)
		}
	}
	return nil
}

func applyOverrides(value map[string]any, overrides map[string]int64) {
	core, _ := value["core"].(map[string]any)
	for key, item := range overrides {
		core[key] = item
	}
}

func sameSharedFields(left, right map[string]any) bool {
	leftCore, _ := left["core"].(map[string]any)
	rightCore, _ := right["core"].(map[string]any)
	for _, key := range []string{"max_worktree_cache_bytes", "volume_reserve_bytes", "volume_warning_bytes"} {
		if fmt.Sprint(leftCore[key]) != fmt.Sprint(rightCore[key]) {
			return false
		}
	}
	return true
}

func merge(base, overlay map[string]any) map[string]any {
	result := cloneMap(base)
	for key, value := range overlay {
		if child, ok := value.(map[string]any); ok {
			if prior, ok := result[key].(map[string]any); ok {
				result[key] = merge(prior, child)
				continue
			}
		}
		result[key] = cloneValue(value)
	}
	return result
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = cloneValue(value)
	}
	return result
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = cloneValue(typed[index])
		}
		return result
	default:
		return typed
	}
}

func defaultValue() map[string]any {
	value, err := jsonValue(DefaultRuntimeConfig())
	if err != nil {
		panic("invalid built-in runtime config: " + err.Error())
	}
	return value
}

func jsonValue(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoded, err := contracts.DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	result, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("value must encode as object")
	}
	return result, nil
}

func decodeConfig(value map[string]any) (RuntimeConfig, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("%w: encode config: %v", ErrInvalidConfig, err)
	}
	var config RuntimeConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return RuntimeConfig{}, fmt.Errorf("%w: decode config: %v", ErrInvalidConfig, err)
	}
	return config, nil
}
