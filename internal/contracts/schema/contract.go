// Package schemacontract parses a deliberately bounded, source-backed subset
// of JSON Schema and Protocol Buffers declarations. It performs no filesystem,
// network, process, or database access.
package schemacontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	MaxDocumentBytes = 2 << 20
	MaxBundleBytes   = 8 << 20
	MaxBundleFiles   = 128
	MaxJSONNodes     = 100_000
	MaxRefDepth      = 32
	MaxFacts         = 100_000
	JSONSchema202012 = "https://json-schema.org/draft/2020-12/schema"
)

var ErrInvalidContract = errors.New("invalid schema contract")

type Protocol string

const (
	ProtocolJSONSchema202012 Protocol = "json_schema_2020-12"
	ProtocolProto3           Protocol = "proto3"
	ProtocolOpenAPI31        Protocol = "openapi_3.1.0"
	ProtocolAsyncAPI31       Protocol = "asyncapi_3.1.0"
)

// Identity is a sanitized, immutable contract scope. ServerBinding is an
// opaque configuration identity, never a URL or credential-bearing value.
type Identity struct {
	RepositoryScope model.Identifier `json:"repository_scope"`
	Environment     model.Identifier `json:"environment"`
	ProfileID       model.Hash       `json:"profile_id"`
	Protocol        Protocol         `json:"protocol"`
	Namespace       model.Identifier `json:"namespace"`
	ServerBinding   model.Identifier `json:"server_binding,omitempty"`
	ContractID      model.Identifier `json:"contract_id"`
	Version         model.Identifier `json:"version"`
	SourceHash      model.Hash       `json:"source_hash"`
}

func (identity Identity) Validate() error {
	for _, item := range []struct {
		name  string
		value model.Identifier
	}{
		{"repository_scope", identity.RepositoryScope},
		{"environment", identity.Environment},
		{"namespace", identity.Namespace},
		{"contract_id", identity.ContractID},
		{"version", identity.Version},
	} {
		if !validLabel(string(item.value)) {
			return fmt.Errorf("%w: %s is not a bounded, sanitized identifier", ErrInvalidContract, item.name)
		}
	}
	if identity.ServerBinding != "" && !validLabel(string(identity.ServerBinding)) {
		return fmt.Errorf("%w: server_binding must be an opaque sanitized identifier", ErrInvalidContract)
	}
	if _, err := model.ParseHash(string(identity.ProfileID)); err != nil {
		return fmt.Errorf("%w: profile_id: %v", ErrInvalidContract, err)
	}
	if _, err := model.ParseHash(string(identity.SourceHash)); err != nil {
		return fmt.Errorf("%w: source_hash: %v", ErrInvalidContract, err)
	}
	switch identity.Protocol {
	case ProtocolJSONSchema202012, ProtocolProto3, ProtocolOpenAPI31, ProtocolAsyncAPI31:
	default:
		return fmt.Errorf("%w: unsupported protocol profile", ErrInvalidContract)
	}
	if (identity.Protocol == ProtocolOpenAPI31 || identity.Protocol == ProtocolAsyncAPI31) && identity.ServerBinding == "" {
		return fmt.Errorf("%w: HTTP and event identities require an opaque server binding", ErrInvalidContract)
	}
	return nil
}

func (identity Identity) Key() (model.Hash, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	return model.HashValue("contract-identity-v1", map[string]any{
		"repository_scope": identity.RepositoryScope,
		"environment":      identity.Environment,
		"profile_id":       identity.ProfileID,
		"protocol":         string(identity.Protocol),
		"namespace":        identity.Namespace,
		"server_binding":   optionalIdentifier(identity.ServerBinding),
		"contract_id":      identity.ContractID,
		"version":          identity.Version,
		"source_hash":      identity.SourceHash,
	})
}

