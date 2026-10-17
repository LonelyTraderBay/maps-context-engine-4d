package golang

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"golang.org/x/tools/go/packages"
)

type TypedStatus string

const (
	TypedNotRequested TypedStatus = "not_requested"
	TypedComplete     TypedStatus = "complete"
	TypedPartial      TypedStatus = "partial"
)

type DefFact struct {
	SymbolID model.Hash
	Path     string
	Name     string
	Range    model.ByteRange
}

type UseFact struct {
	SymbolID model.Hash
	Path     string
	Name     string
	Range    model.ByteRange
}

type SelectionFact struct {
	SymbolID model.Hash
	Path     string
	Name     string
	Range    model.ByteRange
	Kind     string
	Index    []int
	Indirect bool
}

// LoadedPackageInput binds captured source facts to one exact package object
// from a LoadRun. The package pointer is explicit so test variants are never
// selected by import path alone.
type LoadedPackageInput struct {
	Input   PackageInput
	Package *packages.Package
}

var ErrLoadRunMismatch = errors.New("Go typed package does not belong to this LoadRun")

type typedSite struct {
	path   string
	name   string
	range_ model.ByteRange
}

type typedSiteKey struct {
	path  string
	name  string
	start int64
	end   int64
}

// AnalyzeLoadedPackage adds type facts from the exact package owned by run.
// Source bytes and go/types identities are checked before they can be joined
// with syntax declarations.
func AnalyzeLoadedPackage(input PackageInput, run *LoadRun, pkg *packages.Package) (PackageFacts, error) {
	return analyzeLoadedPackage(input, run, pkg, nil, nil, []*packages.Package{pkg})
}

// AnalyzeLoadedPackages analyzes an explicitly selected set from one LoadRun.
// It links cross-package typed targets to local declaration IDs when those
// target packages are included, while keeping absent packages as stable typed
// object identities.
func AnalyzeLoadedPackages(inputs []LoadedPackageInput, run *LoadRun) (Analysis, error) {
	if run == nil || len(inputs) == 0 {
		return Analysis{}, fmt.Errorf("%w: a LoadRun and at least one package are required", ErrLoadRunMismatch)
	}
	candidatePackages := make([]*packages.Package, 0, len(inputs))
	bases := make([]PackageFacts, len(inputs))
	aliases := make(map[types.Object]model.Hash)
	seenPackages := make(map[*types.Package]struct{}, len(inputs))
	seenPackagePointers := make(map[*packages.Package]struct{}, len(inputs))
	generationID := inputs[0].Input.GenerationID
	for index, loaded := range inputs {
		input, pkg := loaded.Input, loaded.Package
		if pkg == nil || input.ProfileID != run.ProfileID || input.GenerationID != generationID ||
			!runOwnsPackage(run, pkg) || !packageIdentityMatches(input, pkg) {
			return Analysis{}, ErrLoadRunMismatch
		}
		if _, duplicate := seenPackagePointers[pkg]; duplicate {
			return Analysis{}, fmt.Errorf("%w: duplicate loaded package input", ErrLoadRunMismatch)
		}
		seenPackagePointers[pkg] = struct{}{}
		if pkg.Types != nil {
			if _, owned := run.typesRegistry[pkg.Types]; !owned {
				return Analysis{}, ErrLoadRunMismatch
			}
			if _, duplicate := seenPackages[pkg.Types]; duplicate {
				return Analysis{}, fmt.Errorf("%w: duplicate loaded package input", ErrLoadRunMismatch)
			}
			seenPackages[pkg.Types] = struct{}{}
		}
		for _, file := range input.Files {
			if run.capturedSourceHashes[file.Path] != model.SHA256(file.Source) {
				return Analysis{}, fmt.Errorf("%w: %s", ErrCapturedSource, file.Path)
			}
		}
		base, err := AnalyzePackage(input)
		if err != nil {
			return Analysis{}, err
		}
		bases[index] = base
		candidatePackages = append(candidatePackages, pkg)
		if pkg.Types == nil || pkg.TypesInfo == nil || pkg.Fset == nil || pkg.TypesInfo.Defs == nil {
			continue
		}
		captured := capturedFiles(input)
		declarations := declarationIDsBySite(base.Declarations)
		for ident, object := range pkg.TypesInfo.Defs {
			if ident == nil || object == nil || ident.Name == "_" {
				continue
			}
			site, err := typedIdentifierSite(run, pkg.Fset, captured, ident)
			if err != nil {
				return Analysis{}, err
			}
			id, ok := declarations[typedSiteKey{path: site.path, name: site.name, start: site.range_.StartByte, end: site.range_.EndByte}]
			if !ok {
				continue
			}
			if previous, exists := aliases[object]; exists && previous != id {
				return Analysis{}, fmt.Errorf("%w: one type object maps to multiple syntax declarations", ErrLoadRunMismatch)
			}
			aliases[object] = id
		}
	}

	packagesFacts := make([]PackageFacts, len(inputs))
	for index, loaded := range inputs {
		facts, err := analyzeLoadedPackage(loaded.Input, run, loaded.Package, &bases[index], aliases, candidatePackages)
		if err != nil {
			return Analysis{}, err
		}
		packagesFacts[index] = facts
	}
	sort.Slice(packagesFacts, func(i, j int) bool { return packagesFacts[i].Key < packagesFacts[j].Key })
	occurrences := declarationOccurrences(packagesFacts)
	relationOccurrences, err := aggregateRelationOccurrences(packagesFacts)
	if err != nil {
		return Analysis{}, err
	}
	return Analysis{Packages: packagesFacts, Occurrences: occurrences, RelationOccurrences: relationOccurrences}, nil
}

