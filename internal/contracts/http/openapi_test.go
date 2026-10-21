package httpcontract

import (
	"encoding/json"
	"strings"
	"testing"

	schema "github.com/LonelyTraderBay/maps-context-engine-4d/internal/contracts/schema"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func testInput(t *testing.T, root string, files map[string][]byte, version string) Input {
	t.Helper()
	profile := model.SHA256([]byte("http-profile"))
	bundle := schema.Bundle{RootPath: root, Documents: make([]schema.SourceDocument, 0, len(files))}
	for pathName, content := range files {
		ref := model.SourceRef{
			WorktreeID:   "11111111-1111-4111-8111-111111111111",
			ProfileID:    profile,
			GenerationID: "22222222-2222-4222-8222-222222222222",
			Path:         pathName, ContentHash: model.SHA256(content),
			Range: model.ByteRange{StartByte: 0, EndByte: int64(len(content))},
		}
		bundle.Documents = append(bundle.Documents, schema.SourceDocument{Path: pathName, Ref: ref, Bytes: content})
	}
	sourceHash, err := bundle.ComputeSourceHash()
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		Identity: schema.Identity{
			RepositoryScope: "LonelyTraderBay/maps-context-engine-4d",
			Environment:     "test",
			ProfileID:       profile,
			Protocol:        schema.ProtocolOpenAPI31,
			Namespace:       "billing",
			ServerBinding:   "public-api",
			ContractID:      "billing-api",
			Version:         model.Identifier(version),
			SourceHash:      sourceHash,
		},
		Bundle: bundle,
	}
}

func sourceRef(path string, source []byte) model.SourceRef {
	return model.SourceRef{
		WorktreeID:   "11111111-1111-4111-8111-111111111111",
		ProfileID:    model.SHA256([]byte("http-profile")),
		GenerationID: "22222222-2222-4222-8222-222222222222",
		Path:         path, ContentHash: model.SHA256(source),
		Range: model.ByteRange{StartByte: 0, EndByte: int64(len(source))},
	}
}

func TestAnalyzeOpenAPI31OperationsServersSecurityAndCandidateBinding(t *testing.T) {
	root := []byte("{\"openapi\":\"3.1.0\",\"info\":{\"title\":\"Billing\",\"version\":\"v1\"},\"servers\":[{\"url\":\"https://api.example.test/v1\"}],\"security\":[{\"oauth\":[\"read\",\"write\"]},{\"apiKey\":[]}],\"paths\":{\"/charges/{id}\":{\"get\":{\"operationId\":\"getCharge\",\"summary\":\"Read one charge\"},\"delete\":{\"security\":[],\"responses\":{}}},\"/charges\":{\"post\":{\"operationId\":\"createCharge\",\"servers\":[{\"url\":\"https://{region}.example.test\",\"variables\":{\"region\":{\"default\":\"eu\"}}}],\"security\":[{\"bearerAuth\":[]}],\"responses\":{}}}},\"components\":{\"securitySchemes\":{\"oauth\":{\"type\":\"oauth2\"},\"apiKey\":{\"type\":\"apiKey\"},\"bearerAuth\":{\"type\":\"http\"}},\"schemas\":{\"Charge\":{\"type\":\"object\",\"properties\":{\"id\":{\"type\":\"string\"}}}}}}")
	input := testInput(t, "spec/openapi.json", map[string][]byte{"spec/openapi.json": root}, "v1")
	handler := []byte("func GetCharge(id string) {}")
	input.Bindings = []BindingCandidate{{
		Method: "GET", Path: "/charges/{id}", Kind: BindingHandler,
		SourceRef: sourceRef("internal/api/handler.go", handler), SourceBytes: handler,
	}}

	result, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.OpenAPIVersion != OpenAPI31 || result.Coverage != schema.CoverageComplete || len(result.Operations) != 3 || len(result.Schemas) != 1 {
		t.Fatalf("unexpected OpenAPI result: %+v", result)
	}
	operations := make(map[string]Operation, len(result.Operations))
	for _, operation := range result.Operations {
		operations[operation.Method+" "+operation.Path] = operation
		if err := operation.SourceRef.ValidateAgainst(root); err != nil {
			t.Fatalf("operation source reference is invalid: %v", err)
		}
	}
	get := operations["GET /charges/{id}"]
	if !get.SecurityDeclared || len(get.Security) != 2 || get.Security[0].Alternative == get.Security[1].Alternative {
		t.Fatalf("security OR alternatives were flattened or lost: %+v", get.Security)
	}
	deleteOperation := operations["DELETE /charges/{id}"]
	if !deleteOperation.SecurityDeclared || len(deleteOperation.Security) != 0 {
		t.Fatalf("explicitly unauthenticated operation lost its declaration: %+v", deleteOperation)
	}
	create := operations["POST /charges"]
	if len(create.Servers) != 1 || len(create.Servers[0].VariableNames) != 1 || create.Servers[0].VariableNames[0] != "region" {
		t.Fatalf("server binding was not retained safely: %+v", create.Servers)
	}
	if create.DeclarationStatus != BindingDeclared || len(result.Bindings) != 1 || result.Bindings[0].Status != BindingSourceLinkedCandidate {
		t.Fatalf("binding evidence overstated or was lost: operations=%+v bindings=%+v", result.Operations, result.Bindings)
	}
	if result.Bindings[0].SourceRef.Path != "internal/api/handler.go" {
		t.Fatalf("candidate source ref is missing: %+v", result.Bindings[0])
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatalf("result is not serializable: %v", err)
	}
}

