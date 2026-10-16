// Package search provides bounded, generation-scoped lookups over indexed metadata.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	DefaultLimit  = 20
	MaxLimit      = 100
	MaxQueryRunes = 512
	MaxIDRunes    = 192
	MaxKinds      = 16
)

type Mode string

const (
	ModeSymbol Mode = "symbol"
	ModeText   Mode = "text"
)

type MatchMode string

const (
	MatchExact  MatchMode = "exact"
	MatchPrefix MatchMode = "prefix"
)

type MatchKind string

const (
	MatchKindExactSymbol  MatchKind = "exact_symbol"
	MatchKindPrefixSymbol MatchKind = "prefix_symbol"
	MatchKindText         MatchKind = "text_match"
)

var (
	ErrInvalidRequest      = errors.New("invalid search request")
	ErrGenerationNotReady  = errors.New("search generation is not published")
	ErrDatabaseUnavailable = errors.New("search database is unavailable")
)

// Request is an internal query contract. GenerationID must come from a scope
// already authorized by the caller; this package never selects an active scope.
type Request struct {
	GenerationID model.UUID
	Mode         Mode
	Match        MatchMode
	Query        string
	Kinds        []string
	Package      string
	Limit        int
}

// Hit contains indexed metadata only. It never contains search_documents.body
// or source bytes. MatchKind distinguishes lexical matches from symbol lookup.
type Hit struct {
	ID            string
	Path          string
	HasPath       bool
	Package       string
	Kind          string
	Name          string
	QualifiedName string
	Signature     string
	MatchKind     MatchKind
}

type Result struct {
	GenerationID model.UUID
	Mode         Mode
	Hits         []Hit
	Ambiguous    bool
	Truncated    bool
	Partial      bool
}

// Searcher only issues SELECT statements. The supplied DB should be the
// bounded read-only pool exposed by the SQLite store.
type Searcher struct {
	db *sql.DB
}

func New(db *sql.DB) *Searcher {
	return &Searcher{db: db}
}

func (searcher *Searcher) Search(ctx context.Context, request Request) (Result, error) {
	if searcher == nil || searcher.db == nil {
		return Result{}, ErrDatabaseUnavailable
	}
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: nil context", ErrInvalidRequest)
	}
	request, err := normalizeRequest(request)
	if err != nil {
		return Result{}, err
	}

	tx, err := searcher.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Result{}, fmt.Errorf("begin read-only search snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM generations WHERE generation_id = ?`, request.GenerationID,
	).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, ErrGenerationNotReady
		}
		return Result{}, fmt.Errorf("read search generation state: %w", err)
	}
	if state != "READY" && state != "READY_PARTIAL" {
		return Result{}, ErrGenerationNotReady
	}
	result := Result{
		GenerationID: request.GenerationID,
		Mode:         request.Mode,
		Hits:         make([]Hit, 0),
		Partial:      state == "READY_PARTIAL",
	}
	if request.Mode == ModeText && !hasFTSToken(request.Query) {
		return result, nil
	}

	var hits []Hit
	var truncated bool
	switch request.Mode {
	case ModeSymbol:
		hits, truncated, err = searchSymbols(ctx, tx, request)
	case ModeText:
		hits, truncated, err = searchText(ctx, tx, request)
	}
	if err != nil {
		return Result{}, err
	}
	result.Hits = hits
	result.Truncated = truncated
	result.Ambiguous = request.Mode == ModeSymbol && (len(hits) > 1 || truncated)
	return result, nil
}

