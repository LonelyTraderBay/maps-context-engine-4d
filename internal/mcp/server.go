// Package mcp exposes the frozen S43 operations over the official MCP SDK.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	appresponse "github.com/LonelyTraderBay/maps-context-engine-4d/internal/app/response"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxRequestBytes        = 32 << 10
	defaultServerVersion   = "1.6.13"
	multiRoundTripProtocol = "2026-07-28"
)

var ErrInvalidHandlers = errors.New("MCP handlers must implement the six frozen operations")

var serverImplementation = &sdkmcp.Implementation{Name: "maps-context-engine-4d", Version: defaultServerVersion}

type Handler func(context.Context, json.RawMessage) ([]byte, error)

// Handlers is keyed by the exact public MCP tool names in S43.
type Handlers map[string]Handler

type toolDefinition struct {
	name        string
	operation   string
	description string
}

var tools = [...]toolDefinition{
	{name: "mce4d_workspace_status", operation: "mce4d_workspace_status", description: "Return workspace and index status."},
	{name: "mce4d_locate", operation: "mce4d_locate", description: "Locate symbols or source positions in the bound workspace."},
	{name: "mce4d_context_pack", operation: "mce4d_context_pack", description: "Build a bounded evidence pack for a selected task."},
	{name: "mce4d_expand_pack", operation: "mce4d_expand_pack", description: "Continue a previously issued context pack."},
	{name: "mce4d_validate_pack", operation: "mce4d_validate_pack", description: "Revalidate pack or rename result preconditions."},
	{name: "mce4d_rename_preview", operation: "mce4d_rename_preview", description: "Preview a scope-validated rename without applying edits."},
}

// NewServer builds an MCP server with exactly the six S43 tool names. Each
// handler receives strict, schema-validated request JSON and must return one
// complete S43 response envelope.
func NewServer(handlers Handlers) (*sdkmcp.Server, error) {
	if len(handlers) != len(tools) {
		return nil, fmt.Errorf("%w: got %d handlers, want %d", ErrInvalidHandlers, len(handlers), len(tools))
	}
	for name, handler := range handlers {
		if !isFrozenTool(name) || handler == nil {
			return nil, fmt.Errorf("%w: invalid handler %q", ErrInvalidHandlers, name)
		}
	}

	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("create S43 validator: %w", err)
	}
	server := sdkmcp.NewServer(
		serverImplementation,
		&sdkmcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)
	for _, definition := range tools {
		handler := handlers[definition.name]
		schema, err := validator.RequestSchema(definition.operation)
		if err != nil {
			return nil, fmt.Errorf("load %s request schema: %w", definition.name, err)
		}
		server.AddTool(&sdkmcp.Tool{
			Name:        definition.name,
			Description: definition.description,
			InputSchema: schema,
		}, operationHandler(validator, definition.operation, handler))
	}
	return server, nil
}

// ServeStdio runs the frozen MCP adapter with the SDK's newline-delimited
// stdio transport. Protocol bytes remain on stdout; SDK diagnostics are sent
// to the discard logger unless operation handlers provide their own logging.
func ServeStdio(ctx context.Context, handlers Handlers) error {
	if ctx == nil {
		return errors.New("MCP context is required")
	}
	server, err := NewServer(handlers)
	if err != nil {
		return err
	}
	if err := server.Run(ctx, &sdkmcp.StdioTransport{}); err != nil {
		return fmt.Errorf("run MCP stdio server: %w", err)
	}
	return nil
}

func operationHandler(validator *contracts.Validator, operation string, handler Handler) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if ctx == nil || request == nil || request.Params == nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tool request is invalid"}
		}
		arguments := request.Params.Arguments
		if len(arguments) == 0 {
			arguments = json.RawMessage("{}")
		}
		if len(arguments) > maxRequestBytes {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tool arguments exceed the 32 KiB request limit"}
		}
		if err := validator.ValidateRequest(operation, arguments); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tool arguments do not match the S43 request contract"}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := handler(ctx, append(json.RawMessage(nil), arguments...))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return toolFailure("Operation failed."), nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := validatedResult(validator, operation, response, request.ProtocolVersion())
		if err != nil {
			return toolFailure("Operation returned an invalid response."), nil
		}
		return result, nil
	}
}

