package search

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/cursor"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const searchOrderVersion = "search-keyset-v1"

// PageRequest adds the caller and pinned scope identity required to safely
// continue a deterministic Search request. The codec key must come from the
// private runtime cursor-key store.
type PageRequest struct {
	Request          Request
	Scope            model.ScopeVector
	PrincipalID      model.Identifier
	AllowedBindingID model.Identifier
	Codec            *cursor.CursorCodec
	Cursor           string
}

type PageResult struct {
	Result     Result
	NextCursor string
	HasMore    bool
	Position   int64
}

type searchPageKey struct {
	Rank  int    `json:"rank"`
	Value string `json:"value"`
	ID    string `json:"id"`
}

type textPageKey struct {
	Rank float64 `json:"rank"`
	Path string  `json:"path"`
	ID   string  `json:"id"`
}

func (searcher *Searcher) SearchPage(ctx context.Context, input PageRequest) (PageResult, error) {
	if searcher == nil || searcher.db == nil {
		return PageResult{}, ErrDatabaseUnavailable
	}
	if ctx == nil || input.Codec == nil {
		return PageResult{}, fmt.Errorf("page context and cursor codec are required: %w", ErrInvalidRequest)
	}
	request, err := normalizeRequest(input.Request)
	if err != nil {
		return PageResult{}, err
	}
	request.Kinds = append([]string(nil), request.Kinds...)
	sort.Strings(request.Kinds)
	if err := input.Scope.Validate(); err != nil || !scopeContainsGeneration(input.Scope, request.GenerationID) {
		return PageResult{}, fmt.Errorf("pinned scope does not contain the requested generation: %w", ErrInvalidRequest)
	}
	requestHash, err := searchRequestHash(request)
	if err != nil {
		return PageResult{}, fmt.Errorf("hash normalized search page request: %w", err)
	}
	identity := cursor.CursorIdentity{
		Purpose: "list", Scope: input.Scope, PrincipalID: input.PrincipalID,
		AllowedBindingID: input.AllowedBindingID, RequestHash: requestHash,
		OrderVersion: searchOrderVersion,
	}
	progress := cursor.CursorProgress{}
	if input.Cursor != "" {
		progress, err = input.Codec.Verify(input.Cursor, identity)
		if err != nil {
			return PageResult{}, err
		}
		if progress.ResultID != nil || progress.ResultDigest != nil {
			return PageResult{}, fmt.Errorf("search keyset cursor cannot carry materialized result identity: %w", cursor.ErrCursorInvalid)
		}
	}

	tx, err := searcher.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PageResult{}, fmt.Errorf("begin read-only search page snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM generations WHERE generation_id = ?`, request.GenerationID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PageResult{}, ErrGenerationNotReady
		}
		return PageResult{}, fmt.Errorf("read search page generation state: %w", err)
	}
	if state != "READY" && state != "READY_PARTIAL" {
		return PageResult{}, ErrGenerationNotReady
	}
	page := PageResult{Result: Result{GenerationID: request.GenerationID, Mode: request.Mode, Hits: make([]Hit, 0), Partial: state == "READY_PARTIAL"}, Position: progress.Position}
	if request.Mode == ModeText && !hasFTSToken(request.Query) {
		return page, nil
	}
	var hits []Hit
	var lastKey string
	if request.Mode == ModeSymbol {
		hits, lastKey, err = searchSymbolPage(ctx, tx, request, progress.LastKey)
	} else {
		hits, lastKey, err = searchTextPage(ctx, tx, request, progress.LastKey)
	}
	if err != nil {
		return PageResult{}, err
	}
	page.HasMore = len(hits) > request.Limit
	if page.HasMore {
		hits = hits[:request.Limit]
	}
	page.Result.Hits = hits
	page.Result.Truncated = page.HasMore
	page.Result.Ambiguous = request.Mode == ModeSymbol && (page.Position+int64(len(hits)) > 1 || page.HasMore)
	if page.Position > model.MaxSafeInteger-int64(len(hits)) {
		return PageResult{}, fmt.Errorf("search page position overflow: %w", cursor.ErrCursorInvalid)
	}
	page.Position += int64(len(hits))
	if page.HasMore {
		token, err := input.Codec.Issue(identity, cursor.CursorProgress{LastKey: lastKey, Position: page.Position})
		if err != nil {
			return PageResult{}, err
		}
		page.NextCursor = token
	}
	return page, nil
}

