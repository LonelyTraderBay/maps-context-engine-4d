package explorer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	transport "github.com/LonelyTraderBay/maps-context-engine-4d/internal/explorer"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

type graphRequest struct {
	Scope json.RawMessage `json:"scope"`
	Root  struct {
		Kind     string `json:"kind"`
		SymbolID string `json:"symbol_id"`
	} `json:"root"`
	Depth     *int64   `json:"depth"`
	NodeLimit *int64   `json:"node_limit"`
	EdgeLimit *int64   `json:"edge_limit"`
	Kinds     []string `json:"kinds"`
	Cursor    *string  `json:"cursor"`
}

type graphContent struct {
	bytes []byte
	hash  model.Hash
}

type graphContentReader struct {
	ctx       context.Context
	snapshot  *sqlite.ReadSnapshot
	service   *Service
	policy    catalogState
	usedBytes int64
	cache     map[string]graphContent
	failed    map[string]bool
}

type graphSymbol struct {
	id     string
	nodeID string
}

func (service *Service) graphResponse(ctx context.Context, raw []byte) ([]byte, error) {
	var request graphRequest
	if err := decodeRequest(raw, &request); err != nil {
		return marshalErrorEnvelope("INVALID_ARGUMENT", "Graph request is invalid.", "fix_request", 1<<20)
	}
	if request.Root.Kind != "workspace" || request.Root.SymbolID != "" || len(request.Scope) > 0 && !bytes.Equal(bytes.TrimSpace(request.Scope), []byte("null")) || request.Cursor != nil {
		return marshalErrorEnvelope("INVALID_ARGUMENT", "Explorer currently supports a workspace query without a cursor.", "fix_request", 1<<20)
	}
	if len(request.Kinds) > 32 ||
		(request.Depth != nil && (*request.Depth < 0 || *request.Depth > 8)) ||
		(request.NodeLimit != nil && *request.NodeLimit > 500) ||
		(request.EdgeLimit != nil && *request.EdgeLimit > 1500) {
		return marshalErrorEnvelope("INVALID_ARGUMENT", "Graph query limits exceed the supported request schema.", "fix_request", 1<<20)
	}
	for _, kind := range request.Kinds {
		if kind != "symbol" {
			return marshalErrorEnvelope("UNSUPPORTED_CAPABILITY", "Explorer supports indexed symbol nodes for this query.", "verify_dependency", 1<<20)
		}
	}
	depth := int64(3)
	if request.Depth != nil {
		depth = *request.Depth
	}
	nodeLimit := int64(maxGraphNodes)
	if request.NodeLimit != nil && *request.NodeLimit < nodeLimit {
		nodeLimit = *request.NodeLimit
	}
	if nodeLimit < 1 {
		return marshalErrorEnvelope("INVALID_ARGUMENT", "Graph node limit must be positive.", "fix_request", 1<<20)
	}
	edgeLimit := int64(maxGraphEdges)
	if request.EdgeLimit != nil && *request.EdgeLimit < edgeLimit {
		edgeLimit = *request.EdgeLimit
	}
	if edgeLimit < 1 {
		return marshalErrorEnvelope("INVALID_ARGUMENT", "Graph edge limit must be positive.", "fix_request", 1<<20)
	}
	limitTruncated := request.NodeLimit != nil && *request.NodeLimit > maxGraphNodes || request.EdgeLimit != nil && *request.EdgeLimit > maxGraphEdges
	var rawResponse []byte
	err := service.withCurrent(ctx, func(state *currentState, snapshot *sqlite.ReadSnapshot) error {
		if state.scope == nil || snapshot == nil {
			var err error
			if state.indexState == "missing" {
				rawResponse, err = marshalErrorEnvelope("INDEX_MISSING", "No current published index snapshot is available.", "explicit_index", 1<<20)
			} else {
				rawResponse, err = marshalErrorEnvelope("INDEX_STALE", "The current published index snapshot cannot be verified.", "retry_bounded", 1<<20)
			}
			return err
		}
		data, truncated, returned, err := service.buildGraph(ctx, state, snapshot, depth, nodeLimit, edgeLimit)
		if err != nil {
			return err
		}
		status, coverage := "ok", "supported_scope_complete"
		if truncated || state.partial || limitTruncated {
			status, coverage = "partial", "partial"
		}
		rawResponse, err = marshalEnvelope(status, state.scope, data, coverage, returned, truncated || state.partial || limitTruncated, !truncated && !state.partial && !limitTruncated)
		return err
	})
	return rawResponse, err
}

