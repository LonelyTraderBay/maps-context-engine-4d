package explorer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	transport "github.com/LonelyTraderBay/maps-context-engine-4d/internal/explorer"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/paths"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/policy"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
	_ "modernc.org/sqlite"
)

const (
	catalogSchemaVersion = 1
	recordsSchemaVersion = 1
	maxSourceReadBytes   = 64 << 10
	maxGraphNodes        = 200
	maxGraphEdges        = 500
	maxGraphSourceBytes  = 8 << 20
)

var (
	errMetadataUnavailable = errors.New("Explorer scope metadata is unavailable")
	errPolicyChanged       = errors.New("Explorer policy no longer matches the published snapshot")
	errUnsupportedRequest  = errors.New("Explorer request is outside the supported workspace scope")
)

type Options struct {
	HomePath   string
	WorktreeID model.UUID
	ProfileID  model.Hash
}

type Service struct {
	home     string
	worktree model.UUID
	profile  model.Hash
}

type catalogState struct {
	incarnation     model.UUID
	binding         int64
	rootStatus      string
	policyDigest    model.Hash
	policyEpoch     int64
	policyConfig    catalog.PolicyConfig
	pathSensitive   *policy.Policy
	pathInsensitive *policy.Policy
}

type currentState struct {
	catalog    catalogState
	indexState string
	scope      *model.ScopeVector
	generation *sqlite.GenerationRecord
	partial    bool
}

type recordsState struct {
	incarnation model.UUID
	epoch       int64
}

func NewService(options Options) (*Service, error) {
	if !filepath.IsAbs(options.HomePath) || options.HomePath == "" {
		return nil, fmt.Errorf("Explorer requires an absolute MCE4D home path")
	}
	worktree, err := model.ParseUUID(string(options.WorktreeID))
	if err != nil || worktree != options.WorktreeID {
		return nil, fmt.Errorf("Explorer worktree ID is invalid")
	}
	profile, err := model.ParseHash(string(options.ProfileID))
	if err != nil || profile != options.ProfileID {
		return nil, fmt.Errorf("Explorer profile ID is invalid")
	}
	return &Service{home: filepath.Clean(options.HomePath), worktree: worktree, profile: profile}, nil
}

func (service *Service) Bootstrap(ctx context.Context) (transport.AccessGrant, error) {
	if service == nil || ctx == nil {
		return transport.AccessGrant{}, transport.ErrAccessDenied
	}
	state, err := service.readCatalog(ctx)
	if err != nil || state.rootStatus != "registered" {
		return transport.AccessGrant{}, transport.ErrAccessDenied
	}
	return transport.AccessGrant{
		ExpiresAt:    time.Now().UTC().Add(30 * time.Minute),
		Capabilities: []string{"workspace.read", "graph.read", "source.read"},
	}, nil
}

func (service *Service) Authorize(ctx context.Context, principal transport.Principal, operation string, raw json.RawMessage) (transport.AccessGrant, error) {
	if service == nil || ctx == nil || principal.SessionID == "" {
		return transport.AccessGrant{}, transport.ErrAccessDenied
	}
	state, err := service.readCatalog(ctx)
	if err != nil || state.rootStatus != "registered" {
		return transport.AccessGrant{}, transport.ErrAccessDenied
	}
	capability := ""
	switch operation {
	case "GET /workspace":
		capability = "workspace.read"
	case "POST /graph/query":
		capability = "graph.read"
	case "POST /source/read":
		capability = "source.read"
	default:
		return transport.AccessGrant{}, transport.ErrAccessDenied
	}
	if !hasCapability(principal.Capabilities, capability) {
		return transport.AccessGrant{}, transport.ErrAccessDenied
	}
	if operation == "POST /source/read" {
		var request struct {
			SourceRef      model.SourceRef `json:"source_ref"`
			MaxOutputBytes int64           `json:"max_output_bytes"`
		}
		if decodeRequest(raw, &request) != nil || request.SourceRef.WorktreeID != service.worktree || request.SourceRef.ProfileID != service.profile || !service.pathAllowed(state, request.SourceRef.Path) {
			return transport.AccessGrant{}, transport.ErrAccessDenied
		}
	}
	return transport.AccessGrant{ExpiresAt: principal.ExpiresAt, Capabilities: principal.Capabilities}, nil
}