func analyzeLoadedPackage(input PackageInput, run *LoadRun, pkg *packages.Package, base *PackageFacts,
	externalSymbols map[types.Object]model.Hash, candidatePackages []*packages.Package) (PackageFacts, error) {
	var facts PackageFacts
	var err error
	if base == nil {
		facts, err = AnalyzePackage(input)
	} else {
		facts = *base
	}
	if err != nil {
		return PackageFacts{}, err
	}
	if run == nil || pkg == nil || input.ProfileID != run.ProfileID || !runOwnsPackage(run, pkg) {
		return PackageFacts{}, ErrLoadRunMismatch
	}
	for _, file := range input.Files {
		if run.capturedSourceHashes[file.Path] != model.SHA256(file.Source) {
			return PackageFacts{}, fmt.Errorf("%w: %s", ErrCapturedSource, file.Path)
		}
	}
	if !packageIdentityMatches(input, pkg) {
		return PackageFacts{}, ErrLoadRunMismatch
	}
	if pkg.Types == nil || pkg.TypesInfo == nil || pkg.Fset == nil {
		return partialTypedFacts(facts), nil
	}
	if _, owned := run.typesRegistry[pkg.Types]; !owned {
		return PackageFacts{}, ErrLoadRunMismatch
	}
	if pkg.TypesInfo.Defs == nil || pkg.TypesInfo.Uses == nil || pkg.TypesInfo.Selections == nil {
		return partialTypedFacts(facts), nil
	}

	captured := make(map[string]CapturedFile, len(input.Files))
	for _, file := range input.Files {
		captured[file.Path] = file
	}
	if len(pkg.Syntax) == 0 {
		return partialTypedFacts(facts), nil
	}
	for _, syntaxFile := range pkg.Syntax {
		if syntaxFile == nil {
			return PackageFacts{}, fmt.Errorf("%w: loaded package has an empty syntax file", ErrCapturedSource)
		}
		position := pkg.Fset.PositionFor(syntaxFile.Pos(), false)
		relative, ok := run.capturedPathByFile[loadPathKey(position.Filename)]
		source, capturedInInput := captured[relative]
		if !position.IsValid() || !ok || !capturedInInput || run.capturedSourceHashes[relative] != model.SHA256(source.Source) {
			return PackageFacts{}, fmt.Errorf("%w: loaded AST is outside the captured source map", ErrCapturedSource)
		}
	}

	declarations := make(map[typedSiteKey]model.Hash, len(facts.Declarations))
	for _, declaration := range facts.Declarations {
		declarations[typedSiteKey{
			path: declaration.Path, name: declaration.Name,
			start: declaration.NameRange.StartByte, end: declaration.NameRange.EndByte,
		}] = declaration.SymbolID
	}
	objectIDs := make(map[types.Object]model.Hash, len(pkg.TypesInfo.Defs))
	complete := !pkg.IllTyped && len(pkg.Errors) == 0 && len(pkg.TypeErrors) == 0

	for ident, object := range pkg.TypesInfo.Defs {
		if ident == nil || object == nil || ident.Name == "_" {
			continue
		}
		site, err := typedIdentifierSite(run, pkg.Fset, captured, ident)
		if err != nil {
			return PackageFacts{}, err
		}
		symbolID, found := declarations[typedSiteKey{
			path: site.path, name: site.name,
			start: site.range_.StartByte, end: site.range_.EndByte,
		}]
		if !found {
			symbolID, err = typedLocalSymbolID(facts, site, object)
			if err != nil {
				return PackageFacts{}, err
			}
			complete = false
			facts.Diagnostics = append(facts.Diagnostics, Diagnostic{
				Path: site.path, Code: "GO_TYPED_DECLARATION_NOT_IN_SYNTAX", Severity: "warning",
				Message: "typed declaration was not matched to a syntax symbol identity", Range: site.range_,
			})
		}
		objectIDs[object] = symbolID
		facts.Defs = append(facts.Defs, DefFact{SymbolID: symbolID, Path: site.path, Name: site.name, Range: site.range_})
	}

	selectionByIdent := make(map[*ast.Ident]*types.Selection, len(pkg.TypesInfo.Selections))
	for expression, selection := range pkg.TypesInfo.Selections {
		if expression != nil && selection != nil && expression.Sel != nil {
			selectionByIdent[expression.Sel] = selection
		}
	}
	objectID := func(object types.Object, selection *types.Selection) (model.Hash, error) {
		if id, ok := objectIDs[object]; ok {
			return id, nil
		}
		if id, ok := externalSymbols[object]; ok {
			objectIDs[object] = id
			return id, nil
		}
		if object == nil {
			return "", errors.New("Go type information contains an empty object")
		}
		var receiver types.Type
		if variable, ok := object.(*types.Var); ok && variable.IsField() {
			var err error
			receiver, err = selectionOwnerType(selection)
			if err != nil {
				return "", err
			}
		}
		if object.Pos().IsValid() {
			position := pkg.Fset.PositionFor(object.Pos(), false)
			if relative, ok := run.capturedPathByFile[loadPathKey(position.Filename)]; ok {
				if source, exists := captured[relative]; exists {
					start := int64(position.Offset)
					end := start + int64(len(object.Name()))
					if start >= 0 && end <= int64(len(source.Source)) {
						if id, exists := declarations[typedSiteKey{path: relative, name: object.Name(), start: start, end: end}]; exists {
							objectIDs[object] = id
							return id, nil
						}
					}
				}
			}
		}
		id, err := typedObjectID(object, receiver, run)
		if err == nil {
			objectIDs[object] = id
		}
		return id, err
	}

	for ident, object := range pkg.TypesInfo.Uses {
		if ident == nil || object == nil || ident.Name == "_" {
			continue
		}
		site, err := typedIdentifierSite(run, pkg.Fset, captured, ident)
		if err != nil {
			return PackageFacts{}, err
		}
		symbolID, err := objectID(object, selectionByIdent[ident])
		if err != nil {
			return PackageFacts{}, err
		}
		facts.Uses = append(facts.Uses, UseFact{SymbolID: symbolID, Path: site.path, Name: site.name, Range: site.range_})
	}

	for expression, selection := range pkg.TypesInfo.Selections {
		if expression == nil || selection == nil || expression.Sel == nil {
			continue
		}
		site, err := typedIdentifierSite(run, pkg.Fset, captured, expression.Sel)
		if err != nil {
			return PackageFacts{}, err
		}
		symbolID, err := objectID(selection.Obj(), selection)
		if err != nil {
			return PackageFacts{}, err
		}
		facts.Selections = append(facts.Selections, SelectionFact{
			SymbolID: symbolID, Path: site.path, Name: site.name, Range: site.range_,
			Kind: selectionKind(selection.Kind()), Index: append([]int(nil), selection.Index()...), Indirect: selection.Indirect(),
		})
	}
	if err := collectCallRelations(input, run, pkg, &facts, objectID, candidatePackages); err != nil {
		return PackageFacts{}, err
	}

	facts.TypedStatus = TypedComplete
	if !complete {
		facts.TypedStatus = TypedPartial
	}
	if pkg.IllTyped || len(pkg.Errors) > 0 || len(pkg.TypeErrors) > 0 {
		facts.Diagnostics = append(facts.Diagnostics, Diagnostic{
			Code: "GO_TYPES_INCOMPLETE", Severity: "warning",
			Message: "typed facts are partial because the package or a dependency has type-check errors",
		})
	}
	sortFacts(&facts)
	sortTypedFacts(&facts)
	return facts, nil
}

