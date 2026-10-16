// Package golang extracts syntax facts from immutable Go source snapshots.
// It does not read the filesystem or resolve names by spelling alone.
package golang

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const DefaultMaxFileBytes int64 = 2 * 1024 * 1024

type PackageVariant string

const (
	VariantProduction   PackageVariant = "production"
	VariantInternalTest PackageVariant = "internal_test"
	VariantExternalTest PackageVariant = "external_test"
)

type PackageInput struct {
	GenerationID model.UUID
	ProfileID    model.Hash
	ImportPath   string
	ModuleKey    string
	Variant      PackageVariant
	MaxFileBytes int64
	Files        []CapturedFile
}

// CapturedFile is the exact byte snapshot parsed by this package. Path is a
// repository-relative slash path; Source is never retained in the result.
type CapturedFile struct {
	Path   string
	Source []byte
}

type SyntaxStatus string

const (
	SyntaxComplete SyntaxStatus = "complete"
	SyntaxPartial  SyntaxStatus = "partial"
)

type ParseStatus string

const (
	ParseComplete ParseStatus = "parsed"
	ParsePartial  ParseStatus = "partial"
	ParseFailed   ParseStatus = "failed"
	ParseSkipped  ParseStatus = "skipped_size"
)

type PackageFacts struct {
	Key          model.Hash
	GenerationID model.UUID
	ProfileID    model.Hash
	ImportPath   string
	ModuleKey    string
	Variant      PackageVariant
	PackageName  string
	InputDigest  model.Hash
	SyntaxStatus SyntaxStatus
	Files        []FileFacts
	Scopes       []Scope
	Declarations []Declaration
	Imports      []ImportFact
	Diagnostics  []Diagnostic
}

type FileFacts struct {
	Path             string
	ContentHash      model.Hash
	PackageName      string
	PackageNameRange model.ByteRange
	ParseStatus      ParseStatus
}

// Scope IDs are generation and package-variant scoped. OccurrenceID omits the
// variant so callers can deduplicate the same captured declaration across Go
// test variants while retaining each variant's distinct SymbolID.
type Scope struct {
	ID                 model.Hash
	OccurrenceID       model.Hash
	ParentID           *model.Hash
	ParentOccurrenceID *model.Hash
	Kind               string
	Path               string
	Range              model.ByteRange
}

type Declaration struct {
	SymbolID               model.Hash
	OccurrenceID           model.Hash
	PackageKey             model.Hash
	ScopeID                model.Hash
	ScopeOccurrenceID      model.Hash
	Path                   string
	Name                   string
	Kind                   string
	NameRange              model.ByteRange
	DeclarationRange       model.ByteRange
	Exported               bool
	RequiresTypeResolution bool
}

type ImportFact struct {
	Path        string
	Alias       *string
	PathRange   model.ByteRange
	SourceRange model.ByteRange
}

type Diagnostic struct {
	Path     string
	Code     string
	Severity string
	Message  string
	Range    model.ByteRange
}

type DeclarationOccurrence struct {
	ID               model.Hash
	ImportPath       string
	Path             string
	Name             string
	Kind             string
	NameRange        model.ByteRange
	DeclarationRange model.ByteRange
	Variants         []PackageVariant
	SymbolIDs        []model.Hash
}

type Analysis struct {
	Packages    []PackageFacts
	Occurrences []DeclarationOccurrence
}

var (
	ErrInvalidInput          = errors.New("invalid Go syntax analysis input")
	ErrVariantSourceMismatch = errors.New("captured bytes differ across package variants")
	ErrInvalidASTRange       = errors.New("Go parser returned a range outside captured bytes")
)

// AnalyzePackage parses one explicit package variant from captured bytes.
func AnalyzePackage(input PackageInput) (PackageFacts, error) {
	analysis, err := AnalyzePackages([]PackageInput{input})
	if err != nil {
		return PackageFacts{}, err
	}
	return analysis.Packages[0], nil
}