func (service *Service) Execute(ctx context.Context, principal transport.Principal, operation string, raw json.RawMessage) ([]byte, error) {
	if service == nil || ctx == nil || principal.SessionID == "" {
		return nil, transport.ErrAccessDenied
	}
	switch operation {
	case "GET /workspace":
		return service.workspaceResponse(ctx)
	case "POST /graph/query":
		return service.graphResponse(ctx, raw)
	case "POST /source/read":
		return service.sourceResponse(ctx, raw)
	default:
		return nil, transport.ErrAccessDenied
	}
}

func hasCapability(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func decodeRequest(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("request contains trailing JSON")
	}
	return nil
}

func (service *Service) readCatalog(ctx context.Context) (catalogState, error) {
	path := filepath.Join(service.home, "catalog.sqlite")
	db, err := openReadOnlySQLite(ctx, path)
	if err != nil {
		return catalogState{}, fmt.Errorf("open local Explorer catalog: %w", errMetadataUnavailable)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return catalogState{}, fmt.Errorf("begin catalog read: %w", errMetadataUnavailable)
	}
	defer tx.Rollback()
	var schemaVersion, minReader int64
	if err := tx.QueryRowContext(ctx, `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&schemaVersion, &minReader); err != nil || schemaVersion < 1 || schemaVersion > catalogSchemaVersion || minReader > catalogSchemaVersion {
		return catalogState{}, fmt.Errorf("catalog schema is unsupported: %w", errMetadataUnavailable)
	}
	var state catalogState
	var rawConfig string
	err = tx.QueryRowContext(ctx, `SELECT ci.incarnation,w.binding_revision,w.status,h.policy_digest,h.policy_epoch,p.config_json
		FROM catalog_identity ci
		JOIN worktrees w ON w.worktree_id=?
		JOIN worktree_policy_heads h ON h.worktree_id=w.worktree_id
		JOIN policies p ON p.worktree_id=h.worktree_id AND p.policy_digest=h.policy_digest
		JOIN profiles pr ON pr.worktree_id=w.worktree_id AND pr.profile_id=?
		WHERE ci.singleton=1`, string(service.worktree), string(service.profile)).Scan(
		&state.incarnation, &state.binding, &state.rootStatus, &state.policyDigest, &state.policyEpoch, &rawConfig)
	if err != nil {
		return catalogState{}, fmt.Errorf("read registered Explorer scope: %w", errMetadataUnavailable)
	}
	if _, err := model.ParseUUID(string(state.incarnation)); err != nil || state.binding < 1 || state.rootStatus != "registered" || state.policyEpoch < 1 || state.policyEpoch > model.MaxSafeInteger {
		return catalogState{}, errMetadataUnavailable
	}
	if _, err := model.ParseHash(string(state.policyDigest)); err != nil {
		return catalogState{}, errMetadataUnavailable
	}
	if len(rawConfig) == 0 || len(rawConfig) > 64<<10 {
		return catalogState{}, errMetadataUnavailable
	}
	decoder := json.NewDecoder(strings.NewReader(rawConfig))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state.policyConfig); err != nil {
		return catalogState{}, errMetadataUnavailable
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return catalogState{}, errMetadataUnavailable
	}
	digest, err := state.policyConfig.Digest()
	if err != nil || digest != state.policyDigest {
		return catalogState{}, errMetadataUnavailable
	}
	state.pathSensitive, err = policy.New(state.policyConfig.DeniedGlobs, paths.CaseSensitive)
	if err != nil {
		return catalogState{}, errMetadataUnavailable
	}
	state.pathInsensitive, err = policy.New(state.policyConfig.DeniedGlobs, paths.CaseInsensitive)
	if err != nil {
		return catalogState{}, errMetadataUnavailable
	}
	if err := tx.Commit(); err != nil {
		return catalogState{}, fmt.Errorf("finish catalog read: %w", errMetadataUnavailable)
	}
	return state, nil
}

func (service *Service) readRecords(ctx context.Context) (recordsState, error) {
	path := filepath.Join(service.home, "worktrees", string(service.worktree), "records.sqlite")
	db, err := openReadOnlySQLite(ctx, path)
	if err != nil {
		return recordsState{}, fmt.Errorf("open Explorer records metadata: %w", errMetadataUnavailable)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return recordsState{}, errMetadataUnavailable
	}
	defer tx.Rollback()
	var schemaVersion, minReader int64
	if err := tx.QueryRowContext(ctx, `SELECT schema_version,min_reader_version FROM schema_meta WHERE singleton=1`).Scan(&schemaVersion, &minReader); err != nil || schemaVersion < 1 || schemaVersion > recordsSchemaVersion || minReader > recordsSchemaVersion {
		return recordsState{}, errMetadataUnavailable
	}
	var result recordsState
	var worktree model.UUID
	if err := tx.QueryRowContext(ctx, `SELECT worktree_id,incarnation,record_epoch FROM records_meta WHERE singleton=1`).Scan(&worktree, &result.incarnation, &result.epoch); err != nil {
		return recordsState{}, errMetadataUnavailable
	}
	if worktree != service.worktree || result.epoch < 0 || result.epoch > model.MaxSafeInteger {
		return recordsState{}, errMetadataUnavailable
	}
	if _, err := model.ParseUUID(string(result.incarnation)); err != nil {
		return recordsState{}, errMetadataUnavailable
	}
	if err := tx.Commit(); err != nil {
		return recordsState{}, errMetadataUnavailable
	}
	return result, nil
}

func openReadOnlySQLite(ctx context.Context, path string) (*sql.DB, error) {
	if ctx == nil || !filepath.IsAbs(path) {
		return nil, errMetadataUnavailable
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errMetadataUnavailable
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameLocalPath(filepath.Clean(path), filepath.Clean(resolved)) {
		return nil, errMetadataUnavailable
	}
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	fileURL := &url.URL{Scheme: "file", Path: uriPath}
	query := fileURL.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "query_only(1)")
	fileURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", fileURL.String())
	if err != nil {
		return nil, errMetadataUnavailable
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, errMetadataUnavailable
	}
	return db, nil
}

func sameLocalPath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func (service *Service) pathAllowed(state catalogState, relative string) bool {
	if model.ValidateRelativePath(relative) != nil || !state.pathSensitive.AllowsText(relative) {
		return false
	}
	for _, rules := range []*policy.Policy{state.pathSensitive, state.pathInsensitive} {
		decision, err := rules.CheckPath(relative, false)
		if err != nil || decision.Denied {
			return false
		}
	}
	return true
}

func (service *Service) withCurrent(ctx context.Context, consume func(*currentState, *sqlite.ReadSnapshot) error) (retErr error) {
	catalogInfo, err := service.readCatalog(ctx)
	if err != nil {
		return err
	}
	indexPath := filepath.Join(service.home, "worktrees", string(service.worktree), "index.sqlite")
	if _, err := os.Lstat(indexPath); errors.Is(err, os.ErrNotExist) {
		return consume(&currentState{catalog: catalogInfo, indexState: "missing"}, nil)
	} else if err != nil {
		return consume(&currentState{catalog: catalogInfo, indexState: "unavailable"}, nil)
	}
	store, err := sqlite.OpenIndexReadOnly(ctx, sqlite.Options{Path: indexPath, HomePath: service.home, WorktreeID: service.worktree, BusyTimeoutMS: 5000, LockWaitMS: 0})
	if err != nil {
		return consume(&currentState{catalog: catalogInfo, indexState: "unavailable"}, nil)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close read-only Explorer index: %w", closeErr))
		}
	}()
	var snapshotFound bool
	var consumeErr error
	err = store.WithPublishedSnapshot(ctx, service.profile, "", func(snapshot *sqlite.ReadSnapshot) error {
		snapshotFound = true
		if snapshot.Generation.PolicyDigest != catalogInfo.policyDigest || snapshot.Generation.PolicyEpoch != catalogInfo.policyEpoch {
			return errPolicyChanged
		}
		records, err := service.readRecords(ctx)
		if err != nil {
			return errMetadataUnavailable
		}
		vector := model.ScopeVector{Kind: "single", Members: []model.ScopeMember{{
			WorktreeID: service.worktree, ProfileID: service.profile, GenerationID: snapshot.Generation.Key.GenerationID,
			SnapshotDigest: snapshot.Generation.SnapshotDigest, PolicyDigest: snapshot.Generation.PolicyDigest,
			PolicyEpoch: snapshot.Generation.PolicyEpoch, BindingRevision: catalogInfo.binding, RecordEpoch: records.epoch,
			ServiceID: nil, IndexIncarnation: store.Incarnation(), RecordsIncarnation: records.incarnation,
		}}, GroupID: nil, GroupRevision: nil, CatalogIncarnation: catalogInfo.incarnation, UnavailableMembers: []model.UnavailableMember{}}
		digest, err := vector.ComputeDigest()
		if err != nil {
			return errMetadataUnavailable
		}
		vector.VectorDigest = digest
		if err := vector.Validate(); err != nil {
			return errMetadataUnavailable
		}
		var buildingCount int
		row, err := snapshot.QueryRowContext(ctx, `SELECT count(*) FROM generations WHERE profile_id=? AND state='BUILDING'`, string(service.profile))
		if err != nil || row.Scan(&buildingCount) != nil {
			return fmt.Errorf("read Explorer generation state: %w", sqlite.ErrInvalidIndex)
		}
		state := &currentState{catalog: catalogInfo, indexState: "published", scope: &vector, generation: &snapshot.Generation, partial: snapshot.Generation.State == sqlite.GenerationReadyPartial}
		if buildingCount > 0 {
			state.indexState = "building_with_previous"
		}
		consumeErr = consume(state, snapshot)
		return consumeErr
	})
	if errors.Is(err, sqlite.ErrGenerationUnavailable) && !snapshotFound {
		return consume(&currentState{catalog: catalogInfo, indexState: "missing"}, nil)
	}
	if errors.Is(err, errPolicyChanged) || errors.Is(err, errMetadataUnavailable) {
		return consume(&currentState{catalog: catalogInfo, indexState: "unavailable"}, nil)
	}
	if err != nil {
		return err
	}
	return consumeErr
}

func (service *Service) workspaceResponse(ctx context.Context) ([]byte, error) {
	var raw []byte
	err := service.withCurrent(ctx, func(state *currentState, _ *sqlite.ReadSnapshot) error {
		scopes := []model.ScopeMember{}
		var scope any
		if state.scope != nil {
			scopes = append(scopes, state.scope.Members...)
			scope = state.scope
		}
		data := map[string]any{
			"operation": "status", "scopes": scopes, "capabilities": serviceCapabilities(state.indexState, state.partial),
			"project_stage": "unknown", "index_state": state.indexState,
			"registered_scopes": []any{map[string]any{"worktree_id": service.worktree, "profile_id": service.profile, "root_status": state.catalog.rootStatus, "index_state": state.indexState}},
		}
		status := "ok"
		coverage := "unknown"
		if state.partial || state.indexState == "building_with_previous" {
			status, coverage = "partial", "partial"
		}
		response, err := marshalEnvelope(status, scope, data, coverage, len(scopes)+1, false, true)
		raw = response
		return err
	})
	return raw, err
}

func marshalErrorEnvelope(code, message, action string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > 1<<20 {
		maxBytes = 1 << 20
	}
	requestID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	response := map[string]any{
		"schema_version": 1, "request_id": requestID, "status": "blocked", "scope": nil,
		"freshness": map[string]any{"status": "not_checked", "scope": "none", "checked_at": nil, "buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot"},
		"coverage":  map[string]any{"status": "unknown", "capabilities": []any{}, "gaps": []any{}, "scope_description": "Không có dữ liệu trong phạm vi được yêu cầu."},
		"readiness": "blocked", "data": nil,
		"pagination": map[string]any{"truncated": false, "enumeration_complete": true, "returned_count": 0, "required_pending": nil, "optional_pending": nil, "next_cursor": nil, "limit_reason": nil},
		"budget":     map[string]any{"max_output_bytes": maxBytes, "serialized_bytes": 0, "token_count": nil, "token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "http"},
		"warnings":   []any{}, "error": map[string]any{"code": code, "message": message, "retryable": false, "suggested_action": action, "details": []any{}},
	}
	for range 8 {
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		budget := response["budget"].(map[string]any)
		if budget["serialized_bytes"] == len(raw) {
			if int64(len(raw)) > maxBytes {
				return nil, errors.New("Explorer error envelope exceeds its byte budget")
			}
			return raw, nil
		}
		budget["serialized_bytes"] = len(raw)
	}
	return nil, errors.New("Explorer error envelope byte count did not converge")
}

func serviceCapabilities(indexState string, partial bool) []any {
	status := "unknown"
	switch {
	case partial:
		status = "partial"
	case indexState == "building_with_previous":
		status = "partial"
	case indexState == "published":
		status = "supported_scope_complete"
	case indexState == "missing":
		status = "unsupported"
	}
	return []any{
		map[string]any{"name": "explorer.workspace.read", "provider_id": "mce4d.explorer", "provider_version": "1.6.13", "status": status},
		map[string]any{"name": "explorer.graph.read", "provider_id": "mce4d.explorer", "provider_version": "1.6.13", "status": status},
		map[string]any{"name": "explorer.source.read", "provider_id": "mce4d.explorer", "provider_version": "1.6.13", "status": status},
	}
}

func marshalEnvelope(status string, scope any, data any, coverageStatus string, returned int, truncated, complete bool) ([]byte, error) {
	return marshalEnvelopeWithBudget(status, scope, data, coverageStatus, returned, truncated, complete, 1<<20)
}

func marshalEnvelopeWithBudget(status string, scope any, data any, coverageStatus string, returned int, truncated, complete bool, maxBytes int64) ([]byte, error) {
	requestID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	checkedAt := time.Now().UTC().Format(time.RFC3339Nano)
	freshnessStatus, freshnessScope := "unknown", "workspace"
	var checkValue any = checkedAt
	if scope == nil {
		freshnessStatus, freshnessScope, checkValue = "not_checked", "none", nil
	}
	if maxBytes <= 0 || maxBytes > 1<<20 {
		maxBytes = 1 << 20
	}
	response := map[string]any{
		"schema_version": 1, "request_id": requestID, "status": status, "scope": scope,
		"freshness": map[string]any{"status": freshnessStatus, "scope": freshnessScope, "checked_at": checkValue, "buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot"},
		"coverage":  map[string]any{"status": coverageStatus, "capabilities": []any{}, "gaps": []any{}, "scope_description": "Dữ liệu lấy từ generation SQLite đã publish trong phạm vi đăng ký hiện tại."},
		"readiness": "needs_context", "data": data,
		"pagination": map[string]any{"truncated": truncated, "enumeration_complete": complete, "returned_count": returned, "required_pending": nil, "optional_pending": nil, "next_cursor": nil, "limit_reason": nil},
		"budget":     map[string]any{"max_output_bytes": maxBytes, "serialized_bytes": 0, "token_count": nil, "token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "http"},
		"warnings":   []any{}, "error": nil,
	}
	for range 8 {
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		budget := response["budget"].(map[string]any)
		if budget["serialized_bytes"] == len(raw) {
			if int64(len(raw)) > maxBytes {
				return nil, errors.New("Explorer response exceeds its byte budget")
			}
			return raw, nil
		}
		budget["serialized_bytes"] = len(raw)
	}
	return nil, errors.New("Explorer response byte count did not converge")
}

func randomUUID() (model.UUID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return model.ParseUUID(encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:])
}

func sha256Hash(raw []byte) model.Hash {
	digest := sha256.Sum256(raw)
	return model.Hash(hex.EncodeToString(digest[:]))
}
