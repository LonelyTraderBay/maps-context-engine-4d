package golang

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"unicode/utf8"

	indexerfacts "github.com/LonelyTraderBay/maps-context-engine-4d/internal/indexer/facts"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"golang.org/x/tools/go/packages"
)

type objectIDResolver func(types.Object, *types.Selection) (model.Hash, error)

func capturedFiles(input PackageInput) map[string]CapturedFile {
	captured := make(map[string]CapturedFile, len(input.Files))
	for _, file := range input.Files {
		captured[file.Path] = file
	}
	return captured
}

func declarationIDsBySite(declarations []Declaration) map[typedSiteKey]model.Hash {
	result := make(map[typedSiteKey]model.Hash, len(declarations))
	for _, declaration := range declarations {
		result[typedSiteKey{
			path: declaration.Path, name: declaration.Name,
			start: declaration.NameRange.StartByte, end: declaration.NameRange.EndByte,
		}] = declaration.SymbolID
	}
	return result
}

func collectCallRelations(input PackageInput, run *LoadRun, pkg *packages.Package, facts *PackageFacts,
	objectID objectIDResolver, candidatePackages []*packages.Package) error {
	if facts == nil || pkg == nil || pkg.Fset == nil || pkg.TypesInfo == nil {
		return nil
	}
	captured := capturedFiles(input)
	declarations := declarationIDsBySite(facts.Declarations)
	functionOwners := make(map[*ast.FuncDecl]model.Hash)
	closureOwners := make(map[typedSiteKey]model.Hash)
	for _, syntaxFile := range pkg.Syntax {
		if syntaxFile == nil {
			return fmt.Errorf("%w: loaded package has an empty syntax file", ErrCapturedSource)
		}
		position := pkg.Fset.PositionFor(syntaxFile.Pos(), false)
		path, ok := run.capturedPathByFile[loadPathKey(position.Filename)]
		if !position.IsValid() || !ok {
			return fmt.Errorf("%w: relation AST is outside the captured source map", ErrCapturedSource)
		}
		for _, declaration := range facts.Declarations {
			if declaration.Path == path && declaration.Kind == "closure" {
				closureOwners[typedSiteKey{path: path, start: declaration.DeclarationRange.StartByte, end: declaration.DeclarationRange.EndByte}] = declaration.SymbolID
			}
		}
		ast.Inspect(syntaxFile, func(node ast.Node) bool {
			function, ok := node.(*ast.FuncDecl)
			if !ok || function.Name == nil {
				return true
			}
			tokenFile := pkg.Fset.File(function.Name.Pos())
			if tokenFile == nil {
				return true
			}
			start, end := int64(tokenFile.Offset(function.Name.Pos())), int64(tokenFile.Offset(function.Name.End()))
			if id, found := declarations[typedSiteKey{path: path, name: function.Name.Name, start: start, end: end}]; found {
				functionOwners[function] = id
			}
			return true
		})
	}

	for _, syntaxFile := range pkg.Syntax {
		position := pkg.Fset.PositionFor(syntaxFile.Pos(), false)
		path := run.capturedPathByFile[loadPathKey(position.Filename)]
		file, ok := captured[path]
		if !ok || run.capturedSourceHashes[path] != model.SHA256(file.Source) {
			return fmt.Errorf("%w: relation source changed for %s", ErrCapturedSource, path)
		}
		tokenFile := pkg.Fset.File(syntaxFile.Pos())
		if tokenFile == nil {
			return fmt.Errorf("%w: relation file has no token file", ErrCapturedSource)
		}
		owners := make([]model.Hash, 0, 4)
		depths := make([]int, 0, 64)
		var visitErr error
		ast.Inspect(syntaxFile, func(node ast.Node) bool {
			if visitErr != nil {
				return false
			}
			if node == nil {
				if len(depths) > 0 {
					depth := depths[len(depths)-1]
					depths = depths[:len(depths)-1]
					owners = owners[:depth]
				}
				return true
			}
			depths = append(depths, len(owners))
			switch current := node.(type) {
			case *ast.FuncDecl:
				if owner, exists := functionOwners[current]; exists {
					owners = append(owners, owner)
				}
			case *ast.FuncLit:
				start, end := int64(tokenFile.Offset(current.Pos())), int64(tokenFile.Offset(current.End()))
				if owner, exists := closureOwners[typedSiteKey{path: path, start: start, end: end}]; exists {
					owners = append(owners, owner)
				}
			case *ast.CallExpr:
				sourceID := facts.Key
				if len(owners) > 0 {
					sourceID = owners[len(owners)-1]
				}
				range_ := model.ByteRange{StartByte: int64(tokenFile.Offset(current.Pos())), EndByte: int64(tokenFile.Offset(current.End()))}
				if err := range_.ValidateAgainst(file.Source); err != nil {
					visitErr = fmt.Errorf("%w: callsite range does not bind captured source %s", ErrCapturedSource, path)
					return false
				}
				if err := appendCallRelations(pkg, facts, objectID, candidatePackages,
					file, path, range_, sourceID, current); err != nil {
					visitErr = err
					return false
				}
			}
			return true
		})
		if visitErr != nil {
			return visitErr
		}
	}
	sortRelationFacts(facts.Relations)
	return nil
}

