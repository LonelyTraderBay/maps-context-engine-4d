package golang

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"golang.org/x/tools/go/packages"
)

func TestRelationFactBindsCapturedCallsiteAndRejectsInvalidEdges(t *testing.T) {
	source := []byte("func Run() { target() }\n")
	start := strings.Index(string(source), "target()")
	site := model.ByteRange{StartByte: int64(start), EndByte: int64(start + len("target()"))}
	firstTarget := model.SHA256([]byte("first-target"))
	input := RelationInput{
		SourceSymbolID: model.SHA256([]byte("caller")), TargetSymbolID: &firstTarget,
		Kind: RelationCallsDirect, EvidenceKind: EvidenceTypeResolved,
		SitePath: "pkg/run.go", Range: site, Reason: "statically resolved call",
	}
	first, err := NewRelationFact(input, source)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := NewRelationFact(input, source)
	if err != nil || first.RelationID != repeated.RelationID {
		t.Fatalf("relation ID is not deterministic: first=%s repeated=%s err=%v", first.RelationID, repeated.RelationID, err)
	}
	if err := first.Validate(source); err != nil {
		t.Fatalf("created relation does not validate: %v", err)
	}
	secondTarget := model.SHA256([]byte("second-target"))
	input.TargetSymbolID = &secondTarget
	second, err := NewRelationFact(input, source)
	if err != nil || first.RelationID == second.RelationID {
		t.Fatalf("parallel targets collapsed at one callsite: first=%s second=%s err=%v", first.RelationID, second.RelationID, err)
	}
	label := "callback"
	input.TargetSymbolID = &firstTarget
	input.Kind, input.EvidenceKind, input.UnresolvedLabel = RelationUnresolvedCall, EvidenceUnresolved, &label
	if _, err := NewRelationFact(input, source); !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("unresolved relation accepted a concrete target: %v", err)
	}
	if _, err := NewRelationFact(input, []byte("short")); !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("relation accepted a site outside captured source: %v", err)
	}
}