func (service *Service) buildGraph(ctx context.Context, state *currentState, snapshot *sqlite.ReadSnapshot, depth, nodeLimit, edgeLimit int64) (map[string]any, bool, int, error) {
	member := state.scope.Members[0]
	generationID := string(member.GenerationID)
	content := &graphContentReader{ctx: ctx, snapshot: snapshot, service: service, policy: state.catalog, cache: make(map[string]graphContent), failed: make(map[string]bool)}
	nodes := make([]map[string]any, 0, nodeLimit)
	symbols := make(map[string]string, nodeLimit)
	truncated := false
	if depth > 0 {
		queryLimit := nodeLimit*8 + 1
		rows, err := snapshot.QueryContext(ctx, `SELECT s.symbol_id,s.path,s.kind,s.name,s.qualified_name,s.start_byte,s.end_byte,
			f.content_hash,f.byte_length,f.status
			FROM symbols s JOIN files f ON f.generation_id=s.generation_id AND f.path=s.path
			WHERE s.generation_id=? AND s.path IS NOT NULL
			ORDER BY s.path,s.start_byte,s.end_byte,s.symbol_id LIMIT ?`, generationID, queryLimit)
		if err != nil {
			return nil, false, 0, fmt.Errorf("query published Explorer symbols: %w", err)
		}
		seen := int64(0)
		for rows.Next() {
			seen++
			if seen > queryLimit-1 {
				truncated = true
				break
			}
			var symbolID, path, symbolKind, name, qualified, fileStatus string
			var startByte, endByte int64
			var contentHash sql.NullString
			var byteLength sql.NullInt64
			if err := rows.Scan(&symbolID, &path, &symbolKind, &name, &qualified, &startByte, &endByte, &contentHash, &byteLength, &fileStatus); err != nil {
				_ = rows.Close()
				return nil, false, 0, fmt.Errorf("read published Explorer symbol: %w", err)
			}
			if !validGraphID(symbolID) || !service.pathAllowed(state.catalog, path) || fileStatus != "indexed" || !contentHash.Valid || !byteLength.Valid {
				truncated = true
				continue
			}
			ref, ok := content.sourceRef(path, model.Hash(contentHash.String), startByte, endByte)
			if !ok {
				truncated = true
				continue
			}
			if int64(len(nodes)) >= nodeLimit {
				truncated = true
				continue
			}
			nodeID, err := graphNodeID("symbol", symbolID)
			if err != nil {
				_ = rows.Close()
				return nil, false, 0, err
			}
			label := cleanLabel(qualified)
			if label == "" {
				label = cleanLabel(name)
			}
			if label == "" {
				label = "symbol"
			}
			refValue := map[string]any{"kind": "symbol", "worktree_id": member.WorktreeID, "profile_id": member.ProfileID, "generation_id": member.GenerationID, "symbol_id": symbolID}
			nodeKind := cleanIdentifier(symbolKind)
			if nodeKind == "" {
				nodeKind = "symbol"
			}
			nodes = append(nodes, map[string]any{
				"node_id": nodeID, "ref": refValue, "kind": nodeKind, "label": label,
				"plane": "observed_static", "evidence_refs": []any{}, "source_refs": []any{sourceRefValue(ref)}, "expandable_count": nil,
			})
			symbols[symbolID] = nodeID
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, false, 0, fmt.Errorf("iterate published Explorer symbols: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, false, 0, fmt.Errorf("close published Explorer symbols: %w", err)
		}
	}

	edges := make([]map[string]any, 0, min(edgeLimit, int64(len(symbols)*2)))
	if depth >= 2 && len(symbols) > 0 {
		ids := make([]string, 0, len(symbols))
		for id := range symbols {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, 0, len(ids)+2)
		args = append(args, generationID)
		for _, id := range ids {
			args = append(args, id)
		}
		args = append(args, edgeLimit+1)
		query := `SELECT relation_id,source_symbol_id,target_symbol_id,kind,evidence_kind,site_path,start_byte,end_byte
			FROM relations WHERE generation_id=? AND source_symbol_id IN (` + placeholders + `)
			ORDER BY relation_id LIMIT ?`
		rows, err := snapshot.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, false, 0, fmt.Errorf("query published Explorer relations: %w", err)
		}
		seen := int64(0)
		for rows.Next() {
			seen++
			if seen > edgeLimit {
				truncated = true
				break
			}
			var relationID, sourceID, kind, evidenceKind, sitePath string
			var targetID sql.NullString
			var startByte, endByte int64
			if err := rows.Scan(&relationID, &sourceID, &targetID, &kind, &evidenceKind, &sitePath, &startByte, &endByte); err != nil {
				_ = rows.Close()
				return nil, false, 0, fmt.Errorf("read published Explorer relation: %w", err)
			}
			fromID, sourceVisible := symbols[sourceID]
			toID, targetVisible := symbols[targetID.String]
			if !validGraphID(relationID) || !validGraphID(kind) || !sourceVisible || !targetID.Valid || !targetVisible {
				truncated = true
				continue
			}
			evidenceSource, ok := content.sourceRef(sitePath, "", startByte, endByte)
			if !ok {
				truncated = true
				continue
			}
			resolution := "unresolved"
			switch evidenceKind {
			case "syntax_exact", "type_resolved":
				resolution = "exact_in_supported_scope"
			case "type_possible":
				resolution = "candidate"
			}
			edgeID, err := graphNodeID("relation", relationID)
			if err != nil {
				_ = rows.Close()
				return nil, false, 0, err
			}
			evidence := map[string]any{
				"evidence_id": relationID, "origin": "static_analyzer", "kind": evidenceKind,
				"source_refs": []any{sourceRefValue(evidenceSource)}, "record_ref": nil, "artifact_id": nil,
			}
			edges = append(edges, map[string]any{
				"edge_id": edgeID, "from_node_id": fromID, "to_node_id": toID, "relation_kind": kind,
				"resolution_status": resolution, "evidence_refs": []any{evidence}, "expand_ref": nil,
			})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, false, 0, fmt.Errorf("iterate published Explorer relations: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, false, 0, fmt.Errorf("close published Explorer relations: %w", err)
		}
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i]["node_id"].(string) < nodes[j]["node_id"].(string) })
	sort.Slice(edges, func(i, j int) bool { return edges[i]["edge_id"].(string) < edges[j]["edge_id"].(string) })
	nodeValues := make([]any, len(nodes))
	for i := range nodes {
		nodeValues[i] = nodes[i]
	}
	edgeValues := make([]any, len(edges))
	for i := range edges {
		edgeValues[i] = edges[i]
	}
	scopeValue, err := jsonCanonicalValue(state.scope)
	if err != nil {
		return nil, false, 0, err
	}
	digest, err := model.HashValue("subgraph-v1", map[string]any{"scope": scopeValue, "nodes": nodeValues, "edges": edgeValues})
	if err != nil {
		return nil, false, 0, fmt.Errorf("hash Explorer subgraph: %w", err)
	}
	return map[string]any{"operation": "graph_query", "nodes": nodeValues, "edges": edgeValues, "subgraph_digest": digest}, truncated, len(nodes) + len(edges), nil
}

