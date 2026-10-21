package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	appgroups "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/groups"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/catalog"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/store/sqlite"
)

const maxGroupCLIOutputBytes = int64(1 << 20)

var errGroupOutputTooLarge = errors.New("group response exceeds the CLI output limit")

type groupAdminRequest struct {
	Action           string               `json:"action"`
	GroupID          *model.UUID          `json:"group_id,omitempty"`
	ExpectedRevision *int64               `json:"expected_revision,omitempty"`
	Config           *catalog.GroupConfig `json:"config,omitempty"`
	Limit            *int64               `json:"limit,omitempty"`
	Cursor           string               `json:"cursor,omitempty"`
}

type groupAdminData struct {
	Operation string                 `json:"operation"`
	Groups    []catalog.GroupSummary `json:"groups"`
}

func runGroups(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "get" && args[0] != "list" && args[0] != "put" && args[0] != "remove") {
		return writeFailure(stderr, fmt.Errorf("%w: group requires get, list, put, or remove", errUsage))
	}
	action := args[0]
	options, err := parseOptions(args[1:], map[string]optionSpec{"input-json": {value: true}, "json": {}})
	if err != nil {
		return writeFailure(stderr, err)
	}
	if !has(options, "input-json") {
		return writeFailure(stderr, fmt.Errorf("%w: group %s requires --input-json", errUsage, action))
	}
	raw, err := readInput(first(options, "input-json"), stdin)
	if err != nil {
		return writeFailure(stderr, err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return writeFailure(stderr, err)
	}
	if err := validator.ValidateRequest("CLI group", raw); err != nil {
		return writeFailure(stderr, fmt.Errorf("%w: group request violates its closed contract: %v", errUsage, err))
	}
	var request groupAdminRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return writeFailure(stderr, fmt.Errorf("%w: decode group request: %v", errUsage, err))
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return writeFailure(stderr, fmt.Errorf("%w: group request contains trailing JSON", errUsage))
	}
	if err := validateGroupAdminRequest(action, request); err != nil {
		return writeFailure(stderr, err)
	}
	homePath, err := resolveHomePath()
	if err != nil {
		return writeFailure(stderr, err)
	}
	store, err := sqlite.OpenGroupCatalog(ctx, homePath)
	if err != nil {
		return writeFailure(stderr, err)
	}
	service, err := appgroups.NewService(store)
	if err != nil {
		return closeGroupCLIWithFailure(stderr, store.Close(), err)
	}
	groups := make([]catalog.GroupSummary, 0)
	var nextCursor string
	var hasMore bool
	var response []byte
	limitReason := ""
	if action == "get" {
		snapshot, serviceErr := service.Get(ctx, *request.GroupID)
		if serviceErr != nil {
			return closeGroupCLIWithFailure(stderr, store.Close(), serviceErr)
		}
		groups = append(groups, snapshot.Group)
	} else if action == "put" {
		snapshot, serviceErr := service.Put(ctx, *request.Config, *request.ExpectedRevision)
		if serviceErr != nil {
			return closeGroupCLIWithFailure(stderr, store.Close(), serviceErr)
		}
		groups = append(groups, snapshot.Group)
	} else if action == "remove" {
		snapshot, serviceErr := service.Remove(ctx, *request.GroupID, *request.ExpectedRevision)
		if serviceErr != nil {
			return closeGroupCLIWithFailure(stderr, store.Close(), serviceErr)
		}
		groups = append(groups, snapshot.Group)
	} else {
		limit := int64(0)
		if request.Limit != nil {
			limit = *request.Limit
		}
		if limit == 0 {
			limit = appgroups.DefaultListLimit
		}
		reducedForBudget := false
		for {
			page, serviceErr := service.List(ctx, limit, request.Cursor)
			if serviceErr != nil {
				return closeGroupCLIWithFailure(stderr, store.Close(), serviceErr)
			}
			groups, nextCursor, hasMore = page.Groups, page.NextCursor, page.HasMore
			limitReason = ""
			if hasMore {
				limitReason = "page_limit"
				if reducedForBudget {
					limitReason = "output_budget"
				}
			}
			if response, err = marshalGroupCLIResponse(groups, hasMore, nextCursor, limitReason); errors.Is(err, errGroupOutputTooLarge) && limit > 1 {
				limit = max(int64(1), limit/2)
				reducedForBudget = true
				continue
			}
			if err != nil {
				return closeGroupCLIWithFailure(stderr, store.Close(), err)
			}
			break
		}
	}
	closeErr := store.Close()
	if closeErr != nil {
		return writeFailure(stderr, closeErr)
	}
	if response == nil {
		response, err = marshalGroupCLIResponse(groups, hasMore, nextCursor, limitReason)
		if err != nil {
			return writeFailure(stderr, err)
		}
	}
	if err := validator.ValidateResponse("CLI group", response); err != nil {
		return writeFailure(stderr, fmt.Errorf("group response failed its contract: %w", err))
	}
	if _, err := stdout.Write(response); err != nil {
		return writeFailure(stderr, err)
	}
	if _, err := io.WriteString(stdout, "\n"); err != nil {
		return writeFailure(stderr, err)
	}
	return 0
}

