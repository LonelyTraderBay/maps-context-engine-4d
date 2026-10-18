package search

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestSymbolPagesUseStableKeysetAndBindCallerRequest(t *testing.T) {
	db := newSearchFixture(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		qualified := "pkg.Find"
		if id == "a" {
			qualified = "Find"
		}
		addSymbol(t, db, searchGenerationA, id, id+"/api.go", "pkg-"+id, "example.test/"+id, "function", "Find", qualified)
	}
	codec := pageTestCodec(t)
	input := PageRequest{
		Request: Request{GenerationID: searchGenerationA, Query: "Find", Limit: 2},
		Scope:   pageTestScope(t), PrincipalID: "principal:test", AllowedBindingID: "binding:test", Codec: codec,
	}
	first, err := New(db).SearchPage(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Result.Hits) != 2 || !first.HasMore || first.NextCursor == "" || !first.Result.Ambiguous || first.Position != 2 {
		t.Fatalf("first keyset page metadata is invalid: %+v", first)
	}
	if first.Result.Hits[0].ID != "a" || first.Result.Hits[1].ID != "b" {
		t.Fatalf("first keyset page order is unstable: %+v", first.Result.Hits)
	}
	input.Cursor = first.NextCursor
	second, err := New(db).SearchPage(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Result.Hits) != 2 || !second.HasMore || second.Position != 4 || second.Result.Hits[0].ID != "c" || second.Result.Hits[1].ID != "d" {
		t.Fatalf("second keyset page repeated or skipped results: %+v", second)
	}
	input.Cursor = second.NextCursor
	third, err := New(db).SearchPage(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Result.Hits) != 1 || third.HasMore || third.NextCursor != "" || third.Position != 5 || !third.Result.Ambiguous || third.Result.Hits[0].ID != "e" {
		t.Fatalf("final keyset page lost cumulative state or result: %+v", third)
	}

	input.PrincipalID = "principal:other"
	if _, err := New(db).SearchPage(context.Background(), input); !errors.Is(err, cursor.ErrCursorScopeMismatch) {
		t.Fatalf("cross-principal continuation was accepted: %v", err)
	}
	input.PrincipalID = "principal:test"
	input.Request.Query = "Different"
	if _, err := New(db).SearchPage(context.Background(), input); !errors.Is(err, cursor.ErrCursorScopeMismatch) {
		t.Fatalf("changed query continuation was accepted: %v", err)
	}
}

func TestTextPagesUseRankPathAndItemIDKeyset(t *testing.T) {
	db := newSearchFixture(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		path := "pkg/" + id + ".go"
		addDocument(t, db, searchGenerationA, id, path, "go", "Needle", "pkg.Needle", "common needle body", "indexed", "", "")
	}
	input := PageRequest{
		Request: Request{GenerationID: searchGenerationA, Mode: ModeText, Query: "needle", Limit: 2},
		Scope:   pageTestScope(t), PrincipalID: "principal:test", AllowedBindingID: "binding:test", Codec: pageTestCodec(t),
	}
	var all []string
	for pageNumber := 0; pageNumber < 4; pageNumber++ {
		page, err := New(db).SearchPage(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range page.Result.Hits {
			all = append(all, hit.ID)
		}
		if !page.HasMore {
			if page.Position != 5 || page.NextCursor != "" {
				t.Fatalf("final text page state is invalid: %+v", page)
			}
			break
		}
		input.Cursor = page.NextCursor
	}
	if strings.Join(all, ",") != "a,b,c,d,e" {
		t.Fatalf("rank/path/item keyset repeated, skipped, or reordered results: %v", all)
	}
}

func pageTestCodec(t *testing.T) *cursor.CursorCodec {
	t.Helper()
	key := []byte(strings.Repeat("k", 32))
	codec, err := cursor.NewCursorCodec(key, cursor.DefaultCursorTTL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func pageTestScope(t *testing.T) model.ScopeVector {
	t.Helper()
	vector := model.ScopeVector{
		Kind: "single", Members: []model.ScopeMember{{
			WorktreeID: "11111111-1111-4111-8111-111111111111", ProfileID: model.Hash(strings.Repeat("a", 64)),
			GenerationID: searchGenerationA, SnapshotDigest: model.Hash(strings.Repeat("b", 64)),
			PolicyDigest: model.Hash(strings.Repeat("c", 64)), PolicyEpoch: 1, BindingRevision: 1,
			IndexIncarnation: "22222222-2222-4222-8222-222222222222", RecordsIncarnation: "33333333-3333-4333-8333-333333333333",
		}}, UnavailableMembers: []model.UnavailableMember{}, CatalogIncarnation: "44444444-4444-4444-8444-444444444444",
	}
	vector.VectorDigest, _ = vector.ComputeDigest()
	if err := vector.Validate(); err != nil {
		t.Fatal(err)
	}
	return vector
}