func selectionKind(kind types.SelectionKind) string {
	switch kind {
	case types.FieldVal:
		return "field_value"
	case types.MethodVal:
		return "method_value"
	case types.MethodExpr:
		return "method_expression"
	default:
		return "unknown"
	}
}

func partialTypedFacts(facts PackageFacts) PackageFacts {
	facts.TypedStatus = TypedPartial
	facts.Diagnostics = append(facts.Diagnostics, Diagnostic{
		Code: "GO_TYPES_UNAVAILABLE", Severity: "warning",
		Message: "complete Go type information is unavailable for this package load",
	})
	return facts
}

func runOwnsPackage(run *LoadRun, target *packages.Package) bool {
	if run == nil || target == nil {
		return false
	}
	seen := make(map[*packages.Package]struct{})
	stack := append([]*packages.Package(nil), run.packages...)
	for len(stack) > 0 {
		pkg := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if pkg == nil {
			continue
		}
		if pkg == target {
			return true
		}
		if _, visited := seen[pkg]; visited {
			continue
		}
		seen[pkg] = struct{}{}
		for _, imported := range pkg.Imports {
			stack = append(stack, imported)
		}
	}
	return false
}

func packageIdentityMatches(input PackageInput, pkg *packages.Package) bool {
	if pkg == nil {
		return false
	}
	if pkg.Types != nil {
		return pkg.Types.Path() == input.ImportPath
	}
	return pkg.ID == input.ImportPath || strings.HasPrefix(pkg.ID, input.ImportPath+" [")
}