// AnalyzePackages parses explicit packages and variants, then provides a
// source-occurrence projection that does not count repeated test-variant facts.
func AnalyzePackages(inputs []PackageInput) (Analysis, error) {
	if len(inputs) == 0 {
		return Analysis{}, fmt.Errorf("%w: at least one package is required", ErrInvalidInput)
	}
	packages := make([]PackageFacts, 0, len(inputs))
	packageKeys := make(map[model.Hash]struct{}, len(inputs))
	type sourceKey struct {
		generation model.UUID
		profile    model.Hash
		importPath string
		moduleKey  string
		path       string
	}
	sources := make(map[sourceKey]model.Hash)
	for _, input := range inputs {
		if err := validateInput(input); err != nil {
			return Analysis{}, err
		}
		for _, file := range input.Files {
			key := sourceKey{input.GenerationID, input.ProfileID, input.ImportPath, input.ModuleKey, file.Path}
			digest := model.SHA256(file.Source)
			if prior, ok := sources[key]; ok && prior != digest {
				return Analysis{}, fmt.Errorf("%w: %s", ErrVariantSourceMismatch, file.Path)
			}
			sources[key] = digest
		}
		facts, err := analyzeOne(input)
		if err != nil {
			return Analysis{}, err
		}
		if _, exists := packageKeys[facts.Key]; exists {
			return Analysis{}, fmt.Errorf("%w: duplicate package variant", ErrInvalidInput)
		}
		packageKeys[facts.Key] = struct{}{}
		packages = append(packages, facts)
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Key < packages[j].Key })

	occurrences := make(map[model.Hash]*DeclarationOccurrence)
	for _, pkg := range packages {
		for _, declaration := range pkg.Declarations {
			occurrence := occurrences[declaration.OccurrenceID]
			if occurrence == nil {
				occurrence = &DeclarationOccurrence{
					ID:               declaration.OccurrenceID,
					ImportPath:       pkg.ImportPath,
					Path:             declaration.Path,
					Name:             declaration.Name,
					Kind:             declaration.Kind,
					NameRange:        declaration.NameRange,
					DeclarationRange: declaration.DeclarationRange,
				}
				occurrences[declaration.OccurrenceID] = occurrence
			}
			occurrence.Variants = append(occurrence.Variants, pkg.Variant)
			occurrence.SymbolIDs = append(occurrence.SymbolIDs, declaration.SymbolID)
		}
	}
	result := Analysis{Packages: packages, Occurrences: make([]DeclarationOccurrence, 0, len(occurrences))}
	for _, occurrence := range occurrences {
		sort.Slice(occurrence.Variants, func(i, j int) bool { return occurrence.Variants[i] < occurrence.Variants[j] })
		sort.Slice(occurrence.SymbolIDs, func(i, j int) bool { return occurrence.SymbolIDs[i] < occurrence.SymbolIDs[j] })
		result.Occurrences = append(result.Occurrences, *occurrence)
	}
	sort.Slice(result.Occurrences, func(i, j int) bool { return result.Occurrences[i].ID < result.Occurrences[j].ID })
	return result, nil
}

func validateInput(input PackageInput) error {
	if _, err := model.ParseUUID(string(input.GenerationID)); err != nil {
		return fmt.Errorf("%w: generation ID: %v", ErrInvalidInput, err)
	}
	if _, err := model.ParseHash(string(input.ProfileID)); err != nil {
		return fmt.Errorf("%w: profile ID: %v", ErrInvalidInput, err)
	}
	if !validPackagePath(input.ImportPath) || !validIdentityText(input.ModuleKey) {
		return fmt.Errorf("%w: explicit import path and module key are required", ErrInvalidInput)
	}
	switch input.Variant {
	case VariantProduction, VariantInternalTest, VariantExternalTest:
	default:
		return fmt.Errorf("%w: unknown package variant", ErrInvalidInput)
	}
	if input.MaxFileBytes < 1 || input.MaxFileBytes > DefaultMaxFileBytes {
		return fmt.Errorf("%w: effective max file bytes must be between 1 and %d", ErrInvalidInput, DefaultMaxFileBytes)
	}
	if len(input.Files) == 0 {
		return fmt.Errorf("%w: package has no captured files", ErrInvalidInput)
	}
	seen := make(map[string]struct{}, len(input.Files))
	for _, file := range input.Files {
		if err := model.ValidateRelativePath(file.Path); err != nil {
			return fmt.Errorf("%w: invalid captured path", ErrInvalidInput)
		}
		if _, exists := seen[file.Path]; exists {
			return fmt.Errorf("%w: duplicate captured path %q", ErrInvalidInput, file.Path)
		}
		seen[file.Path] = struct{}{}
	}
	return nil
}

