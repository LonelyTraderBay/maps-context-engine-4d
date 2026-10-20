// Package providers defines the versioned, source-only boundary shared by
// language analyzers. It contains no project loader or plugin discovery.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/contracts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	SchemaVersion            = 1
	MaxProviderFrameBytes    = 16 << 20
	MaxProviderHeaderBytes   = 64 << 10
	MaxProviderFiles         = 4096
	MaxProviderSourceBytes   = 10 << 20
	MaxProviderPathBytes     = 1 << 20
	MaxProviderProfileBytes  = 64 << 10
	MaxProviderTimeout       = 10 * time.Minute
	MaxProviderResponseBytes = MaxProviderFrameBytes + MaxProviderHeaderBytes
)

var (
	ErrInvalidProvider       = errors.New("invalid language provider")
	ErrInvalidProviderInput  = errors.New("invalid language provider input")
	ErrInvalidProviderOutput = errors.New("invalid language provider output")
	ErrProviderTimeout       = errors.New("language provider timed out")
	ErrUnsupportedLanguage   = errors.New("language provider does not support language")
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:/+-]*$`)
	versionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)
	licensePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

type Capability struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Descriptor binds provider behavior to an immutable implementation digest
// and explicit license/version metadata. Capabilities are individually
// versioned so consumers cannot silently widen the protocol.
type Descriptor struct {
	ID           string       `json:"id"`
	Version      string       `json:"version"`
	License      string       `json:"license"`
	Digest       model.Hash   `json:"digest"`
	Languages    []string     `json:"languages"`
	Capabilities []Capability `json:"capabilities"`
}

type Profile struct {
	SchemaID      string          `json:"schema_id"`
	SchemaVersion int             `json:"schema_version"`
	ProfileID     model.Hash      `json:"profile_id"`
	Config        json.RawMessage `json:"config"`
}

type SourceFile struct {
	Path   string
	Source []byte
}

type Request struct {
	RequestID model.UUID
	Language  string
	Profile   Profile
	Files     []SourceFile
}

type ByteRange struct {
	StartByte int64 `json:"start_byte"`
	EndByte   int64 `json:"end_byte"`
}

type AnalysisStatus string

const (
	AnalysisSyntax  AnalysisStatus = "syntax"
	AnalysisTyped   AnalysisStatus = "typed"
	AnalysisUnknown AnalysisStatus = "unknown"
)

type Symbol struct {
	ID             string         `json:"id"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Path           string         `json:"path"`
	Range          ByteRange      `json:"range"`
	AnalysisStatus AnalysisStatus `json:"analysis_status"`
}

type EvidenceKind string

const (
	EvidenceDeclared EvidenceKind = "declared"
	EvidenceInferred EvidenceKind = "inferred"
	EvidenceUnknown  EvidenceKind = "unknown"
)

type Relation struct {
	ID             string         `json:"id"`
	SourceSymbolID string         `json:"source_symbol_id"`
	TargetSymbolID *string        `json:"target_symbol_id"`
	Kind           string         `json:"kind"`
	EvidenceKind   EvidenceKind   `json:"evidence_kind"`
	Path           string         `json:"path"`
	Range          ByteRange      `json:"range"`
	AnalysisStatus AnalysisStatus `json:"analysis_status"`
}

type Diagnostic struct {
	Code     string    `json:"code"`
	Severity string    `json:"severity"`
	Message  string    `json:"message"`
	Path     string    `json:"path"`
	Range    ByteRange `json:"range"`
}

