package contracts

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"github.com/google/jsonschema-go/jsonschema"
)

//go:embed mce4d.v1.schema.json operations.v1.json
var contractFiles embed.FS

var (
	ErrUnknownOperation  = errors.New("unknown contract operation")
	ErrUnknownDefinition = errors.New("unknown contract definition")
	ErrContractViolation = errors.New("contract validation failed")
)

type Operation struct {
	RequestType string
	DataType    string
}

type Validator struct {
	schemaRoot map[string]any
	operations map[string]Operation
	resolved   map[string]*jsonschema.Resolved
	mu         sync.Mutex
}

func NewValidator() (*Validator, error) {
	schemaBytes, err := fs.ReadFile(contractFiles, "mce4d.v1.schema.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded contract schema: %w", err)
	}
	registryBytes, err := fs.ReadFile(contractFiles, "operations.v1.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded operation registry: %w", err)
	}
	schemaValue, err := DecodeJSON(schemaBytes)
	if err != nil {
		return nil, fmt.Errorf("parse embedded contract schema: %w", err)
	}
	schemaRoot, ok := schemaValue.(map[string]any)
	if !ok || schemaRoot["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		return nil, fmt.Errorf("%w: schema must declare Draft 2020-12", ErrContractViolation)
	}
	definitions, ok := schemaRoot["$defs"].(map[string]any)
	if !ok || len(definitions) == 0 {
		return nil, fmt.Errorf("%w: schema has no definitions", ErrContractViolation)
	}
	registryValue, err := DecodeJSON(registryBytes)
	if err != nil {
		return nil, fmt.Errorf("parse embedded operation registry: %w", err)
	}
	registry, ok := registryValue.(map[string]any)
	if !ok || len(registry) == 0 {
		return nil, fmt.Errorf("%w: operation registry is empty", ErrContractViolation)
	}
	operations := make(map[string]Operation, len(registry))
	for name, value := range registry {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: empty operation name", ErrContractViolation)
		}
		pair, ok := value.([]any)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("%w: operation %q must bind one request and one data schema", ErrContractViolation, name)
		}
		requestType, requestOK := pair[0].(string)
		dataType, dataOK := pair[1].(string)
		if !requestOK || !dataOK || definitions[requestType] == nil || definitions[dataType] == nil {
			return nil, fmt.Errorf("%w: operation %q references an unknown schema", ErrContractViolation, name)
		}
		operations[name] = Operation{RequestType: requestType, DataType: dataType}
	}
	return &Validator{
		schemaRoot: schemaRoot,
		operations: operations,
		resolved:   make(map[string]*jsonschema.Resolved),
	}, nil
}

func (v *Validator) Operations() map[string]Operation {
	copyOf := make(map[string]Operation, len(v.operations))
	for name, operation := range v.operations {
		copyOf[name] = operation
	}
	return copyOf
}

func (v *Validator) ValidateRequest(operation string, raw []byte) error {
	registered, ok := v.operations[operation]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownOperation, operation)
	}
	instance, err := DecodeJSON(raw)
	if err != nil {
		return err
	}
	if err := v.validateDefinition(registered.RequestType, instance); err != nil {
		return err
	}
	return validateNestedSemantics(instance)
}

func (v *Validator) ValidateResponse(operation string, raw []byte) error {
	registered, ok := v.operations[operation]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownOperation, operation)
	}
	instance, err := DecodeJSON(raw)
	if err != nil {
		return err
	}
	if err := v.validateDefinition("Response", instance); err != nil {
		return err
	}
	response, ok := instance.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: response must be an object", ErrContractViolation)
	}
	status, _ := response["status"].(string)
	readiness, _ := response["readiness"].(string)
	responseError := response["error"]
	data := response["data"]
	if status == "error" || status == "blocked" {
		if responseError == nil || readiness != "blocked" || data != nil {
			return fmt.Errorf("%w: blocked/error response requires error, blocked readiness, and null data", ErrContractViolation)
		}
	} else if responseError != nil {
		return fmt.Errorf("%w: successful/partial response cannot contain error", ErrContractViolation)
	}
	if data != nil {
		if err := v.validateDefinition(registered.DataType, data); err != nil {
			return fmt.Errorf("operation %q response data: %w", operation, err)
		}
	}
	return validateNestedSemantics(instance)
}

