package groups

import (
	"context"
	"fmt"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/ports"
)

const (
	DefaultListLimit = int64(20)
	MaxListLimit     = int64(100)
	MaxCursorBytes   = 32 << 10
)

type Service struct {
	catalog ports.GroupCatalog
}

func NewService(store ports.GroupCatalog) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("group catalog is required")
	}
	return &Service{catalog: store}, nil
}

func (service *Service) Get(ctx context.Context, groupID model.UUID) (catalog.GroupSnapshot, error) {
	if ctx == nil || service == nil {
		return catalog.GroupSnapshot{}, fmt.Errorf("group service and context are required")
	}
	parsed, err := model.ParseUUID(string(groupID))
	if err != nil || parsed != groupID {
		return catalog.GroupSnapshot{}, fmt.Errorf("group ID is invalid: %w", model.ErrInvalidValue)
	}
	return service.catalog.GetGroup(ctx, groupID)
}

func (service *Service) List(ctx context.Context, limit int64, cursor string) (catalog.GroupListPage, error) {
	if ctx == nil || service == nil {
		return catalog.GroupListPage{}, fmt.Errorf("group service and context are required")
	}
	if limit == 0 {
		limit = DefaultListLimit
	}
	if limit < 1 || limit > MaxListLimit || len(cursor) > MaxCursorBytes {
		return catalog.GroupListPage{}, fmt.Errorf("group list limit or cursor is invalid: %w", model.ErrInvalidValue)
	}
	return service.catalog.ListGroups(ctx, limit, cursor)
}

func (service *Service) Put(ctx context.Context, config catalog.GroupConfig, expectedRevision int64) (catalog.GroupSnapshot, error) {
	if ctx == nil || service == nil || expectedRevision < 0 || expectedRevision > model.MaxSafeInteger {
		return catalog.GroupSnapshot{}, fmt.Errorf("group put request is invalid: %w", model.ErrInvalidValue)
	}
	if err := catalog.ValidateGroupConfig(config); err != nil {
		return catalog.GroupSnapshot{}, err
	}
	return service.catalog.PutGroup(ctx, config, expectedRevision)
}

func (service *Service) Remove(ctx context.Context, groupID model.UUID, expectedRevision int64) (catalog.GroupSnapshot, error) {
	if ctx == nil || service == nil || expectedRevision < 0 || expectedRevision > model.MaxSafeInteger {
		return catalog.GroupSnapshot{}, fmt.Errorf("group remove request is invalid: %w", model.ErrInvalidValue)
	}
	parsed, err := model.ParseUUID(string(groupID))
	if err != nil || parsed != groupID {
		return catalog.GroupSnapshot{}, fmt.Errorf("group ID is invalid: %w", model.ErrInvalidValue)
	}
	return service.catalog.RemoveGroup(ctx, groupID, expectedRevision)
}
