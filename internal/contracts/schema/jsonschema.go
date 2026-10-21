package schemacontract

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

type Coverage string

const (
	CoverageComplete Coverage = "complete_supported_subset"
	CoveragePartial  Coverage = "partial_with_gaps"
)

type Diagnostic struct {
	Code         string `json:"code"`
	DocumentPath string `json:"document_path"`
	Pointer      string `json:"pointer"`
	Detail       string `json:"detail"`
}

type Reference struct {
	FromPath    string    `json:"from_path"`
	FromPointer string    `json:"from_pointer"`
	Reference   string    `json:"reference"`
	TargetPath  string    `json:"target_path,omitempty"`
	Target      string    `json:"target_pointer,omitempty"`
	Status      RefStatus `json:"status"`
}

type Field struct {
	ID           model.Hash      `json:"id"`
	IdentityKey  model.Hash      `json:"identity_key"`
	DocumentPath string          `json:"document_path"`
	JSONPointer  string          `json:"json_pointer"`
	Name         string          `json:"name"`
	Types        []string        `json:"types"`
	Required     bool            `json:"required"`
	Nullable     bool            `json:"nullable"`
	Units        json.RawMessage `json:"units,omitempty"`
	Precision    json.RawMessage `json:"precision,omitempty"`
	Declaration  json.RawMessage `json:"declaration"`
	SourceRef    model.SourceRef `json:"source_ref"`
}