// ValidateCaptureManifest validates a bounded contract artifact. Large on-disk
// manifests must use the streaming verifier owned by the storage pipeline.
func (v *Validator) ValidateCaptureManifest(raw []byte) (model.CaptureManifest, error) {
	var manifest model.CaptureManifest
	instance, err := DecodeJSON(raw)
	if err != nil {
		return manifest, err
	}
	if err := v.validateDefinition("CaptureManifest", instance); err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return model.CaptureManifest{}, fmt.Errorf("%w: decode capture manifest: %v", ErrContractViolation, err)
	}
	if err := manifest.Validate(); err != nil {
		return model.CaptureManifest{}, fmt.Errorf("%w: capture manifest semantics: %v", ErrContractViolation, err)
	}
	var canonical bytes.Buffer
	if err := model.WriteCaptureManifestJSON1(&canonical, manifest); err != nil {
		return model.CaptureManifest{}, fmt.Errorf("%w: encode canonical capture manifest: %v", ErrContractViolation, err)
	}
	if !bytes.Equal(canonical.Bytes(), raw) {
		return model.CaptureManifest{}, fmt.Errorf("%w: capture manifest is not canonical MCE-MANIFEST-JSON1", ErrContractViolation)
	}
	return manifest, nil
}

// ValidatePrivateRecord validates only storage definitions that are explicitly private in S43.
func (v *Validator) ValidatePrivateRecord(name string, raw []byte) error {
	limit := MaxJSONBytes
	switch name {
	case "VolumeAdmissionJournal":
		limit = 1 << 20
	case "StoredCaptureManifestRef":
		limit = 4096
	case "VolumeAdmissionEntry", "AdmissionOwnedPath":
	default:
		return fmt.Errorf("%w: %q is not a private storage contract", ErrUnknownDefinition, name)
	}
	if len(raw) > limit {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrContractViolation, name, limit)
	}
	instance, err := DecodeJSON(raw)
	if err != nil {
		return err
	}
	if err := v.validateDefinition(name, instance); err != nil {
		return err
	}
	switch name {
	case "VolumeAdmissionJournal":
		var journal model.VolumeAdmissionJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			return fmt.Errorf("%w: decode admission journal: %v", ErrContractViolation, err)
		}
		if err := journal.Validate(); err != nil {
			return fmt.Errorf("%w: admission journal semantics: %v", ErrContractViolation, err)
		}
	case "VolumeAdmissionEntry":
		var entry model.VolumeAdmissionEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("%w: decode admission entry: %v", ErrContractViolation, err)
		}
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("%w: admission entry semantics: %v", ErrContractViolation, err)
		}
	case "AdmissionOwnedPath":
		value, ok := instance.(string)
		if !ok {
			return fmt.Errorf("%w: owned path must be a string", ErrContractViolation)
		}
		if err := model.AdmissionOwnedPath(value).Validate(); err != nil {
			return fmt.Errorf("%w: owned path semantics: %v", ErrContractViolation, err)
		}
	case "StoredCaptureManifestRef":
		var ref model.StoredCaptureManifestRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return fmt.Errorf("%w: decode manifest reference: %v", ErrContractViolation, err)
		}
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%w: manifest reference semantics: %v", ErrContractViolation, err)
		}
	}
	return nil
}

func (v *Validator) validateDefinition(name string, value any) error {
	resolved, err := v.resolvedDefinition(name)
	if err != nil {
		return err
	}
	schemaValue, err := numericSchemaValue(value)
	if err != nil {
		return err
	}
	if err := resolved.Validate(schemaValue); err != nil {
		return fmt.Errorf("%w: schema %s: %v", ErrContractViolation, name, err)
	}
	return nil
}

