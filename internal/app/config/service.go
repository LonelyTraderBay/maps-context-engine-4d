// Package config coordinates RuntimeConfig operations without owning filesystem access.
package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	runtimeconfig "github.com/LonelyTraderBay/maps-context-engine-4d/internal/runtime/config"
)

var (
	ErrInvalidRequest = errors.New("invalid runtime config request")
	ErrConflict       = runtimeconfig.ErrConfigConflict
)

// Store implementations must CAS the canonical source digest while holding only a
// short home-config lock, then atomically replace runtime.json. They must not
// persist defaults, CPU resolution, alternate layers, or process overrides.
type Store interface {
	ReadCanonical(ctx context.Context) (raw []byte, present bool, err error)
	CompareAndSwapCanonical(ctx context.Context, expected *model.Hash, canonicalJSON []byte) error
}

type AdmissionSnapshot struct {
	Config       model.AdmissionConfig
	ConfigDigest model.Hash
	SourceDigest *model.Hash
}

// AdmissionConfigReader is consumed by home-shared quota admission. The
// returned digest covers only canonical shared fields, never process overrides.
type AdmissionConfigReader interface {
	ReadAdmissionSnapshot(ctx context.Context) (AdmissionSnapshot, error)
}

type Service struct {
	store        Store
	pipeline     *runtimeconfig.Pipeline
	activeDigest model.Hash
}

func NewService(store Store, pipeline *runtimeconfig.Pipeline, activeDigest model.Hash) (*Service, error) {
	if store == nil || pipeline == nil {
		return nil, fmt.Errorf("%w: store and pipeline are required", ErrInvalidRequest)
	}
	parsed, err := model.ParseHash(string(activeDigest))
	if err != nil {
		return nil, fmt.Errorf("%w: active config digest: %v", ErrInvalidRequest, err)
	}
	return &Service{store: store, pipeline: pipeline, activeDigest: parsed}, nil
}

func (service *Service) Show(ctx context.Context, alternateRaw []byte, alternatePresent bool, overrides map[string]int64, cpuCount int) (runtimeconfig.RuntimeConfigData, error) {
	home, err := service.readHome(ctx)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	var alternate *runtimeconfig.Source
	if alternatePresent {
		parsed, parseErr := service.pipeline.CanonicalizeCandidate(alternateRaw)
		if parseErr != nil {
			return runtimeconfig.RuntimeConfigData{}, parseErr
		}
		alternate = &parsed
	} else if len(alternateRaw) != 0 {
		return runtimeconfig.RuntimeConfigData{}, fmt.Errorf("%w: absent alternate included bytes", ErrInvalidRequest)
	}
	preview, err := service.pipeline.Resolve(home, nil, alternate, overrides, cpuCount, runtimeconfig.ModeShow)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	return preview.Data(preview.ConfigDigest != service.activeDigest), nil
}

func (service *Service) ReadAdmissionSnapshot(ctx context.Context) (AdmissionSnapshot, error) {
	home, err := service.readHome(ctx)
	if err != nil {
		return AdmissionSnapshot{}, err
	}
	// Parse-worker resolution does not affect the shared admission fields.
	preview, err := service.pipeline.Resolve(home, nil, nil, nil, 1, runtimeconfig.ModeShow)
	if err != nil {
		return AdmissionSnapshot{}, err
	}
	config := model.AdmissionConfig{
		SchemaVersion:         1,
		MaxWorktreeCacheBytes: preview.Config.Core.MaxWorktreeCacheBytes,
		VolumeReserveBytes:    preview.Config.Core.VolumeReserveBytes,
		VolumeWarningBytes:    preview.Config.Core.VolumeWarningBytes,
	}
	digest, err := config.Digest()
	if err != nil {
		return AdmissionSnapshot{}, fmt.Errorf("validate shared admission config: %w", err)
	}
	return AdmissionSnapshot{Config: config, ConfigDigest: digest, SourceDigest: copyDigest(home.Digest())}, nil
}

func (service *Service) Validate(ctx context.Context, candidateRaw []byte, alternatePresent bool, overrides map[string]int64, cpuCount int) (runtimeconfig.RuntimeConfigData, error) {
	if alternatePresent {
		return runtimeconfig.RuntimeConfigData{}, fmt.Errorf("%w: validate does not accept an alternate config file", ErrInvalidRequest)
	}
	home, err := service.readHome(ctx)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	candidate, err := service.pipeline.CanonicalizeCandidate(candidateRaw)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	preview, err := service.pipeline.Resolve(home, &candidate, nil, overrides, cpuCount, runtimeconfig.ModeValidate)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	return preview.Data(preview.ConfigDigest != service.activeDigest), nil
}

func (service *Service) Set(ctx context.Context, candidateRaw []byte, alternatePresent bool, expectedDigest *model.Hash, overrides map[string]int64, cpuCount int) (runtimeconfig.RuntimeConfigData, error) {
	if alternatePresent {
		return runtimeconfig.RuntimeConfigData{}, fmt.Errorf("%w: set does not accept an alternate config file", ErrInvalidRequest)
	}
	home, err := service.readHome(ctx)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	if expectedDigest != nil {
		if _, err := model.ParseHash(string(*expectedDigest)); err != nil {
			return runtimeconfig.RuntimeConfigData{}, fmt.Errorf("%w: expected digest: %v", ErrInvalidRequest, err)
		}
	}
	if !sameDigest(home.Digest(), expectedDigest) {
		return runtimeconfig.RuntimeConfigData{}, ErrConflict
	}
	candidate, err := service.pipeline.CanonicalizeCandidate(candidateRaw)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	preview, err := service.pipeline.Resolve(home, &candidate, nil, overrides, cpuCount, runtimeconfig.ModeSet)
	if err != nil {
		return runtimeconfig.RuntimeConfigData{}, err
	}
	canonical := candidate.CanonicalJSON()
	if len(canonical) == 0 {
		return runtimeconfig.RuntimeConfigData{}, fmt.Errorf("%w: candidate canonicalization failed", ErrInvalidRequest)
	}
	if err := service.store.CompareAndSwapCanonical(ctx, copyDigest(expectedDigest), canonical); err != nil {
		if errors.Is(err, runtimeconfig.ErrConfigConflict) {
			return runtimeconfig.RuntimeConfigData{}, ErrConflict
		}
		return runtimeconfig.RuntimeConfigData{}, fmt.Errorf("replace canonical runtime config: %w", err)
	}
	return preview.Data(preview.ConfigDigest != service.activeDigest), nil
}

func (service *Service) readHome(ctx context.Context) (runtimeconfig.Source, error) {
	raw, present, err := service.store.ReadCanonical(ctx)
	if err != nil {
		return runtimeconfig.Source{}, fmt.Errorf("read canonical runtime config: %w", err)
	}
	return service.pipeline.ParseSource(raw, present)
}

func sameDigest(left, right *model.Hash) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func copyDigest(value *model.Hash) *model.Hash {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}
