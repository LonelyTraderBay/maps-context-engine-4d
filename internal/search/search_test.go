package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	_ "modernc.org/sqlite"
)

const (
	searchGenerationA = model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	searchGenerationB = model.UUID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
)

func newSearchFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	statements := []string{
		`CREATE TABLE generations (generation_id TEXT PRIMARY KEY, state TEXT NOT NULL)`,
		`CREATE TABLE packages (generation_id TEXT NOT NULL, package_key TEXT NOT NULL, import_path TEXT NOT NULL, PRIMARY KEY(generation_id, package_key))`,
		`CREATE INDEX packages_import ON packages(generation_id, import_path)`,
		`CREATE TABLE files (generation_id TEXT NOT NULL, path TEXT NOT NULL, status TEXT NOT NULL, package_key TEXT, PRIMARY KEY(generation_id, path))`,
		`CREATE TABLE symbols (generation_id TEXT NOT NULL, symbol_id TEXT NOT NULL, path TEXT, package_key TEXT, kind TEXT NOT NULL, name TEXT NOT NULL, qualified_name TEXT NOT NULL, scope_key TEXT NOT NULL, start_byte INTEGER, end_byte INTEGER, signature TEXT NOT NULL, exported INTEGER NOT NULL, PRIMARY KEY(generation_id, symbol_id))`,
		`CREATE INDEX symbols_name ON symbols(generation_id, name, symbol_id)`,
		`CREATE INDEX symbols_qualified ON symbols(generation_id, qualified_name, symbol_id)`,
		`CREATE TABLE search_documents (rowid INTEGER PRIMARY KEY, generation_id TEXT NOT NULL, item_id TEXT NOT NULL, path TEXT NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL, qualified_name TEXT NOT NULL, body TEXT NOT NULL, UNIQUE(generation_id, item_id))`,
		`CREATE VIRTUAL TABLE search_fts USING fts5(name, qualified_name, body, content='search_documents', content_rowid='rowid', tokenize='unicode61')`,
		`CREATE TRIGGER search_documents_ai AFTER INSERT ON search_documents BEGIN INSERT INTO search_fts(rowid,name,qualified_name,body) VALUES(new.rowid,new.name,new.qualified_name,new.body); END`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create search fixture: %v", err)
		}
	}
	for _, id := range []model.UUID{searchGenerationA, searchGenerationB} {
		if _, err := db.Exec(`INSERT INTO generations(generation_id,state) VALUES(?, 'READY')`, id); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func addSymbol(t *testing.T, db *sql.DB, generation model.UUID, id, path, packageKey, importPath, kind, name, qualified string) {
	t.Helper()
	if packageKey != "" {
		if _, err := db.Exec(`INSERT OR IGNORE INTO packages(generation_id,package_key,import_path) VALUES(?,?,?)`, generation, packageKey, importPath); err != nil {
			t.Fatal(err)
		}
	}
	if path != "" {
		if _, err := db.Exec(`INSERT OR IGNORE INTO files(generation_id,path,status,package_key) VALUES(?,?,'indexed',?)`, generation, path, packageKey); err != nil {
			t.Fatal(err)
		}
	}
	var pathValue any
	var packageValue any
	if path != "" {
		pathValue = path
	}
	if packageKey != "" {
		packageValue = packageKey
	}
	if _, err := db.Exec(`INSERT INTO symbols(generation_id,symbol_id,path,package_key,kind,name,qualified_name,scope_key,start_byte,end_byte,signature,exported) VALUES(?,?,?,?,?,?,?,?,0,1,?,1)`, generation, id, pathValue, packageValue, kind, name, qualified, "scope-"+id, "func()"+name); err != nil {
		t.Fatal(err)
	}
}

func addDocument(t *testing.T, db *sql.DB, generation model.UUID, itemID, path, kind, name, qualified, body, status, packageKey, importPath string) {
	t.Helper()
	if packageKey != "" {
		if _, err := db.Exec(`INSERT OR IGNORE INTO packages(generation_id,package_key,import_path) VALUES(?,?,?)`, generation, packageKey, importPath); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO files(generation_id,path,status,package_key) VALUES(?,?,?,?)`, generation, path, status, nullableString(packageKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO search_documents(generation_id,item_id,path,kind,name,qualified_name,body) VALUES(?,?,?,?,?,?,?)`, generation, itemID, path, kind, name, qualified, body); err != nil {
		t.Fatal(err)
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestExactLookupKeepsCrossPackageAmbiguityAndGenerationBoundary(t *testing.T) {
	db := newSearchFixture(t)
	addSymbol(t, db, searchGenerationA, "one", "one/api.go", "pkg-one", "example.test/one", "function", "Apply", "example.test/one.Apply")
	addSymbol(t, db, searchGenerationA, "two", "two/types.go", "pkg-two", "example.test/two", "field", "Apply", "example.test/two.Record.Apply")
	addSymbol(t, db, searchGenerationB, "other-generation", "third/api.go", "pkg-three", "example.test/three", "function", "Apply", "example.test/three.Apply")

	result, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Query: "Apply"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 2 || !result.Ambiguous || result.Truncated {
		t.Fatalf("exact lookup must preserve both candidates: %+v", result)
	}
	if result.Hits[0].ID != "one" || result.Hits[1].ID != "two" {
		t.Fatalf("results are not ordered deterministically by path: %+v", result.Hits)
	}
	if result.Hits[0].Signature == "" || strings.Contains(result.Hits[0].Signature, "{") {
		t.Fatalf("expected signature metadata without source body: %+v", result.Hits[0])
	}
	qualified, err := New(db).Search(context.Background(), Request{
		GenerationID: searchGenerationA, Query: "example.test/one.Apply",
	})
	if err != nil || len(qualified.Hits) != 1 || qualified.Hits[0].ID != "one" {
		t.Fatalf("exact qualified-name lookup failed: %+v err=%v", qualified, err)
	}

	limited, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Query: "Apply", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Hits) != 1 || !limited.Truncated || !limited.Ambiguous {
		t.Fatalf("limited exact lookup must signal both truncation and ambiguity: %+v", limited)
	}

	filtered, err := New(db).Search(context.Background(), Request{
		GenerationID: searchGenerationA, Query: "Apply", Package: "example.test/two", Kinds: []string{"field"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Hits) != 1 || filtered.Hits[0].ID != "two" || filtered.Ambiguous {
		t.Fatalf("kind/package filters were not applied: %+v", filtered)
	}
}

func TestExactLookupDeduplicatesSymbolMatchingNameAndQualifiedName(t *testing.T) {
	db := newSearchFixture(t)
	addSymbol(t, db, searchGenerationA, "same", "same/api.go", "pkg-same", "example.test/same", "function", "Apply", "Apply")

	result, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Query: "Apply"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].ID != "same" || result.Ambiguous || result.Truncated {
		t.Fatalf("the same symbol matched through both indexes must appear once: %+v", result)
	}
}

func TestPrefixLookupUsesLiteralUnicodePrefix(t *testing.T) {
	db := newSearchFixture(t)
	addSymbol(t, db, searchGenerationA, "map", "a/map.go", "pkg-a", "example.test/a", "type", "Map", "example.test/a.Map")
	addSymbol(t, db, searchGenerationA, "maple", "b/maple.go", "pkg-b", "example.test/b", "type", "Maple", "example.test/b.Maple")
	addSymbol(t, db, searchGenerationA, "may", "c/may.go", "pkg-c", "example.test/c", "type", "May", "example.test/c.May")
	addSymbol(t, db, searchGenerationA, "han", "d/han.go", "pkg-d", "example.test/d", "type", "名字", "example.test/d.名字")
	addSymbol(t, db, searchGenerationA, "hanx", "e/hanx.go", "pkg-e", "example.test/e", "type", "名字表", "example.test/e.名字表")
	addSymbol(t, db, searchGenerationA, "max-rune", "f/max.go", "pkg-f", "example.test/f", "type", string(utf8.MaxRune)+"Tail", "example.test/f.Max")
	addSymbol(t, db, searchGenerationA, "before-max", "g/before.go", "pkg-g", "example.test/g", "type", string(utf8.MaxRune-1)+"Tail", "example.test/g.Before")

	plain, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Match: MatchPrefix, Query: "Map"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Hits) != 2 || plain.Hits[0].ID != "map" || plain.Hits[1].ID != "maple" {
		t.Fatalf("prefix should match only literal Map* symbols, not qualified names here: %+v", plain.Hits)
	}
	unicodeResult, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Match: MatchPrefix, Query: "名"})
	if err != nil {
		t.Fatal(err)
	}
	if len(unicodeResult.Hits) != 2 {
		t.Fatalf("Unicode prefix missed matching symbols: %+v", unicodeResult.Hits)
	}
	maxRune, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Match: MatchPrefix, Query: string(utf8.MaxRune)})
	if err != nil {
		t.Fatal(err)
	}
	if len(maxRune.Hits) != 1 || maxRune.Hits[0].ID != "max-rune" {
		t.Fatalf("maximum-scalar prefix was not handled literally: %+v", maxRune.Hits)
	}
	if got, ok := prefixUpperBound(string(rune(0xD7FF))); !ok || got != string(rune(0xE000)) {
		t.Fatalf("prefix upper bound crossed the invalid surrogate range: %U, %v", []rune(got), ok)
	}
}

