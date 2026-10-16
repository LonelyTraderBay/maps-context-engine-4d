package contract_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
)

func TestClosedRequestSchemaAndOperationRegistry(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateRequest("mce4d_workspace_status", []byte(`{}`)); err != nil {
		t.Fatalf("empty status request is valid: %v", err)
	}
	if err := validator.ValidateRequest("mce4d_workspace_status", []byte(`{"unknown":true}`)); !errors.Is(err, contracts.ErrContractViolation) {
		t.Fatalf("unknown field error=%v", err)
	}
	if err := validator.ValidateRequest("unregistered", []byte(`{}`)); !errors.Is(err, contracts.ErrUnknownOperation) {
		t.Fatalf("unknown operation error=%v", err)
	}
	operations := validator.Operations()
	delete(operations, "mce4d_workspace_status")
	if _, exists := validator.Operations()["mce4d_workspace_status"]; !exists {
		t.Fatal("caller mutation must not alter the closed registry")
	}
}

func TestScopeRequestRequiresOneCompleteSelector(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	valid := []byte(`{"scope":{"worktree_id":"11111111-1111-4111-8111-111111111111","profile_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)
	if err := validator.ValidateRequest("mce4d_workspace_status", valid); err != nil {
		t.Fatalf("complete single selector error=%v", err)
	}
	ambiguous := []byte(`{"scope":{"worktree_id":"11111111-1111-4111-8111-111111111111","profile_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","group_id":"22222222-2222-4222-8222-222222222222"}}`)
	if err := validator.ValidateRequest("mce4d_workspace_status", ambiguous); !errors.Is(err, contracts.ErrContractViolation) {
		t.Fatalf("mixed selectors error=%v", err)
	}
}

func TestResponseDataMustMatchRegisteredOperation(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	response := statusResponse(statusData())
	if err := validator.ValidateResponse("mce4d_workspace_status", response); err != nil {
		t.Fatalf("valid status response error=%v", err)
	}
	if err := validator.ValidateResponse("POST /graph/query", response); !errors.Is(err, contracts.ErrContractViolation) {
		t.Fatalf("status data registered as graph data error=%v", err)
	}
	blockedWithData := statusResponse(statusData())
	var value map[string]any
	if err := json.Unmarshal(blockedWithData, &value); err != nil {
		t.Fatal(err)
	}
	value["status"] = "blocked"
	value["readiness"] = "blocked"
	value["error"] = map[string]any{
		"code": "ACCESS_DENIED", "message": "denied", "retryable": false,
		"suggested_action": "none", "details": []any{},
	}
	blockedWithData, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateResponse("mce4d_workspace_status", blockedWithData); !errors.Is(err, contracts.ErrContractViolation) {
		t.Fatalf("blocked response with data error=%v", err)
	}
}

func TestStrictJSONRejectsLossyInputs(t *testing.T) {
	for name, raw := range map[string][]byte{
		"duplicate":         []byte(`{"verify":"none","verify":"workspace"}`),
		"escaped duplicate": []byte(`{"verify":"none","ver\u0069fy":"workspace"}`),
		"trailing value":    []byte(`{} {}`),
		"high surrogate":    []byte(`{"text":"\uD800"}`),
		"low surrogate":     []byte(`{"text":"\uDC00"}`),
	} {
		if _, err := contracts.DecodeJSON(raw); !errors.Is(err, contracts.ErrInvalidJSON) {
			t.Errorf("%s error=%v", name, err)
		}
	}
	invalidUTF8 := []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
	if _, err := contracts.DecodeJSON(invalidUTF8); !errors.Is(err, contracts.ErrInvalidJSON) {
		t.Fatalf("invalid UTF-8 error=%v", err)
	}
	if _, err := contracts.DecodeJSON([]byte(`{"text":"\uD83D\uDE00"}`)); err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
	emptyArray, err := contracts.DecodeJSON([]byte(`{"items":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	items, ok := emptyArray.(map[string]any)["items"].([]any)
	if !ok || items == nil {
		t.Fatalf("empty JSON array lost its non-null identity: %#v", emptyArray)
	}
	deep := strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33)
	if _, err := contracts.DecodeJSON([]byte(deep)); !errors.Is(err, contracts.ErrInvalidJSON) {
		t.Fatalf("excessive depth error=%v", err)
	}
	var tooMany strings.Builder
	tooMany.WriteByte('{')
	for i := 0; i < 4097; i++ {
		if i > 0 {
			tooMany.WriteByte(',')
		}
		_, _ = tooMany.WriteString(`"k`)
		_, _ = tooMany.WriteString(strconv.Itoa(i))
		_, _ = tooMany.WriteString(`":0`)
	}
	tooMany.WriteByte('}')
	if _, err := contracts.DecodeJSON([]byte(tooMany.String())); !errors.Is(err, contracts.ErrInvalidJSON) {
		t.Fatalf("excessive object keys error=%v", err)
	}
}

func TestPrivateJournalSemanticsAndManifestReference(t *testing.T) {
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	journal := []byte(`{"format_version":1,"volume_id":"volume-a","home_identity":"home-a","admission_config_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","entries":[{"operation_id":"11111111-1111-4111-8111-111111111111","scope_kind":"worktree","worktree_id":"22222222-2222-4222-8222-222222222222","owner_identity":"owner-a","ownership_ref":"leases/active.json","operation_kind":"index","allowed_paths":["cache/worktree"],"volume_reserved_remaining_bytes":20,"cache_reserved_remaining_bytes":10,"created_at":"2026-10-16T00:00:00Z"}]}`)
	if err := validator.ValidatePrivateRecord("VolumeAdmissionJournal", journal); err != nil {
		t.Fatalf("valid private journal error=%v", err)
	}
	reservationMismatch := []byte(strings.Replace(string(journal), `"volume_reserved_remaining_bytes":20`, `"volume_reserved_remaining_bytes":9`, 1))
	if err := validator.ValidatePrivateRecord("VolumeAdmissionJournal", reservationMismatch); !errors.Is(err, contracts.ErrContractViolation) {
		t.Fatalf("reservation mismatch error=%v", err)
	}
	ref := []byte(`{"format_version":1,"kind":"external_capture_manifest","path":"manifests/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.json","byte_length":123,"artifact_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","snapshot_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}`)
	if err := validator.ValidatePrivateRecord("StoredCaptureManifestRef", ref); err != nil {
		t.Fatalf("valid external manifest reference error=%v", err)
	}
	badLocator := []byte(strings.Replace(string(ref), "manifests/bbbbb", "manifests/../bbbb", 1))
	if err := validator.ValidatePrivateRecord("StoredCaptureManifestRef", badLocator); !errors.Is(err, contracts.ErrContractViolation) {
		t.Fatalf("traversal locator error=%v", err)
	}
	if err := validator.ValidatePrivateRecord("StatusData", []byte(`{}`)); !errors.Is(err, contracts.ErrUnknownDefinition) {
		t.Fatalf("wire schema cannot be exposed as private storage: %v", err)
	}
}

func statusData() map[string]any {
	return map[string]any{
		"operation": "status", "scopes": []any{}, "capabilities": []any{},
		"project_stage": "spec_only", "index_state": "missing", "registered_scopes": []any{},
	}
}

func statusResponse(data any) []byte {
	value := map[string]any{
		"schema_version": 1,
		"request_id":     "123e4567-e89b-42d3-a456-426614174000",
		"status":         "ok", "scope": nil,
		"freshness": map[string]any{
			"status": "not_checked", "scope": "none", "checked_at": nil,
			"buffer_scope": "disk_only", "consistency": "captured_bytes_with_observed_stability",
			"changed_count": nil, "source_view": "indexed_snapshot",
		},
		"coverage": map[string]any{
			"status": "supported_scope_complete", "capabilities": []any{}, "gaps": []any{},
			"scope_description": "explicit test scope",
		},
		"readiness": "needs_context", "data": data,
		"pagination": map[string]any{
			"truncated": false, "enumeration_complete": true, "returned_count": 0,
			"required_pending": 0, "optional_pending": nil, "next_cursor": nil, "limit_reason": nil,
		},
		"budget": map[string]any{
			"max_output_bytes": 4096, "serialized_bytes": 0, "token_count": nil,
			"token_estimate": nil, "tokenizer_id": nil, "estimate_only": true, "delivery_mode": "cli",
		},
		"warnings": []any{}, "error": nil,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
