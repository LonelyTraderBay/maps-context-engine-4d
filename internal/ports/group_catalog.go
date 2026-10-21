package ports

import (
	"context"
	"errors"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var (
	ErrGroupNotFound  = errors.New("repository group not found")
	ErrGroupConflict  = errors.New("repository group revision conflict")
	ErrGroupStalePage = errors.New("repository group page is stale")
)

type GroupCatalog interface {
	GetGroup(context.Context, model.UUID) (catalog.GroupSnapshot, error)
	ListGroups(context.Context, int64, string) (catalog.GroupListPage, error)
	PutGroup(context.Context, catalog.GroupConfig, int64) (catalog.GroupSnapshot, error)
	RemoveGroup(context.Context, model.UUID, int64) (catalog.GroupSnapshot, error)
}