func (reader *graphContentReader) sourceRef(path string, expectedHash model.Hash, startByte, endByte int64) (model.SourceRef, bool) {
	if !reader.service.pathAllowed(reader.policy, path) || startByte < 0 || endByte < startByte || endByte-startByte > maxSourceReadBytes {
		return model.SourceRef{}, false
	}
	content, ok := reader.readFile(path, expectedHash)
	if !ok {
		return model.SourceRef{}, false
	}
	ref := model.SourceRef{
		WorktreeID: reader.service.worktree, ProfileID: reader.service.profile,
		GenerationID: reader.snapshot.Generation.Key.GenerationID, Path: path,
		ContentHash: content.hash, Range: model.ByteRange{StartByte: startByte, EndByte: endByte},
	}
	if err := ref.ValidateAgainst(content.bytes); err != nil {
		return model.SourceRef{}, false
	}
	return ref, true
}

func (reader *graphContentReader) readFile(path string, expectedHash model.Hash) (graphContent, bool) {
	if value, ok := reader.cache[path]; ok {
		if expectedHash != "" && expectedHash != value.hash {
			return graphContent{}, false
		}
		return value, true
	}
	if reader.failed[path] || !reader.service.pathAllowed(reader.policy, path) {
		return graphContent{}, false
	}
	var hashText sql.NullString
	var byteLength sql.NullInt64
	var status string
	fileRow, err := reader.snapshot.QueryRowContext(reader.ctx, `SELECT content_hash,byte_length,status FROM files WHERE generation_id=? AND path=?`,
		string(reader.snapshot.Generation.Key.GenerationID), path)
	if err != nil || fileRow.Scan(&hashText, &byteLength, &status) != nil || !hashText.Valid || !byteLength.Valid || status != "indexed" || byteLength.Int64 < 0 || byteLength.Int64 > maxGraphSourceBytes-reader.usedBytes {
		reader.failed[path] = true
		return graphContent{}, false
	}
	hash, err := model.ParseHash(hashText.String)
	if err != nil || expectedHash != "" && expectedHash != hash {
		reader.failed[path] = true
		return graphContent{}, false
	}
	row, err := reader.snapshot.QueryRowContext(reader.ctx, `SELECT bytes,byte_length FROM content_blobs WHERE content_hash=?`, hashText.String)
	if err != nil {
		reader.failed[path] = true
		return graphContent{}, false
	}
	var raw []byte
	var storedLength int64
	if err := row.Scan(&raw, &storedLength); err != nil || storedLength != byteLength.Int64 || int64(len(raw)) != byteLength.Int64 || model.SHA256(raw) != hash {
		reader.failed[path] = true
		return graphContent{}, false
	}
	if _, err := reader.policy.pathSensitive.ScanContent(raw); err != nil {
		reader.failed[path] = true
		return graphContent{}, false
	}
	reader.usedBytes += int64(len(raw))
	value := graphContent{bytes: append([]byte(nil), raw...), hash: hash}
	reader.cache[path] = value
	return value, true
}