type JSONSchemaResult struct {
	FormatVersion uint16       `json:"format_version"`
	Identity      Identity     `json:"identity"`
	IdentityKey   model.Hash   `json:"identity_key"`
	Dialect       string       `json:"dialect,omitempty"`
	Coverage      Coverage     `json:"coverage"`
	Fields        []Field      `json:"fields"`
	References    []Reference  `json:"references"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
}

type schemaWalker struct {
	result          *JSONSchemaResult
	identity        model.Hash
	rootPath        string
	docs            map[string]SourceDocument
	values          map[string]any
	visited         map[string]bool
	active          map[string]bool
	seenDiag        map[string]bool
	seenRefs        map[string]bool
	unsupportedBase map[string]bool
}

var supportedSchemaKeywords = map[string]bool{
	"$schema": true, "$id": true, "$anchor": true, "$defs": true, "$ref": true,
	"title": true, "description": true, "type": true, "properties": true,
	"required": true, "items": true, "prefixItems": true, "additionalProperties": true,
	"unevaluatedProperties": true, "allOf": true, "anyOf": true, "oneOf": true,
	"not": true, "if": true, "then": true, "else": true, "contains": true,
	"propertyNames": true, "enum": true, "const": true, "default": true,
	"examples": true, "format": true, "deprecated": true, "readOnly": true,
	"writeOnly": true, "x-units": true, "x-precision": true,
}

func AnalyzeJSONSchema(identity Identity, bundle Bundle) (JSONSchemaResult, error) {
	result := JSONSchemaResult{
		FormatVersion: 1,
		Identity:      identity,
		Coverage:      CoverageComplete,
		Fields:        []Field{},
		References:    []Reference{},
		Diagnostics:   []Diagnostic{},
	}
	if identity.Protocol != ProtocolJSONSchema202012 {
		return JSONSchemaResult{}, fmt.Errorf("%w: JSON Schema analyzer requires protocol %q", ErrInvalidContract, ProtocolJSONSchema202012)
	}
	docs, err := bundle.Validate(identity)
	if err != nil {
		return JSONSchemaResult{}, err
	}
	identityKey, err := identity.Key()
	if err != nil {
		return JSONSchemaResult{}, err
	}
	result.IdentityKey = identityKey
	walker := &schemaWalker{
		result:          &result,
		identity:        identityKey,
		rootPath:        bundle.RootPath,
		docs:            docs,
		values:          make(map[string]any, len(docs)),
		visited:         make(map[string]bool),
		active:          make(map[string]bool),
		seenDiag:        make(map[string]bool),
		seenRefs:        make(map[string]bool),
		unsupportedBase: make(map[string]bool),
	}
	values, err := DecodeJSONBundle(docs)
	if err != nil {
		return JSONSchemaResult{}, err
	}
	walker.values = values
	for pathName, value := range values {
		if containsNonemptyID(value) {
			walker.unsupportedBase[pathName] = true
			walker.addDiagnostic("unsupported_base_uri", pathName, "", "$id base URI changes are outside the source-relative reference subset")
		}
	}
	root := walker.values[bundle.RootPath].(map[string]any)
	if root["$schema"] != JSONSchema202012 {
		walker.addDiagnostic("unsupported_dialect", bundle.RootPath, "", "only JSON Schema Draft 2020-12 is supported")
	} else {
		result.Dialect = JSONSchema202012
	}
	walker.walk(bundle.RootPath, "", root, 0)
	if len(result.Diagnostics) != 0 {
		result.Coverage = CoveragePartial
	}
	sort.Slice(result.Fields, func(i, j int) bool {
		left, right := result.Fields[i], result.Fields[j]
		if left.DocumentPath != right.DocumentPath {
			return left.DocumentPath < right.DocumentPath
		}
		return left.JSONPointer < right.JSONPointer
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

func (walker *schemaWalker) walk(documentPath, pointer string, node any, depth int) {
	if depth > MaxRefDepth {
		walker.addDiagnostic("reference_depth_limit", documentPath, pointer, "reference expansion depth exceeds the supported bound")
		return
	}
	nodeID := documentPath + "#" + pointer
	if walker.active[nodeID] {
		walker.addDiagnostic("reference_cycle", documentPath, pointer, "reference cycle remains unresolved")
		return
	}
	if walker.visited[nodeID] {
		return
	}
	object, ok := node.(map[string]any)
	if !ok {
		walker.addDiagnostic("invalid_schema_node", documentPath, pointer, "supported schema nodes must be JSON objects")
		return
	}
	walker.visited[nodeID] = true
	walker.active[nodeID] = true
	defer delete(walker.active, nodeID)

	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
		if !supportedSchemaKeywords[key] {
			walker.addDiagnostic("unsupported_keyword", documentPath, joinPointer(pointer, key), "keyword is retained in the declaration but not interpreted")
		}
	}
	sort.Strings(keys)
	if rawRef, exists := object["$ref"]; exists {
		reference, ok := rawRef.(string)
		if !ok || reference == "" {
			walker.addDiagnostic("invalid_reference", documentPath, joinPointer(pointer, "$ref"), "$ref must be a nonempty string")
		} else if walker.unsupportedBase[documentPath] {
			record := Reference{FromPath: documentPath, FromPointer: joinPointer(pointer, "$ref"), Reference: reference, Status: RefUnsupported}
			walker.addReference(record)
			walker.addDiagnostic("unsupported_reference_base", documentPath, record.FromPointer, "$id base URI was not resolved")
		} else {
			resolved := ResolveBundleRef(walker.values, documentPath, reference)
			record := Reference{FromPath: documentPath, FromPointer: joinPointer(pointer, "$ref"), Reference: reference, TargetPath: resolved.Path, Target: resolved.Pointer, Status: resolved.Status}
			if resolved.Status == RefResolved {
				if resolved.Path != walker.rootPath && !walker.supportedExternalDocument(resolved.Path, record.FromPointer) {
					record.Status = RefUnsupported
				} else {
					targetID := resolved.Path + "#" + resolved.Pointer
					if walker.active[targetID] {
						record.Status = RefCycle
						walker.addDiagnostic("reference_cycle", documentPath, record.FromPointer, "reference cycle remains unresolved")
					} else {
						walker.walk(resolved.Path, resolved.Pointer, resolved.Target, depth+1)
					}
				}
			} else {
				walker.addDiagnostic(refDiagnosticCode(resolved.Status), documentPath, record.FromPointer, "reference is not resolved outside the supplied allowlisted bundle")
			}
			walker.addReference(record)
		}
	}

	if properties, exists := object["properties"]; exists {
		propertyMap, ok := properties.(map[string]any)
		if !ok {
			walker.addDiagnostic("invalid_properties", documentPath, joinPointer(pointer, "properties"), "properties must be an object")
		} else {
			required := requiredNames(object["required"], walker, documentPath, pointer)
			propertyNames := make([]string, 0, len(propertyMap))
			for name := range propertyMap {
				propertyNames = append(propertyNames, name)
			}
			sort.Strings(propertyNames)
			for _, name := range propertyNames {
				propertyPointer := joinPointer(joinPointer(pointer, "properties"), name)
				walker.addField(documentPath, propertyPointer, name, propertyMap[name], required[name])
				walker.walk(documentPath, propertyPointer, propertyMap[name], depth)
			}
		}
	}

	for _, key := range keys {
		if key == "$ref" || key == "properties" || key == "required" {
			continue
		}
		child := object[key]
		switch key {
		case "$defs", "definitions", "patternProperties":
			if childMap, ok := child.(map[string]any); ok {
				childKeys := sortedKeys(childMap)
				for _, childKey := range childKeys {
					walker.walk(documentPath, joinPointer(joinPointer(pointer, key), childKey), childMap[childKey], depth)
				}
			} else {
				walker.addDiagnostic("invalid_schema_container", documentPath, joinPointer(pointer, key), "schema container must be an object")
			}
		case "items", "additionalProperties", "unevaluatedProperties", "not", "if", "then", "else", "contains", "propertyNames":
			if _, ok := child.(map[string]any); ok {
				walker.walk(documentPath, joinPointer(pointer, key), child, depth)
			} else if _, ok := child.([]any); ok && key == "items" {
				walker.addDiagnostic("unsupported_keyword_shape", documentPath, joinPointer(pointer, key), "array-form items is not supported by this draft profile")
			}
		case "prefixItems", "allOf", "anyOf", "oneOf":
			items, ok := child.([]any)
			if !ok {
				walker.addDiagnostic("invalid_schema_container", documentPath, joinPointer(pointer, key), "schema composition must be an array")
				continue
			}
			for index, item := range items {
				walker.walk(documentPath, joinPointer(joinPointer(pointer, key), strconv.Itoa(index)), item, depth)
			}
		}
	}
}

func (walker *schemaWalker) supportedExternalDocument(documentPath, fromPointer string) bool {
	root, ok := walker.values[documentPath].(map[string]any)
	if ok && root["$schema"] == JSONSchema202012 {
		return true
	}
	walker.addDiagnostic("unsupported_external_dialect", documentPath, fromPointer, "external schema document must explicitly declare Draft 2020-12")
	return false
}

func (walker *schemaWalker) addField(documentPath, pointer, name string, declaration any, required bool) {
	if len(walker.result.Fields) >= MaxFacts {
		walker.addDiagnostic("fact_limit", documentPath, pointer, "schema field count exceeds the supported bound")
		return
	}
	object, ok := declaration.(map[string]any)
	if !ok {
		walker.addDiagnostic("invalid_property_schema", documentPath, pointer, "property declaration must be a schema object")
		return
	}
	types := []string{}
	nullable := false
	switch value := object["type"].(type) {
	case string:
		types = append(types, value)
		nullable = value == "null"
	case []any:
		seen := make(map[string]bool, len(value))
		for _, item := range value {
			name, ok := item.(string)
			if !ok || seen[name] {
				walker.addDiagnostic("invalid_type", documentPath, joinPointer(pointer, "type"), "type array must contain unique strings")
				continue
			}
			seen[name] = true
			types = append(types, name)
			nullable = nullable || name == "null"
		}
	case nil:
	default:
		walker.addDiagnostic("invalid_type", documentPath, joinPointer(pointer, "type"), "type must be a string or a string array")
	}
	if object["type"] != nil && len(types) == 0 {
		walker.addDiagnostic("invalid_type", documentPath, joinPointer(pointer, "type"), "type declaration must not be empty")
	}
	for _, typeName := range types {
		if !oneOf(typeName, "null", "boolean", "object", "array", "number", "integer", "string") {
			walker.addDiagnostic("unsupported_type", documentPath, joinPointer(pointer, "type"), "type value is outside the JSON Schema vocabulary")
		}
	}
	raw, err := json.Marshal(declaration)
	if err != nil {
		walker.addDiagnostic("invalid_property_schema", documentPath, pointer, "property declaration cannot be retained as JSON")
		return
	}
	fieldID, err := model.HashValue("json-schema-field-v1", map[string]any{
		"identity_key": walker.identity,
		"document":     documentPath,
		"pointer":      pointer,
	})
	if err != nil {
		walker.addDiagnostic("invalid_identity", documentPath, pointer, "field identity could not be computed")
		return
	}
	document := walker.docs[documentPath]
	units := rawMember(object, "x-units")
	if value, exists := object["x-units"]; exists {
		unitName, ok := value.(string)
		if !ok || !validUnitName(unitName) {
			walker.addDiagnostic("invalid_units", documentPath, joinPointer(pointer, "x-units"), "units must use a bounded sanitized identifier")
			units = nil
		}
	}
	precision := rawMember(object, "x-precision")
	if value, exists := object["x-precision"]; exists {
		number, ok := value.(json.Number)
		parsed, parseErr := strconv.ParseFloat(string(number), 64)
		if !ok || parseErr != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
			walker.addDiagnostic("invalid_precision", documentPath, joinPointer(pointer, "x-precision"), "precision must be a finite nonnegative JSON number")
			precision = nil
		}
	}
	walker.result.Fields = append(walker.result.Fields, Field{
		ID:           fieldID,
		IdentityKey:  walker.identity,
		DocumentPath: documentPath,
		JSONPointer:  pointer,
		Name:         name,
		Types:        types,
		Required:     required,
		Nullable:     nullable,
		Units:        units,
		Precision:    precision,
		Declaration:  raw,
		SourceRef:    document.Ref,
	})
}

func containsNonemptyID(value any) bool {
	switch item := value.(type) {
	case map[string]any:
		if id, ok := item["$id"].(string); ok && id != "" {
			return true
		}
		for _, child := range item {
			if containsNonemptyID(child) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if containsNonemptyID(child) {
				return true
			}
		}
	}
	return false
}

func validUnitName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' || char == '/' {
			continue
		}
		return false
	}
	return true
}

func (walker *schemaWalker) addDiagnostic(code, documentPath, pointer, detail string) {
	key := strings.Join([]string{code, documentPath, pointer, detail}, "\x00")
	if walker.seenDiag[key] || len(walker.result.Diagnostics) >= MaxFacts {
		return
	}
	walker.seenDiag[key] = true
	walker.result.Diagnostics = append(walker.result.Diagnostics, Diagnostic{Code: code, DocumentPath: documentPath, Pointer: pointer, Detail: detail})
}

func (walker *schemaWalker) addReference(reference Reference) {
	key := strings.Join([]string{reference.FromPath, reference.FromPointer, reference.Reference}, "\x00")
	if walker.seenRefs[key] || len(walker.result.References) >= MaxFacts {
		return
	}
	walker.seenRefs[key] = true
	walker.result.References = append(walker.result.References, reference)
}

func requiredNames(value any, walker *schemaWalker, documentPath, pointer string) map[string]bool {
	result := make(map[string]bool)
	if value == nil {
		return result
	}
	items, ok := value.([]any)
	if !ok {
		walker.addDiagnostic("invalid_required", documentPath, joinPointer(pointer, "required"), "required must be an array of strings")
		return result
	}
	for _, item := range items {
		name, ok := item.(string)
		if !ok || name == "" || result[name] {
			walker.addDiagnostic("invalid_required", documentPath, joinPointer(pointer, "required"), "required entries must be unique nonempty strings")
			continue
		}
		result[name] = true
	}
	return result
}

func rawMember(object map[string]any, name string) json.RawMessage {
	value, exists := object[name]
	if !exists {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func joinPointer(pointer, token string) string {
	token = strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
	return pointer + "/" + token
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func refDiagnosticCode(status RefStatus) string {
	switch status {
	case RefRemote:
		return "remote_reference_unresolved"
	case RefTraversal:
		return "unsafe_reference_unresolved"
	case RefMissing:
		return "local_reference_unresolved"
	default:
		return "unsupported_reference_unresolved"
	}
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
