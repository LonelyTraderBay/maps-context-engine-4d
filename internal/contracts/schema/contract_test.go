package schemacontract

import (
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func testBundle(t *testing.T, root string, files map[string][]byte, protocol Protocol, version string) (Identity, Bundle) {
	t.Helper()
	profile := model.SHA256([]byte("profile-test"))
	bundle := Bundle{RootPath: root, Documents: make([]SourceDocument, 0, len(files))}
	for pathName, content := range files {
		ref := model.SourceRef{
			WorktreeID:   "11111111-1111-4111-8111-111111111111",
			ProfileID:    profile,
			GenerationID: "22222222-2222-4222-8222-222222222222",
			Path:         pathName, ContentHash: model.SHA256(content),
			Range: model.ByteRange{StartByte: 0, EndByte: int64(len(content))},
		}
		bundle.Documents = append(bundle.Documents, SourceDocument{Path: pathName, Ref: ref, Bytes: content})
	}
	sourceHash, err := bundle.ComputeSourceHash()
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{
		RepositoryScope: "LonelyTraderBay/maps-context-engine-4d",
		Environment:     "test",
		ProfileID:       profile,
		Protocol:        protocol,
		Namespace:       "payments",
		ServerBinding:   "public-api",
		ContractID:      "billing",
		Version:         model.Identifier(version),
		SourceHash:      sourceHash,
	}
	return identity, bundle
}

func TestAnalyzeJSONSchemaResolvesOnlyAllowlistedSources(t *testing.T) {
	root := []byte("{\"$schema\":\"" + JSONSchema202012 + "\",\"type\":\"object\",\"properties\":{\"id\":{\"type\":\"string\"},\"amount\":{\"$ref\":\"parts/common.json#/$defs/Money\"}},\"required\":[\"id\",\"amount\"]}")
	common := []byte("{\"$schema\":\"" + JSONSchema202012 + "\",\"$defs\":{\"Money\":{\"type\":\"object\",\"properties\":{\"value\":{\"type\":[\"number\",\"null\"],\"x-units\":\"USD\",\"x-precision\":2}}}}}")
	identity, bundle := testBundle(t, "schemas/root.json", map[string][]byte{
		"schemas/root.json":         root,
		"schemas/parts/common.json": common,
	}, ProtocolJSONSchema202012, "v1")

	result, err := AnalyzeJSONSchema(identity, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage != CoverageComplete || len(result.Diagnostics) != 0 {
		t.Fatalf("coverage=%s diagnostics=%+v", result.Coverage, result.Diagnostics)
	}
	if len(result.References) != 1 || result.References[0].Status != RefResolved || result.References[0].TargetPath != "schemas/parts/common.json" {
		t.Fatalf("local reference was not resolved from the bundle: %+v", result.References)
	}
	fields := make(map[string]Field, len(result.Fields))
	for _, field := range result.Fields {
		fields[field.Name] = field
		if err := field.SourceRef.ValidateAgainst(map[string][]byte{
			"schemas/root.json":         root,
			"schemas/parts/common.json": common,
		}[field.DocumentPath]); err != nil {
			t.Fatalf("field source reference is not bound to captured bytes: %v", err)
		}
	}
	if !fields["id"].Required || fields["id"].Nullable || len(fields["id"].Types) != 1 || fields["id"].Types[0] != "string" {
		t.Fatalf("required string field was not retained: %+v", fields["id"])
	}
	if !fields["amount"].Required {
		t.Fatalf("referencing field lost required declaration: %+v", fields["amount"])
	}
	if !fields["value"].Nullable || fields["value"].Required || string(fields["value"].Units) != "\"USD\"" || string(fields["value"].Precision) != "2" {
		t.Fatalf("nullability, units, precision, or optionality were lost: %+v", fields["value"])
	}
}

func TestJSONSchemaReportsCyclesRemoteTraversalAndUnsupportedKeywords(t *testing.T) {
	source := []byte("{\"$schema\":\"" + JSONSchema202012 + "\",\"$defs\":{\"A\":{\"$ref\":\"#/$defs/B\"},\"B\":{\"$ref\":\"#/$defs/A\"}},\"properties\":{\"remote\":{\"$ref\":\"https://example.invalid/schema.json\"},\"escape\":{\"$ref\":\"../outside.json#/$defs/x\"},\"patterned\":{\"type\":\"string\",\"pattern\":\"^[a-z]+$\"}}}")
	identity, bundle := testBundle(t, "schemas/root.json", map[string][]byte{"schemas/root.json": source}, ProtocolJSONSchema202012, "v1")
	result, err := AnalyzeJSONSchema(identity, bundle)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[RefStatus]bool{}
	codes := map[string]bool{}
	for _, reference := range result.References {
		statuses[reference.Status] = true
	}
	for _, diagnostic := range result.Diagnostics {
		codes[diagnostic.Code] = true
	}
	if result.Coverage != CoveragePartial || !statuses[RefCycle] || !statuses[RefRemote] || !statuses[RefTraversal] {
		t.Fatalf("coverage or reference outcomes are incomplete: %+v", result)
	}
	if !codes["unsupported_keyword"] {
		t.Fatalf("unsupported keyword did not produce a gap: %+v", result.Diagnostics)
	}
}

func TestJSONSchemaVersionMismatchRemainsAGap(t *testing.T) {
	source := []byte("{\"$schema\":\"https://json-schema.org/draft/2019-09/schema\",\"properties\":{\"name\":{\"type\":\"string\"}}}")
	identity, bundle := testBundle(t, "schema.json", map[string][]byte{"schema.json": source}, ProtocolJSONSchema202012, "v1")
	result, err := AnalyzeJSONSchema(identity, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Dialect != "" || result.Coverage != CoveragePartial || len(result.Fields) != 1 || result.Diagnostics[0].Code != "unsupported_dialect" {
		t.Fatalf("unsupported dialect was treated as compatible: %+v", result)
	}
}

func TestStrictJSONRejectsMalformedDuplicateAndResourceBoundaries(t *testing.T) {
	for name, input := range map[string][]byte{
		"duplicate key":      []byte("{\"a\":1,\"a\":2}"),
		"malformed":          []byte("{\"a\":"),
		"trailing":           []byte("{}{}"),
		"unpaired surrogate": []byte("{\"a\":\"\\uD800\"}"),
		"too deep":           []byte(strings.Repeat("[", MaxRefDepth+3) + "0" + strings.Repeat("]", MaxRefDepth+3)),
		"too large":          []byte(strings.Repeat(" ", MaxDocumentBytes+1)),
		"too many nodes":     []byte("[" + strings.Repeat("0,", MaxJSONNodes) + "0]"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeJSONDocument(input); err == nil {
				t.Fatalf("accepted invalid or over-bound JSON input")
			}
		})
	}
}

func TestJSONBundleAppliesOneAggregateNodeBudgetAndStableSourceHash(t *testing.T) {
	document := []byte("{\"values\":[" + strings.Repeat("0,", 60_000) + "0]}")
	if _, err := DecodeJSONBundle(map[string]SourceDocument{
		"a.json": {Bytes: document},
		"b.json": {Bytes: document},
	}); err == nil {
		t.Fatal("accepted a bundle whose aggregate JSON nodes exceed the limit")
	}
	identity, bundle := testBundle(t, "root.json", map[string][]byte{
		"root.json": []byte("{\"$schema\":\"" + JSONSchema202012 + "\"}"),
		"part.json": []byte("{\"$schema\":\"" + JSONSchema202012 + "\"}"),
	}, ProtocolJSONSchema202012, "v1")
	first, err := bundle.ComputeSourceHash()
	if err != nil {
		t.Fatal(err)
	}
	bundle.Documents[0], bundle.Documents[1] = bundle.Documents[1], bundle.Documents[0]
	second, err := bundle.ComputeSourceHash()
	if err != nil || first != second || identity.SourceHash != first {
		t.Fatalf("bundle source hash depends on caller order: first=%s second=%s err=%v", first, second, err)
	}
}

func TestJSONSchemaWithIDDoesNotResolveAgainstAnUnmodeledBaseURI(t *testing.T) {
	source := []byte("{\"$schema\":\"" + JSONSchema202012 + "\",\"$id\":\"https://example.invalid/schemas/root.json\",\"$ref\":\"part.json\"}")
	identity, bundle := testBundle(t, "schemas/root.json", map[string][]byte{"schemas/root.json": source}, ProtocolJSONSchema202012, "v1")
	result, err := AnalyzeJSONSchema(identity, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage != CoveragePartial || len(result.References) != 1 || result.References[0].Status != RefUnsupported {
		t.Fatalf("unsupported $id base URI was used to resolve a ref: %+v", result)
	}
}

func TestBundleIdentityAndPathsAreClosed(t *testing.T) {
	source := []byte("{\"$schema\":\"" + JSONSchema202012 + "\"}")
	identity, bundle := testBundle(t, "schema.json", map[string][]byte{"schema.json": source}, ProtocolJSONSchema202012, "v1")
	identity.Namespace = "https://user:secret@example.invalid"
	if _, err := AnalyzeJSONSchema(identity, bundle); err == nil {
		t.Fatal("credential-bearing namespace was accepted")
	}
	identity, bundle = testBundle(t, "schema.json", map[string][]byte{"schema.json": source}, ProtocolJSONSchema202012, "v1")
	bundle.Documents[0].Path = "../schema.json"
	if _, err := AnalyzeJSONSchema(identity, bundle); err == nil {
		t.Fatal("traversal path was accepted")
	}
}

func TestAsyncAPIIdentityRequiresOpaqueServerBinding(t *testing.T) {
	source := []byte("{\"asyncapi\":\"3.1.0\"}")
	identity, _ := testBundle(t, "asyncapi.json", map[string][]byte{"asyncapi.json": source}, ProtocolAsyncAPI31, "v1")
	identity.ServerBinding = ""
	if err := identity.Validate(); err == nil {
		t.Fatal("shared identity accepted AsyncAPI without an opaque server binding")
	}
	identity.ServerBinding = "broker-prod"
	if err := identity.Validate(); err != nil {
		t.Fatalf("valid AsyncAPI identity rejected: %v", err)
	}
}

func TestAnalyzeProto3ResolvesAllowlistedImportsAndFieldTags(t *testing.T) {
	root := []byte("syntax = \"proto3\";\npackage billing.v1;\nimport \"common/money.proto\";\nmessage Charge {\n  string id = 1;\n  Money amount = 2;\n  repeated string labels = 3;\n}\nenum State {\n  STATE_UNSPECIFIED = 0;\n  SETTLED = 1;\n}\n")
	common := []byte("syntax = \"proto3\";\npackage billing.v1;\nmessage Money {\n  int64 units = 1;\n}\n")
	identity, bundle := testBundle(t, "proto/billing.proto", map[string][]byte{
		"proto/billing.proto":      root,
		"proto/common/money.proto": common,
	}, ProtocolProto3, "v1")

	result, err := AnalyzeProto3(identity, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Syntax != "proto3" || result.Coverage != CoverageComplete || len(result.Diagnostics) != 0 {
		t.Fatalf("syntax=%q coverage=%s diagnostics=%+v", result.Syntax, result.Coverage, result.Diagnostics)
	}
	if len(result.Imports) != 1 || result.Imports[0].Status != RefResolved || len(result.Messages) != 2 || len(result.Enums) != 1 {
		t.Fatalf("message/enum/import declarations were not retained: %+v", result)
	}
	var charge *ProtoMessage
	for index := range result.Messages {
		if result.Messages[index].FullName == "billing.v1.Charge" {
			charge = &result.Messages[index]
		}
	}
	if charge == nil || len(charge.Fields) != 3 || charge.Fields[1].Type != "Money" || charge.Fields[2].Label != "repeated" {
		t.Fatalf("message fields were not retained: %+v", charge)
	}
	for _, field := range charge.Fields {
		if err := field.SourceRef.ValidateAgainst(root); err != nil || field.SourceRef.Range.EndByte <= field.SourceRef.Range.StartByte {
			t.Fatalf("field source range is not bound to captured bytes: %+v err=%v", field.SourceRef, err)
		}
	}
	if result.Enums[0].Values[0].Number != 0 || result.Enums[0].Values[1].Number != 1 {
		t.Fatalf("enum values were not retained: %+v", result.Enums[0])
	}
}

func TestProto3CyclesAndUnsafeImportsRemainGaps(t *testing.T) {
	root := []byte("syntax = \"proto3\";\nimport \"common.proto\";\nimport \"../outside.proto\";\nmessage Root { string id = 1; }\n")
	common := []byte("syntax = \"proto3\";\nimport \"root.proto\";\nmessage Common { string id = 1; }\n")
	identity, bundle := testBundle(t, "proto/root.proto", map[string][]byte{
		"proto/root.proto":   root,
		"proto/common.proto": common,
	}, ProtocolProto3, "v1")
	result, err := AnalyzeProto3(identity, bundle)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[RefStatus]bool{}
	for _, imported := range result.Imports {
		statuses[imported.Status] = true
	}
	if result.Coverage != CoveragePartial || !statuses[RefCycle] || !statuses[RefTraversal] {
		t.Fatalf("proto import cycle/traversal did not remain explicit gaps: %+v", result)
	}
}

func TestProto3RejectsUnsupportedSyntaxDuplicateTagsAndMalformedInput(t *testing.T) {
	for name, source := range map[string][]byte{
		"unsupported proto2":      []byte("syntax = \"proto2\"; message A { string id = 1; }"),
		"duplicate field tag":     []byte("syntax = \"proto3\"; message A { string first = 1; int32 second = 1; }"),
		"enum must start at zero": []byte("syntax = \"proto3\"; enum A { ONE = 1; }"),
		"malformed field":         []byte("syntax = \"proto3\"; message A { string id = ; }"),
		"leading-zero tag":        []byte("syntax = \"proto3\"; message A { string id = 01; }"),
	} {
		t.Run(name, func(t *testing.T) {
			identity, bundle := testBundle(t, "proto/root.proto", map[string][]byte{"proto/root.proto": source}, ProtocolProto3, "v1")
			if _, err := AnalyzeProto3(identity, bundle); err == nil {
				t.Fatal("accepted unsupported or malformed proto declaration")
			}
		})
	}
}