func numericSchemaValue(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		lexeme := string(typed)
		if !strings.ContainsAny(lexeme, ".eE") {
			integer, err := strconv.ParseInt(lexeme, 10, 64)
			if err != nil || integer > model.MaxSafeInteger || integer < -model.MaxSafeInteger {
				return nil, fmt.Errorf("%w: integer outside safe JSON range", ErrInvalidJSON)
			}
			return integer, nil
		}
		number, err := strconv.ParseFloat(lexeme, 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, fmt.Errorf("%w: non-finite or unrepresentable JSON number", ErrInvalidJSON)
		}
		return number, nil
	case []any:
		converted := make([]any, len(typed))
		for index, item := range typed {
			value, err := numericSchemaValue(item)
			if err != nil {
				return nil, err
			}
			converted[index] = value
		}
		return converted, nil
	case map[string]any:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			value, err := numericSchemaValue(item)
			if err != nil {
				return nil, err
			}
			converted[key] = value
		}
		return converted, nil
	default:
		return value, nil
	}
}

func (v *Validator) resolvedDefinition(name string) (*jsonschema.Resolved, error) {
	if _, exists := v.schemaRoot["$defs"].(map[string]any)[name]; !exists {
		return nil, fmt.Errorf("%w: %q", ErrUnknownDefinition, name)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if resolved, ok := v.resolved[name]; ok {
		return resolved, nil
	}
	root := map[string]any{
		"$schema": v.schemaRoot["$schema"],
		"$id":     v.schemaRoot["$id"],
		"$defs":   v.schemaRoot["$defs"],
		"$ref":    "#/$defs/" + name,
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode validator root: %w", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return nil, fmt.Errorf("decode Draft 2020-12 schema: %w", err)
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		return nil, fmt.Errorf("resolve contract schema %s: %w", name, err)
	}
	v.resolved[name] = resolved
	return resolved, nil
}

func validateNestedSemantics(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if err := validateNestedSemantics(child); err != nil {
				return err
			}
		}
	case map[string]any:
		if scope, exists := typed["scope"]; exists {
			if requestScope, ok := scope.(map[string]any); ok {
				if err := validateScopeRequest(requestScope); err != nil {
					return err
				}
			}
		}
		if _, hasVector := typed["vector_digest"]; hasVector {
			if _, hasMembers := typed["members"]; hasMembers {
				encoded, err := json.Marshal(typed)
				if err != nil {
					return fmt.Errorf("%w: encode scope vector: %v", ErrContractViolation, err)
				}
				var vector model.ScopeVector
				if err := json.Unmarshal(encoded, &vector); err != nil {
					return fmt.Errorf("%w: decode scope vector: %v", ErrContractViolation, err)
				}
				if err := vector.Validate(); err != nil {
					return fmt.Errorf("%w: scope vector semantics: %v", ErrContractViolation, err)
				}
			}
		}
		for _, child := range typed {
			if err := validateNestedSemantics(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateScopeRequest(scope map[string]any) error {
	_, worktree := scope["worktree_id"]
	_, profile := scope["profile_id"]
	_, generation := scope["generation_id"]
	_, group := scope["group_id"]
	_, pinned := scope["pinned_vector"]
	single := worktree || profile || generation
	modeCount := 0
	if single {
		modeCount++
	}
	if group {
		modeCount++
	}
	if pinned {
		modeCount++
	}
	if modeCount == 0 {
		return nil // Empty scope is resolved only against an explicit server binding.
	}
	if modeCount != 1 || (single && (!worktree || !profile)) || (generation && !worktree) {
		return fmt.Errorf("%w: scope selector must be one complete single, group, or pinned-vector selector", ErrContractViolation)
	}
	return nil
}