func scopeContainsGeneration(scope model.ScopeVector, generationID model.UUID) bool {
	for _, member := range scope.Members {
		if member.GenerationID == generationID {
			return true
		}
	}
	return false
}

func searchRequestHash(request Request) (model.Hash, error) {
	kinds := make([]any, len(request.Kinds))
	for index, kind := range request.Kinds {
		kinds[index] = kind
	}
	return model.HashValue("search-page-request-v1", map[string]any{
		"generation_id": string(request.GenerationID),
		"mode":          string(request.Mode),
		"match":         string(request.Match),
		"query":         request.Query,
		"kinds":         kinds,
		"package":       request.Package,
		"limit":         request.Limit,
	})
}

func searchSymbolPage(ctx context.Context, tx *sql.Tx, request Request, lastKey string) ([]Hit, string, error) {
	var after searchPageKey
	if lastKey != "" {
		if err := decodePageKey(lastKey, &after); err != nil || (after.Rank != 0 && after.Rank != 1) || after.ID == "" {
			return nil, "", cursor.ErrCursorInvalid
		}
	}
	first := buildSymbolPageCandidate(request, "s.name", 0)
	second := buildSymbolPageCandidate(request, "s.qualified_name", 1)
	query := `WITH matches AS (` + first.sql + ` UNION ALL ` + second.sql + `),
		deduplicated AS (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY symbol_id ORDER BY match_rank, match_value) AS match_choice
			FROM matches
		)
		SELECT symbol_id,path,package,kind,name,qualified_name,signature,match_rank,match_value
		FROM deduplicated WHERE match_choice=1`
	args := append(first.args, second.args...)
	if lastKey != "" {
		query += ` AND (match_rank > ? OR (match_rank = ? AND (match_value > ? OR (match_value = ? AND symbol_id > ?))))`
		args = append(args, after.Rank, after.Rank, after.Value, after.Value, after.ID)
	}
	query += ` ORDER BY match_rank,match_value,symbol_id LIMIT ?`
	args = append(args, request.Limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query keyset symbol page: %w", err)
	}
	defer rows.Close()
	hits := make([]Hit, 0, request.Limit+1)
	keys := make([]string, 0, request.Limit+1)
	var next string
	for rows.Next() {
		var hit Hit
		var path sql.NullString
		var rank int
		var value string
		if err := rows.Scan(&hit.ID, &path, &hit.Package, &hit.Kind, &hit.Name, &hit.QualifiedName, &hit.Signature, &rank, &value); err != nil {
			return nil, "", fmt.Errorf("scan keyset symbol page: %w", err)
		}
		if path.Valid {
			hit.Path, hit.HasPath = path.String, true
		}
		if request.Match == MatchPrefix {
			hit.MatchKind = MatchKindPrefixSymbol
		} else {
			hit.MatchKind = MatchKindExactSymbol
		}
		hits = append(hits, hit)
		key, err := encodePageKey(searchPageKey{Rank: rank, Value: value, ID: hit.ID})
		if err != nil {
			return nil, "", err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate keyset symbol page: %w", err)
	}
	if len(keys) > 0 {
		keyIndex := len(keys) - 1
		if keyIndex >= request.Limit {
			keyIndex = request.Limit - 1
		}
		next = keys[keyIndex]
	}
	return hits, next, nil
}

type symbolPageCandidate struct {
	sql  string
	args []any
}

func buildSymbolPageCandidate(request Request, column string, rank int) symbolPageCandidate {
	predicate, predicateArgs := symbolPredicate(column, request)
	filters, filterArgs := buildFilters("s.kind", "s", "", request)
	query := `SELECT s.symbol_id AS symbol_id,s.path AS path,COALESCE(p.import_path,'') AS package,
		s.kind AS kind,s.name AS name,s.qualified_name AS qualified_name,s.signature AS signature,
		` + strconv.Itoa(rank) + ` AS match_rank,` + column + ` AS match_value
		FROM symbols s LEFT JOIN packages p ON p.generation_id=s.generation_id AND p.package_key=s.package_key
		WHERE s.generation_id=? AND ` + predicate + `
		AND (s.path IS NULL OR EXISTS (SELECT 1 FROM files f WHERE f.generation_id=s.generation_id AND f.path=s.path AND f.status='indexed'))` + filters
	args := []any{request.GenerationID}
	args = append(args, predicateArgs...)
	args = append(args, filterArgs...)
	return symbolPageCandidate{sql: query, args: args}
}

func searchTextPage(ctx context.Context, tx *sql.Tx, request Request, lastKey string) ([]Hit, string, error) {
	var after textPageKey
	if lastKey != "" {
		if err := decodePageKey(lastKey, &after); err != nil || math.IsNaN(after.Rank) || math.IsInf(after.Rank, 0) || after.Path == "" || after.ID == "" {
			return nil, "", cursor.ErrCursorInvalid
		}
	}
	filters, filterArgs := buildFilters("d.kind", "", "f", request)
	query := `SELECT d.item_id,d.path,d.kind,d.name,d.qualified_name,search_fts.rank
		FROM search_fts JOIN search_documents d ON d.rowid=search_fts.rowid
		JOIN files f ON f.generation_id=d.generation_id AND f.path=d.path
		WHERE search_fts MATCH ? AND d.generation_id=? AND f.status='indexed'` + filters
	args := []any{quoteFTSPhrase(request.Query), request.GenerationID}
	args = append(args, filterArgs...)
	if lastKey != "" {
		query += ` AND (search_fts.rank > ? OR (search_fts.rank = ? AND (d.path > ? OR (d.path = ? AND d.item_id > ?))))`
		args = append(args, after.Rank, after.Rank, after.Path, after.Path, after.ID)
	}
	query += ` ORDER BY search_fts.rank,d.path,d.item_id LIMIT ?`
	args = append(args, request.Limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query keyset text page: %w", err)
	}
	defer rows.Close()
	hits := make([]Hit, 0, request.Limit+1)
	keys := make([]string, 0, request.Limit+1)
	var next string
	for rows.Next() {
		var hit Hit
		var rank float64
		hit.HasPath = true
		hit.MatchKind = MatchKindText
		if err := rows.Scan(&hit.ID, &hit.Path, &hit.Kind, &hit.Name, &hit.QualifiedName, &rank); err != nil {
			return nil, "", fmt.Errorf("scan keyset text page: %w", err)
		}
		if math.IsNaN(rank) || math.IsInf(rank, 0) {
			return nil, "", fmt.Errorf("FTS rank is not finite: %w", ErrInvalidRequest)
		}
		hits = append(hits, hit)
		key, err := encodePageKey(textPageKey{Rank: rank, Path: hit.Path, ID: hit.ID})
		if err != nil {
			return nil, "", err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate keyset text page: %w", err)
	}
	if len(keys) > 0 {
		keyIndex := len(keys) - 1
		if keyIndex >= request.Limit {
			keyIndex = request.Limit - 1
		}
		next = keys[keyIndex]
	}
	return hits, next, nil
}

func encodePageKey(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode search page key: %w", cursor.ErrCursorInvalid)
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > 4096 {
		return "", fmt.Errorf("search key exceeds cursor bound: %w", cursor.ErrCursorInvalid)
	}
	return encoded, nil
}

func decodePageKey(value string, target any) error {
	if len(value) == 0 || len(value) > 4096 {
		return cursor.ErrCursorInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor.ErrCursorInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return cursor.ErrCursorInvalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return cursor.ErrCursorInvalid
	}
	return nil
}