func appendCallRelations(pkg *packages.Package, facts *PackageFacts,
	objectID objectIDResolver, candidatePackages []*packages.Package, source CapturedFile,
	path string, site model.ByteRange, caller model.Hash, call *ast.CallExpr) error {
	if literal, ok := call.Fun.(*ast.FuncLit); ok {
		target, found := closureSymbolAt(facts.Declarations, pkg.Fset, path, literal)
		if !found {
			return fmt.Errorf("%w: direct function literal has no closure symbol", ErrInvalidInput)
		}
		return appendRelation(facts, indexerfacts.RelationInput{
			SourceSymbolID: caller, TargetSymbolID: &target,
			Kind: indexerfacts.RelationCallsDirect, EvidenceKind: indexerfacts.EvidenceSyntaxExact,
			SitePath: path, Range: site, Reason: "callsite names this function literal directly",
		}, source.Source)
	}

	object, selection := calledObject(call.Fun, pkg.TypesInfo)
	if _, builtin := object.(*types.Builtin); builtin {
		return nil
	}
	if tv, exists := pkg.TypesInfo.Types[call.Fun]; exists && tv.IsType() {
		return nil // A type conversion is syntax that looks like a call, not a call edge.
	}
	function, static := object.(*types.Func)
	if !static {
		label := boundedCallLabel(call.Fun)
		return appendRelation(facts, indexerfacts.RelationInput{
			SourceSymbolID: caller, Kind: indexerfacts.RelationUnresolvedCall,
			EvidenceKind: indexerfacts.EvidenceUnresolved, SitePath: path, Range: site,
			Reason:          "go/types did not prove a static callable target; runtime target remains unknown",
			UnresolvedLabel: &label,
		}, source.Source)
	}
	target, err := objectID(function, selection)
	if err != nil {
		return fmt.Errorf("resolve Go call target %q: %w", function.Name(), err)
	}
	interfaceType := interfaceCallType(selection, function)
	kind := indexerfacts.RelationCallsDirect
	reason := "go/types resolved a static function or method target"
	if interfaceType != nil {
		kind = indexerfacts.RelationCallsInterface
		reason = "go/types resolved the interface method; concrete runtime dispatch is not known"
	}
	if err := appendRelation(facts, indexerfacts.RelationInput{
		SourceSymbolID: caller, TargetSymbolID: &target, Kind: kind,
		EvidenceKind: indexerfacts.EvidenceTypeResolved, SitePath: path,
		Range: site, Reason: reason,
	}, source.Source); err != nil {
		return err
	}
	if interfaceType != nil {
		if err := appendDispatchCandidates(facts, objectID, candidatePackages,
			caller, target, function, interfaceType, path, site, source.Source); err != nil {
			return err
		}
	}
	if isReflectInvocation(function) {
		label := "reflect.Value." + function.Name()
		return appendRelation(facts, indexerfacts.RelationInput{
			SourceSymbolID: caller, Kind: indexerfacts.RelationUnresolvedCall,
			EvidenceKind: indexerfacts.EvidenceUnresolved, SitePath: path, Range: site,
			Reason:          "reflection invokes a function value whose concrete runtime target is not statically known",
			UnresolvedLabel: &label,
		}, source.Source)
	}
	return nil
}

