package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

type memorySpool struct {
	entries map[model.UUID][]byte
}

func (spool *memorySpool) WriteRenameResultSpool(_ context.Context, id model.UUID, raw []byte, digest model.Hash) error {
	if int64(len(raw)) > sqlite.MaxRenameResultSpoolBytes || model.SHA256(raw) != digest {
		return sqlite.ErrRenameResultSpoolInvalid
	}
	if spool.entries == nil {
		spool.entries = make(map[model.UUID][]byte)
	}
	if existing, ok := spool.entries[id]; ok && string(existing) != string(raw) {
		return sqlite.ErrRenameResultSpoolConflict
	}
	spool.entries[id] = append([]byte(nil), raw...)
	return nil
}

func (spool *memorySpool) ReadRenameResultSpool(_ context.Context, id model.UUID, digest model.Hash) ([]byte, error) {
	raw, ok := spool.entries[id]
	if !ok {
		return nil, sqlite.ErrRenameResultSpoolExpired
	}
	if model.SHA256(raw) != digest {
		return nil, sqlite.ErrRenameResultSpoolInvalid
	}
	return append([]byte(nil), raw...), nil
}

func (spool *memorySpool) RemoveRenameResultSpool(_ context.Context, id model.UUID) error {
	if _, ok := spool.entries[id]; !ok {
		return sqlite.ErrRenameResultSpoolExpired
	}
	delete(spool.entries, id)
	return nil
}

func TestImmutableRenameResultPagesDoNotRerunAndBindFullIdentity(t *testing.T) {
	ctx := context.Background()
	spool := &memorySpool{}
	store, err := NewResultStore(spool)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_800_000_000, 0).UTC()
	store.now = func() time.Time { return clock }
	identity := resultTestIdentity(t)
	ref, err := store.Publish(ctx, identity, []json.RawMessage{
		json.RawMessage(`{"uri":"file:///a.go","start":1,"end":2,"text":"x"}`),
		json.RawMessage(`{"uri":"file:///b.go","start":3,"end":4,"text":"y"}`),
		json.RawMessage(`{"uri":"file:///c.go","start":5,"end":6,"text":"z"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := cursor.NewCursorCodec([]byte(strings.Repeat("r", 32)), cursor.DefaultCursorTTL, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Page(ctx, identity, ref, codec, "", 2)
	if err != nil || len(first.Edits) != 2 || !first.HasMore || first.Position != 2 || first.NextCursor == "" {
		t.Fatalf("first immutable page=%+v err=%v", first, err)
	}
	second, err := store.Page(ctx, identity, ref, codec, first.NextCursor, 2)
	if err != nil || len(second.Edits) != 1 || second.HasMore || second.NextCursor != "" || second.Position != 3 {
		t.Fatalf("second immutable page=%+v err=%v", second, err)
	}

	otherPrincipal := identity
	otherPrincipal.PrincipalID = "principal:other"
	if _, err := store.Page(ctx, otherPrincipal, ref, codec, first.NextCursor, 2); !errors.Is(err, cursor.ErrCursorScopeMismatch) {
		t.Fatalf("cross-principal page was accepted: %v", err)
	}
	ref.Digest = model.SHA256([]byte("different"))
	if _, err := store.Page(ctx, identity, ref, codec, "", 2); !errors.Is(err, sqlite.ErrRenameResultSpoolInvalid) {
		t.Fatalf("modified result digest was accepted: %v", err)
	}
	ref.Digest = model.SHA256(spool.entries[ref.ResultID])
	clock = time.Unix(ref.Expires, 0).UTC()
	if _, err := store.Page(ctx, identity, ref, codec, "", 2); !errors.Is(err, ErrResultExpired) {
		t.Fatalf("expired result was served: %v", err)
	}
}

func resultTestIdentity(t *testing.T) ResultIdentity {
	t.Helper()
	scope := model.ScopeVector{
		Kind: "single", Members: []model.ScopeMember{{
			WorktreeID: "11111111-1111-4111-8111-111111111111", ProfileID: model.Hash(strings.Repeat("a", 64)),
			GenerationID: "22222222-2222-4222-8222-222222222222", SnapshotDigest: model.Hash(strings.Repeat("b", 64)),
			PolicyDigest: model.Hash(strings.Repeat("c", 64)), PolicyEpoch: 1, BindingRevision: 1,
			IndexIncarnation: "33333333-3333-4333-8333-333333333333", RecordsIncarnation: "44444444-4444-4444-8444-444444444444",
		}}, UnavailableMembers: []model.UnavailableMember{}, CatalogIncarnation: "55555555-5555-4555-8555-555555555555",
	}
	var err error
	scope.VectorDigest, err = scope.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Validate(); err != nil {
		t.Fatal(err)
	}
	return ResultIdentity{Scope: scope, PrincipalID: "principal:test", AllowedBindingID: "binding:test", EngineSchema: EngineSchemaVersion}
}