func normalizeRequest(request Request) (Request, error) {
	if _, err := model.ParseUUID(string(request.GenerationID)); err != nil {
		return Request{}, fmt.Errorf("%w: generation_id must be a UUID", ErrInvalidRequest)
	}
	if !utf8.ValidString(request.Query) || utf8.RuneCountInString(request.Query) == 0 ||
		utf8.RuneCountInString(request.Query) > MaxQueryRunes {
		return Request{}, fmt.Errorf("%w: query must contain 1 to %d valid Unicode characters", ErrInvalidRequest, MaxQueryRunes)
	}
	if request.Mode == "" {
		request.Mode = ModeSymbol
	}
	if request.Mode != ModeSymbol && request.Mode != ModeText {
		return Request{}, fmt.Errorf("%w: unsupported mode %q", ErrInvalidRequest, request.Mode)
	}
	if request.Match == "" {
		request.Match = MatchExact
	}
	if request.Match != MatchExact && request.Match != MatchPrefix {
		return Request{}, fmt.Errorf("%w: unsupported symbol match mode %q", ErrInvalidRequest, request.Match)
	}
	if request.Mode == ModeText && request.Match != MatchExact {
		return Request{}, fmt.Errorf("%w: prefix matching applies only to symbols", ErrInvalidRequest)
	}
	if request.Limit == 0 {
		request.Limit = DefaultLimit
	}
	if request.Limit < 1 || request.Limit > MaxLimit {
		return Request{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidRequest, MaxLimit)
	}
	if len(request.Kinds) > MaxKinds {
		return Request{}, fmt.Errorf("%w: kinds accepts at most %d entries", ErrInvalidRequest, MaxKinds)
	}
	for _, kind := range request.Kinds {
		if !utf8.ValidString(kind) || utf8.RuneCountInString(kind) == 0 || utf8.RuneCountInString(kind) > MaxIDRunes {
			return Request{}, fmt.Errorf("%w: kinds entries must contain 1 to %d valid Unicode characters", ErrInvalidRequest, MaxIDRunes)
		}
	}
	if request.Package != "" && (!utf8.ValidString(request.Package) || utf8.RuneCountInString(request.Package) > MaxQueryRunes) {
		return Request{}, fmt.Errorf("%w: package must be valid UTF-8 of at most %d characters", ErrInvalidRequest, MaxQueryRunes)
	}
	return request, nil
}

func searchSymbols(ctx context.Context, tx *sql.Tx, request Request) ([]Hit, bool, error) {
	queries := buildSymbolQueries(request)
	type candidate struct {
		hit        Hit
		matchRank  int
		matchValue string
	}
	byID := make(map[string]candidate, 2*(request.Limit+1))
	for _, query := range queries {
		rows, err := tx.QueryContext(ctx, query.statement, query.args...)
		if err != nil {
			return nil, false, fmt.Errorf("query indexed symbols by %s: %w", query.column, err)
		}
		for rows.Next() {
			var hit Hit
			var path sql.NullString
			if request.Match == MatchPrefix {
				hit.MatchKind = MatchKindPrefixSymbol
			} else {
				hit.MatchKind = MatchKindExactSymbol
			}
			if err := rows.Scan(&hit.ID, &path, &hit.Package, &hit.Kind, &hit.Name, &hit.QualifiedName, &hit.Signature); err != nil {
				_ = rows.Close()
				return nil, false, fmt.Errorf("scan indexed symbol: %w", err)
			}
			if path.Valid {
				hit.Path, hit.HasPath = path.String, true
			}
			value := hit.Name
			if query.rank != 0 {
				value = hit.QualifiedName
			}
			if _, exists := byID[hit.ID]; !exists {
				byID[hit.ID] = candidate{hit: hit, matchRank: query.rank, matchValue: value}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("iterate indexed symbols: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, false, fmt.Errorf("close indexed symbol rows: %w", err)
		}
	}
	candidates := make([]candidate, 0, len(byID))
	for _, item := range byID {
		candidates = append(candidates, item)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.matchRank != right.matchRank {
			return left.matchRank < right.matchRank
		}
		if left.matchValue != right.matchValue {
			return left.matchValue < right.matchValue
		}
		return left.hit.ID < right.hit.ID
	})
	hits := make([]Hit, 0, len(candidates))
	for _, item := range candidates {
		hits = append(hits, item.hit)
	}
	hits, truncated := trimHits(hits, request.Limit)
	return hits, truncated, nil
}

type symbolQuery struct {
	column    string
	rank      int
	statement string
	args      []any
}

func buildSymbolQueries(request Request) []symbolQuery {
	return []symbolQuery{
		buildSymbolQuery("s.name", 0, request),
		buildSymbolQuery("s.qualified_name", 1, request),
	}
}

func buildSymbolQuery(column string, rank int, request Request) symbolQuery {
	predicate, predicateArgs := symbolPredicate(column, request)
	filters, filterArgs := buildFilters("s.kind", "s", "", request)
	selectSQL := `SELECT s.symbol_id AS symbol_id, s.path AS path,
		COALESCE(p.import_path, '') AS package, s.kind AS kind,
		s.name AS name, s.qualified_name AS qualified_name, s.signature AS signature
		FROM symbols s
		LEFT JOIN packages p ON p.generation_id = s.generation_id AND p.package_key = s.package_key
		WHERE s.generation_id = ? AND `
	scopeFilter := `
		AND (s.path IS NULL OR EXISTS (
			SELECT 1 FROM files f WHERE f.generation_id = s.generation_id
			AND f.path = s.path AND f.status = 'indexed'
		))` + filters
	query := selectSQL + predicate + scopeFilter + " ORDER BY " + column + ", s.symbol_id LIMIT ?"
	args := []any{request.GenerationID}
	args = append(args, predicateArgs...)
	args = append(args, filterArgs...)
	args = append(args, request.Limit+1)
	return symbolQuery{column: column, rank: rank, statement: query, args: args}
}

func symbolPredicate(column string, request Request) (string, []any) {
	if request.Match == MatchExact {
		return column + " = ?", []any{request.Query}
	}
	return prefixPredicate(column, request.Query)
}

func searchText(ctx context.Context, tx *sql.Tx, request Request) ([]Hit, bool, error) {
	query, args := buildTextQuery(request)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("query generation-scoped FTS5 index: %w", err)
	}
	defer rows.Close()
	type rankedHit struct {
		hit  Hit
		rank float64
	}
	ranked := make([]rankedHit, 0, request.Limit+1)
	for rows.Next() {
		var item rankedHit
		hit := &item.hit
		hit.HasPath = true
		hit.MatchKind = MatchKindText
		if err := rows.Scan(&hit.ID, &hit.Path, &hit.Kind, &hit.Name, &hit.QualifiedName, &item.rank); err != nil {
			return nil, false, fmt.Errorf("scan FTS5 result: %w", err)
		}
		ranked = append(ranked, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate FTS5 results: %w", err)
	}
	sort.Slice(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.rank != right.rank {
			return left.rank < right.rank
		}
		if left.hit.Path != right.hit.Path {
			return left.hit.Path < right.hit.Path
		}
		return left.hit.ID < right.hit.ID
	})
	hits := make([]Hit, 0, len(ranked))
	for _, item := range ranked {
		hits = append(hits, item.hit)
	}
	hits, truncated := trimHits(hits, request.Limit)
	return hits, truncated, nil
}