func appendDispatchCandidates(facts *PackageFacts, objectID objectIDResolver, candidatePackages []*packages.Package,
	caller, interfaceMethodID model.Hash, interfaceMethod *types.Func, interfaceType *types.Interface,
	path string, site model.ByteRange, source []byte) error {
	if interfaceMethod == nil {
		return nil
	}
	seen := make(map[model.Hash]struct{})
	for _, candidatePackage := range candidatePackages {
		if candidatePackage == nil || candidatePackage.Types == nil {
			continue
		}
		scope := candidatePackage.Types.Scope()
		if scope == nil {
			continue
		}
		for _, name := range scope.Names() {
			typeName, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := types.Unalias(typeName.Type()).(*types.Named)
			if !ok {
				continue
			}
			if _, isInterface := named.Underlying().(*types.Interface); isInterface {
				continue
			}
			for _, candidateType := range []types.Type{named, types.NewPointer(named)} {
				if !types.Implements(candidateType, interfaceType) {
					continue
				}
				selection := types.NewMethodSet(candidateType).Lookup(interfaceMethod.Pkg(), interfaceMethod.Name())
				if selection == nil {
					continue
				}
				function, ok := selection.Obj().(*types.Func)
				if !ok {
					continue
				}
				target, err := objectID(function, selection)
				if err != nil {
					return fmt.Errorf("resolve candidate method %q: %w", function.Name(), err)
				}
				if target == interfaceMethodID {
					continue
				}
				if _, exists := seen[target]; exists {
					continue
				}
				seen[target] = struct{}{}
				if err := appendRelation(facts, indexerfacts.RelationInput{
					SourceSymbolID: caller, TargetSymbolID: &target,
					Kind: indexerfacts.RelationMayDispatchTo, EvidenceKind: indexerfacts.EvidenceTypePossible,
					SitePath: path, Range: site,
					Reason: "candidate type implements the interface; runtime dispatch was not observed",
				}, source); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func appendRelation(facts *PackageFacts, input indexerfacts.RelationInput, source []byte) error {
	relation, err := indexerfacts.NewRelationFact(input, source)
	if err != nil {
		return fmt.Errorf("create Go relation fact: %w", err)
	}
	facts.Relations = append(facts.Relations, relation)
	return nil
}

func calledObject(expression ast.Expr, info *types.Info) (types.Object, *types.Selection) {
	if info == nil || expression == nil {
		return nil, nil
	}
	switch current := expression.(type) {
	case *ast.Ident:
		if object := info.Uses[current]; object != nil {
			return object, nil
		}
		return info.Defs[current], nil
	case *ast.SelectorExpr:
		if selection := info.Selections[current]; selection != nil {
			return selection.Obj(), selection
		}
		return info.Uses[current.Sel], nil
	case *ast.IndexExpr:
		return calledObject(current.X, info)
	case *ast.IndexListExpr:
		return calledObject(current.X, info)
	case *ast.ParenExpr:
		return calledObject(current.X, info)
	default:
		return nil, nil
	}
}

func interfaceCallType(selection *types.Selection, function *types.Func) *types.Interface {
	if selection != nil {
		if interfaceType := underlyingInterface(selection.Recv()); interfaceType != nil {
			return interfaceType
		}
	}
	if function != nil {
		if signature, ok := function.Type().(*types.Signature); ok && signature.Recv() != nil {
			return underlyingInterface(signature.Recv().Type())
		}
	}
	return nil
}

func underlyingInterface(value types.Type) *types.Interface {
	if value == nil {
		return nil
	}
	value = types.Unalias(value)
	if pointer, ok := value.(*types.Pointer); ok {
		value = types.Unalias(pointer.Elem())
	}
	interfaceType, ok := value.Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	interfaceType.Complete()
	return interfaceType
}

func closureSymbolAt(declarations []Declaration, fset *token.FileSet, path string, literal *ast.FuncLit) (model.Hash, bool) {
	if fset == nil || literal == nil {
		return "", false
	}
	tokenFile := fset.File(literal.Pos())
	if tokenFile == nil {
		return "", false
	}
	start, end := int64(tokenFile.Offset(literal.Pos())), int64(tokenFile.Offset(literal.End()))
	for _, declaration := range declarations {
		if declaration.Kind == "closure" && declaration.Path == path &&
			declaration.DeclarationRange.StartByte == start && declaration.DeclarationRange.EndByte == end {
			return declaration.SymbolID, true
		}
	}
	return "", false
}

func callLabel(expression ast.Expr) string {
	switch current := expression.(type) {
	case *ast.Ident:
		return current.Name
	case *ast.SelectorExpr:
		if receiver, ok := current.X.(*ast.Ident); ok {
			return receiver.Name + "." + current.Sel.Name
		}
		return "selector." + current.Sel.Name
	case *ast.IndexExpr:
		return callLabel(current.X)
	case *ast.IndexListExpr:
		return callLabel(current.X)
	case *ast.ParenExpr:
		return callLabel(current.X)
	default:
		return "dynamic expression"
	}
}

func boundedCallLabel(expression ast.Expr) string {
	label := callLabel(expression)
	if utf8.RuneCountInString(label) <= 512 {
		return label
	}
	return string([]rune(label)[:509]) + "..."
}

func isReflectInvocation(function *types.Func) bool {
	if function == nil || function.Pkg() == nil || function.Pkg().Path() != "reflect" {
		return false
	}
	return function.Name() == "Call" || function.Name() == "CallSlice"
}

func sortRelationFacts(relations []indexerfacts.RelationFact) {
	sort.Slice(relations, func(i, j int) bool {
		left, right := relations[i], relations[j]
		if left.SitePath != right.SitePath {
			return left.SitePath < right.SitePath
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
		leftTarget, rightTarget := "", ""
		if left.TargetSymbolID != nil {
			leftTarget = string(*left.TargetSymbolID)
		}
		if right.TargetSymbolID != nil {
			rightTarget = string(*right.TargetSymbolID)
		}
		if leftTarget != rightTarget {
			return leftTarget < rightTarget
		}
		return left.RelationID < right.RelationID
	})
}

func aggregateRelationOccurrences(packages []PackageFacts) ([]RelationOccurrence, error) {
	symbolOccurrences := make(map[model.Hash]model.Hash)
	for _, pkg := range packages {
		for _, scope := range pkg.Scopes {
			if scope.Kind == "package" && scope.ID == pkg.Key {
				symbolOccurrences[pkg.Key] = scope.OccurrenceID
			}
		}
		for _, declaration := range pkg.Declarations {
			symbolOccurrences[declaration.SymbolID] = declaration.OccurrenceID
		}
	}
	type accumulator struct {
		occurrence RelationOccurrence
		variants   map[PackageVariant]struct{}
	}
	grouped := make(map[model.Hash]*accumulator)
	for _, pkg := range packages {
		for _, relation := range pkg.Relations {
			sourceOccurrence, ok := symbolOccurrences[relation.SourceSymbolID]
			if !ok {
				return nil, fmt.Errorf("%w: relation source has no symbol occurrence", ErrInvalidInput)
			}
			var targetOccurrence, targetSymbol *model.Hash
			if relation.TargetSymbolID != nil {
				if occurrence, exists := symbolOccurrences[*relation.TargetSymbolID]; exists {
					copy := occurrence
					targetOccurrence = &copy
				} else {
					copy := *relation.TargetSymbolID
					targetSymbol = &copy
				}
			}
			id, err := model.HashValue("go-relation-occurrence-v1", map[string]any{
				"generation_id": pkg.GenerationID, "profile_id": pkg.ProfileID,
				"source_occurrence_id": sourceOccurrence,
				"target_occurrence_id": optionalHashValue(targetOccurrence), "target_symbol_id": optionalHashValue(targetSymbol),
				"kind": string(relation.Kind), "evidence_kind": string(relation.EvidenceKind),
				"site_path": relation.SitePath, "start_byte": relation.Range.StartByte, "end_byte": relation.Range.EndByte,
				"reason": relation.Reason, "unresolved_label": optionalStringValue(relation.UnresolvedLabel),
			})
			if err != nil {
				return nil, err
			}
			item := grouped[id]
			if item == nil {
				item = &accumulator{occurrence: RelationOccurrence{
					ID: id, SourceOccurrenceID: sourceOccurrence,
					TargetOccurrenceID: targetOccurrence, TargetSymbolID: targetSymbol,
					Kind: relation.Kind, EvidenceKind: relation.EvidenceKind,
					SitePath: relation.SitePath, Range: relation.Range,
					Reason: relation.Reason, UnresolvedLabel: relation.UnresolvedLabel,
				}, variants: make(map[PackageVariant]struct{})}
				grouped[id] = item
			}
			item.variants[pkg.Variant] = struct{}{}
		}
	}
	result := make([]RelationOccurrence, 0, len(grouped))
	for _, item := range grouped {
		for variant := range item.variants {
			item.occurrence.Variants = append(item.occurrence.Variants, variant)
		}
		sort.Slice(item.occurrence.Variants, func(i, j int) bool { return item.occurrence.Variants[i] < item.occurrence.Variants[j] })
		result = append(result, item.occurrence)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.SitePath != right.SitePath {
			return left.SitePath < right.SitePath
		}
		if left.Range.StartByte != right.Range.StartByte {
			return left.Range.StartByte < right.Range.StartByte
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.ID < right.ID
	})
	return result, nil
}

func optionalHashValue(value *model.Hash) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalStringValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
