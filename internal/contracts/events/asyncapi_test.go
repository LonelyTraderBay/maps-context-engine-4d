package eventscontract

import (
	"encoding/json"
	"strings"
	"testing"

	schema "github.com/LonelyTraderBay/maps-context-engine-4d/internal/contracts/schema"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const eventIdentityProfile = "11111111-1111-4111-8111-111111111111"

func fixtureInput(t *testing.T, source []byte, extra map[string][]byte) Input {
	t.Helper()
	profile := model.SHA256([]byte(eventIdentityProfile))
	files := map[string][]byte{"asyncapi.json": source}
	for path, content := range extra {
		files[path] = content
	}
	bundle := schema.Bundle{RootPath: "asyncapi.json", Documents: make([]schema.SourceDocument, 0, len(files))}
	for path, content := range files {
		ref := model.SourceRef{
			WorktreeID: "11111111-1111-4111-8111-111111111111", ProfileID: profile,
			GenerationID: "22222222-2222-4222-8222-222222222222", Path: path,
			ContentHash: model.SHA256(content),
			Range:       model.ByteRange{StartByte: 0, EndByte: int64(len(content))},
		}
		bundle.Documents = append(bundle.Documents, schema.SourceDocument{Path: path, Ref: ref, Bytes: content})
	}
	sourceHash, err := bundle.ComputeSourceHash()
	if err != nil {
		t.Fatal(err)
	}
	identity := schema.Identity{
		RepositoryScope: "LonelyTraderBay/maps-context-engine-4d", Environment: "test",
		ProfileID: profile, Protocol: schema.ProtocolAsyncAPI31, Namespace: "payments",
		ServerBinding: "broker-test", ContractID: "orders", Version: "v1", SourceHash: sourceHash,
	}
	return Input{Identity: identity, Bundle: bundle}
}

func baseDocument(payload string) []byte {
	return []byte(`{
  "asyncapi":"3.1.0",
  "info":{"title":"Orders","version":"v1"},
  "servers":{"primary":{"host":"user:server-secret@broker.example","protocol":"mqtts"}},
  "channels":{"orders":{"address":"orders.{region}.#","messages":{"created":{"$ref":"#/components/messages/Created"}},"bindings":{"mqtt":{"qos":2,"privateBindingValue":"hidden"}}}},
  "operations":{
    "publish":{"action":"send","channel":{"$ref":"#/channels/orders"},"messages":[{"$ref":"#/components/messages/Created"}],"bindings":{"mqtt":{"qos":1,"retain":true},"amqp":{"ack":false}}},
    "consume":{"action":"receive","channel":{"$ref":"#/channels/orders"},"messages":[{"$ref":"#/components/messages/Created"}]}
  },
  "components":{"messages":{"Created":{"payload":` + payload + `,"bindings":{"amqp":{"ack":"raw-secret-value"}}}}}
}`)
}

func TestAnalyzeAsyncAPI31LinksDirectionsAndSourceBackedMessages(t *testing.T) {
	input := fixtureInput(t, baseDocument(`{"type":"object","required":["id"],"properties":{"id":{"type":"string","format":"uuid"},"note":{"type":["string","null"],"default":"must-not-escape"}}}`), nil)
	result, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.AsyncAPIVersion != AsyncAPI31 || result.Coverage != schema.CoverageComplete || len(result.Diagnostics) != 0 {
		t.Fatalf("version=%q coverage=%s diagnostics=%+v", result.AsyncAPIVersion, result.Coverage, result.Diagnostics)
	}
	if len(result.Channels) != 1 || result.Channels[0].AddressKind != AddressTemplate || result.Channels[0].Address != "orders.{region}.#" {
		t.Fatalf("unexpected channel facts: %+v", result.Channels)
	}
	if len(result.Operations) != 2 || result.Operations[0].Key == result.Operations[1].Key {
		t.Fatalf("send and receive operations were not preserved: %+v", result.Operations)
	}
	for _, operation := range result.Operations {
		if len(operation.MessageKeys) != 1 {
			t.Fatalf("operation did not link one channel message: %+v", operation)
		}
	}
	if len(result.Messages) != 1 || len(result.PayloadFields) != 2 {
		t.Fatalf("messages=%d payload_fields=%d", len(result.Messages), len(result.PayloadFields))
	}
	if result.PayloadFields[0].SourceRef.Path != "asyncapi.json" || !strings.Contains(result.PayloadFields[0].JSONPointer, "/payload/properties/") {
		t.Fatalf("payload field lost source pointer: %+v", result.PayloadFields[0])
	}
	var idField, noteField *PayloadField
	for index := range result.PayloadFields {
		field := &result.PayloadFields[index]
		if field.Name == "id" {
			idField = field
		}
		if field.Name == "note" {
			noteField = field
		}
	}
	if idField == nil || !idField.Required || idField.Format != "uuid" || noteField == nil || !noteField.Nullable || noteField.Required {
		t.Fatalf("types, required, nullable or format facts are incorrect: %+v", result.PayloadFields)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"server-secret", "broker.example", "hidden", "raw-secret-value", "must-not-escape", "server_url"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("result exposed input value %q", forbidden)
		}
	}
	if len(result.Controls) != 3 {
		t.Fatalf("expected allowlisted controls only, got %+v", result.Controls)
	}
	for _, control := range result.Controls {
		if control.Status != BindingDeclaredOnly || !strings.HasPrefix(control.JSONPointer, "/operations/") || strings.Contains(control.JSONPointer, "privateBindingValue") {
			t.Fatalf("binding control is not presence-only: %+v", control)
		}
	}
}