func buildTextQuery(request Request) (string, []any) {
	filters, filterArgs := buildFilters("d.kind", "", "f", request)
	args := []any{quoteFTSPhrase(request.Query), request.GenerationID}
	args = append(args, filterArgs...)
	args = append(args, request.Limit+1)
	query := `SELECT d.item_id, d.path, d.kind, d.name, d.qualified_name, search_fts.rank
		FROM search_fts
		JOIN search_documents d ON d.rowid = search_fts.rowid
		JOIN files f ON f.generation_id = d.generation_id AND f.path = d.path
		WHERE search_fts MATCH ? AND d.generation_id = ? AND f.status = 'indexed'` + filters + `
		ORDER BY search_fts.rank
		LIMIT ?`
	return query, args
}

func buildFilters(kindColumn, symbolAlias, fileAlias string, request Request) (string, []any) {
	var clauses []string
	args := make([]any, 0, len(request.Kinds)+1)
	if len(request.Kinds) > 0 {
		placeholders := make([]string, len(request.Kinds))
		for i, kind := range request.Kinds {
			placeholders[i] = "?"
			args = append(args, kind)
		}
		clauses = append(clauses, kindColumn+" IN ("+strings.Join(placeholders, ",")+")")
	}
	if request.Package != "" {
		if symbolAlias != "" {
			clauses = append(clauses, `EXISTS (
				SELECT 1 FROM packages p_filter WHERE p_filter.generation_id = `+symbolAlias+`.generation_id
				AND p_filter.package_key = `+symbolAlias+`.package_key AND p_filter.import_path = ?
			)`)
		} else {
			clauses = append(clauses, `EXISTS (
				SELECT 1 FROM packages p_filter WHERE p_filter.generation_id = `+fileAlias+`.generation_id
				AND p_filter.package_key = `+fileAlias+`.package_key AND p_filter.import_path = ?
			)`)
		}
		args = append(args, request.Package)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

func prefixPredicate(column, prefix string) (string, []any) {
	if upper, ok := prefixUpperBound(prefix); ok {
		return "(" + column + " >= ? AND " + column + " < ?)", []any{prefix, upper}
	}
	// A prefix made entirely of the maximum Unicode scalar has no finite
	// successor. Its lexicographic tail contains only strings with that prefix.
	return "(" + column + " >= ? AND substr(" + column + ", 1, length(?)) = ?)",
		[]any{prefix, prefix, prefix}
}

func prefixUpperBound(prefix string) (string, bool) {
	runes := []rune(prefix)
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == utf8.MaxRune {
			continue
		}
		next := runes[i] + 1
		if next == 0xD800 {
			next = 0xE000
		}
		return string(runes[:i]) + string(next), true
	}
	return "", false
}

func trimHits(hits []Hit, limit int) ([]Hit, bool) {
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	return hits, truncated
}

func quoteFTSPhrase(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

func hasFTSToken(query string) bool {
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}
