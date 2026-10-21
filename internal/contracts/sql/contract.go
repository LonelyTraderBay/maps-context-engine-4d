// Package sqlcontract defines the closed, source-backed contract for the
// MySQL static SQL subset. It accepts captured bytes and has no I/O behavior.
package sqlcontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

var ErrInvalidContract = errors.New("invalid SQL contract")

const ResultFormatVersion = 1

type Dialect string

const DialectMySQL Dialect = "mysql"

type Profile struct {
	Dialect       Dialect `json:"dialect"`
	ParserVersion uint16  `json:"parser_version"`
}

func (profile Profile) Validate() error {
	if profile.Dialect != DialectMySQL || profile.ParserVersion != 1 {
		return fmt.Errorf("%w: unsupported SQL dialect or parser profile", ErrInvalidContract)
	}
	return nil
}

type SourceRole string

const (
	RoleSchemaSource    SourceRole = "schema_source"
	RoleMigrationSource SourceRole = "migration_source"
)

// Source is an immutable capture supplied by the caller. Analyze never opens
// a path, connects to a database, executes SQL, or fetches another source.
type Source struct {
	Ref   model.SourceRef `json:"source_ref"`
	Role  SourceRole      `json:"role"`
	Bytes []byte          `json:"-"`
}

type ObjectKind string

const (
	ObjectTable     ObjectKind = "table"
	ObjectColumn    ObjectKind = "column"
	ObjectView      ObjectKind = "view"
	ObjectProcedure ObjectKind = "procedure"
	ObjectFunction  ObjectKind = "function"
	ObjectTrigger   ObjectKind = "trigger"
	ObjectEvent     ObjectKind = "event"
)

type EngineClass string

const (
	EngineUnknown     EngineClass = "unknown"
	EngineInnoDB      EngineClass = "innodb"
	EngineNDBCluster  EngineClass = "ndbcluster"
	EngineUnsupported EngineClass = "unsupported"
)

type EngineIdentity struct {
	Class        EngineClass `json:"class"`
	DeclaredName string      `json:"declared_name"`
}

type Object struct {
	ID        string          `json:"id"`
	Kind      ObjectKind      `json:"kind"`
	Schema    string          `json:"schema,omitempty"`
	Name      string          `json:"name"`
	ParentID  string          `json:"parent_id,omitempty"`
	Engine    *EngineIdentity `json:"engine,omitempty"`
	SourceRef model.SourceRef `json:"source_ref"`
}

type RelationKind string

const (
	RelationTableHasColumn  RelationKind = "table_has_column"
	RelationViewReadsTable  RelationKind = "view_reads_table"
	RelationViewReadsColumn RelationKind = "view_reads_column"
	RelationTriggerOnTable  RelationKind = "trigger_on_table"
)

type Relation struct {
	FromID    string          `json:"from_id"`
	ToID      string          `json:"to_id"`
	Kind      RelationKind    `json:"kind"`
	SourceRef model.SourceRef `json:"source_ref"`
}

type DiagnosticCode string

const (
	DiagnosticUnsupportedStatement DiagnosticCode = "unsupported_statement"
	DiagnosticUnsupportedSyntax    DiagnosticCode = "unsupported_syntax"
	DiagnosticDynamicSQL           DiagnosticCode = "dynamic_sql"
	DiagnosticUnknownEngine        DiagnosticCode = "unknown_engine"
	DiagnosticUnsupportedEngine    DiagnosticCode = "unsupported_engine"
	DiagnosticUnqualifiedReference DiagnosticCode = "unqualified_reference"
	DiagnosticUnresolvedReference  DiagnosticCode = "unresolved_reference"
	DiagnosticAmbiguousReference   DiagnosticCode = "ambiguous_reference"
	DiagnosticExecutableComment    DiagnosticCode = "executable_comment"
	DiagnosticMalformedSource      DiagnosticCode = "malformed_source"
)