func validPackagePath(value string) bool {
	if !validIdentityText(value) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validIdentityText(value string) bool {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\u0085' || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func analyzeOne(input PackageInput) (PackageFacts, error) {
	packageKey, err := model.HashValue("go-package-v1", map[string]any{
		"generation_id": input.GenerationID,
		"profile_id":    input.ProfileID,
		"import_path":   input.ImportPath,
		"module_key":    input.ModuleKey,
		"variant":       string(input.Variant),
	})
	if err != nil {
		return PackageFacts{}, err
	}
	files := append([]CapturedFile(nil), input.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	fileInputs := make([]any, 0, len(files))
	for _, file := range files {
		fileInputs = append(fileInputs, map[string]any{"path": file.Path, "content_hash": model.SHA256(file.Source)})
	}
	inputDigest, err := model.HashValue("go-package-syntax-input-v1", map[string]any{
		"package_key": packageKey,
		"files":       fileInputs,
	})
	if err != nil {
		return PackageFacts{}, err
	}
	facts := PackageFacts{
		Key:          packageKey,
		GenerationID: input.GenerationID,
		ProfileID:    input.ProfileID,
		ImportPath:   input.ImportPath,
		ModuleKey:    input.ModuleKey,
		Variant:      input.Variant,
		InputDigest:  inputDigest,
		SyntaxStatus: SyntaxComplete,
		Files:        make([]FileFacts, 0, len(files)),
		Scopes:       make([]Scope, 0),
		Declarations: make([]Declaration, 0),
		Imports:      make([]ImportFact, 0),
		Diagnostics:  make([]Diagnostic, 0),
	}
	packageOccurrence, err := model.HashValue("go-scope-occurrence-v1", map[string]any{
		"generation_id": input.GenerationID,
		"profile_id":    input.ProfileID,
		"import_path":   input.ImportPath,
		"module_key":    input.ModuleKey,
		"path":          "",
		"kind":          "package",
		"start_byte":    int64(0),
		"end_byte":      int64(0),
		"parent":        "",
	})
	if err != nil {
		return PackageFacts{}, err
	}
	packageScope := Scope{ID: packageKey, OccurrenceID: packageOccurrence, Kind: "package"}
	facts.Scopes = append(facts.Scopes, packageScope)

	for _, file := range files {
		fileFacts, err := analyzeFile(input, packageKey, packageScope, file, &facts)
		if err != nil {
			return PackageFacts{}, err
		}
		facts.Files = append(facts.Files, fileFacts)
		if fileFacts.ParseStatus != ParseComplete {
			facts.SyntaxStatus = SyntaxPartial
		}
		if fileFacts.PackageName != "" {
			if facts.PackageName == "" {
				facts.PackageName = fileFacts.PackageName
			} else if facts.PackageName != fileFacts.PackageName {
				facts.SyntaxStatus = SyntaxPartial
				facts.Diagnostics = append(facts.Diagnostics, Diagnostic{
					Path: file.Path, Code: "GO_PACKAGE_NAME_MISMATCH", Severity: "error",
					Message: "captured files in one package variant declare different package names",
					Range:   fileFacts.PackageNameRange,
				})
			}
		}
	}
	sortFacts(&facts)
	return facts, nil
}

func analyzeFile(input PackageInput, packageKey model.Hash, packageScope Scope, captured CapturedFile, facts *PackageFacts) (FileFacts, error) {
	fileFacts := FileFacts{Path: captured.Path, ContentHash: model.SHA256(captured.Source), ParseStatus: ParseComplete}
	if int64(len(captured.Source)) > input.MaxFileBytes {
		fileFacts.ParseStatus = ParseSkipped
		facts.Diagnostics = append(facts.Diagnostics, Diagnostic{
			Path: captured.Path, Code: "GO_FILE_TOO_LARGE", Severity: "warning",
			Message: "captured Go file exceeds the effective source-size limit",
		})
		return fileFacts, nil
	}
	if !utf8.Valid(captured.Source) {
		offset := firstInvalidUTF8(captured.Source)
		fileFacts.ParseStatus = ParseFailed
		facts.Diagnostics = append(facts.Diagnostics, Diagnostic{
			Path: captured.Path, Code: "GO_INVALID_UTF8", Severity: "error",
			Message: "captured Go source contains invalid UTF-8",
			Range:   model.ByteRange{StartByte: int64(offset), EndByte: int64(offset)},
		})
		return fileFacts, nil
	}

	fset := token.NewFileSet()
	parsed, parseErr := parser.ParseFile(fset, captured.Path, captured.Source, parser.AllErrors)
	if parseErr != nil {
		fileFacts.ParseStatus = ParsePartial
		facts.Diagnostics = append(facts.Diagnostics, parseDiagnostics(captured.Path, parseErr, captured.Source)...)
	}
	if parsed == nil {
		fileFacts.ParseStatus = ParseFailed
		return fileFacts, nil
	}
	if parsed.Name != nil {
		fileFacts.PackageName = parsed.Name.Name
		if tokenFile := fset.File(parsed.Name.Pos()); tokenFile != nil {
			fileFacts.PackageNameRange = model.ByteRange{
				StartByte: int64(tokenFile.Offset(parsed.Name.Pos())),
				EndByte:   int64(tokenFile.Offset(parsed.Name.End())),
			}
		}
	}

	visitor := fileVisitor{
		input: input, packageKey: packageKey, path: captured.Path,
		source: captured.Source, fileSet: fset, facts: facts,
	}
	fileScope := visitor.addScope(nil, &packageScope, "file", model.ByteRange{StartByte: 0, EndByte: int64(len(captured.Source))})
	if visitor.err != nil {
		return FileFacts{}, visitor.err
	}
	visitor.walkImports(parsed.Imports, fileScope)
	for _, declaration := range parsed.Decls {
		switch node := declaration.(type) {
		case *ast.GenDecl:
			visitor.walkGenDecl(node, packageScope)
		case *ast.FuncDecl:
			visitor.walkFuncDecl(node, packageScope)
		}
	}
	if visitor.err != nil {
		return FileFacts{}, visitor.err
	}
	return fileFacts, nil
}

func firstInvalidUTF8(source []byte) int {
	for offset := 0; offset < len(source); {
		_, size := utf8.DecodeRune(source[offset:])
		if size == 1 && source[offset] >= utf8.RuneSelf {
			return offset
		}
		offset += size
	}
	return 0
}

func parseDiagnostics(filePath string, parseErr error, source []byte) []Diagnostic {
	var list scanner.ErrorList
	if !errors.As(parseErr, &list) {
		return []Diagnostic{{Path: filePath, Code: "GO_PARSE_ERROR", Severity: "error", Message: "Go source could not be fully parsed"}}
	}
	result := make([]Diagnostic, 0, len(list))
	sourceLength := int64(len(source))
	for _, parseIssue := range list {
		offset := int64(parseIssue.Pos.Offset)
		if offset < 0 {
			offset = 0
		}
		if offset > sourceLength {
			offset = sourceLength
		}
		result = append(result, Diagnostic{
			Path: filePath, Code: "GO_PARSE_ERROR", Severity: "error",
			Message: "Go source could not be fully parsed",
			Range:   diagnosticRange(source, offset),
		})
	}
	return result
}

func diagnosticRange(source []byte, offset int64) model.ByteRange {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(source)) {
		offset = int64(len(source))
	}
	start := int(offset)
	for start > 0 && start < len(source) && source[start]&0xc0 == 0x80 {
		start--
	}
	end := start
	if start < len(source) {
		_, size := utf8.DecodeRune(source[start:])
		if size > 0 {
			end += size
		}
	}
	return model.ByteRange{StartByte: int64(start), EndByte: int64(end)}
}

type fileVisitor struct {
	input      PackageInput
	packageKey model.Hash
	path       string
	source     []byte
	fileSet    *token.FileSet
	facts      *PackageFacts
	err        error
}

func (visitor *fileVisitor) addScope(node ast.Node, parent *Scope, kind string, fixedRange model.ByteRange) *Scope {
	if visitor.err != nil {
		return nil
	}
	byteRange := fixedRange
	if node != nil {
		var ok bool
		byteRange, ok = visitor.rangeOf(node)
		if !ok {
			return nil
		}
	}
	parentID, parentOccurrence := "", ""
	var parentPointer, parentOccurrencePointer *model.Hash
	if parent != nil {
		parentID = string(parent.ID)
		parentOccurrence = string(parent.OccurrenceID)
		id, occurrenceID := parent.ID, parent.OccurrenceID
		parentPointer, parentOccurrencePointer = &id, &occurrenceID
	}
	identity, err := model.HashValue("go-scope-v1", map[string]any{
		"package_key": visitor.packageKey, "path": visitor.path, "kind": kind,
		"lexical_owner": parentID, "start_byte": byteRange.StartByte, "end_byte": byteRange.EndByte,
	})
	if err != nil {
		visitor.err = err
		return nil
	}
	occurrence, err := model.HashValue("go-scope-occurrence-v1", map[string]any{
		"generation_id": visitor.input.GenerationID, "profile_id": visitor.input.ProfileID,
		"import_path": visitor.input.ImportPath, "module_key": visitor.input.ModuleKey,
		"path": visitor.path, "kind": kind, "parent": parentOccurrence,
		"start_byte": byteRange.StartByte, "end_byte": byteRange.EndByte,
	})
	if err != nil {
		visitor.err = err
		return nil
	}
	scope := Scope{
		ID: identity, OccurrenceID: occurrence, ParentID: parentPointer,
		ParentOccurrenceID: parentOccurrencePointer, Kind: kind, Path: visitor.path, Range: byteRange,
	}
	visitor.facts.Scopes = append(visitor.facts.Scopes, scope)
	return &scope
}

func (visitor *fileVisitor) addDeclaration(name *ast.Ident, declaration ast.Node, scope *Scope, kind string, typeDependent bool) {
	if visitor.err != nil || name == nil || name.Name == "_" || scope == nil {
		return
	}
	nameRange, ok := visitor.rangeOf(name)
	if !ok {
		return
	}
	declarationRange, ok := visitor.rangeOf(declaration)
	if !ok {
		return
	}
	identity, err := model.HashValue("go-symbol-v1", map[string]any{
		"generation_id": visitor.input.GenerationID, "profile_id": visitor.input.ProfileID,
		"package_key": visitor.packageKey, "package_variant": string(visitor.input.Variant),
		"language": "go", "path": visitor.path, "kind": kind,
		"lexical_owner":          scope.ID,
		"declaration_start_byte": declarationRange.StartByte, "declaration_end_byte": declarationRange.EndByte,
		"name_start_byte": nameRange.StartByte, "name_end_byte": nameRange.EndByte,
	})
	if err != nil {
		visitor.err = err
		return
	}
	occurrence, err := model.HashValue("go-symbol-occurrence-v1", map[string]any{
		"generation_id": visitor.input.GenerationID, "profile_id": visitor.input.ProfileID,
		"import_path": visitor.input.ImportPath, "module_key": visitor.input.ModuleKey,
		"path": visitor.path, "kind": kind, "lexical_owner": scope.OccurrenceID,
		"content_hash":           model.SHA256(visitor.source),
		"declaration_start_byte": declarationRange.StartByte, "declaration_end_byte": declarationRange.EndByte,
		"name_start_byte": nameRange.StartByte, "name_end_byte": nameRange.EndByte,
	})
	if err != nil {
		visitor.err = err
		return
	}
	visitor.facts.Declarations = append(visitor.facts.Declarations, Declaration{
		SymbolID: identity, OccurrenceID: occurrence, PackageKey: visitor.packageKey,
		ScopeID: scope.ID, ScopeOccurrenceID: scope.OccurrenceID, Path: visitor.path,
		Name: name.Name, Kind: kind, NameRange: nameRange, DeclarationRange: declarationRange,
		Exported: declarationExported(name.Name, kind, scope.Kind), RequiresTypeResolution: typeDependent,
	})
}

func declarationExported(name, kind, scopeKind string) bool {
	if !ast.IsExported(name) {
		return false
	}
	switch kind {
	case "type", "function", "method", "var", "const", "field", "interface_method":
		return scopeKind == "package" || kind == "field" || kind == "interface_method" || kind == "method"
	default:
		return false
	}
}

func (visitor *fileVisitor) rangeOf(node ast.Node) (model.ByteRange, bool) {
	if node == nil {
		visitor.err = ErrInvalidASTRange
		return model.ByteRange{}, false
	}
	file := visitor.fileSet.File(node.Pos())
	if file == nil {
		visitor.err = ErrInvalidASTRange
		return model.ByteRange{}, false
	}
	result := model.ByteRange{StartByte: int64(file.Offset(node.Pos())), EndByte: int64(file.Offset(node.End()))}
	if err := result.ValidateAgainst(visitor.source); err != nil {
		visitor.err = fmt.Errorf("%w: %v", ErrInvalidASTRange, err)
		return model.ByteRange{}, false
	}
	return result, true
}

func (visitor *fileVisitor) walkImports(imports []*ast.ImportSpec, fileScope *Scope) {
	for _, spec := range imports {
		pathRange, pathOK := visitor.rangeOf(spec.Path)
		sourceRange, sourceOK := visitor.rangeOf(spec)
		if !pathOK || !sourceOK {
			return
		}
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			visitor.facts.Diagnostics = append(visitor.facts.Diagnostics, Diagnostic{
				Path: visitor.path, Code: "GO_IMPORT_PATH_INVALID", Severity: "error",
				Message: "Go import path literal could not be decoded", Range: pathRange,
			})
			continue
		}
		fact := ImportFact{Path: importPath, PathRange: pathRange, SourceRange: sourceRange}
		if spec.Name != nil {
			alias := spec.Name.Name
			fact.Alias = &alias
			if alias == "." {
				visitor.facts.Diagnostics = append(visitor.facts.Diagnostics, Diagnostic{
					Path: visitor.path, Code: "GO_DOT_IMPORT_UNRESOLVED", Severity: "warning",
					Message: "dot-imported declarations need package metadata to resolve names", Range: sourceRange,
				})
			} else if alias != "_" {
				visitor.addDeclaration(spec.Name, spec, fileScope, "import", false)
			}
		}
		visitor.facts.Imports = append(visitor.facts.Imports, fact)
	}
}

