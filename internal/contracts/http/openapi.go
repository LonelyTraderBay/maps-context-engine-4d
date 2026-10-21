// Package httpcontract extracts source-backed declarations from OpenAPI 3.1.0
// JSON documents. It does not register routes, execute handlers, or fetch refs.
package httpcontract

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	schema "github.com/LonelyTraderBay/maps-context-engine-4d/internal/contracts/schema"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const OpenAPI31 = "3.1.0"

type BindingKind string

const (
	BindingHandler BindingKind = "handler"
	BindingClient  BindingKind = "client"
)

type BindingStatus string

const (
	BindingDeclared              BindingStatus = "declared"
	BindingSourceLinkedCandidate BindingStatus = "source_linked_candidate"
)

type BindingCandidate struct {
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Kind        BindingKind     `json:"kind"`
	SourceRef   model.SourceRef `json:"source_ref"`
	SourceBytes []byte          `json:"-"`
}

type Binding struct {
	OperationID model.Hash      `json:"operation_key"`
	Kind        BindingKind     `json:"kind"`
	Status      BindingStatus   `json:"status"`
	SourceRef   model.SourceRef `json:"source_ref"`
}

type Server struct {
	URL           string   `json:"url"`
	VariableNames []string `json:"variable_names"`
}

type SecurityRequirement struct {
	Alternative int      `json:"alternative"`
	Scheme      string   `json:"scheme"`
	Scopes      []string `json:"scopes"`
}

type Operation struct {
	Key               model.Hash            `json:"key"`
	Method            string                `json:"method"`
	Path              string                `json:"path"`
	OperationID       string                `json:"operation_id,omitempty"`
	Summary           string                `json:"summary,omitempty"`
	ContractVersion   string                `json:"contract_version"`
	Servers           []Server              `json:"servers"`
	Security          []SecurityRequirement `json:"security"`
	SecurityDeclared  bool                  `json:"security_declared"`
	DeclarationStatus BindingStatus         `json:"declaration_status"`
	SourceRef         model.SourceRef       `json:"source_ref"`
}

type SchemaDeclaration struct {
	Name        string          `json:"name"`
	JSONPointer string          `json:"json_pointer"`
	Declaration json.RawMessage `json:"declaration"`
	SourceRef   model.SourceRef `json:"source_ref"`
}

type Reference struct {
	FromPath    string           `json:"from_path"`
	FromPointer string           `json:"from_pointer"`
	Reference   string           `json:"reference"`
	TargetPath  string           `json:"target_path,omitempty"`
	Target      string           `json:"target_pointer,omitempty"`
	Status      schema.RefStatus `json:"status"`
}

type Result struct {
	FormatVersion  uint16              `json:"format_version"`
	Identity       schema.Identity     `json:"identity"`
	IdentityKey    model.Hash          `json:"identity_key"`
	OpenAPIVersion string              `json:"openapi_version,omitempty"`
	Coverage       schema.Coverage     `json:"coverage"`
	Operations     []Operation         `json:"operations"`
	Schemas        []SchemaDeclaration `json:"schemas"`
	Bindings       []Binding           `json:"bindings"`
	References     []Reference         `json:"references"`
	Diagnostics    []schema.Diagnostic `json:"diagnostics"`
}

type Input struct {
	Identity schema.Identity
	Bundle   schema.Bundle
	Bindings []BindingCandidate
}

type analyzer struct {
	result       *Result
	identityKey  model.Hash
	rootPath     string
	rootRef      model.SourceRef
	docs         map[string]schema.SourceDocument
	values       map[string]any
	refsSeen     map[string]bool
	visited      map[string]bool
	active       map[string]bool
	seenDiag     map[string]bool
	operationIDs map[string]bool
}