func validLabel(value string) bool {
	if len(value) == 0 || len(value) > 192 || !utf8.ValidString(value) {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' || char == '/' || char == '+' {
			if char == '/' && (index == 0 || index == len(value)-1 || value[index-1] == '/') {
				return false
			}
			continue
		}
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func optionalIdentifier(value model.Identifier) any {
	if value == "" {
		return nil
	}
	return value
}

// SourceDocument is an immutable byte capture supplied by the caller. Path
// must equal Ref.Path and all bytes must be verified against Ref.ContentHash.
type SourceDocument struct {
	Path  string          `json:"path"`
	Ref   model.SourceRef `json:"source_ref"`
	Bytes []byte          `json:"-"`
}

type Bundle struct {
	RootPath  string           `json:"root_path"`
	Documents []SourceDocument `json:"documents"`
}

func (bundle Bundle) Validate(identity Identity) (map[string]SourceDocument, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if len(bundle.Documents) == 0 || len(bundle.Documents) > MaxBundleFiles {
		return nil, fmt.Errorf("%w: source bundle file count is outside 1..%d", ErrInvalidContract, MaxBundleFiles)
	}
	if err := model.ValidateRelativePath(bundle.RootPath); err != nil {
		return nil, fmt.Errorf("%w: root path: %v", ErrInvalidContract, err)
	}
	documents := make(map[string]SourceDocument, len(bundle.Documents))
	var totalBytes int64
	var rootFound bool
	var rootRef model.SourceRef
	for _, document := range bundle.Documents {
		if err := model.ValidateRelativePath(document.Path); err != nil || document.Path != document.Ref.Path {
			return nil, fmt.Errorf("%w: source document path is unsafe or differs from its source reference", ErrInvalidContract)
		}
		if _, exists := documents[document.Path]; exists {
			return nil, fmt.Errorf("%w: duplicate source document path", ErrInvalidContract)
		}
		if len(document.Bytes) == 0 || len(document.Bytes) > MaxDocumentBytes || int64(len(document.Bytes)) > MaxBundleBytes-totalBytes {
			return nil, fmt.Errorf("%w: source bundle exceeds its byte bounds", ErrInvalidContract)
		}
		totalBytes += int64(len(document.Bytes))
		if err := document.Ref.ValidateAgainst(document.Bytes); err != nil {
			return nil, fmt.Errorf("%w: source reference %q: %v", ErrInvalidContract, document.Path, err)
		}
		if document.Ref.ProfileID != identity.ProfileID {
			return nil, fmt.Errorf("%w: source document crosses profile identity", ErrInvalidContract)
		}
		if document.Path == bundle.RootPath {
			rootFound = true
			rootRef = document.Ref
		} else if rootFound && (document.Ref.WorktreeID != rootRef.WorktreeID || document.Ref.GenerationID != rootRef.GenerationID) {
			return nil, fmt.Errorf("%w: source bundle crosses worktree or generation identity", ErrInvalidContract)
		}
		documents[document.Path] = document
	}
	if !rootFound {
		return nil, fmt.Errorf("%w: root document is not in the supplied bundle", ErrInvalidContract)
	}
	for _, document := range documents {
		if document.Ref.WorktreeID != rootRef.WorktreeID || document.Ref.GenerationID != rootRef.GenerationID {
			return nil, fmt.Errorf("%w: source bundle crosses worktree or generation identity", ErrInvalidContract)
		}
	}
	sourceHash, err := bundle.ComputeSourceHash()
	if err != nil || sourceHash != identity.SourceHash {
		return nil, fmt.Errorf("%w: identity source_hash does not match canonical bundle source hash", ErrInvalidContract)
	}
	return documents, nil
}

// ComputeSourceHash returns the stable digest of the explicit path/hash pairs
// in the bundle. Bytes still must pass Validate before the digest is trusted.
func (bundle Bundle) ComputeSourceHash() (model.Hash, error) {
	if err := model.ValidateRelativePath(bundle.RootPath); err != nil {
		return "", fmt.Errorf("%w: root path: %v", ErrInvalidContract, err)
	}
	if len(bundle.Documents) == 0 || len(bundle.Documents) > MaxBundleFiles {
		return "", fmt.Errorf("%w: source bundle file count is outside 1..%d", ErrInvalidContract, MaxBundleFiles)
	}
	documents := append([]SourceDocument(nil), bundle.Documents...)
	sort.Slice(documents, func(i, j int) bool { return documents[i].Path < documents[j].Path })
	entries := make([]any, 0, len(documents))
	rootFound := false
	for index, document := range documents {
		if err := model.ValidateRelativePath(document.Path); err != nil || document.Path != document.Ref.Path {
			return "", fmt.Errorf("%w: source document path is unsafe or differs from source reference", ErrInvalidContract)
		}
		if _, err := model.ParseHash(string(document.Ref.ContentHash)); err != nil {
			return "", fmt.Errorf("%w: source document hash is invalid", ErrInvalidContract)
		}
		if index > 0 && documents[index-1].Path == document.Path {
			return "", fmt.Errorf("%w: duplicate source document path", ErrInvalidContract)
		}
		if document.Path == bundle.RootPath {
			rootFound = true
		}
		entries = append(entries, map[string]any{"path": document.Path, "content_hash": document.Ref.ContentHash})
	}
	if !rootFound {
		return "", fmt.Errorf("%w: root document is not in the supplied bundle", ErrInvalidContract)
	}
	return model.HashValue("contract-source-bundle-v1", entries)
}

// DecodeJSONDocument rejects duplicate members, malformed UTF-8, trailing
// values, excessive depth, excessive node counts, and oversized documents.
func DecodeJSONDocument(raw []byte) (any, error) {
	value, _, err := decodeJSONDocument(raw, MaxJSONNodes)
	return value, err
}

// DecodeJSONBundle applies one aggregate node budget across the supplied
// captured documents and rejects any non-object root.
func DecodeJSONBundle(documents map[string]SourceDocument) (map[string]any, error) {
	paths := make([]string, 0, len(documents))
	for pathName := range documents {
		paths = append(paths, pathName)
	}
	sort.Strings(paths)
	values := make(map[string]any, len(documents))
	remaining := MaxJSONNodes
	for _, pathName := range paths {
		value, nodes, err := decodeJSONDocument(documents[pathName].Bytes, remaining)
		if err != nil {
			return nil, fmt.Errorf("%w: document %q: %v", ErrInvalidContract, pathName, err)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("%w: JSON document %q must be an object", ErrInvalidContract, pathName)
		}
		remaining -= nodes
		values[pathName] = value
	}
	return values, nil
}

func decodeJSONDocument(raw []byte, maxNodes int) (any, int, error) {
	if len(raw) == 0 || len(raw) > MaxDocumentBytes || !utf8.Valid(raw) {
		return nil, 0, fmt.Errorf("%w: JSON document size or UTF-8 is invalid", ErrInvalidContract)
	}
	if err := validateSurrogateEscapes(raw); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	value, err := decodeJSONValue(decoder, 0, &nodes, maxNodes)
	if err != nil {
		return nil, nodes, fmt.Errorf("%w: malformed JSON: %v", ErrInvalidContract, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, nodes, fmt.Errorf("%w: trailing JSON value", ErrInvalidContract)
		}
		return nil, nodes, fmt.Errorf("%w: trailing JSON input: %v", ErrInvalidContract, err)
	}
	return value, nodes, nil
}

func decodeJSONValue(decoder *json.Decoder, depth int, nodes *int, maxNodes int) (any, error) {
	(*nodes)++
	if *nodes > maxNodes || depth > MaxRefDepth {
		return nil, fmt.Errorf("JSON node or nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			if len(object) >= 4096 {
				return nil, errors.New("object member limit exceeded")
			}
			value, err := decodeJSONValue(decoder, depth+1, nodes, maxNodes)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("unterminated object")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			if len(array) >= maxNodes {
				return nil, errors.New("array element limit exceeded")
			}
			value, err := decodeJSONValue(decoder, depth+1, nodes, maxNodes)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("unterminated array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delimiter)
	}
}

func validateSurrogateEscapes(raw []byte) error {
	for index := 0; index < len(raw); {
		if raw[index] != '"' {
			index++
			continue
		}
		index++
		for index < len(raw) && raw[index] != '"' {
			if raw[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(raw) {
				return errors.New("truncated string escape")
			}
			if raw[index+1] != 'u' {
				index += 2
				continue
			}
			if index+6 > len(raw) {
				return errors.New("truncated Unicode escape")
			}
			code, ok := parseHex4(raw[index+2 : index+6])
			if !ok {
				return errors.New("invalid Unicode escape")
			}
			index += 6
			if code >= 0xD800 && code <= 0xDBFF {
				if index+6 > len(raw) || raw[index] != '\\' || raw[index+1] != 'u' {
					return errors.New("unpaired high surrogate")
				}
				low, ok := parseHex4(raw[index+2 : index+6])
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return errors.New("invalid low surrogate")
				}
				index += 6
			} else if code >= 0xDC00 && code <= 0xDFFF {
				return errors.New("unpaired low surrogate")
			}
		}
		if index >= len(raw) {
			return errors.New("unterminated JSON string")
		}
		index++
	}
	return nil
}

func parseHex4(raw []byte) (uint16, bool) {
	value, err := strconv.ParseUint(string(raw), 16, 16)
	return uint16(value), err == nil
}

// ResolveJSONPointer resolves only a local RFC 6901 JSON pointer. It never
// interprets a URI or performs I/O.
func ResolveJSONPointer(document any, pointer string) (any, error) {
	if pointer == "" {
		return document, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("JSON pointer must be empty or start with slash")
	}
	current := document
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		if strings.Contains(token, "~") {
			for index := 0; index < len(rawToken); index++ {
				if rawToken[index] == '~' && (index+1 >= len(rawToken) || (rawToken[index+1] != '0' && rawToken[index+1] != '1')) {
					return nil, errors.New("invalid JSON pointer escape")
				}
			}
		}
		switch value := current.(type) {
		case map[string]any:
			next, ok := value[token]
			if !ok {
				return nil, errors.New("JSON pointer target is absent")
			}
			current = next
		case []any:
			if token == "" || (len(token) > 1 && token[0] == '0') {
				return nil, errors.New("invalid JSON array pointer index")
			}
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(value) {
				return nil, errors.New("JSON array pointer index is out of range")
			}
			current = value[index]
		default:
			return nil, errors.New("JSON pointer traverses a scalar value")
		}
	}
	return current, nil
}

type RefStatus string

const (
	RefResolved    RefStatus = "resolved"
	RefRemote      RefStatus = "remote_unresolved"
	RefMissing     RefStatus = "local_unresolved"
	RefTraversal   RefStatus = "unsafe_path_unresolved"
	RefUnsupported RefStatus = "unsupported_unresolved"
	RefCycle       RefStatus = "cycle_unresolved"
)

type ResolvedRef struct {
	Path    string
	Pointer string
	Target  any
	Status  RefStatus
}

// ResolveBundleRef resolves a URI reference only against the supplied capture
// set. The caller owns diagnostics for non-resolved outcomes.
func ResolveBundleRef(documents map[string]any, fromPath, reference string) ResolvedRef {
	if len(reference) == 0 || strings.ContainsAny(reference, "\\\x00\r\n") || strings.Contains(reference, "%") {
		return ResolvedRef{Status: RefUnsupported}
	}
	parsed, err := url.Parse(reference)
	if err != nil {
		return ResolvedRef{Status: RefUnsupported}
	}
	if parsed.IsAbs() || parsed.Host != "" || strings.HasPrefix(reference, "//") {
		return ResolvedRef{Status: RefRemote}
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return ResolvedRef{Status: RefUnsupported}
	}
	pathPart := parsed.Path
	resolvedPath := fromPath
	if pathPart != "" {
		for _, segment := range strings.Split(pathPart, "/") {
			if segment == ".." || segment == "." || segment == "" {
				return ResolvedRef{Status: RefTraversal}
			}
		}
		resolvedPath = path.Clean(path.Join(path.Dir(fromPath), pathPart))
		if err := model.ValidateRelativePath(resolvedPath); err != nil {
			return ResolvedRef{Status: RefTraversal}
		}
	}
	document, exists := documents[resolvedPath]
	if !exists {
		return ResolvedRef{Path: resolvedPath, Pointer: parsed.Fragment, Status: RefMissing}
	}
	target, err := ResolveJSONPointer(document, parsed.Fragment)
	if err != nil {
		return ResolvedRef{Path: resolvedPath, Pointer: parsed.Fragment, Status: RefMissing}
	}
	return ResolvedRef{Path: resolvedPath, Pointer: parsed.Fragment, Target: target, Status: RefResolved}
}