func (visitor *fileVisitor) walkGenDecl(declaration *ast.GenDecl, scope Scope) {
	if declaration == nil || visitor.err != nil {
		return
	}
	owner := &scope
	switch declaration.Tok {
	case token.VAR, token.CONST:
		kind := "var"
		if declaration.Tok == token.CONST {
			kind = "const"
		}
		for _, spec := range declaration.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				visitor.addDeclaration(name, value, owner, kind, false)
			}
			visitor.walkTypeExpr(value.Type, owner)
			for _, expression := range value.Values {
				visitor.walkExpr(expression, owner)
			}
		}
	case token.TYPE:
		for _, spec := range declaration.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			visitor.addDeclaration(typeSpec.Name, typeSpec, owner, "type", false)
			typeScope := visitor.addScope(typeSpec, owner, "type", model.ByteRange{})
			if typeScope == nil {
				continue
			}
			visitor.addTypeParameters(typeSpec.TypeParams, typeScope)
			visitor.walkTypeExpr(typeSpec.Type, typeScope)
		}
	}
}

func (visitor *fileVisitor) walkFuncDecl(declaration *ast.FuncDecl, packageScope Scope) {
	kind := "function"
	if declaration.Recv != nil {
		kind = "method"
	}
	visitor.addDeclaration(declaration.Name, declaration, &packageScope, kind, false)
	functionScope := visitor.addScope(declaration, &packageScope, "function", model.ByteRange{})
	if functionScope == nil {
		return
	}
	visitor.addReceiverParameters(declaration.Recv, functionScope)
	visitor.addFuncSignature(declaration.Type, functionScope)
	if declaration.Body != nil {
		visitor.walkStatements(declaration.Body.List, functionScope, functionScope)
	}
}