func Analyze(input Input) (Result, error) {
	result := Result{
		FormatVersion: 1,
		Identity:      input.Identity,
		Coverage:      schema.CoverageComplete,
		Operations:    []Operation{},
		Schemas:       []SchemaDeclaration{},
		Bindings:      []Binding{},
		References:    []Reference{},
		Diagnostics:   []schema.Diagnostic{},
	}
	if input.Identity.Protocol != schema.ProtocolOpenAPI31 {
		return Result{}, fmt.Errorf("invalid HTTP contract: protocol profile must be %q", schema.ProtocolOpenAPI31)
	}
	documents, err := input.Bundle.Validate(input.Identity)
	if err != nil {
		return Result{}, err
	}
	identityKey, err := input.Identity.Key()
	if err != nil {
		return Result{}, err
	}
	values, err := schema.DecodeJSONBundle(documents)
	if err != nil {
		return Result{}, fmt.Errorf("invalid OpenAPI bundle: %v", err)
	}
	root := values[input.Bundle.RootPath].(map[string]any)
	result.IdentityKey = identityKey
	version, _ := root["openapi"].(string)
	result.OpenAPIVersion = version
	rootRef := documents[input.Bundle.RootPath].Ref
	a := &analyzer{
		result: &result, identityKey: identityKey, rootPath: input.Bundle.RootPath,
		rootRef: rootRef, docs: documents, values: values, refsSeen: make(map[string]bool),
		visited: make(map[string]bool), active: make(map[string]bool), seenDiag: make(map[string]bool),
		operationIDs: make(map[string]bool),
	}
	if version != OpenAPI31 {
		a.addDiagnostic("unsupported_openapi_version", input.Bundle.RootPath, "/openapi", "only OpenAPI 3.1.0 is supported; operations were not interpreted")
	} else {
		identityMatches := true
		if info, ok := root["info"].(map[string]any); !ok {
			a.addDiagnostic("invalid_info", input.Bundle.RootPath, "/info", "OpenAPI info must be an object")
			identityMatches = false
		} else if declaredVersion, ok := info["version"].(string); !ok || declaredVersion != string(input.Identity.Version) {
			a.addDiagnostic("contract_version_mismatch", input.Bundle.RootPath, "/info/version", "contract identity version must match the declared OpenAPI version")
			identityMatches = false
		}
		if identityMatches {
			a.scanReferences(input.Bundle.RootPath, "", root, 0)
			a.extractSchemas(root)
			a.extractOperations(root)
			a.validateSecuritySchemes(root)
		}
	}
	a.attachBindings(input.Bindings)
	if len(result.Diagnostics) != 0 {
		result.Coverage = schema.CoveragePartial
	}
	sort.Slice(result.Operations, func(i, j int) bool {
		if result.Operations[i].Path != result.Operations[j].Path {
			return result.Operations[i].Path < result.Operations[j].Path
		}
		return result.Operations[i].Method < result.Operations[j].Method
	})
	sort.Slice(result.Schemas, func(i, j int) bool { return result.Schemas[i].JSONPointer < result.Schemas[j].JSONPointer })
	sort.Slice(result.Bindings, func(i, j int) bool {
		left, right := result.Bindings[i], result.Bindings[j]
		if left.OperationID != right.OperationID {
			return left.OperationID < right.OperationID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.SourceRef.Path < right.SourceRef.Path
	})
	sort.Slice(result.References, func(i, j int) bool {
		left, right := result.References[i], result.References[j]
		if left.FromPath != right.FromPath {
			return left.FromPath < right.FromPath
		}
		if left.FromPointer != right.FromPointer {
			return left.FromPointer < right.FromPointer
		}
		return left.Reference < right.Reference
	})
	sort.Slice(result.Diagnostics, func(i, j int) bool {
		left, right := result.Diagnostics[i], result.Diagnostics[j]
		if left.DocumentPath != right.DocumentPath {
			return left.DocumentPath < right.DocumentPath
		}
		if left.Pointer != right.Pointer {
			return left.Pointer < right.Pointer
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Detail < right.Detail
	})
	return result, nil
}

func (a *analyzer) extractOperations(root map[string]any) {
	paths, ok := root["paths"].(map[string]any)
	if !ok {
		a.addDiagnostic("invalid_paths", a.rootPath, "/paths", "OpenAPI paths must be an object")
		return
	}
	pathNames := sortedObjectKeys(paths)
	rootServers := a.readServers(root["servers"], "/servers")
	rootSecurity := []SecurityRequirement{}
	rootSecurityDeclared := false
	if rawSecurity, exists := root["security"]; exists {
		rootSecurity, rootSecurityDeclared = a.readSecurity(rawSecurity, "/security")
	}
	for _, routePath := range pathNames {
		if !validRoutePath(routePath) {
			a.addDiagnostic("invalid_route_path", a.rootPath, pointer("/paths", routePath), "route path is not a supported absolute OpenAPI template")
			continue
		}
		pathPointer := pointer("/paths", routePath)
		pathItem, refTarget := a.resolvePathItem(a.rootPath, pathPointer, paths[routePath], 0)
		if pathItem == nil {
			continue
		}
		servers := rootServers
		if rawServers, exists := pathItem["servers"]; exists {
			servers = a.readServers(rawServers, pointer(pathPointer, "servers"))
		}
		pathSecurity, pathSecurityDeclared := rootSecurity, rootSecurityDeclared
		if _, exists := pathItem["security"]; exists {
			a.addDiagnostic("unsupported_path_item_security", a.rootPath, pointer(pathPointer, "security"), "security policy is supported only at root or operation level")
		}
		for _, key := range sortedObjectKeys(pathItem) {
			if strings.HasPrefix(key, "x-") || key == "$ref" || key == "parameters" || key == "servers" || key == "summary" || key == "description" || key == "security" {
				continue
			}
			method := strings.ToUpper(key)
			if key != strings.ToLower(key) || !validMethod(method) {
				a.addDiagnostic("unsupported_path_item_member", a.rootPath, pointer(pathPointer, key), "path item member is outside the supported HTTP operation subset")
				continue
			}
			operationObject, ok := pathItem[key].(map[string]any)
			if !ok {
				a.addDiagnostic("invalid_operation", a.rootPath, pointer(pathPointer, key), "operation declaration must be an object")
				continue
			}
			operationServers := servers
			if rawServers, exists := operationObject["servers"]; exists {
				operationServers = a.readServers(rawServers, pointer(pointer(pathPointer, key), "servers"))
			}
			security, securityDeclared := pathSecurity, pathSecurityDeclared
			if rawSecurity, exists := operationObject["security"]; exists {
				security, securityDeclared = a.readSecurity(rawSecurity, pointer(pointer(pathPointer, key), "security"))
			}
			operationID, _ := operationObject["operationId"].(string)
			if operationID != "" {
				if !safeText(operationID, 256) || a.operationIDs[operationID] {
					a.addDiagnostic("duplicate_or_invalid_operation_id", a.rootPath, pointer(pointer(pathPointer, key), "operationId"), "operationId must be unique and bounded")
				} else {
					a.operationIDs[operationID] = true
				}
			}
			summary, _ := operationObject["summary"].(string)
			if !safeText(summary, 1024) {
				a.addDiagnostic("invalid_operation_summary", a.rootPath, pointer(pointer(pathPointer, key), "summary"), "summary contains invalid or oversized text")
				summary = ""
			}
			operationKey, err := model.HashValue("http-operation-v1", map[string]any{
				"identity_key": a.identityKey,
				"path":         routePath,
				"method":       method,
			})
			if err != nil {
				a.addDiagnostic("invalid_operation_identity", a.rootPath, pointer(pathPointer, key), "operation identity could not be computed")
				continue
			}
			operationSource := a.rootRef
			if targetPath, _, found := strings.Cut(refTarget, "#"); found {
				if targetDocument, exists := a.docs[targetPath]; exists {
					operationSource = targetDocument.Ref
				}
			}
			a.result.Operations = append(a.result.Operations, Operation{
				Key: operationKey, Method: method, Path: routePath, OperationID: operationID,
				Summary: summary, ContractVersion: string(a.result.Identity.Version), Servers: cloneServers(operationServers),
				Security: cloneSecurity(security), SecurityDeclared: securityDeclared,
				DeclarationStatus: BindingDeclared, SourceRef: operationSource,
			})
		}
	}
}

func (a *analyzer) resolvePathItem(documentPath, fromPointer string, value any, depth int) (map[string]any, string) {
	if depth > schema.MaxRefDepth {
		a.addDiagnostic("reference_depth_limit", documentPath, fromPointer, "path-item reference depth exceeds the supported bound")
		return nil, ""
	}
	item, ok := value.(map[string]any)
	if !ok {
		a.addDiagnostic("invalid_path_item", documentPath, fromPointer, "path item must be an object")
		return nil, ""
	}
	ref, exists := item["$ref"]
	if !exists {
		return item, documentPath + "#" + fromPointer
	}
	if len(item) > 1 {
		a.addDiagnostic("unsupported_path_item_ref_siblings", documentPath, fromPointer, "path-item siblings beside $ref are not merged")
	}
	reference, ok := ref.(string)
	if !ok || reference == "" {
		a.addDiagnostic("invalid_reference", documentPath, pointer(fromPointer, "$ref"), "$ref must be a nonempty string")
		return nil, ""
	}
	resolved := schema.ResolveBundleRef(a.values, documentPath, reference)
	if resolved.Status != schema.RefResolved {
		a.addDiagnostic("path_item_reference_"+string(resolved.Status), documentPath, pointer(fromPointer, "$ref"), "path-item reference is unresolved within the supplied bundle")
		return nil, ""
	}
	key := resolved.Path + "#" + resolved.Pointer
	if a.active[key] || depth >= schema.MaxRefDepth {
		a.addDiagnostic("reference_cycle", documentPath, pointer(fromPointer, "$ref"), "path-item reference cycle remains unresolved")
		return nil, key
	}
	a.active[key] = true
	defer delete(a.active, key)
	return a.resolvePathItem(resolved.Path, resolved.Pointer, resolved.Target, depth+1)
}

func (a *analyzer) validateSecuritySchemes(root map[string]any) {
	components, _ := root["components"].(map[string]any)
	declared, _ := components["securitySchemes"].(map[string]any)
	for _, operation := range a.result.Operations {
		for _, requirement := range operation.Security {
			if _, exists := declared[requirement.Scheme]; !exists {
				a.addDiagnostic("unresolved_security_scheme", operation.SourceRef.Path, "", "security requirement names no declared security scheme")
			}
		}
	}
}

func (a *analyzer) extractSchemas(root map[string]any) {
	components, ok := root["components"].(map[string]any)
	if !ok {
		return
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		return
	}
	for _, name := range sortedObjectKeys(schemas) {
		raw, err := json.Marshal(schemas[name])
		if err != nil {
			a.addDiagnostic("invalid_schema_declaration", a.rootPath, pointer("/components/schemas", name), "schema declaration could not be retained")
			continue
		}
		a.result.Schemas = append(a.result.Schemas, SchemaDeclaration{
			Name: name, JSONPointer: pointer("/components/schemas", name), Declaration: raw, SourceRef: a.rootRef,
		})
	}
}

func (a *analyzer) scanReferences(documentPath, fromPointer string, value any, depth int) {
	if depth > schema.MaxRefDepth*2 {
		a.addDiagnostic("reference_scan_depth_limit", documentPath, fromPointer, "nested reference scan exceeds the supported bound")
		return
	}
	nodeID := documentPath + "#" + fromPointer
	if a.active[nodeID] || a.visited[nodeID] {
		return
	}
	a.active[nodeID] = true
	a.visited[nodeID] = true
	defer delete(a.active, nodeID)
	if object, ok := value.(map[string]any); ok {
		if rawRef, exists := object["$ref"]; exists {
			reference, ok := rawRef.(string)
			if !ok || reference == "" {
				a.addDiagnostic("invalid_reference", documentPath, pointer(fromPointer, "$ref"), "$ref must be a nonempty string")
			} else {
				resolved := schema.ResolveBundleRef(a.values, documentPath, reference)
				record := Reference{FromPath: documentPath, FromPointer: pointer(fromPointer, "$ref"), Reference: reference, TargetPath: resolved.Path, Target: resolved.Pointer, Status: resolved.Status}
				targetID := resolved.Path + "#" + resolved.Pointer
				if resolved.Status == schema.RefResolved {
					if a.active[targetID] {
						record.Status = schema.RefCycle
						a.addDiagnostic("reference_cycle", documentPath, record.FromPointer, "reference cycle remains unresolved")
					} else if !a.visited[targetID] {
						a.scanReferences(resolved.Path, resolved.Pointer, resolved.Target, depth+1)
					}
				} else {
					a.addDiagnostic("reference_"+string(resolved.Status), documentPath, record.FromPointer, "reference is unresolved within the supplied allowlisted bundle")
				}
				a.addReference(record)
			}
		}
		keys := sortedObjectKeys(object)
		for _, key := range keys {
			if key == "$ref" || isReferenceDataMember(key) {
				continue
			}
			a.scanReferences(documentPath, pointer(fromPointer, key), object[key], depth+1)
		}
		return
	}
	if values, ok := value.([]any); ok {
		for index, child := range values {
			childPointer := pointer(fromPointer, fmt.Sprint(index))
			a.scanReferences(documentPath, childPointer, child, depth+1)
		}
	}
}

func isReferenceDataMember(key string) bool {
	if strings.HasPrefix(key, "x-") {
		return true
	}
	switch key {
	case "example", "examples", "default", "enum", "const":
		return true
	default:
		return false
	}
}

func (a *analyzer) readServers(value any, at string) []Server {
	items, ok := value.([]any)
	if !ok {
		if value == nil {
			return []Server{}
		}
		a.addDiagnostic("invalid_servers", a.rootPath, at, "servers must be an array")
		return []Server{}
	}
	servers := make([]Server, 0, len(items))
	for index, item := range items {
		server, ok := item.(map[string]any)
		if !ok {
			a.addDiagnostic("invalid_server", a.rootPath, pointer(at, fmt.Sprint(index)), "server declaration must be an object")
			continue
		}
		serverURL, ok := server["url"].(string)
		if !ok || !safeServerURL(serverURL) {
			a.addDiagnostic("unsafe_or_invalid_server", a.rootPath, pointer(pointer(at, fmt.Sprint(index)), "url"), "server URL is invalid or contains credentials/query data and was omitted")
			continue
		}
		variables, ok := serverVariableNames(serverURL)
		if !ok {
			a.addDiagnostic("invalid_server_template", a.rootPath, pointer(pointer(at, fmt.Sprint(index)), "url"), "server URL template contains an invalid variable name")
			continue
		}
		variableObject, hasVariables := server["variables"].(map[string]any)
		validVariables := true
		for _, name := range variables {
			declaration, exists := variableObject[name].(map[string]any)
			defaultValue, hasDefault := declaration["default"].(string)
			if !hasVariables || !exists || !hasDefault || !safeText(defaultValue, 1024) {
				a.addDiagnostic("missing_server_variable", a.rootPath, pointer(pointer(at, fmt.Sprint(index)), "variables"), "each server template variable needs a bounded declared default")
				validVariables = false
			}
		}
		if hasVariables {
			for name := range variableObject {
				if !containsString(variables, name) {
					a.addDiagnostic("unused_server_variable", a.rootPath, pointer(pointer(at, fmt.Sprint(index)), "variables"), "server variable is not present in the URL template")
					validVariables = false
				}
			}
		}
		if !validVariables {
			continue
		}
		servers = append(servers, Server{URL: serverURL, VariableNames: variables})
	}
	return servers
}

func (a *analyzer) readSecurity(value any, at string) ([]SecurityRequirement, bool) {
	items, ok := value.([]any)
	if !ok {
		a.addDiagnostic("invalid_security", a.rootPath, at, "security must be an array")
		return []SecurityRequirement{}, true
	}
	requirements := []SecurityRequirement{}
	for index, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			a.addDiagnostic("invalid_security_requirement", a.rootPath, pointer(at, fmt.Sprint(index)), "security requirement must be an object")
			continue
		}
		for _, scheme := range sortedObjectKeys(object) {
			rawScopes, ok := object[scheme].([]any)
			if !ok || !safeText(scheme, 192) {
				a.addDiagnostic("invalid_security_scopes", a.rootPath, pointer(pointer(at, fmt.Sprint(index)), scheme), "security scheme name/scopes are invalid")
				continue
			}
			scopes := make([]string, 0, len(rawScopes))
			for _, rawScope := range rawScopes {
				scopeName, ok := rawScope.(string)
				if !ok || !safeText(scopeName, 256) {
					a.addDiagnostic("invalid_security_scope", a.rootPath, pointer(pointer(at, fmt.Sprint(index)), scheme), "security scope must be bounded safe text")
					continue
				}
				scopes = append(scopes, scopeName)
			}
			sort.Strings(scopes)
			requirements = append(requirements, SecurityRequirement{Alternative: index, Scheme: scheme, Scopes: scopes})
		}
	}
	return requirements, true
}

