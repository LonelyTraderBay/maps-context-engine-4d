package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	apprecords "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/records"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/history"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

const maxRecordCLIOutputBytes = int64(1 << 20)

type recordScopeRequest struct {
	WorktreeID   model.UUID         `json:"worktree_id,omitempty"`
	ProfileID    model.Hash         `json:"profile_id,omitempty"`
	GenerationID model.UUID         `json:"generation_id,omitempty"`
	GroupID      *model.UUID        `json:"group_id,omitempty"`
	PinnedVector *model.ScopeVector `json:"pinned_vector,omitempty"`
}

type recordListRequest struct {
	Scope  *recordScopeRequest `json:"scope,omitempty"`
	Kind   history.Kind        `json:"record_kind,omitempty"`
	Status history.Status      `json:"status,omitempty"`
	Limit  int64               `json:"limit,omitempty"`
	Cursor string              `json:"cursor,omitempty"`
}

type recordReadRequest struct {
	Scope    *recordScopeRequest `json:"scope,omitempty"`
	RecordID model.UUID          `json:"record_id"`
	Version  *int64              `json:"version,omitempty"`
}

type recordListData struct {
	Operation string            `json:"operation"`
	Records   []history.Summary `json:"records"`
}

type recordReadData struct {
	Operation   string          `json:"operation"`
	Record      history.Summary `json:"record"`
	UIMapping   any             `json:"ui_mapping"`
	Limitations []recordCLIGap  `json:"limitations"`
}

type recordCLIGap struct {
	Code       string            `json:"code"`
	Reason     string            `json:"reason"`
	Count      *int64            `json:"count"`
	SourceRefs []model.SourceRef `json:"source_refs"`
}

func runRecords(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeFailure(stderr, fmt.Errorf("%w: records requires list or read", errUsage))
	}
	if args[0] != "list" && args[0] != "read" {
		return writeFailure(stderr, fmt.Errorf("%w: records action %q is unavailable", errUsage, args[0]))
	}
	options, err := parseOptions(args[1:], map[string]optionSpec{
		"json": {}, "input-json": {value: true}, "worktree-id": {value: true},
	})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "json") || !has(options, "input-json") {
		return writeFailure(stderr, fmt.Errorf("%w: records %s requires --input-json and --json", errUsage, args[0]))
	}
	raw, err := readInput(first(options, "input-json"), stdin)
	if err != nil {
		return writeFailure(stderr, err)
	}
	operation := "CLI records list"
	if args[0] == "read" {
		operation = "CLI records read"
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return writeFailure(stderr, err)
	}
	if err := validator.ValidateRequest(operation, raw); err != nil {
		return writeFailure(stderr, fmt.Errorf("%w: records request violates its closed contract: %v", errUsage, err))
	}
	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	worktreeID, scope, err := resolveRecordScope(first(options, "worktree-id"), raw, args[0])
	if err != nil {
		return writeFailure(stderr, err)
	}
	service, closeStores, err := openRecordCLIService(ctx, homePath, worktreeID)
	if err != nil {
		return writeFailure(stderr, err)
	}
	var response []byte
	if args[0] == "list" {
		var request recordListRequest
		if err := decodeRecordRequest(raw, &request); err != nil {
			return closeRecordCLIWithFailure(stderr, closeStores, fmt.Errorf("%w: decode records list request: %v", errUsage, err))
		}
		result, err := service.List(ctx, apprecords.ListRequest{Kind: request.Kind, Status: request.Status, Limit: request.Limit,
			Cursor: request.Cursor, ProfileID: scope.ProfileID, GenerationID: scope.GenerationID})
		if err != nil {
			return closeRecordCLIWithFailure(stderr, closeStores, err)
		}
		gaps := recordStatusGaps(result.Records)
		response, err = marshalRecordsEnvelope(recordListData{Operation: "records_list", Records: result.Records},
			len(result.Records), result.HasMore, result.NextCursor, gaps)
	} else {
		var request recordReadRequest
		if err := decodeRecordRequest(raw, &request); err != nil {
			return closeRecordCLIWithFailure(stderr, closeStores, fmt.Errorf("%w: decode records read request: %v", errUsage, err))
		}
		result, err := service.Read(ctx, request.RecordID, request.Version, scope.ProfileID, scope.GenerationID)
		if err != nil {
			return closeRecordCLIWithFailure(stderr, closeStores, err)
		}
		gaps := recordStatusGaps([]history.Summary{result.Record})
		limitations := append([]recordCLIGap(nil), gaps...)
		if result.Record.Kind == history.KindUIMap {
			limitations = append(limitations, recordCLIGap{Code: "unsupported_mapping_payload",
				Reason: "Payload mapping chưa có contract đọc được trong capability này.", Count: int64Pointer(1), SourceRefs: append([]model.SourceRef{}, result.Record.SourceRefs...)})
		}
		response, err = marshalRecordsEnvelope(recordReadData{Operation: "records_read", Record: result.Record,
			UIMapping: nil, Limitations: limitations}, 1, false, "", gaps)
	}
	closeErr := closeStores()
	if err != nil || closeErr != nil {
		return writeFailure(stderr, errors.Join(err, closeErr))
	}
	if err := validator.ValidateResponse(operation, response); err != nil {
		return writeFailure(stderr, fmt.Errorf("records response failed its contract: %w", err))
	}
	if _, err := stdout.Write(response); err != nil {
		return writeFailure(stderr, err)
	}
	_, err = io.WriteString(stdout, "\n")
	if err != nil {
		return writeFailure(stderr, err)
	}
	return 0
}