func validatedResult(validator *contracts.Validator, operation string, raw []byte, protocolVersion string) (*sdkmcp.CallToolResult, error) {
	value, err := contracts.DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode operation response: %w", err)
	}
	response, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("operation response is not an object")
	}
	if err := validator.ValidateResponse(operation, raw); err != nil {
		return nil, fmt.Errorf("validate operation response: %w", err)
	}
	budget, ok := response["budget"].(map[string]any)
	if !ok {
		return nil, errors.New("operation response budget is missing")
	}
	maxBytes, ok := integerValue(budget["max_output_bytes"])
	if !ok || maxBytes > appresponse.MaxMaxOutputBytes {
		return nil, errors.New("operation response budget is invalid")
	}
	budget["delivery_mode"] = "mcp_dual"
	result, size, err := fixedPointResult(response, protocolVersion)
	if err != nil {
		return nil, err
	}
	if size > maxBytes {
		response, err = budgetErrorResponse(response, maxBytes)
		if err != nil {
			return nil, err
		}
		result, size, err = fixedPointResult(response, protocolVersion)
		if err != nil {
			return nil, err
		}
		if size > maxBytes {
			return nil, errors.New("minimum S43 budget error exceeds the MCP output limit")
		}
	}
	finalResponse, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode final operation response: %w", err)
	}
	if err := validator.ValidateResponse(operation, finalResponse); err != nil {
		return nil, fmt.Errorf("validate final operation response: %w", err)
	}
	return result, nil
}

func fixedPointResult(response map[string]any, protocolVersion string) (*sdkmcp.CallToolResult, int64, error) {
	budget, ok := response["budget"].(map[string]any)
	if !ok {
		return nil, 0, errors.New("operation response budget is missing")
	}
	for range 8 {
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, 0, fmt.Errorf("encode operation response: %w", err)
		}
		isError := response["status"] == "error" || response["status"] == "blocked"
		result := &sdkmcp.CallToolResult{
			Meta:              sdkmcp.Meta{sdkmcp.MetaKeyServerInfo: serverImplementation},
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(raw)}},
			StructuredContent: response,
			IsError:           isError,
		}
		counted := resultForByteCount{
			Meta: result.Meta, Content: result.Content, StructuredContent: result.StructuredContent, IsError: result.IsError,
		}
		if protocolVersion >= multiRoundTripProtocol {
			counted.ResultType = "complete"
		}
		wire, err := json.Marshal(counted)
		if err != nil {
			return nil, 0, fmt.Errorf("encode MCP tool result: %w", err)
		}
		size := int64(len(wire))
		if budget["serialized_bytes"] == size {
			return result, size, nil
		}
		budget["serialized_bytes"] = size
	}
	return nil, 0, errors.New("MCP output byte count did not converge")
}

func budgetErrorResponse(original map[string]any, maxBytes int64) (map[string]any, error) {
	requestID, ok := original["request_id"].(string)
	if !ok || requestID == "" {
		return nil, errors.New("operation response request ID is missing")
	}
	gap := map[string]any{
		"code": "required_evidence_pending", "reason": "Required evidence and MCP framing do not fit the output byte budget.",
		"count": nil, "source_refs": []any{},
	}
	response := map[string]any{
		"schema_version": 1, "request_id": requestID, "status": "error", "scope": nil,
		"freshness": map[string]any{
			"status": "not_checked", "scope": "none", "checked_at": nil, "buffer_scope": "disk_only",
			"consistency": "captured_bytes_with_observed_stability", "changed_count": nil, "source_view": "indexed_snapshot",
		},
		"coverage": map[string]any{
			"status": "unknown", "capabilities": []any{}, "gaps": []any{gap},
			"scope_description": "Context could not be delivered within the MCP output byte budget.",
		},
		"readiness": "blocked", "data": nil,
		"pagination": map[string]any{
			"truncated": true, "enumeration_complete": false, "returned_count": 0,
			"required_pending": nil, "optional_pending": nil, "next_cursor": nil, "limit_reason": "output_byte_budget",
		},
		"budget": map[string]any{
			"max_output_bytes": maxBytes, "serialized_bytes": 0, "token_count": nil, "token_estimate": nil,
			"tokenizer_id": nil, "estimate_only": false, "delivery_mode": "mcp_dual",
		},
		"warnings": []any{gap},
		"error": map[string]any{
			"code": "BUDGET_TOO_SMALL", "message": "Required context and MCP framing exceed the output byte budget.",
			"retryable": false, "suggested_action": "expand", "details": []any{gap},
		},
	}
	return response, nil
}

func integerValue(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		result, err := number.Int64()
		return result, err == nil
	case int64:
		return number, true
	case int:
		return int64(number), true
	default:
		return 0, false
	}
}

func toolFailure(message string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: message}}, IsError: true}
}

type resultForByteCount struct {
	Meta              sdkmcp.Meta      `json:"_meta,omitempty"`
	Content           []sdkmcp.Content `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError,omitempty"`
	ResultType        string           `json:"resultType,omitempty"`
}

func isFrozenTool(name string) bool {
	for _, definition := range tools {
		if name == definition.name {
			return true
		}
	}
	return false
}