type Diagnostic struct {
	Code      DiagnosticCode  `json:"code"`
	SourceRef model.SourceRef `json:"source_ref"`
}

type Coverage string

const (
	CoverageComplete Coverage = "complete_supported_subset"
	CoveragePartial  Coverage = "partial_with_gaps"
	CoverageUnknown  Coverage = "unknown"
)

type MigrationState string

const MigrationStateUnknown MigrationState = "unknown"

type Migration struct {
	Path         string          `json:"path"`
	Order        uint32          `json:"order"`
	AppliedState MigrationState  `json:"applied_state"`
	SourceRef    model.SourceRef `json:"source_ref"`
}

type Result struct {
	FormatVersion uint16       `json:"format_version"`
	Profile       Profile      `json:"profile"`
	Coverage      Coverage     `json:"coverage"`
	Objects       []Object     `json:"objects"`
	Relations     []Relation   `json:"relations"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
	Migrations    []Migration  `json:"migrations"`
}

// ObjectIDFor returns a stable opaque ID tied to the captured source range.
// It deliberately does not use a name as identity.
func ObjectIDFor(kind ObjectKind, source model.SourceRef) string {
	return objectID(kind, source)
}

func objectID(kind ObjectKind, source model.SourceRef) string {
	identity := strings.Join([]string{
		"sql-object-v1", string(source.WorktreeID), string(source.ProfileID),
		string(source.GenerationID), source.Path, string(source.ContentHash),
		strconv.FormatInt(source.Range.StartByte, 10), string(kind),
	}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return "sql1:" + hex.EncodeToString(digest[:])
}

// DecodeResult rejects unknown fields, duplicate JSON object keys, invalid
// UTF-8, oversized values, and trailing JSON before returning the typed value.
func DecodeResult(data []byte) (Result, error) {
	if len(data) > 8<<20 || !utf8.Valid(data) {
		return Result{}, fmt.Errorf("%w: result JSON size or UTF-8 is invalid", ErrInvalidContract)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result Result
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("%w: decode result: %v", ErrInvalidContract, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Result{}, fmt.Errorf("%w: trailing JSON value", ErrInvalidContract)
	}
	return result, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			if len(seen) > 4096 {
				return errors.New("JSON object member limit exceeded")
			}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

// ValidateAgainst verifies all identities, ranges, and relation endpoints
// against the exact captured source bytes supplied to the analyzer.
func (result Result) ValidateAgainst(sources []Source) error {
	if result.FormatVersion != ResultFormatVersion {
		return fmt.Errorf("%w: unsupported result format version", ErrInvalidContract)
	}
	if err := result.Profile.Validate(); err != nil {
		return err
	}
	if result.Coverage != CoverageComplete && result.Coverage != CoveragePartial && result.Coverage != CoverageUnknown {
		return fmt.Errorf("%w: unknown coverage", ErrInvalidContract)
	}
	if result.Coverage == CoverageComplete && len(result.Diagnostics) != 0 {
		return fmt.Errorf("%w: complete coverage cannot contain gaps", ErrInvalidContract)
	}
	if result.Coverage == CoveragePartial && len(result.Diagnostics) == 0 {
		return fmt.Errorf("%w: partial coverage must retain a gap diagnostic", ErrInvalidContract)
	}

	sourceByPath := make(map[string]Source, len(sources))
	for _, source := range sources {
		if source.Role != RoleSchemaSource && source.Role != RoleMigrationSource {
			return fmt.Errorf("%w: unknown source role", ErrInvalidContract)
		}
		if _, exists := sourceByPath[source.Ref.Path]; exists {
			return fmt.Errorf("%w: duplicate captured source path", ErrInvalidContract)
		}
		if source.Ref.Range.StartByte != 0 || source.Ref.Range.EndByte != int64(len(source.Bytes)) {
			return fmt.Errorf("%w: captured source reference must cover the full file", ErrInvalidContract)
		}
		if err := source.Ref.ValidateAgainst(source.Bytes); err != nil {
			return fmt.Errorf("%w: invalid captured source: %v", ErrInvalidContract, err)
		}
		sourceByPath[source.Ref.Path] = source
	}

	objects := make(map[string]Object, len(result.Objects))
	tableNameCounts := make(map[string]int)
	for _, object := range result.Objects {
		if !validObjectKind(object.Kind) || !validText(object.Name) || (object.Schema != "" && !validText(object.Schema)) {
			return fmt.Errorf("%w: invalid SQL object identity", ErrInvalidContract)
		}
		source, ok := sourceByPath[object.SourceRef.Path]
		if !ok || !sameSourceIdentity(object.SourceRef, source.Ref) {
			return fmt.Errorf("%w: object source is outside the captured input", ErrInvalidContract)
		}
		if err := object.SourceRef.ValidateAgainst(source.Bytes); err != nil {
			return fmt.Errorf("%w: invalid object source range: %v", ErrInvalidContract, err)
		}
		if object.SourceRef.Range.EndByte <= object.SourceRef.Range.StartByte {
			return fmt.Errorf("%w: object source range must be nonempty", ErrInvalidContract)
		}
		if object.ID == "" || object.ID != objectID(object.Kind, object.SourceRef) {
			return fmt.Errorf("%w: object ID is not bound to its source range", ErrInvalidContract)
		}
		if _, exists := objects[object.ID]; exists {
			return fmt.Errorf("%w: duplicate object ID", ErrInvalidContract)
		}
		if object.Kind == ObjectTable {
			if err := validateEngine(object.Engine); err != nil {
				return err
			}
			tableNameCounts[snapshotObjectNameKey(object.SourceRef, object.Schema, object.Name)]++
		} else if object.Engine != nil {
			return fmt.Errorf("%w: engine identity applies only to tables", ErrInvalidContract)
		}
		if object.Kind == ObjectColumn && object.ParentID == "" {
			return fmt.Errorf("%w: column needs a parent table", ErrInvalidContract)
		}
		if object.Kind != ObjectColumn && object.ParentID != "" {
			return fmt.Errorf("%w: parent table applies only to columns", ErrInvalidContract)
		}
		objects[object.ID] = object
	}
	if !sort.SliceIsSorted(result.Objects, func(i, j int) bool { return lessObject(result.Objects[i], result.Objects[j]) }) {
		return fmt.Errorf("%w: objects are not in canonical source order", ErrInvalidContract)
	}
	for _, object := range result.Objects {
		if object.Kind != ObjectColumn {
			continue
		}
		parent, ok := objects[object.ParentID]
		if !ok || parent.Kind != ObjectTable || object.Schema != parent.Schema ||
			!sameSourceIdentity(object.SourceRef, parent.SourceRef) ||
			!containsRange(parent.SourceRef.Range, object.SourceRef.Range) {
			return fmt.Errorf("%w: column parent does not contain the declared source range", ErrInvalidContract)
		}
	}

	relationKeys := make(map[string]struct{}, len(result.Relations))
	viewTableReads := make(map[string]struct{})
	requiredViewTableReads := make(map[string]struct{})
	for _, relation := range result.Relations {
		from, fromOK := objects[relation.FromID]
		to, toOK := objects[relation.ToID]
		if !fromOK || !toOK || relation.FromID == relation.ToID {
			return fmt.Errorf("%w: relation endpoint is missing or self-referential", ErrInvalidContract)
		}
		switch relation.Kind {
		case RelationTableHasColumn:
			if from.Kind != ObjectTable || to.Kind != ObjectColumn || to.ParentID != from.ID {
				return fmt.Errorf("%w: invalid table-column relation", ErrInvalidContract)
			}
		case RelationViewReadsTable:
			if from.Kind != ObjectView || to.Kind != ObjectTable || from.Schema == "" || to.Schema == "" ||
				!sameSnapshot(from.SourceRef, to.SourceRef) {
				return fmt.Errorf("%w: view lineage requires explicit schema-qualified endpoints", ErrInvalidContract)
			}
			viewTableReads[relation.FromID+"\x00"+relation.ToID] = struct{}{}
		case RelationViewReadsColumn:
			if from.Kind != ObjectView || to.Kind != ObjectColumn || from.Schema == "" || to.Schema == "" ||
				!sameSnapshot(from.SourceRef, to.SourceRef) {
				return fmt.Errorf("%w: view column lineage requires explicit same-snapshot endpoints", ErrInvalidContract)
			}
			parent, ok := objects[to.ParentID]
			if !ok || parent.Kind != ObjectTable || parent.Schema != to.Schema ||
				tableNameCounts[snapshotObjectNameKey(parent.SourceRef, parent.Schema, parent.Name)] != 1 {
				return fmt.Errorf("%w: view column lineage has an ambiguous table parent", ErrInvalidContract)
			}
			requiredViewTableReads[relation.FromID+"\x00"+parent.ID] = struct{}{}
		case RelationTriggerOnTable:
			if from.Kind != ObjectTrigger || to.Kind != ObjectTable || from.Schema == "" || to.Schema == "" ||
				!sameSnapshot(from.SourceRef, to.SourceRef) {
				return fmt.Errorf("%w: trigger lineage requires explicit schema-qualified endpoints", ErrInvalidContract)
			}
		default:
			return fmt.Errorf("%w: unknown SQL relation kind", ErrInvalidContract)
		}
		if (relation.Kind == RelationViewReadsTable || relation.Kind == RelationTriggerOnTable) &&
			tableNameCounts[snapshotObjectNameKey(to.SourceRef, to.Schema, to.Name)] != 1 {
			return fmt.Errorf("%w: lineage target table identity is ambiguous", ErrInvalidContract)
		}
		source, ok := sourceByPath[relation.SourceRef.Path]
		if !ok || !sameSourceIdentity(relation.SourceRef, source.Ref) ||
			!sameSourceIdentity(relation.SourceRef, from.SourceRef) ||
			!containsRange(from.SourceRef.Range, relation.SourceRef.Range) {
			return fmt.Errorf("%w: relation evidence is not inside its source declaration", ErrInvalidContract)
		}
		if err := relation.SourceRef.ValidateAgainst(source.Bytes); err != nil {
			return fmt.Errorf("%w: invalid relation evidence: %v", ErrInvalidContract, err)
		}
		if relation.SourceRef.Range.EndByte <= relation.SourceRef.Range.StartByte {
			return fmt.Errorf("%w: relation evidence range must be nonempty", ErrInvalidContract)
		}
		start, end := relation.SourceRef.Range.StartByte, relation.SourceRef.Range.EndByte
		expected := to.Name
		if relation.Kind == RelationViewReadsColumn && string(source.Bytes[start:end]) == "*" {
			expected = "*"
		} else if relation.Kind == RelationViewReadsTable || relation.Kind == RelationTriggerOnTable {
			if !qualifiedIdentifierEvidenceMatches(source.Bytes[start:end], to.Schema, to.Name) {
				return fmt.Errorf("%w: relation evidence does not name its exact target", ErrInvalidContract)
			}
			expected = string(source.Bytes[start:end])
		}
		if string(source.Bytes[start:end]) != expected {
			return fmt.Errorf("%w: relation evidence does not name its exact target", ErrInvalidContract)
		}
		key := string(relation.Kind) + "\x00" + relation.FromID + "\x00" + relation.ToID
		if _, exists := relationKeys[key]; exists {
			return fmt.Errorf("%w: duplicate SQL relation", ErrInvalidContract)
		}
		relationKeys[key] = struct{}{}
	}
	for key := range requiredViewTableReads {
		if _, ok := viewTableReads[key]; !ok {
			return fmt.Errorf("%w: view column lineage lacks its table lineage", ErrInvalidContract)
		}
	}
	if !sort.SliceIsSorted(result.Relations, func(i, j int) bool { return lessRelation(result.Relations[i], result.Relations[j]) }) {
		return fmt.Errorf("%w: relations are not in canonical source order", ErrInvalidContract)
	}

	diagnosticKeys := make(map[string]struct{}, len(result.Diagnostics))
	for _, diagnostic := range result.Diagnostics {
		if !validDiagnosticCode(diagnostic.Code) {
			return fmt.Errorf("%w: unknown SQL diagnostic code", ErrInvalidContract)
		}
		source, ok := sourceByPath[diagnostic.SourceRef.Path]
		if !ok || !sameSourceIdentity(diagnostic.SourceRef, source.Ref) {
			return fmt.Errorf("%w: diagnostic source is outside the captured input", ErrInvalidContract)
		}
		if err := diagnostic.SourceRef.ValidateAgainst(source.Bytes); err != nil {
			return fmt.Errorf("%w: invalid diagnostic evidence: %v", ErrInvalidContract, err)
		}
		key := string(diagnostic.Code) + "\x00" + diagnostic.SourceRef.Path + "\x00" + fmt.Sprint(diagnostic.SourceRef.Range.StartByte)
		if _, exists := diagnosticKeys[key]; exists {
			return fmt.Errorf("%w: duplicate SQL diagnostic", ErrInvalidContract)
		}
		diagnosticKeys[key] = struct{}{}
	}
	if !sort.SliceIsSorted(result.Diagnostics, func(i, j int) bool { return lessDiagnostic(result.Diagnostics[i], result.Diagnostics[j]) }) {
		return fmt.Errorf("%w: diagnostics are not in canonical source order", ErrInvalidContract)
	}

	migrationSources := make([]Source, 0, len(sources))
	for _, source := range sources {
		if source.Role == RoleMigrationSource {
			migrationSources = append(migrationSources, source)
		}
	}
	sort.Slice(migrationSources, func(i, j int) bool { return migrationSources[i].Ref.Path < migrationSources[j].Ref.Path })
	if len(migrationSources) != len(result.Migrations) {
		return fmt.Errorf("%w: migration source coverage is incomplete", ErrInvalidContract)
	}
	for i, migration := range result.Migrations {
		source := migrationSources[i]
		if migration.Order != uint32(i+1) || migration.Path != source.Ref.Path ||
			migration.AppliedState != MigrationStateUnknown ||
			!sameSourceIdentity(migration.SourceRef, source.Ref) ||
			migration.SourceRef.Range != source.Ref.Range {
			return fmt.Errorf("%w: migration order or applied-state claim is invalid", ErrInvalidContract)
		}
		if err := migration.SourceRef.ValidateAgainst(source.Bytes); err != nil {
			return fmt.Errorf("%w: invalid migration evidence: %v", ErrInvalidContract, err)
		}
	}
	return nil
}

func validateEngine(engine *EngineIdentity) error {
	if engine == nil {
		return fmt.Errorf("%w: table needs an explicit or unknown engine identity", ErrInvalidContract)
	}
	switch engine.Class {
	case EngineUnknown:
		if engine.DeclaredName != "" {
			return fmt.Errorf("%w: unknown engine cannot claim a declared name", ErrInvalidContract)
		}
	case EngineInnoDB:
		if !strings.EqualFold(engine.DeclaredName, "InnoDB") {
			return fmt.Errorf("%w: InnoDB identity must retain its declared name", ErrInvalidContract)
		}
	case EngineNDBCluster:
		if !strings.EqualFold(engine.DeclaredName, "NDBCLUSTER") {
			return fmt.Errorf("%w: NDB identity must retain its declared name", ErrInvalidContract)
		}
	case EngineUnsupported:
		if !validText(engine.DeclaredName) || strings.EqualFold(engine.DeclaredName, "InnoDB") || strings.EqualFold(engine.DeclaredName, "NDBCLUSTER") {
			return fmt.Errorf("%w: unsupported engine must retain a distinct declared name", ErrInvalidContract)
		}
	default:
		return fmt.Errorf("%w: unknown engine class", ErrInvalidContract)
	}
	return nil
}

func validObjectKind(kind ObjectKind) bool {
	switch kind {
	case ObjectTable, ObjectColumn, ObjectView, ObjectProcedure, ObjectFunction, ObjectTrigger, ObjectEvent:
		return true
	default:
		return false
	}
}

func validDiagnosticCode(code DiagnosticCode) bool {
	switch code {
	case DiagnosticUnsupportedStatement, DiagnosticUnsupportedSyntax, DiagnosticDynamicSQL,
		DiagnosticUnknownEngine, DiagnosticUnsupportedEngine, DiagnosticUnqualifiedReference,
		DiagnosticUnresolvedReference, DiagnosticAmbiguousReference, DiagnosticExecutableComment,
		DiagnosticMalformedSource:
		return true
	default:
		return false
	}
}

func validText(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func sameSourceIdentity(left, right model.SourceRef) bool {
	return left.WorktreeID == right.WorktreeID && left.ProfileID == right.ProfileID &&
		left.GenerationID == right.GenerationID && left.Path == right.Path && left.ContentHash == right.ContentHash
}

func sameSnapshot(left, right model.SourceRef) bool {
	return left.WorktreeID == right.WorktreeID && left.ProfileID == right.ProfileID &&
		left.GenerationID == right.GenerationID
}

func snapshotObjectNameKey(source model.SourceRef, schema, name string) string {
	return string(source.WorktreeID) + "\x00" + string(source.ProfileID) + "\x00" +
		string(source.GenerationID) + "\x00" + schema + "\x00" + name
}

func qualifiedIdentifierEvidenceMatches(raw []byte, schema, name string) bool {
	tokens, err := tokenizeSQLStatement(raw, sqlStatement{start: 0, end: len(raw)})
	if err != nil || len(tokens) != 3 || !isIdentifier(tokens[0]) || !symbol(tokens[1], ".") || !isIdentifier(tokens[2]) {
		return false
	}
	return !tokens[0].identifierHasEscaping && !tokens[2].identifierHasEscaping &&
		tokens[0].text == schema && tokens[2].text == name &&
		exactIdentifierBytes(raw, tokens[0]) && exactIdentifierBytes(raw, tokens[2])
}

func containsRange(parent, child model.ByteRange) bool {
	return parent.StartByte <= child.StartByte && child.EndByte <= parent.EndByte
}

func lessObject(left, right Object) bool {
	if left.SourceRef.Path != right.SourceRef.Path {
		return left.SourceRef.Path < right.SourceRef.Path
	}
	if left.SourceRef.Range.StartByte != right.SourceRef.Range.StartByte {
		return left.SourceRef.Range.StartByte < right.SourceRef.Range.StartByte
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.Schema != right.Schema {
		return left.Schema < right.Schema
	}
	return left.Name < right.Name
}

func lessRelation(left, right Relation) bool {
	if left.SourceRef.Path != right.SourceRef.Path {
		return left.SourceRef.Path < right.SourceRef.Path
	}
	if left.SourceRef.Range.StartByte != right.SourceRef.Range.StartByte {
		return left.SourceRef.Range.StartByte < right.SourceRef.Range.StartByte
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.FromID != right.FromID {
		return left.FromID < right.FromID
	}
	return left.ToID < right.ToID
}

func lessDiagnostic(left, right Diagnostic) bool {
	if left.SourceRef.Path != right.SourceRef.Path {
		return left.SourceRef.Path < right.SourceRef.Path
	}
	if left.SourceRef.Range.StartByte != right.SourceRef.Range.StartByte {
		return left.SourceRef.Range.StartByte < right.SourceRef.Range.StartByte
	}
	return left.Code < right.Code
}