func (a *analyzer) attachBindings(candidates []BindingCandidate) {
	if len(candidates) > schema.MaxFacts {
		a.addDiagnostic("binding_candidate_limit", a.rootPath, "", "binding candidate count exceeds the supported bound")
		return
	}
	operations := make(map[string]Operation, len(a.result.Operations))
	for _, operation := range a.result.Operations {
		operations[operation.Method+"\x00"+operation.Path] = operation
	}
	for _, candidate := range candidates {
		method := strings.ToUpper(candidate.Method)
		operation, exists := operations[method+"\x00"+candidate.Path]
		if !exists || (candidate.Kind != BindingHandler && candidate.Kind != BindingClient) {
			a.addDiagnostic("binding_candidate_unmatched", candidate.SourceRef.Path, "", "binding candidate must name a declared operation and handler/client kind")
			continue
		}
		if len(candidate.SourceBytes) == 0 || len(candidate.SourceBytes) > schema.MaxDocumentBytes {
			a.addDiagnostic("binding_candidate_invalid_source", candidate.SourceRef.Path, "", "binding source capture is empty or exceeds the source byte bound")
			continue
		}
		if err := candidate.SourceRef.ValidateAgainst(candidate.SourceBytes); err != nil {
			a.addDiagnostic("binding_candidate_invalid_source", candidate.SourceRef.Path, "", "binding source reference does not match captured source bytes")
			continue
		}
		if candidate.SourceRef.ProfileID != a.result.Identity.ProfileID ||
			candidate.SourceRef.WorktreeID != a.rootRef.WorktreeID ||
			candidate.SourceRef.GenerationID != a.rootRef.GenerationID {
			a.addDiagnostic("binding_candidate_cross_scope", candidate.SourceRef.Path, "", "binding evidence crosses worktree, generation, or profile scope")
			continue
		}
		if candidate.SourceRef.Path == a.rootRef.Path && candidate.SourceRef.ContentHash == a.rootRef.ContentHash {
			a.addDiagnostic("binding_candidate_is_contract_source", candidate.SourceRef.Path, "", "contract declaration cannot serve as its own handler/client source evidence")
			continue
		}
		a.result.Bindings = append(a.result.Bindings, Binding{
			OperationID: operation.Key, Kind: candidate.Kind,
			Status: BindingSourceLinkedCandidate, SourceRef: candidate.SourceRef,
		})
	}
}