func typedIdentifierSite(run *LoadRun, fset *token.FileSet, captured map[string]CapturedFile, ident *ast.Ident) (typedSite, error) {
	if ident == nil || fset == nil {
		return typedSite{}, ErrCapturedSource
	}
	position := fset.PositionFor(ident.Pos(), false)
	tokenFile := fset.File(ident.Pos())
	if !position.IsValid() || tokenFile == nil {
		return typedSite{}, ErrCapturedSource
	}
	relative, ok := run.capturedPathByFile[loadPathKey(position.Filename)]
	if !ok {
		return typedSite{}, fmt.Errorf("%w: loaded Go file is outside the captured source map", ErrCapturedSource)
	}
	file, ok := captured[relative]
	if !ok || run.capturedSourceHashes[relative] != model.SHA256(file.Source) {
		return typedSite{}, fmt.Errorf("%w: %s", ErrCapturedSource, relative)
	}
	start := tokenFile.Offset(ident.Pos())
	end := tokenFile.Offset(ident.End())
	byteRange := model.ByteRange{StartByte: int64(start), EndByte: int64(end)}
	if err := byteRange.ValidateAgainst(file.Source); err != nil || start < 0 || end > len(file.Source) || string(file.Source[start:end]) != ident.Name {
		return typedSite{}, fmt.Errorf("%w: typed identifier range does not select captured name", ErrCapturedSource)
	}
	return typedSite{path: relative, name: ident.Name, range_: byteRange}, nil
}

