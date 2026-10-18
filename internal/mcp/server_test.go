package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewServerRequiresExactlyTheFrozenToolSet(t *testing.T) {
	if _, err := NewServer(Handlers{}); !errors.Is(err, ErrInvalidHandlers) {
		t.Fatalf("NewServer with no handlers error = %v, want ErrInvalidHandlers", err)
	}
	handlers := testHandlers()
	handlers["mce4d_alias"] = handlers["mce4d_workspace_status"]
	if _, err := NewServer(handlers); !errors.Is(err, ErrInvalidHandlers) {
		t.Fatalf("NewServer with an alias error = %v, want ErrInvalidHandlers", err)
	}
}

func TestOperationHandlerValidatesStrictArgumentsAndCancellation(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	handler := operationHandler(validator, "mce4d_workspace_status", func(context.Context, json.RawMessage) ([]byte, error) {
		called = true
		return statusResponse(), nil
	})
	for _, raw := range []string{
		`{"unexpected":true}`,
		`{"verify":"none","verify":"workspace"}`,
	} {
		called = false
		_, err := handler(context.Background(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
			Name: "mce4d_workspace_status", Arguments: json.RawMessage(raw),
		}})
		var protocolErr *jsonrpc.Error
		if !errors.As(err, &protocolErr) || protocolErr.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("handler error for %s = %v, want JSON-RPC invalid params", raw, err)
		}
		if called {
			t.Fatalf("handler ran for invalid arguments %s", raw)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = handler(ctx, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{
		Name: "mce4d_workspace_status", Arguments: json.RawMessage(`{}`),
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handler error for cancelled request = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("handler ran after request cancellation")
	}
}

func TestMCPProtocolCancellationReachesTheOperationHandler(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	handlers := testHandlers()
	handlers["mce4d_workspace_status"] = func(ctx context.Context, _ json.RawMessage) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	server, err := NewServer(handlers)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverConnection, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverConnection.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "mce4d-test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	callCtx, cancelCall := context.WithCancel(ctx)
	callDone := make(chan error, 1)
	go func() {
		_, err := session.CallTool(callCtx, &sdkmcp.CallToolParams{
			Name: "mce4d_workspace_status", Arguments: map[string]any{},
		})
		callDone <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("operation handler did not receive the call")
	}
	cancelCall()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("MCP cancellation was not propagated to the operation handler")
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled MCP call error = %v, want context.Canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("cancelled MCP call did not return")
	}
}

func TestValidatedResultCountsActualDualMCPPayloadAndFallsBackSafely(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := contracts.DecodeJSON(statusResponse())
	if err != nil {
		t.Fatal(err)
	}
	response := decoded.(map[string]any)
	budget := response["budget"].(map[string]any)
	budget["max_output_bytes"] = 4096
	warnings := make([]any, 64)
	for index := range warnings {
		warnings[index] = map[string]any{
			"code": "required_evidence_pending", "reason": strings.Repeat("x", 512),
			"count": nil, "source_refs": []any{},
		}
	}
	response["warnings"] = warnings
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	result, err := validatedResult(validator, "mce4d_workspace_status", raw, multiRoundTripProtocol)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("over-budget response did not become an S43 budget error")
	}
	if len(result.Content) != 1 {
		t.Fatalf("result content count = %d, want 1", len(result.Content))
	}
	textContent, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("result content type = %T, want TextContent", result.Content[0])
	}
	if !json.Valid([]byte(textContent.Text)) {
		t.Fatal("budget error text content is not valid JSON")
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(textContent.Text), &envelope); err != nil {
		t.Fatal(err)
	}
	if got := envelope["error"].(map[string]any)["code"]; got != "BUDGET_TOO_SMALL" {
		t.Fatalf("budget error code = %v, want BUDGET_TOO_SMALL", got)
	}
	wire, err := json.Marshal(resultForByteCount{
		Meta: result.Meta, Content: result.Content, StructuredContent: result.StructuredContent,
		IsError: result.IsError, ResultType: "complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(wire)) > 4096 {
		t.Fatalf("actual MCP payload is %d bytes, over its 4096-byte limit", len(wire))
	}
	if got := int64(envelope["budget"].(map[string]any)["serialized_bytes"].(float64)); got != int64(len(wire)) {
		t.Fatalf("recorded output bytes = %d, actual MCP result bytes = %d", got, len(wire))
	}
}