type Result struct {
	SchemaVersion int          `json:"schema_version"`
	MessageType   string       `json:"message_type"`
	RequestID     model.UUID   `json:"request_id"`
	Provider      Descriptor   `json:"provider"`
	Symbols       []Symbol     `json:"symbols"`
	Relations     []Relation   `json:"relations"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
}

type LanguageProvider interface {
	Descriptor() Descriptor
	Analyze(context.Context, Request) (Result, error)
}

type requestEnvelope struct {
	SchemaVersion   int             `json:"schema_version"`
	MessageType     string          `json:"message_type"`
	RequestID       model.UUID      `json:"request_id"`
	Language        string          `json:"language"`
	Profile         Profile         `json:"profile"`
	Files           []requestFile   `json:"files"`
	ExecutionPolicy executionPolicy `json:"execution_policy"`
}

type requestFile struct {
	Path          string     `json:"path"`
	ContentBase64 []byte     `json:"content_base64"`
	ContentSHA256 model.Hash `json:"content_sha256"`
}

type executionPolicy struct {
	TargetCode     string `json:"target_code"`
	ProjectScripts string `json:"project_scripts"`
	ProjectPlugins string `json:"project_plugins"`
	NotebookCells  string `json:"notebook_cells"`
	Network        bool   `json:"network"`
}

func ValidateDescriptor(descriptor Descriptor) (Descriptor, error) {
	if !validIdentifier(descriptor.ID) || !versionPattern.MatchString(descriptor.Version) ||
		!licensePattern.MatchString(descriptor.License) || !validHash(descriptor.Digest) ||
		len(descriptor.Languages) == 0 || len(descriptor.Languages) > 64 ||
		len(descriptor.Capabilities) == 0 || len(descriptor.Capabilities) > 128 {
		return Descriptor{}, fmt.Errorf("%w: identity, license, digest, language or capability metadata", ErrInvalidProvider)
	}
	result := descriptor
	result.Languages = append([]string(nil), descriptor.Languages...)
	result.Capabilities = append([]Capability(nil), descriptor.Capabilities...)
	for _, language := range result.Languages {
		if !validIdentifier(language) {
			return Descriptor{}, fmt.Errorf("%w: invalid language %q", ErrInvalidProvider, language)
		}
	}
	sort.Strings(result.Languages)
	for index := 1; index < len(result.Languages); index++ {
		if result.Languages[index-1] == result.Languages[index] {
			return Descriptor{}, fmt.Errorf("%w: duplicate language %q", ErrInvalidProvider, result.Languages[index])
		}
	}
	for _, capability := range result.Capabilities {
		if !validIdentifier(capability.ID) || !versionPattern.MatchString(capability.Version) {
			return Descriptor{}, fmt.Errorf("%w: capability ID and version are required", ErrInvalidProvider)
		}
	}
	sort.Slice(result.Capabilities, func(i, j int) bool { return result.Capabilities[i].ID < result.Capabilities[j].ID })
	for index := 1; index < len(result.Capabilities); index++ {
		if result.Capabilities[index-1].ID == result.Capabilities[index].ID {
			return Descriptor{}, fmt.Errorf("%w: duplicate capability %q", ErrInvalidProvider, result.Capabilities[index].ID)
		}
	}
	return result, nil
}

func normalizeRequest(request Request, descriptor Descriptor) (requestEnvelope, map[string][]byte, error) {
	if _, err := model.ParseUUID(string(request.RequestID)); err != nil {
		return requestEnvelope{}, nil, fmt.Errorf("%w: request ID: %v", ErrInvalidProviderInput, err)
	}
	if !validIdentifier(request.Language) || !containsString(descriptor.Languages, request.Language) {
		return requestEnvelope{}, nil, fmt.Errorf("%w: %s", ErrUnsupportedLanguage, request.Language)
	}
	if !validIdentifier(request.Profile.SchemaID) || request.Profile.SchemaVersion < 1 || !validHash(request.Profile.ProfileID) ||
		len(request.Profile.Config) == 0 || len(request.Profile.Config) > MaxProviderProfileBytes {
		return requestEnvelope{}, nil, fmt.Errorf("%w: profile identity or byte limit", ErrInvalidProviderInput)
	}
	profileValue, err := contracts.DecodeJSON(request.Profile.Config)
	if err != nil {
		return requestEnvelope{}, nil, fmt.Errorf("%w: profile config: %v", ErrInvalidProviderInput, err)
	}
	if _, ok := profileValue.(map[string]any); !ok {
		return requestEnvelope{}, nil, fmt.Errorf("%w: profile config must be a JSON object", ErrInvalidProviderInput)
	}
	if len(request.Files) == 0 || len(request.Files) > MaxProviderFiles {
		return requestEnvelope{}, nil, fmt.Errorf("%w: file count outside 1..%d", ErrInvalidProviderInput, MaxProviderFiles)
	}
	files := append([]SourceFile(nil), request.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	encodedFiles := make([]requestFile, 0, len(files))
	fileBytes := make(map[string][]byte, len(files))
	var sourceBytes, pathBytes int64
	for index, file := range files {
		if err := validateRelativePath(file.Path); err != nil {
			return requestEnvelope{}, nil, fmt.Errorf("%w: %v", ErrInvalidProviderInput, err)
		}
		if index > 0 && files[index-1].Path == file.Path {
			return requestEnvelope{}, nil, fmt.Errorf("%w: duplicate source path %q", ErrInvalidProviderInput, file.Path)
		}
		pathBytes += int64(len(file.Path))
		sourceBytes += int64(len(file.Source))
		if pathBytes > MaxProviderPathBytes || sourceBytes > MaxProviderSourceBytes {
			return requestEnvelope{}, nil, fmt.Errorf("%w: source or path byte budget exceeded", ErrInvalidProviderInput)
		}
		copyOfSource := make([]byte, len(file.Source))
		copy(copyOfSource, file.Source)
		fileBytes[file.Path] = copyOfSource
		encodedFiles = append(encodedFiles, requestFile{
			Path: file.Path, ContentBase64: copyOfSource, ContentSHA256: model.SHA256(copyOfSource),
		})
	}
	profile := request.Profile
	profile.Config = append(json.RawMessage(nil), request.Profile.Config...)
	envelope := requestEnvelope{
		SchemaVersion: SchemaVersion,
		MessageType:   "analyze_request",
		RequestID:     request.RequestID,
		Language:      request.Language,
		Profile:       profile,
		Files:         encodedFiles,
		ExecutionPolicy: executionPolicy{
			TargetCode: "never_execute", ProjectScripts: "never_execute", ProjectPlugins: "never_execute",
			NotebookCells: "never_execute", Network: false,
		},
	}
	return envelope, fileBytes, nil
}

func validateResult(result Result, request Request, descriptor Descriptor, sourceByPath map[string][]byte) error {
	if result.SchemaVersion != SchemaVersion || result.MessageType != "analyze_response" || result.RequestID != request.RequestID {
		return fmt.Errorf("%w: protocol or request identity mismatch", ErrInvalidProviderOutput)
	}
	resultDescriptor, err := ValidateDescriptor(result.Provider)
	if err != nil || !sameDescriptor(resultDescriptor, descriptor) {
		return fmt.Errorf("%w: provider license/version/digest/capabilities mismatch", ErrInvalidProviderOutput)
	}
	symbols := make(map[string]struct{}, len(result.Symbols))
	for _, symbol := range result.Symbols {
		if !validIdentifier(symbol.ID) || !validIdentifier(symbol.Kind) || !validText(symbol.Name) || symbol.Name == "" ||
			!validPathInSources(symbol.Path, sourceByPath) || !validStatus(symbol.AnalysisStatus) {
			return fmt.Errorf("%w: invalid symbol identity, path or status", ErrInvalidProviderOutput)
		}
		if _, exists := symbols[symbol.ID]; exists {
			return fmt.Errorf("%w: duplicate symbol ID %q", ErrInvalidProviderOutput, symbol.ID)
		}
		if err := validateRange(symbol.Range, sourceByPath[symbol.Path]); err != nil {
			return fmt.Errorf("%w: symbol %q range: %v", ErrInvalidProviderOutput, symbol.ID, err)
		}
		symbols[symbol.ID] = struct{}{}
	}
	relationIDs := make(map[string]struct{}, len(result.Relations))
	for _, relation := range result.Relations {
		if !validIdentifier(relation.ID) || !validIdentifier(relation.SourceSymbolID) || !validIdentifier(relation.Kind) ||
			!validEvidenceKind(relation.EvidenceKind) || !validStatus(relation.AnalysisStatus) ||
			!validPathInSources(relation.Path, sourceByPath) {
			return fmt.Errorf("%w: invalid relation identity, path, kind or status", ErrInvalidProviderOutput)
		}
		if _, exists := symbols[relation.SourceSymbolID]; !exists {
			return fmt.Errorf("%w: relation %q has dangling source symbol", ErrInvalidProviderOutput, relation.ID)
		}
		if relation.TargetSymbolID != nil {
			if _, exists := symbols[*relation.TargetSymbolID]; !exists {
				return fmt.Errorf("%w: relation %q has dangling target symbol", ErrInvalidProviderOutput, relation.ID)
			}
		}
		if _, exists := relationIDs[relation.ID]; exists {
			return fmt.Errorf("%w: duplicate relation ID %q", ErrInvalidProviderOutput, relation.ID)
		}
		if err := validateRange(relation.Range, sourceByPath[relation.Path]); err != nil {
			return fmt.Errorf("%w: relation %q range: %v", ErrInvalidProviderOutput, relation.ID, err)
		}
		relationIDs[relation.ID] = struct{}{}
	}
	for _, diagnostic := range result.Diagnostics {
		if !validIdentifier(diagnostic.Code) || !validText(diagnostic.Message) || diagnostic.Message == "" ||
			!validPathInSources(diagnostic.Path, sourceByPath) ||
			(diagnostic.Severity != "info" && diagnostic.Severity != "warning" && diagnostic.Severity != "error") {
			return fmt.Errorf("%w: invalid diagnostic", ErrInvalidProviderOutput)
		}
		if err := validateRange(diagnostic.Range, sourceByPath[diagnostic.Path]); err != nil {
			return fmt.Errorf("%w: diagnostic range: %v", ErrInvalidProviderOutput, err)
		}
	}
	return nil
}

func validIdentifier(value string) bool {
	return len(value) <= 128 && validText(value) && identifierPattern.MatchString(value)
}

func validText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validHash(value model.Hash) bool {
	_, err := model.ParseHash(string(value))
	return err == nil
}

func validStatus(status AnalysisStatus) bool {
	return status == AnalysisSyntax || status == AnalysisTyped || status == AnalysisUnknown
}

func validEvidenceKind(kind EvidenceKind) bool {
	return kind == EvidenceDeclared || kind == EvidenceInferred || kind == EvidenceUnknown
}

func validateRelativePath(value string) error {
	if !validText(value) || value == "" || len(value) > 4096 || strings.ContainsRune(value, '\\') ||
		strings.HasPrefix(value, "/") || strings.Contains(value, ":") || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("invalid repository-relative slash path %q", value)
	}
	return nil
}

func validPathInSources(value string, sources map[string][]byte) bool {
	if validateRelativePath(value) != nil {
		return false
	}
	_, exists := sources[value]
	return exists
}

func validateRange(value ByteRange, source []byte) error {
	if value.StartByte < 0 || value.EndByte < value.StartByte || value.EndByte > int64(len(source)) {
		return errors.New("byte range is outside captured source")
	}
	return nil
}

func containsString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func sameDescriptor(left, right Descriptor) bool {
	if left.ID != right.ID || left.Version != right.Version || left.License != right.License || left.Digest != right.Digest ||
		len(left.Languages) != len(right.Languages) || len(left.Capabilities) != len(right.Capabilities) {
		return false
	}
	for index := range left.Languages {
		if left.Languages[index] != right.Languages[index] {
			return false
		}
	}
	for index := range left.Capabilities {
		if left.Capabilities[index] != right.Capabilities[index] {
			return false
		}
	}
	return true
}