func (visitor *fileVisitor) walkFuncLit(literal *ast.FuncLit, parent *Scope) {
	if literal == nil || visitor.err != nil {
		return
	}
	functionScope := visitor.addScope(literal, parent, "closure", model.ByteRange{})
	if functionScope == nil {
		return
	}
	visitor.addFuncSignature(literal.Type, functionScope)
	if literal.Body != nil {
		visitor.walkStatements(literal.Body.List, functionScope, functionScope)
	}
}

func (visitor *fileVisitor) addFuncSignature(functionType *ast.FuncType, scope *Scope) {
	if functionType == nil || scope == nil {
		return
	}
	visitor.addTypeParameters(functionType.TypeParams, scope)
	visitor.addValueParameters(functionType.Params, scope, "parameter")
	visitor.addValueParameters(functionType.Results, scope, "named_result")
	if functionType.Params != nil {
		for _, field := range functionType.Params.List {
			visitor.walkTypeExpr(field.Type, scope)
		}
	}
	if functionType.Results != nil {
		for _, field := range functionType.Results.List {
			visitor.walkTypeExpr(field.Type, scope)
		}
	}
}

func (visitor *fileVisitor) addReceiverParameters(fields *ast.FieldList, scope *Scope) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		for _, name := range field.Names {
			visitor.addDeclaration(name, field, scope, "receiver", false)
		}
		visitor.walkTypeExpr(field.Type, scope)
	}
}

