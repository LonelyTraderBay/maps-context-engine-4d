package groups

import (
	"context"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/ports"
)

func TestListDefaultsAndRejectsOutOfRangeRequests(t *testing.T) {
	store := &groupStoreSpy{}
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(context.Background(), 0, ""); err != nil {
		t.Fatal(err)
	}
	if store.limit != DefaultListLimit {
		t.Fatalf("default limit=%d, want %d", store.limit, DefaultListLimit)
	}
	if _, err := service.List(context.Background(), MaxListLimit+1, ""); err == nil {
		t.Fatal("over-limit request was accepted")
	}
	if _, err := service.List(context.Background(), 1, string(make([]byte, MaxCursorBytes+1))); err == nil {
		t.Fatal("oversized cursor was accepted")
	}
}

func TestPutValidatesGroupBeforeStoreCall(t *testing.T) {
	store := &groupStoreSpy{}
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Put(context.Background(), catalog.GroupConfig{}, 0); err == nil {
		t.Fatal("invalid group was accepted")
	}
	if store.putCalled {
		t.Fatal("invalid group reached the catalog writer")
	}
}

type groupStoreSpy struct {
	limit     int64
	putCalled bool
}

func (*groupStoreSpy) GetGroup(context.Context, model.UUID) (catalog.GroupSnapshot, error) {
	return catalog.GroupSnapshot{}, ports.ErrGroupNotFound
}
func (store *groupStoreSpy) ListGroups(_ context.Context, limit int64, _ string) (catalog.GroupListPage, error) {
	store.limit = limit
	return catalog.GroupListPage{Groups: []catalog.GroupSummary{}}, nil
}
func (store *groupStoreSpy) PutGroup(context.Context, catalog.GroupConfig, int64) (catalog.GroupSnapshot, error) {
	store.putCalled = true
	return catalog.GroupSnapshot{}, nil
}
func (*groupStoreSpy) RemoveGroup(context.Context, model.UUID, int64) (catalog.GroupSnapshot, error) {
	return catalog.GroupSnapshot{}, ports.ErrGroupNotFound
}