func (a *analyzer) addReference(reference Reference) {
	key := reference.FromPath + "\x00" + reference.FromPointer + "\x00" + reference.Reference
	if a.refsSeen[key] || len(a.result.References) >= schema.MaxFacts {
		return
	}
	a.refsSeen[key] = true
	a.result.References = append(a.result.References, reference)
}

func (a *analyzer) addDiagnostic(code, documentPath, at, detail string) {
	key := strings.Join([]string{code, documentPath, at, detail}, "\x00")
	if a.seenDiag[key] || len(a.result.Diagnostics) >= schema.MaxFacts {
		return
	}
	a.seenDiag[key] = true
	a.result.Diagnostics = append(a.result.Diagnostics, schema.Diagnostic{Code: code, DocumentPath: documentPath, Pointer: at, Detail: detail})
}

func safeServerURL(value string) bool {
	if !safeText(value, 2048) || strings.ContainsAny(value, "?#") {
		return false
	}
	for strings.Contains(value, "{") {
		start := strings.IndexByte(value, '{')
		endRelative := strings.IndexByte(value[start:], '}')
		if endRelative < 0 {
			return false
		}
		end := start + endRelative
		if start+1 == end || !safeText(value[start+1:end], 192) {
			return false
		}
		value = value[:start] + "placeholder" + value[end+1:]
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return true
}

func validRoutePath(value string) bool {
	if !strings.HasPrefix(value, "/") || !safeText(value, 2048) || strings.ContainsAny(value, "?#") {
		return false
	}
	depth := 0
	for _, char := range value {
		switch char {
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 || depth > 1 {
				return false
			}
		}
	}
	return depth == 0
}

func validMethod(value string) bool {
	switch value {
	case "GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH", "TRACE":
		return true
	default:
		return false
	}
}

func safeText(value string, max int) bool {
	return len(value) <= max && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func serverVariableNames(value string) ([]string, bool) {
	result := []string{}
	for {
		start := strings.IndexByte(value, '{')
		if start < 0 {
			if strings.Contains(value, "}") {
				return nil, false
			}
			break
		}
		endRelative := strings.IndexByte(value[start:], '}')
		if endRelative < 0 {
			return nil, false
		}
		end := start + endRelative
		name := value[start+1 : end]
		if !safeVariableName(name) || containsString(result, name) {
			return nil, false
		}
		result = append(result, name)
		value = value[:start] + value[end+1:]
	}
	sort.Strings(result)
	return result, true
}

func safeVariableName(value string) bool {
	if value == "" || len(value) > 192 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func pointer(base, token string) string {
	token = strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
	if base == "" {
		return "/" + token
	}
	if base == "/" {
		return "/" + token
	}
	return base + "/" + token
}

func sortedObjectKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneServers(value []Server) []Server {
	clone := append([]Server(nil), value...)
	for index := range clone {
		clone[index].VariableNames = append([]string(nil), clone[index].VariableNames...)
	}
	if clone == nil {
		return []Server{}
	}
	return clone
}

func cloneSecurity(value []SecurityRequirement) []SecurityRequirement {
	clone := append([]SecurityRequirement(nil), value...)
	for index := range clone {
		clone[index].Scopes = append([]string(nil), clone[index].Scopes...)
	}
	if clone == nil {
		return []SecurityRequirement{}
	}
	return clone
}