func resolveRecordScope(flagValue string, raw []byte, action string) (model.UUID, recordScopeRequest, error) {
	scope := recordScopeRequest{}
	var requestedScope *recordScopeRequest
	if action == "list" {
		var parsed recordListRequest
		if err := decodeRecordRequest(raw, &parsed); err != nil {
			return "", scope, fmt.Errorf("%w: cannot decode records scope: %v", errUsage, err)
		}
		requestedScope = parsed.Scope
	} else {
		var parsed recordReadRequest
		if err := decodeRecordRequest(raw, &parsed); err != nil {
			return "", scope, fmt.Errorf("%w: cannot decode records scope: %v", errUsage, err)
		}
		requestedScope = parsed.Scope
	}
	if requestedScope != nil {
		scope = *requestedScope
		if scope.GroupID != nil || scope.PinnedVector != nil {
			return "", scope, fmt.Errorf("%w: records %s supports one local worktree scope", errUsage, action)
		}
	}
	var fromFlag model.UUID
	if flagValue != "" {
		parsedID, err := model.ParseUUID(flagValue)
		if err != nil || parsedID != model.UUID(flagValue) {
			return "", scope, fmt.Errorf("%w: --worktree-id must be a canonical UUID", errUsage)
		}
		fromFlag = parsedID
	}
	if scope.WorktreeID != "" {
		parsedID, err := model.ParseUUID(string(scope.WorktreeID))
		if err != nil || parsedID != scope.WorktreeID {
			return "", scope, fmt.Errorf("%w: scope worktree_id must be a canonical UUID", errUsage)
		}
		if fromFlag != "" && fromFlag != parsedID {
			return "", scope, fmt.Errorf("%w: --worktree-id conflicts with scope.worktree_id", errUsage)
		}
		fromFlag = parsedID
	}
	if fromFlag == "" {
		return "", scope, fmt.Errorf("%w: records %s requires --worktree-id or scope.worktree_id", errUsage, action)
	}
	return fromFlag, scope, nil
}

func decodeRecordRequest(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("records request contains trailing JSON")
	}
	return nil
}