func (service *Service) sourceResponse(ctx context.Context, raw []byte) ([]byte, error) {
	var request struct {
		SourceRef      model.SourceRef `json:"source_ref"`
		MaxOutputBytes int64           `json:"max_output_bytes"`
	}
	if err := decodeRequest(raw, &request); err != nil {
		return marshalErrorEnvelope("INVALID_ARGUMENT", "Source request is invalid.", "fix_request", 1<<20)
	}
	maxBytes := request.MaxOutputBytes
	if maxBytes == 0 {
		maxBytes = 65_536
	}
	if maxBytes < 4096 || maxBytes > 131_072 {
		return marshalErrorEnvelope("INVALID_ARGUMENT", "Source output limit is outside the supported range.", "fix_request", 1<<20)
	}
	if request.SourceRef.WorktreeID != service.worktree || request.SourceRef.ProfileID != service.profile {
		return marshalErrorEnvelope("SOURCE_PRECONDITION_FAILED", "The requested indexed source is unavailable.", "recreate_pack", maxBytes)
	}
	var response []byte
	err := service.withCurrent(ctx, func(state *currentState, snapshot *sqlite.ReadSnapshot) error {
		if state.scope == nil || snapshot == nil || !service.pathAllowed(state.catalog, request.SourceRef.Path) || request.SourceRef.GenerationID != snapshot.Generation.Key.GenerationID {
			var err error
			response, err = marshalErrorEnvelope("SOURCE_PRECONDITION_FAILED", "The requested indexed source is no longer available in the current snapshot.", "recreate_pack", maxBytes)
			return err
		}
		reader := &graphContentReader{ctx: ctx, snapshot: snapshot, service: service, policy: state.catalog, cache: make(map[string]graphContent), failed: make(map[string]bool)}
		content, ok := reader.readFile(request.SourceRef.Path, request.SourceRef.ContentHash)
		if !ok || request.SourceRef.ValidateAgainst(content.bytes) != nil {
			var err error
			response, err = marshalErrorEnvelope("SOURCE_PRECONDITION_FAILED", "The requested indexed source could not be verified against the current snapshot and policy.", "recreate_pack", maxBytes)
			return err
		}
		if request.SourceRef.Range.EndByte-request.SourceRef.Range.StartByte > maxSourceReadBytes {
			var err error
			response, err = marshalErrorEnvelope("RESULT_TOO_LARGE", "The requested source range exceeds the supported byte limit.", "fix_request", maxBytes)
			return err
		}
		start, end := request.SourceRef.Range.StartByte, request.SourceRef.Range.EndByte
		data := map[string]any{"operation": "source_read", "source_ref": sourceRefValue(request.SourceRef), "content": string(content.bytes[start:end])}
		responseBytes, marshalErr := marshalEnvelopeWithBudget("ok", state.scope, data, "supported_scope_complete", 1, false, true, maxBytes)
		if marshalErr != nil {
			responseBytes, marshalErr = marshalErrorEnvelope("RESULT_TOO_LARGE", "The requested source range does not fit the output budget.", "fix_request", maxBytes)
		}
		response = responseBytes
		return marshalErr
	})
	return response, err
}

func sourceRefValue(ref model.SourceRef) map[string]any {
	return map[string]any{
		"worktree_id": ref.WorktreeID, "profile_id": ref.ProfileID, "generation_id": ref.GenerationID,
		"path": ref.Path, "content_hash": ref.ContentHash,
		"range": map[string]any{"start_byte": ref.Range.StartByte, "end_byte": ref.Range.EndByte},
	}
}

func graphNodeID(kind, identity string) (string, error) {
	digest, err := model.HashValue("explorer-"+kind+"-node-v1", map[string]any{"identity": identity})
	if err != nil {
		return "", err
	}
	return kind + ":" + string(digest), nil
}

func validGraphID(value string) bool {
	if len(value) < 1 || len(value) > 192 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func cleanIdentifier(value string) string {
	if validGraphID(value) {
		return value
	}
	return ""
}

func cleanLabel(value string) string {
	var builder strings.Builder
	count := 0
	for _, r := range value {
		if unicode.IsControl(r) || !utf8.ValidRune(r) {
			continue
		}
		if count >= 512 {
			break
		}
		builder.WriteRune(r)
		count++
	}
	return strings.TrimSpace(builder.String())
}

func jsonCanonicalValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("canonical JSON value has trailing data")
	}
	return decoded, nil
}

var _ transport.Services = (*Service)(nil)