func TestTextSearchIsPhraseBoundedGenerationScopedAndLexical(t *testing.T) {
	db := newSearchFixture(t)
	addDocument(t, db, searchGenerationA, "first", "one/routes.go", "go", "Routes", "example.test/one.Routes", "source alpha OR secret phrase content", "indexed", "pkg-one", "example.test/one")
	addDocument(t, db, searchGenerationA, "filtered", "hidden/routes.go", "go", "Hidden", "example.test/hidden.Hidden", "secret phrase outside the query", "metadata_only", "pkg-hidden", "example.test/hidden")
	addDocument(t, db, searchGenerationB, "other-generation", "two/routes.go", "go", "Routes", "example.test/two.Routes", "source alpha OR secret phrase content", "indexed", "pkg-two", "example.test/two")
	addDocument(t, db, searchGenerationA, "second", "one/worker.go", "go", "Worker", "example.test/one.Worker", "source alpha OR secret phrase content", "indexed", "pkg-one", "example.test/one")

	result, err := New(db).Search(context.Background(), Request{
		GenerationID: searchGenerationA, Mode: ModeText, Query: `alpha" OR secret*`, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].ID != "first" || !result.Truncated {
		t.Fatalf("FTS phrase, generation filter, or limit failed: %+v", result)
	}
	if result.Hits[0].MatchKind != MatchKindText || result.Hits[0].Signature != "" || !result.Hits[0].HasPath {
		t.Fatalf("text hit must be lexical metadata without body/source bytes: %+v", result.Hits[0])
	}
	byPackage, err := New(db).Search(context.Background(), Request{
		GenerationID: searchGenerationA, Mode: ModeText, Query: "alpha", Package: "example.test/one", Kinds: []string{"go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byPackage.Hits) != 2 || byPackage.Hits[0].ID != "first" || byPackage.Hits[1].ID != "second" {
		t.Fatalf("FTS kind/package filtering or stable ordering failed: %+v", byPackage.Hits)
	}
	noTokens, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Mode: ModeText, Query: `***`})
	if err != nil || len(noTokens.Hits) != 0 {
		t.Fatalf("punctuation-only FTS input should return no hits without an error: result=%+v err=%v", noTokens, err)
	}
}

func TestTextSearchCoversNonGoDocumentsWithoutCreatingSemanticFacts(t *testing.T) {
	db := newSearchFixture(t)
	addDocument(t, db, searchGenerationA, "typescript", "web/client.ts", "typescript", "Client", "web.Client", "calls external Client.doThing()", "indexed", "", "")
	addDocument(t, db, searchGenerationA, "sql", "db/schema.sql", "sql", "schema", "db.schema", "CREATE TABLE external_client (id INTEGER)", "indexed", "", "")
	addDocument(t, db, searchGenerationA, "config", "deploy/settings.yaml", "config", "settings", "deploy.settings", "external_endpoint: https://example.test", "indexed", "", "")

	result, err := New(db).Search(context.Background(), Request{
		GenerationID: searchGenerationA, Mode: ModeText, Query: "external",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 3 || result.Ambiguous || result.Truncated {
		t.Fatalf("text search should return all three lexical candidates: %+v", result)
	}
	seen := make(map[string]bool, len(result.Hits))
	for _, hit := range result.Hits {
		seen[hit.Kind] = hit.MatchKind == MatchKindText && hit.Signature == ""
	}
	for _, kind := range []string{"typescript", "sql", "config"} {
		if !seen[kind] {
			t.Errorf("missing lexical-only %s result: %+v", kind, result.Hits)
		}
	}
}

func TestSearchRejectsInvalidAndUnboundedRequests(t *testing.T) {
	base := Request{GenerationID: searchGenerationA, Query: "x"}
	cases := []struct {
		name string
		edit func(*Request)
	}{
		{"invalid generation", func(r *Request) { r.GenerationID = "bad" }},
		{"empty query", func(r *Request) { r.Query = "" }},
		{"invalid UTF-8", func(r *Request) { r.Query = string([]byte{0xff}) }},
		{"query too long", func(r *Request) { r.Query = strings.Repeat("界", MaxQueryRunes+1) }},
		{"limit too large", func(r *Request) { r.Limit = MaxLimit + 1 }},
		{"negative limit", func(r *Request) { r.Limit = -1 }},
		{"too many kinds", func(r *Request) { r.Kinds = make([]string, MaxKinds+1) }},
		{"empty kind", func(r *Request) { r.Kinds = []string{""} }},
		{"kind too long", func(r *Request) { r.Kinds = []string{strings.Repeat("k", MaxIDRunes+1)} }},
		{"package too long", func(r *Request) { r.Package = strings.Repeat("p", MaxQueryRunes+1) }},
		{"unsupported mode", func(r *Request) { r.Mode = "topic" }},
		{"prefix text mode", func(r *Request) { r.Mode, r.Match = ModeText, MatchPrefix }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.edit(&request)
			if _, err := normalizeRequest(request); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("normalizeRequest error = %v, want ErrInvalidRequest", err)
			}
		})
	}
	request, err := normalizeRequest(Request{GenerationID: searchGenerationA, Query: "x"})
	if err != nil || request.Mode != ModeSymbol || request.Match != MatchExact || request.Limit != DefaultLimit {
		t.Fatalf("defaults do not match Locate contract: request=%+v err=%v", request, err)
	}
}

func TestSearchRequiresPublishedGenerationAndHonorsCancellation(t *testing.T) {
	db := newSearchFixture(t)
	if _, err := db.Exec(`INSERT INTO generations(generation_id,state) VALUES('cccccccc-cccc-4ccc-8ccc-cccccccccccc','READY_PARTIAL')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO generations(generation_id,state) VALUES('dddddddd-dddd-4ddd-8ddd-dddddddddddd','BUILDING')`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(db).Search(context.Background(), Request{GenerationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Query: "x"}); !errors.Is(err, ErrGenerationNotReady) {
		t.Fatalf("building generation must be unavailable, got %v", err)
	}
	partial, err := New(db).Search(context.Background(), Request{GenerationID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Query: "x"})
	if err != nil || !partial.Partial {
		t.Fatalf("READY_PARTIAL generation must be explicit in result: %+v err=%v", partial, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(db).Search(ctx, Request{GenerationID: searchGenerationA, Query: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query must preserve cancellation: %v", err)
	}
}

func TestQueryPlansUseSymbolIndexesAndFTSIndexAtFixtureScale(t *testing.T) {
	db := newSearchFixture(t)
	for i := 0; i < 512; i++ {
		name := fmt.Sprintf("Entry%04d", i)
		path := fmt.Sprintf("pkg/file-%04d.go", i)
		id := fmt.Sprintf("symbol-%04d", i)
		addSymbol(t, db, searchGenerationA, id, path, "pkg", "example.test/pkg", "function", name, "example.test/pkg."+name)
		addDocument(t, db, searchGenerationA, id, path, "go", name, "example.test/pkg."+name, "needle common indexed body", "indexed", "pkg", "example.test/pkg")
	}
	request := Request{GenerationID: searchGenerationA, Query: "Entry0256", Mode: ModeSymbol, Match: MatchExact, Limit: DefaultLimit}
	queries := buildSymbolQueries(request)
	checkQueryPlan(t, db, queries[0].statement, queries[0].args, []string{"symbols_name"})
	checkQueryPlan(t, db, queries[1].statement, queries[1].args, []string{"symbols_qualified"})
	assertNoTempOrderBy(t, db, queries[0].statement, queries[0].args)
	assertNoTempOrderBy(t, db, queries[1].statement, queries[1].args)
	request.Match, request.Query = MatchPrefix, "Entry02"
	queries = buildSymbolQueries(request)
	checkQueryPlan(t, db, queries[0].statement, queries[0].args, []string{"symbols_name"})
	checkQueryPlan(t, db, queries[1].statement, queries[1].args, []string{"symbols_qualified"})
	assertNoTempOrderBy(t, db, queries[0].statement, queries[0].args)
	assertNoTempOrderBy(t, db, queries[1].statement, queries[1].args)
	textRequest := Request{GenerationID: searchGenerationA, Query: "needle", Mode: ModeText, Match: MatchExact, Limit: DefaultLimit}
	query, args := buildTextQuery(textRequest)
	checkQueryPlan(t, db, query, args, []string{"VIRTUAL TABLE INDEX"})
	assertNoTempOrderBy(t, db, query, args)

	bounded, err := New(db).Search(context.Background(), Request{GenerationID: searchGenerationA, Mode: ModeText, Query: "needle", Limit: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Hits) != 7 || !bounded.Truncated {
		t.Fatalf("large fixture result was not capped at the requested limit: %+v", bounded)
	}
}

func checkQueryPlan(t *testing.T, db *sql.DB, query string, args []any, wanted []string) {
	t.Helper()
	plan := strings.Join(queryPlanDetails(t, db, query, args), "\n")
	for _, fragment := range wanted {
		if !strings.Contains(plan, fragment) {
			t.Fatalf("query plan missing %q:\n%s", fragment, plan)
		}
	}
}

func assertNoTempOrderBy(t *testing.T, db *sql.DB, query string, args []any) {
	t.Helper()
	plan := strings.Join(queryPlanDetails(t, db, query, args), "\n")
	if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("query plan requires a temporary ORDER BY sort:\n%s", plan)
	}
}

func queryPlanDetails(t *testing.T, db *sql.DB, query string, args []any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}