func TestAnalyzeOpenAPIResolvesAllowlistedPathItemsAndTracksTheirSource(t *testing.T) {
	root := []byte("{\"openapi\":\"3.1.0\",\"info\":{\"title\":\"Billing\",\"version\":\"v1\"},\"paths\":{\"/charges\":{\"$ref\":\"paths/charges.json\"}}}")
	pathItem := []byte("{\"post\":{\"operationId\":\"createCharge\",\"responses\":{}}}")
	input := testInput(t, "spec/openapi.json", map[string][]byte{
		"spec/openapi.json":       root,
		"spec/paths/charges.json": pathItem,
	}, "v1")
	result, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Operations) != 1 || result.Operations[0].SourceRef.Path != "spec/paths/charges.json" {
		t.Fatalf("external local path item lost its source identity: %+v", result.Operations)
	}
	if len(result.References) != 1 || result.References[0].Status != schema.RefResolved {
		t.Fatalf("allowlisted path item ref did not resolve: %+v", result.References)
	}
}

func TestAnalyzeOpenAPILeavesRemoteTraversalCycleAndUnsafeServerAsGaps(t *testing.T) {
	root := []byte("{\"openapi\":\"3.1.0\",\"info\":{\"title\":\"Billing\",\"version\":\"v1\"},\"servers\":[{\"url\":\"https://user:password@example.test/api?token=secret\"}],\"paths\":{\"/remote\":{\"$ref\":\"https://example.invalid/path.json\"},\"/escape\":{\"$ref\":\"../outside.json\"},\"/ok\":{\"get\":{\"responses\":{\"200\":{\"description\":\"ok\",\"content\":{\"application/json\":{\"example\":{\"$ref\":\"https://not-a-reference.invalid/secret\"}}}}}}}},\"components\":{\"schemas\":{\"Cycle\":{\"$ref\":\"#/components/schemas/Cycle\"},\"Remote\":{\"$ref\":\"https://example.invalid/schema.json\"}}}}")
	input := testInput(t, "spec/openapi.json", map[string][]byte{"spec/openapi.json": root}, "v1")
	result, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[schema.RefStatus]bool{}
	codes := map[string]bool{}
	for _, reference := range result.References {
		statuses[reference.Status] = true
	}
	for _, diagnostic := range result.Diagnostics {
		codes[diagnostic.Code] = true
	}
	if result.Coverage != schema.CoveragePartial || len(result.References) != 4 || !statuses[schema.RefRemote] || !statuses[schema.RefTraversal] || !statuses[schema.RefCycle] || !codes["unsafe_or_invalid_server"] {
		t.Fatalf("unsafe reference/server gaps are incomplete: %+v", result)
	}
	serialized, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "password") || strings.Contains(string(serialized), "token=secret") {
		t.Fatal("unsafe server credential/query data leaked into result")
	}
}

func TestAnalyzeOpenAPIDoesNotAssumeUnsupportedOrMismatchedVersions(t *testing.T) {
	for name, source := range map[string][]byte{
		"unsupported OpenAPI": []byte("{\"openapi\":\"3.0.3\",\"info\":{\"title\":\"Billing\",\"version\":\"v1\"},\"paths\":{\"/x\":{\"get\":{}}}}"),
		"identity mismatch":   []byte("{\"openapi\":\"3.1.0\",\"info\":{\"title\":\"Billing\",\"version\":\"v2\"},\"paths\":{\"/x\":{\"get\":{}}}}"),
	} {
		t.Run(name, func(t *testing.T) {
			input := testInput(t, "spec/openapi.json", map[string][]byte{"spec/openapi.json": source}, "v1")
			result, err := Analyze(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Operations) != 0 || result.Coverage != schema.CoveragePartial || len(result.Diagnostics) == 0 {
				t.Fatalf("version mismatch was interpreted as supported: %+v", result)
			}
		})
	}
}

func TestAnalyzeOpenAPIRejectsDuplicateJSONMembersAndSelfBinding(t *testing.T) {
	duplicate := []byte("{\"openapi\":\"3.1.0\",\"openapi\":\"3.1.0\",\"info\":{\"title\":\"Billing\",\"version\":\"v1\"},\"paths\":{}}")
	input := testInput(t, "spec/openapi.json", map[string][]byte{"spec/openapi.json": duplicate}, "v1")
	if _, err := Analyze(input); err == nil {
		t.Fatal("duplicate JSON member was accepted")
	}

	root := []byte("{\"openapi\":\"3.1.0\",\"info\":{\"title\":\"Billing\",\"version\":\"v1\"},\"paths\":{\"/x\":{\"get\":{}}}}")
	input = testInput(t, "spec/openapi.json", map[string][]byte{"spec/openapi.json": root}, "v1")
	input.Bindings = []BindingCandidate{{Method: "GET", Path: "/x", Kind: BindingHandler, SourceRef: sourceRef("spec/openapi.json", root), SourceBytes: root}}
	result, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Bindings) != 0 || result.Coverage != schema.CoveragePartial {
		t.Fatalf("contract source was used to prove its own handler: %+v", result)
	}
}