func (visitor *fileVisitor) addValueParameters(fields *ast.FieldList, scope *Scope, kind string) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		for _, name := range field.Names {
			visitor.addDeclaration(name, field, scope, kind, false)
		}
	}
}

func (visitor *fileVisitor) addTypeParameters(fields *ast.FieldList, scope *Scope) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		for _, name := range field.Names {
			visitor.addDeclaration(name, field, scope, "type_parameter", false)
		}
		visitor.walkTypeExpr(field.Type, scope)
	}
}

func (visitor *fileVisitor) walkTypeExpr(expression ast.Expr, parent *Scope) {
	if expression == nil || parent == nil || visitor.err != nil {
		return
	}
	switch node := expression.(type) {
	case *ast.StructType:
		scope := visitor.addScope(node, parent, "struct", model.ByteRange{})
		if scope == nil || node.Fields == nil {
			return
		}
		for _, field := range node.Fields.List {
			if len(field.Names) != 0 {
				for _, name := range field.Names {
					visitor.addDeclaration(name, field, scope, "field", false)
				}
			} else if name := embeddedFieldName(field.Type); name != nil {
				visitor.addDeclaration(name, field, scope, "field", false)
			}
			visitor.walkTypeExpr(field.Type, scope)
		}
	case *ast.InterfaceType:
		scope := visitor.addScope(node, parent, "interface", model.ByteRange{})
		if scope == nil || node.Methods == nil {
			return
		}
		for _, method := range node.Methods.List {
			for _, name := range method.Names {
				visitor.addDeclaration(name, method, scope, "interface_method", false)
			}
			visitor.walkTypeExpr(method.Type, scope)
		}
	case *ast.FuncType:
		if node.TypeParams != nil {
			for _, field := range node.TypeParams.List {
				visitor.walkTypeExpr(field.Type, parent)
			}
		}
		for _, list := range []*ast.FieldList{node.Params, node.Results} {
			if list == nil {
				continue
			}
			for _, field := range list.List {
				visitor.walkTypeExpr(field.Type, parent)
			}
		}
	case *ast.StarExpr:
		visitor.walkTypeExpr(node.X, parent)
	case *ast.ArrayType:
		visitor.walkTypeExpr(node.Elt, parent)
	case *ast.MapType:
		visitor.walkTypeExpr(node.Key, parent)
		visitor.walkTypeExpr(node.Value, parent)
	case *ast.ChanType:
		visitor.walkTypeExpr(node.Value, parent)
	case *ast.Ellipsis:
		visitor.walkTypeExpr(node.Elt, parent)
	case *ast.ParenExpr:
		visitor.walkTypeExpr(node.X, parent)
	case *ast.SelectorExpr:
		visitor.walkTypeExpr(node.X, parent)
	case *ast.IndexExpr:
		visitor.walkTypeExpr(node.X, parent)
		visitor.walkTypeExpr(node.Index, parent)
	case *ast.IndexListExpr:
		visitor.walkTypeExpr(node.X, parent)
		for _, index := range node.Indices {
			visitor.walkTypeExpr(index, parent)
		}
	case *ast.UnaryExpr:
		visitor.walkTypeExpr(node.X, parent)
	}
}