func validateGroupAdminRequest(action string, request groupAdminRequest) error {
	if request.Action != action {
		return fmt.Errorf("%w: group action does not match request body", errUsage)
	}
	validID := request.GroupID != nil && isCanonicalGroupID(*request.GroupID)
	switch action {
	case "get":
		if !validID || request.ExpectedRevision != nil || request.Config != nil || request.Limit != nil || request.Cursor != "" {
			return fmt.Errorf("%w: group get requires only group_id", errUsage)
		}
	case "list":
		if request.GroupID != nil || request.ExpectedRevision != nil || request.Config != nil {
			return fmt.Errorf("%w: group list accepts only limit and cursor", errUsage)
		}
	case "put":
		if request.Config == nil || request.ExpectedRevision == nil || request.GroupID != nil || request.Limit != nil || request.Cursor != "" {
			return fmt.Errorf("%w: group put requires config and expected_revision", errUsage)
		}
	case "remove":
		if !validID || request.ExpectedRevision == nil || request.Config != nil || request.Limit != nil || request.Cursor != "" {
			return fmt.Errorf("%w: group remove requires group_id and expected_revision", errUsage)
		}
	default:
		return fmt.Errorf("%w: unsupported group action", errUsage)
	}
	if request.ExpectedRevision != nil && (*request.ExpectedRevision < 0 || *request.ExpectedRevision > model.MaxSafeInteger) {
		return fmt.Errorf("%w: expected_revision is out of range", errUsage)
	}
	if request.Limit != nil && (*request.Limit < 1 || *request.Limit > appgroups.MaxListLimit) {
		return fmt.Errorf("%w: group list limit must be between 1 and 100", errUsage)
	}
	return nil
}

func isCanonicalGroupID(value model.UUID) bool {
	parsed, err := model.ParseUUID(string(value))
	return err == nil && parsed == value
}

func closeGroupCLIWithFailure(stderr io.Writer, closeErr, operationErr error) int {
	return writeFailure(stderr, errors.Join(operationErr, closeErr))
}

func marshalGroupCLIResponse(groups []catalog.GroupSummary, truncated bool, nextCursor, limitReason string) ([]byte, error) {
	requestID, err := newRecordRequestID()
	if err != nil {
		return nil, err
	}
	var cursor any
	var renderedLimitReason any
	if nextCursor != "" {
		cursor = nextCursor
		if limitReason == "" {
			limitReason = "page_limit"
		}
		renderedLimitReason = limitReason
	}
	if groups == nil {
		groups = []catalog.GroupSummary{}
	}
	response := map[string]any{
		"schema_version": 1, "request_id": requestID, "status": "ok", "scope": nil,
		"freshness": map[string]any{"status": "not_checked", "scope": "none", "checked_at": nil,
			"buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot"},
		"coverage": map[string]any{"status": "supported_scope_complete", "capabilities": []any{}, "gaps": []any{},
			"scope_description": "Kết quả quản trị nhóm trong catalog cục bộ; thành viên nguồn được khai báo tường minh."},
		"readiness": "needs_context", "data": groupAdminData{Operation: "group_admin", Groups: groups},
		"pagination": map[string]any{"truncated": truncated, "enumeration_complete": !truncated, "returned_count": len(groups),
			"required_pending": nil, "optional_pending": nil, "next_cursor": cursor, "limit_reason": renderedLimitReason},
		"budget": map[string]any{"max_output_bytes": maxGroupCLIOutputBytes, "serialized_bytes": 0,
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
			if int64(len(raw)) > maxGroupCLIOutputBytes {
				return nil, errGroupOutputTooLarge
			}
			return raw, nil
		}
		budget["serialized_bytes"] = len(raw)
	}
	return nil, fmt.Errorf("group response byte count did not converge")
}