func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("MCE4D_MCP_STDIO_HELPER") != "1" {
		return
	}
	server, err := NewServer(testHandlers())
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(2)
	}
	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(3)
	}
	os.Exit(0)
}

func TestMCPStdioUsesSDKFramingAndExposesOnlySixTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.Command(os.Args[0], "-test.run=^TestMCPStdioHelper$")
	command.Env = append(os.Environ(), "MCE4D_MCP_STDIO_HELPER=1")
	command.Stderr = os.Stderr
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "mce4d-test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()

	listed, err := session.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		input, ok := tool.InputSchema.(map[string]any)
		if !ok || input["type"] != "object" {
			t.Fatalf("tool %s input schema = %T %v, want object schema", tool.Name, tool.InputSchema, tool.InputSchema)
		}
	}
	sort.Strings(names)
	want := []string{
		"mce4d_context_pack", "mce4d_expand_pack", "mce4d_locate",
		"mce4d_rename_preview", "mce4d_validate_pack", "mce4d_workspace_status",
	}
	if len(names) != len(want) {
		t.Fatalf("listed tools = %v, want exactly %v", names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("listed tools = %v, want exactly %v", names, want)
		}
	}

	result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "mce4d_workspace_status", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.StructuredContent == nil {
		t.Fatalf("status result error=%t structured=%T", result.IsError, result.StructuredContent)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	textContent, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("status content type = %T, want TextContent", result.Content[0])
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(textContent.Text), &envelope); err != nil {
		t.Fatal(err)
	}
	serializedBytes := int64(envelope["budget"].(map[string]any)["serialized_bytes"].(float64))
	if serializedBytes != int64(len(encoded)) {
		structured, _ := json.Marshal(result.StructuredContent)
		t.Fatalf("recorded output bytes = %d, actual MCP result bytes = %d (text=%d structured=%d wrapper=%d)", serializedBytes, len(encoded), len(textContent.Text), len(structured), len(encoded)-len(textContent.Text)-len(structured))
	}
	if serializedBytes > int64(envelope["budget"].(map[string]any)["max_output_bytes"].(float64)) {
		t.Fatal("MCP output exceeded its declared byte budget")
	}

	_, err = session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "mce4d_workspace_status", Arguments: map[string]any{"unknown": true},
	})
	var protocolErr *jsonrpc.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("call with unknown input field error = %v, want JSON-RPC invalid params", err)
	}
}

func testHandlers() Handlers {
	handlers := make(Handlers, len(tools))
	for _, definition := range tools {
		handlers[definition.name] = func(context.Context, json.RawMessage) ([]byte, error) {
			return statusResponse(), nil
		}
	}
	return handlers
}

func statusResponse() []byte {
	return []byte(`{"schema_version":1,"request_id":"123e4567-e89b-42d3-a456-426614174000","status":"ok","scope":null,"freshness":{"status":"not_checked","scope":"none","checked_at":null,"buffer_scope":"disk_only","consistency":"captured_bytes_with_observed_stability","changed_count":null,"source_view":"indexed_snapshot"},"coverage":{"status":"unknown","capabilities":[],"gaps":[],"scope_description":"Workspace status is available; no generation is published."},"readiness":"needs_context","data":{"operation":"status","scopes":[],"capabilities":[],"project_stage":"implemented_scope","index_state":"missing","registered_scopes":[]},"pagination":{"truncated":false,"enumeration_complete":true,"returned_count":0,"required_pending":null,"optional_pending":null,"next_cursor":null,"limit_reason":null},"budget":{"max_output_bytes":4096,"serialized_bytes":0,"token_count":null,"token_estimate":null,"tokenizer_id":null,"estimate_only":false,"delivery_mode":"mcp_dual"},"warnings":[],"error":null}`)
}