func embeddedFieldName(expression ast.Expr) *ast.Ident {
	switch node := expression.(type) {
	case *ast.Ident:
		return node
	case *ast.SelectorExpr:
		return node.Sel
	case *ast.StarExpr:
		return embeddedFieldName(node.X)
	case *ast.IndexExpr:
		return embeddedFieldName(node.X)
	case *ast.IndexListExpr:
		return embeddedFieldName(node.X)
	case *ast.ParenExpr:
		return embeddedFieldName(node.X)
	default:
		return nil
	}
}

func (visitor *fileVisitor) walkStatements(statements []ast.Stmt, scope, functionScope *Scope) {
	for _, statement := range statements {
		visitor.walkStmt(statement, scope, functionScope)
		if visitor.err != nil {
			return
		}
	}
}

func (visitor *fileVisitor) walkStmt(statement ast.Stmt, scope, functionScope *Scope) {
	if statement == nil || scope == nil || visitor.err != nil {
		return
	}
	switch node := statement.(type) {
	case *ast.BlockStmt:
		child := visitor.addScope(node, scope, "block", model.ByteRange{})
		if child != nil {
			visitor.walkStatements(node.List, child, functionScope)
		}
	case *ast.DeclStmt:
		if declaration, ok := node.Decl.(*ast.GenDecl); ok {
			visitor.walkGenDecl(declaration, *scope)
		}
	case *ast.AssignStmt:
		if node.Tok == token.DEFINE {
			for _, left := range node.Lhs {
				if name, ok := left.(*ast.Ident); ok {
					visitor.addDeclaration(name, node, scope, "short_var_candidate", true)
				}
			}
		}
		for _, expression := range node.Rhs {
			visitor.walkExpr(expression, scope)
		}
		for _, expression := range node.Lhs {
			if _, ok := expression.(*ast.Ident); !ok {
				visitor.walkExpr(expression, scope)
			}
		}
	case *ast.RangeStmt:
		loopScope := visitor.addScope(node, scope, "range", model.ByteRange{})
		if loopScope == nil {
			return
		}
		if node.Tok == token.DEFINE {
			if name, ok := node.Key.(*ast.Ident); ok {
				visitor.addDeclaration(name, node, loopScope, "range_variable", false)
			}
			if name, ok := node.Value.(*ast.Ident); ok {
				visitor.addDeclaration(name, node, loopScope, "range_variable", false)
			}
		}
		visitor.walkExpr(node.X, loopScope)
		visitor.walkStmt(node.Body, loopScope, functionScope)
	case *ast.IfStmt:
		child := visitor.addScope(node, scope, "if", model.ByteRange{})
		if child == nil {
			return
		}
		visitor.walkStmt(node.Init, child, functionScope)
		visitor.walkExpr(node.Cond, child)
		visitor.walkStmt(node.Body, child, functionScope)
		visitor.walkStmt(node.Else, child, functionScope)
	case *ast.ForStmt:
		child := visitor.addScope(node, scope, "for", model.ByteRange{})
		if child == nil {
			return
		}
		visitor.walkStmt(node.Init, child, functionScope)
		visitor.walkExpr(node.Cond, child)
		visitor.walkStmt(node.Post, child, functionScope)
		visitor.walkStmt(node.Body, child, functionScope)
	case *ast.SwitchStmt:
		child := visitor.addScope(node, scope, "switch", model.ByteRange{})
		if child == nil {
			return
		}
		visitor.walkStmt(node.Init, child, functionScope)
		visitor.walkExpr(node.Tag, child)
		visitor.walkCaseClauses(node.Body, child, functionScope)
	case *ast.TypeSwitchStmt:
		child := visitor.addScope(node, scope, "type_switch", model.ByteRange{})
		if child == nil {
			return
		}
		if assignment, ok := node.Assign.(*ast.AssignStmt); ok && assignment.Tok == token.DEFINE {
			for _, left := range assignment.Lhs {
				if name, ok := left.(*ast.Ident); ok {
					visitor.addDeclaration(name, assignment, child, "type_switch_variable", false)
				}
			}
			for _, expression := range assignment.Rhs {
				visitor.walkExpr(expression, child)
			}
		} else {
			visitor.walkStmt(node.Assign, child, functionScope)
		}
		visitor.walkCaseClauses(node.Body, child, functionScope)
	case *ast.CaseClause:
		visitor.walkCaseClause(node, scope, functionScope)
	case *ast.SelectStmt:
		child := visitor.addScope(node, scope, "select", model.ByteRange{})
		if child != nil && node.Body != nil {
			for _, statement := range node.Body.List {
				clause, ok := statement.(*ast.CommClause)
				if !ok {
					continue
				}
				clauseScope := visitor.addScope(clause, child, "select_case", model.ByteRange{})
				visitor.walkStmt(clause.Comm, clauseScope, functionScope)
				visitor.walkStatements(clause.Body, clauseScope, functionScope)
			}
		}
	case *ast.LabeledStmt:
		visitor.addDeclaration(node.Label, node, functionScope, "label", false)
		visitor.walkStmt(node.Stmt, scope, functionScope)
	case *ast.ExprStmt:
		visitor.walkExpr(node.X, scope)
	case *ast.GoStmt:
		visitor.walkExpr(node.Call, scope)
	case *ast.DeferStmt:
		visitor.walkExpr(node.Call, scope)
	case *ast.ReturnStmt:
		for _, expression := range node.Results {
			visitor.walkExpr(expression, scope)
		}
	case *ast.SendStmt:
		visitor.walkExpr(node.Chan, scope)
		visitor.walkExpr(node.Value, scope)
	case *ast.IncDecStmt:
		visitor.walkExpr(node.X, scope)
	case *ast.CommClause:
		visitor.walkStmt(node.Comm, scope, functionScope)
		visitor.walkStatements(node.Body, scope, functionScope)
	}
}