func TestIdentityScopesKeepSameChannelAddressDistinct(t *testing.T) {
	input := fixtureInput(t, baseDocument(`{"type":"object"}`), nil)
	first, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Identity.Environment = "production"
	second, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Channels[0].Address != second.Channels[0].Address || first.Channels[0].Key == second.Channels[0].Key {
		t.Fatal("same address under distinct contract identity scopes collided")
	}
	input.Identity.ServerBinding = "another-broker"
	third, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Channels[0].Key == third.Channels[0].Key {
		t.Fatal("channel identity omitted opaque server binding")
	}
}

func TestAddressAndUnsupportedFormatAreExplicitGaps(t *testing.T) {
	data := strings.Replace(string(baseDocument(`{"type":"object"}`)), `"address":"orders.{region}.#",`, ``, 1)
	data = strings.Replace(data, `"payload":{"type":"object"}`, `"schemaFormat":"application/x-custom-schema","payload":{"type":"object"}`, 1)
	result, err := Analyze(fixtureInput(t, []byte(data), nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Channels[0].AddressKind != AddressUnknown || result.Channels[0].Address != "" {
		t.Fatalf("absent address must remain unknown: %+v", result.Channels[0])
	}
	if result.Coverage != schema.CoveragePartial || !hasDiagnostic(result, "unsupported_schema_format") {
		t.Fatalf("unsupported payload format was not an explicit gap: %+v", result.Diagnostics)
	}
}

func TestPayloadReferencesStayInsideBundleAndReportCycles(t *testing.T) {
	remote := baseDocument(`{"$ref":"https://user:password@example.invalid/schema.json#/Order"}`)
	missing := baseDocument(`{"$ref":"missing.json#/Order"}`)
	traversal := baseDocument(`{"$ref":"../secret.json#/Order"}`)
	cycle := baseDocument(`{"$ref":"#/components/schemas/Order"}`)
	cycleText := strings.Replace(string(cycle), `"components":{"messages":`, `"components":{"schemas":{"Order":{"$ref":"#/components/schemas/Order"}},"messages":`, 1)
	for name, document := range map[string][]byte{
		"remote": remote, "missing": missing, "traversal": traversal, "cycle": []byte(cycleText),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := Analyze(fixtureInput(t, document, nil))
			if err != nil {
				t.Fatal(err)
			}
			if result.Coverage != schema.CoveragePartial || len(result.References) == 0 {
				t.Fatalf("reference gap was not retained: %+v", result)
			}
			want := map[string]string{"remote": "remote_reference_unresolved", "missing": "missing_reference_unresolved", "traversal": "unsafe_reference_unresolved", "cycle": "cyclic_reference_unresolved"}[name]
			if !hasDiagnostic(result, want) {
				t.Fatalf("expected %s gap, got %+v", want, result.Diagnostics)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "example.invalid") {
				t.Fatal("unresolved reference leaked raw remote URI")
			}
		})
	}
}

func TestPayloadLocalReferenceResolvesCapturedJSONSchema(t *testing.T) {
	root := baseDocument(`{"$ref":"payload.json#/definitions/Order"}`)
	root = []byte(strings.Replace(string(root), `"payload":{"$ref":"payload.json#/definitions/Order"}`, `"schemaFormat":"application/schema+json;version=draft-07","payload":{"$ref":"payload.json#/definitions/Order"}`, 1))
	input := fixtureInput(t, root, map[string][]byte{
		"payload.json": []byte(`{"$schema":"http://json-schema.org/draft-07/schema#","definitions":{"Order":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}}`),
	})
	result, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage != schema.CoverageComplete || len(result.PayloadFields) != 1 || result.PayloadFields[0].DocumentPath != "payload.json" || !result.PayloadFields[0].Required {
		t.Fatalf("local payload reference was not resolved from the captured bundle: %+v", result)
	}
	foundResolved := false
	for _, reference := range result.References {
		foundResolved = foundResolved || reference.Status == schema.RefResolved && reference.TargetPath == "payload.json"
	}
	if !foundResolved {
		t.Fatalf("resolved source reference missing: %+v", result.References)
	}
}

func TestUnsupportedVersionAndStrictJSONFailures(t *testing.T) {
	unsupported := strings.Replace(string(baseDocument(`{"type":"object"}`)), `"asyncapi":"3.1.0"`, `"asyncapi":"2.6.0"`, 1)
	result, err := Analyze(fixtureInput(t, []byte(unsupported), nil))
	if err != nil || result.Coverage != schema.CoveragePartial || len(result.Operations) != 0 || !hasDiagnostic(result, "unsupported_asyncapi_version") {
		t.Fatalf("unsupported version was not a non-interpreted gap: result=%+v err=%v", result, err)
	}
	for _, source := range [][]byte{
		[]byte(`{"asyncapi":"3.1.0","asyncapi":"3.1.0"}`),
		[]byte(`{"asyncapi":`),
		[]byte(strings.Repeat(" ", schema.MaxDocumentBytes+1)),
		[]byte(`{"deep":` + strings.Repeat("[", schema.MaxRefDepth+2) + `0` + strings.Repeat("]", schema.MaxRefDepth+2) + `}`),
	} {
		if _, err := Analyze(fixtureInput(t, source, nil)); err == nil {
			t.Fatal("malformed, duplicate-key or oversized JSON was accepted")
		}
	}
}

func hasDiagnostic(result Result, code string) bool {
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