func TestReflectInvocationKeepsStaticMethodAndDynamicBoundary(t *testing.T) {
	fset := token.NewFileSet()
	reflectSource := []byte("package reflect\ntype Value struct{}\nfunc (Value) Call(args []Value) []Value { return nil }\n")
	reflectFile, err := parser.ParseFile(fset, "reflect.go", reflectSource, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	reflectInfo := newTypesInfo()
	reflectPackage, err := (&types.Config{}).Check("reflect", fset, []*ast.File{reflectFile}, reflectInfo)
	if err != nil {
		t.Fatal(err)
	}

	source := []byte("package p\nimport \"reflect\"\nfunc Caller(value reflect.Value) { value.Call(nil) }\n")
	mainFile, err := parser.ParseFile(fset, "main.go", source, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	mainInfo := newTypesInfo()
	mainPackage, err := (&types.Config{Importer: singlePackageImporter{path: "reflect", pkg: reflectPackage}}).
		Check("example.test/p", fset, []*ast.File{mainFile}, mainInfo)
	if err != nil {
		t.Fatal(err)
	}
	var call *ast.CallExpr
	ast.Inspect(mainFile, func(node ast.Node) bool {
		if candidate, ok := node.(*ast.CallExpr); ok {
			call = candidate
			return false
		}
		return true
	})
	if call == nil {
		t.Fatal("reflection call was not parsed")
	}
	tokenFile := fset.File(call.Pos())
	site := model.ByteRange{StartByte: int64(tokenFile.Offset(call.Pos())), EndByte: int64(tokenFile.Offset(call.End()))}
	caller := model.SHA256([]byte("caller"))
	facts := PackageFacts{}
	pkg := &packages.Package{Fset: fset, Syntax: []*ast.File{mainFile}, Types: mainPackage, TypesInfo: mainInfo}
	resolve := func(types.Object, *types.Selection) (model.Hash, error) {
		return model.SHA256([]byte("reflect-call")), nil
	}
	if err := appendCallRelations(pkg, &facts, resolve, nil, CapturedFile{Path: "main.go", Source: source},
		"main.go", site, caller, call); err != nil {
		t.Fatal(err)
	}
	if len(facts.Relations) != 2 {
		t.Fatalf("reflection invocation should retain its static method and unresolved target boundary, got %+v", facts.Relations)
	}
	kinds := map[RelationKind]int{}
	for _, relation := range facts.Relations {
		if err := relation.Validate(source); err != nil {
			t.Fatalf("reflection relation lost captured source binding: %+v: %v", relation, err)
		}
		kinds[relation.Kind]++
		if relation.Kind == RelationUnresolvedCall &&
			(relation.TargetSymbolID != nil || relation.UnresolvedLabel == nil || *relation.UnresolvedLabel != "reflect.Value.Call") {
			t.Fatalf("reflection boundary was represented as a concrete target: %+v", relation)
		}
	}
	if kinds[RelationCallsDirect] != 1 || kinds[RelationUnresolvedCall] != 1 {
		t.Fatalf("unexpected reflection relation kinds: %v", kinds)
	}
}

func TestAggregateRelationOccurrencesDeduplicatesPackageVariants(t *testing.T) {
	source := []byte("func Caller() { Target() }\n")
	start := strings.Index(string(source), "Target()")
	site := model.ByteRange{StartByte: int64(start), EndByte: int64(start + len("Target()"))}
	packageOccurrence := model.SHA256([]byte("package-occurrence"))
	callerOccurrence := model.SHA256([]byte("caller-occurrence"))
	targetOccurrence := model.SHA256([]byte("target-occurrence"))
	packages := make([]PackageFacts, 0, 2)
	for _, variant := range []PackageVariant{VariantProduction, VariantInternalTest} {
		key := model.SHA256([]byte("package-" + string(variant)))
		caller := model.SHA256([]byte("caller-" + string(variant)))
		target := model.SHA256([]byte("target-" + string(variant)))
		relation, err := NewRelationFact(RelationInput{
			SourceSymbolID: caller, TargetSymbolID: &target, Kind: RelationCallsDirect,
			EvidenceKind: EvidenceTypeResolved, SitePath: "main.go", Range: site,
			Reason: "statically resolved call",
		}, source)
		if err != nil {
			t.Fatal(err)
		}
		packages = append(packages, PackageFacts{
			Key: key, GenerationID: testGeneration, ProfileID: testProfile,
			ImportPath: "example.test/p", ModuleKey: "example.test", Variant: variant,
			Scopes: []Scope{{ID: key, OccurrenceID: packageOccurrence, Kind: "package"}},
			Declarations: []Declaration{
				{SymbolID: caller, OccurrenceID: callerOccurrence},
				{SymbolID: target, OccurrenceID: targetOccurrence},
			},
			Relations: []RelationFact{relation},
		})
	}
	occurrences, err := aggregateRelationOccurrences(packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(occurrences) != 1 || len(occurrences[0].Variants) != 2 {
		t.Fatalf("shared relation occurrences were multiplied across variants: %+v", occurrences)
	}
}

func TestUnresolvedCallLabelRespectsSchemaLimit(t *testing.T) {
	label := boundedCallLabel(ast.NewIdent(strings.Repeat("a", 600)))
	if got := len([]rune(label)); got != 512 {
		t.Fatalf("unresolved label length = %d runes, want schema maximum 512", got)
	}
}

func newTypesInfo() *types.Info {
	return &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Implicits:  make(map[ast.Node]types.Object),
		Scopes:     make(map[ast.Node]*types.Scope),
	}
}

type singlePackageImporter struct {
	path string
	pkg  *types.Package
}

func (importer singlePackageImporter) Import(path string) (*types.Package, error) {
	if path != importer.path {
		return nil, fmt.Errorf("unexpected test import %s", path)
	}
	return importer.pkg, nil
}