func typedLocalSymbolID(facts PackageFacts, site typedSite, object types.Object) (model.Hash, error) {
	return model.HashValue("go-typed-local-symbol-v1", map[string]any{
		"package_key": facts.Key, "path": site.path, "name": site.name,
		"start_byte": site.range_.StartByte, "end_byte": site.range_.EndByte,
		"object_kind": fmt.Sprintf("%T", object),
	})
}

func typedObjectID(object types.Object, receiver types.Type, run *LoadRun) (model.Hash, error) {
	if object == nil || run == nil {
		return "", ErrLoadRunMismatch
	}
	packagePath := ""
	if object.Pkg() != nil {
		packagePath = object.Pkg().Path()
	}
	qualifier := func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		return pkg.Path()
	}
	receiverType := ""
	if receiver != nil {
		receiverType = types.TypeString(receiver, qualifier)
	}
	if function, ok := object.(*types.Func); ok {
		if signature, ok := function.Type().(*types.Signature); ok && signature.Recv() != nil {
			receiverType = types.TypeString(signature.Recv().Type(), qualifier)
		}
	}
	return model.HashValue("go-typed-object-v1", map[string]any{
		"dependency_digest": run.DependencyDigest, "package_path": packagePath,
		"object_kind": fmt.Sprintf("%T", object), "name": object.Name(),
		"type": types.TypeString(object.Type(), qualifier), "receiver_type": receiverType,
	})
}

func selectionOwnerType(selection *types.Selection) (types.Type, error) {
	if selection == nil {
		return nil, errors.New("field selection is missing its type selection")
	}
	owner := selection.Recv()
	indices := selection.Index()
	for position, index := range indices {
		if position == len(indices)-1 {
			break
		}
		structure := underlyingStruct(owner)
		if structure == nil || index < 0 || index >= structure.NumFields() {
			return nil, errors.New("field selection index is outside the receiver type")
		}
		owner = structure.Field(index).Type()
	}
	owner = types.Unalias(owner)
	for {
		pointer, ok := owner.(*types.Pointer)
		if !ok {
			break
		}
		owner = types.Unalias(pointer.Elem())
	}
	if named, ok := owner.(*types.Named); ok {
		return named.Origin(), nil
	}
	return owner, nil
}

func underlyingStruct(typ types.Type) *types.Struct {
	typ = types.Unalias(typ)
	for {
		pointer, ok := typ.(*types.Pointer)
		if !ok {
			break
		}
		typ = types.Unalias(pointer.Elem())
	}
	if named, ok := typ.(*types.Named); ok {
		typ = named.Underlying()
	}
	structure, _ := typ.Underlying().(*types.Struct)
	return structure
}

func loadPathKey(path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func sortTypedFacts(facts *PackageFacts) {
	sort.Slice(facts.Defs, func(i, j int) bool {
		return typedFactLess(facts.Defs[i].Path, facts.Defs[i].Range, facts.Defs[i].SymbolID, facts.Defs[j].Path, facts.Defs[j].Range, facts.Defs[j].SymbolID)
	})
	sort.Slice(facts.Uses, func(i, j int) bool {
		return typedFactLess(facts.Uses[i].Path, facts.Uses[i].Range, facts.Uses[i].SymbolID, facts.Uses[j].Path, facts.Uses[j].Range, facts.Uses[j].SymbolID)
	})
	sort.Slice(facts.Selections, func(i, j int) bool {
		return typedFactLess(facts.Selections[i].Path, facts.Selections[i].Range, facts.Selections[i].SymbolID, facts.Selections[j].Path, facts.Selections[j].Range, facts.Selections[j].SymbolID)
	})
}

func typedFactLess(leftPath string, leftRange model.ByteRange, leftID model.Hash, rightPath string, rightRange model.ByteRange, rightID model.Hash) bool {
	if leftPath != rightPath {
		return leftPath < rightPath
	}
	if leftRange.StartByte != rightRange.StartByte {
		return leftRange.StartByte < rightRange.StartByte
	}
	if leftRange.EndByte != rightRange.EndByte {
		return leftRange.EndByte < rightRange.EndByte
	}
	return leftID < rightID
}