func openRecordCLIService(ctx context.Context, homePath string, worktreeID model.UUID) (*apprecords.Service, func() error, error) {
	if ctx == nil || !filepath.IsAbs(homePath) {
		return nil, func() error { return nil }, errInvalidHome
	}
	worktreeRoot := filepath.Join(homePath, "worktrees", string(worktreeID))
	recordsPath := filepath.Join(worktreeRoot, "records.sqlite")
	indexPath := filepath.Join(worktreeRoot, "index.sqlite")
	var recordStore *sqlite.RecordsStore
	var indexStore *sqlite.Store
	closeStores := func() error {
		var errorsToJoin []error
		if indexStore != nil {
			errorsToJoin = append(errorsToJoin, indexStore.Close())
		}
		if recordStore != nil {
			errorsToJoin = append(errorsToJoin, recordStore.Close())
		}
		return errors.Join(errorsToJoin...)
	}
	if _, err := os.Lstat(recordsPath); err == nil {
		var openErr error
		recordStore, openErr = sqlite.OpenRecordsReadOnly(ctx, sqlite.Options{Path: filepath.Join(worktreeRoot, "index.sqlite"),
			HomePath: homePath, WorktreeID: worktreeID, LockWaitMS: 5_000})
		if openErr != nil {
			return nil, closeStores, fmt.Errorf("open durable records read-only: %w", openErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, closeStores, fmt.Errorf("inspect durable records database: %w", err)
	}
	indexUnavailable := false
	if _, err := os.Lstat(indexPath); err == nil {
		var openErr error
		indexStore, openErr = sqlite.OpenIndexReadOnly(ctx, sqlite.Options{Path: indexPath, HomePath: homePath, WorktreeID: worktreeID, LockWaitMS: 5_000})
		if openErr != nil {
			indexUnavailable = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		indexUnavailable = true
	}
	policies, err := apprecords.NewSQLitePolicyReader(homePath)
	if err != nil {
		return nil, closeStores, err
	}
	var recordPort apprecords.Store
	if recordStore != nil {
		recordPort = recordStore
	}
	var indexPort apprecords.Index
	if indexStore != nil {
		indexPort = indexStore
	}
	service, err := apprecords.NewService(apprecords.Options{WorktreeID: worktreeID, Records: recordPort,
		Index: indexPort, IndexUnavailable: indexUnavailable, Policies: policies})
	if err != nil {
		return nil, closeStores, err
	}
	return service, closeStores, nil
}

func closeRecordCLIWithFailure(stderr io.Writer, closeStores func() error, err error) int {
	if closeStores != nil {
		err = errors.Join(err, closeStores())
	}
	return writeFailure(stderr, err)
}

func recordStatusGaps(records []history.Summary) []recordCLIGap {
	counts := make(map[history.ProvenanceStatus]int64)
	refs := make(map[history.ProvenanceStatus][]model.SourceRef)
	for _, record := range records {
		if record.ProvenanceStatus == history.ProvenanceRetainedSnapshot {
			continue
		}
		counts[record.ProvenanceStatus]++
		for _, ref := range record.SourceRefs {
			if len(refs[record.ProvenanceStatus]) < 16 {
				refs[record.ProvenanceStatus] = append(refs[record.ProvenanceStatus], ref)
			}
		}
	}
	statuses := []history.ProvenanceStatus{history.ProvenanceSnapshotExpired, history.ProvenanceSourceChanged, history.ProvenanceAccessDenied, history.ProvenanceUnverified}
	gaps := make([]recordCLIGap, 0, len(statuses))
	for _, status := range statuses {
		if count := counts[status]; count > 0 {
			gaps = append(gaps, recordCLIGap{Code: string(status), Reason: provenanceReason(status), Count: int64Pointer(count), SourceRefs: append([]model.SourceRef{}, refs[status]...)})
		}
	}
	return gaps
}

func provenanceReason(status history.ProvenanceStatus) string {
	switch status {
	case history.ProvenanceSnapshotExpired:
		return "Snapshot nguồn đã hết hạn; chỉ hiển thị metadata bền vững, không dựng lại nội dung nguồn."
	case history.ProvenanceSourceChanged:
		return "Hash hoặc phạm vi nguồn không còn khớp phiên bản đã ghi."
	case history.ProvenanceAccessDenied:
		return "Policy hiện tại không cho phép xác minh nguồn đã ghi."
	default:
		return "Nguồn chưa được xác minh theo policy và snapshot hiện tại."
	}
}

func marshalRecordsEnvelope(data any, count int, truncated bool, nextCursor string, gaps []recordCLIGap) ([]byte, error) {
	requestID, err := newRecordRequestID()
	if err != nil {
		return nil, err
	}
	status, coverage := "ok", "supported_scope_complete"
	if len(gaps) > 0 {
		status, coverage = "partial", "partial"
	}
	var cursor any
	var limitReason any
	if nextCursor != "" {
		cursor = nextCursor
		limitReason = "page_limit"
	}
	response := map[string]any{
		"schema_version": 1, "request_id": requestID, "status": status, "scope": nil,
		"freshness": map[string]any{"status": "not_checked", "scope": "none", "checked_at": nil,
			"buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot"},
		"coverage": map[string]any{"status": coverage, "capabilities": []any{}, "gaps": gaps,
			"scope_description": "Metadata record bền vững trong worktree đã chọn; trạng thái nguồn được báo riêng cho từng record."},
		"readiness": "needs_context", "data": data,
		"pagination": map[string]any{"truncated": truncated, "enumeration_complete": !truncated, "returned_count": count,
			"required_pending": nil, "optional_pending": nil, "next_cursor": cursor, "limit_reason": limitReason},
		"budget": map[string]any{"max_output_bytes": maxRecordCLIOutputBytes, "serialized_bytes": 0,
			"token_count": nil, "token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "cli"},
		"warnings": []any{}, "error": nil,
	}
	for range 8 {
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		budget := response["budget"].(map[string]any)
		if budget["serialized_bytes"] == len(raw) {
			if int64(len(raw)) > maxRecordCLIOutputBytes {
				return nil, apprecords.ErrPageTooLarge
			}
			return raw, nil
		}
		budget["serialized_bytes"] = len(raw)
	}
	return nil, fmt.Errorf("records response byte count did not converge")
}

func newRecordRequestID() (model.UUID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create records request identity: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return model.ParseUUID(encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:])
}

func int64Pointer(value int64) *int64 { return &value }