func (visitor *fileVisitor) walkCaseClauses(block *ast.BlockStmt, switchScope, functionScope *Scope) {
	if block == nil {
		return
	}
	for _, statement := range block.List {
		clause, ok := statement.(*ast.CaseClause)
		if ok {
			visitor.walkCaseClause(clause, switchScope, functionScope)
		}
	}
}

func (visitor *fileVisitor) walkCaseClause(clause *ast.CaseClause, switchScope, functionScope *Scope) {
	if clause == nil || switchScope == nil {
		return
	}
	for _, expression := range clause.List {
		visitor.walkExpr(expression, switchScope)
	}
	caseScope := visitor.addScope(clause, switchScope, "case", model.ByteRange{})
	visitor.walkStatements(clause.Body, caseScope, functionScope)
}

func (visitor *fileVisitor) walkExpr(expression ast.Expr, scope *Scope) {
	if expression == nil || scope == nil || visitor.err != nil {
		return
	}
	ast.Inspect(expression, func(node ast.Node) bool {
		if visitor.err != nil || node == nil {
			return visitor.err == nil
		}
		switch current := node.(type) {
		case *ast.FuncLit:
			visitor.walkFuncLit(current, scope)
			return false
		case *ast.CompositeLit:
			visitor.walkTypeExpr(current.Type, scope)
		}
		return visitor.err == nil
	})
}

func sortFacts(facts *PackageFacts) {
	sort.Slice(facts.Files, func(i, j int) bool { return facts.Files[i].Path < facts.Files[j].Path })
	sort.Slice(facts.Scopes, func(i, j int) bool {
		left, right := facts.Scopes[i], facts.Scopes[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Range.StartByte != right.Range.StartByte {
			return left.Range.StartByte < right.Range.StartByte
		}
		if left.Range.EndByte != right.Range.EndByte {
			return left.Range.EndByte < right.Range.EndByte
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.ID < right.ID
	})
	sort.Slice(facts.Declarations, func(i, j int) bool {
		left, right := facts.Declarations[i], facts.Declarations[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.NameRange.StartByte != right.NameRange.StartByte {
			return left.NameRange.StartByte < right.NameRange.StartByte
		}
		if left.NameRange.EndByte != right.NameRange.EndByte {
			return left.NameRange.EndByte < right.NameRange.EndByte
		}
		return left.SymbolID < right.SymbolID
	})
	sort.Slice(facts.Imports, func(i, j int) bool {
		if facts.Imports[i].PathRange.StartByte != facts.Imports[j].PathRange.StartByte {
			return facts.Imports[i].PathRange.StartByte < facts.Imports[j].PathRange.StartByte
		}
		return facts.Imports[i].Path < facts.Imports[j].Path
	})
	sort.Slice(facts.Diagnostics, func(i, j int) bool {
		left, right := facts.Diagnostics[i], facts.Diagnostics[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Range.StartByte != right.Range.StartByte {
			return left.Range.StartByte < right.Range.StartByte
		}
		return left.Code < right.Code
	})
}
