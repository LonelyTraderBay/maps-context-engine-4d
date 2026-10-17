package golang

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
	"golang.org/x/tools/go/packages"
)

func TestSyntaxCoverageStaysScopedAndDoesNotOverclaimEmptyRelations(t *testing.T) {
	analysis, err := AnalyzePackages([]PackageInput{
		packageInput("example.test/widget", "widget.go", "package widget\n", VariantProduction),
	})
	if err != nil {
		t.Fatal(err)
	}
	coverage := analysis.Coverage
	if coverage.Status != CoveragePartial || coverage.TypedNotRequestedCount != 1 {
		t.Fatalf("syntax-only facts must be partial: %+v", coverage)
	}
	if coverage.PackageCount != 1 || coverage.FileCount != 1 || coverage.ParsedFileCount != 1 {
		t.Fatalf("coverage counts do not match the explicit captured input: %+v", coverage)
	}
	for _, code := range []string{
		"captured_package_scope_only", "inventory_boundaries_not_attached",
		"non_go_semantics_not_analyzed", "runtime_test_coverage_not_collected",
		"dynamic_runtime_targets_not_proven", "typed_facts_not_requested",
		"empty_relation_result_is_scoped",
	} {
		if !coverageHasGap(coverage, code) {
			t.Errorf("coverage omitted boundary %q: %+v", code, coverage.Gaps)
		}
	}
	if empty := coverageForAnalysis(nil, nil, "", nil); empty.Status != CoverageUnknown || !coverageHasGap(empty, "empty_analysis_scope") {
		t.Fatalf("empty scope must remain unknown: %+v", empty)
	}
}

func TestTestReferencesLinkOnlyCapturedTestSourceToProductionOccurrences(t *testing.T) {
	const productionSource = "package widget\nfunc Apply() {}\n"
	const internalTestSource = "package widget\nfunc TestApply() { Apply() }\nfunc helper() { Apply() }\n"
	const externalTestSource = "package widget_test\nimport \"example.test/widget\"\nfunc TestPublicAPI() { widget.Apply() }\n"
	productionInput := packageInput("example.test/widget", "widget.go", productionSource, VariantProduction)
	internalInput := PackageInput{
		GenerationID: testGeneration, ProfileID: testProfile, ImportPath: "example.test/widget",
		ModuleKey: "example.test/module", Variant: VariantInternalTest, MaxFileBytes: DefaultMaxFileBytes,
		Files: []CapturedFile{
			{Path: "widget.go", Source: []byte(productionSource)},
			{Path: "widget_test.go", Source: []byte(internalTestSource)},
		},
	}
	externalInput := packageInput("example.test/widget_test", "widget_external_test.go", externalTestSource, VariantExternalTest)

	analysis, err := AnalyzePackages([]PackageInput{productionInput, internalInput, externalInput})
	if err != nil {
		t.Fatal(err)
	}
	var production Declaration
	for _, facts := range analysis.Packages {
		if facts.Variant != VariantProduction {
			continue
		}
		for _, declaration := range facts.Declarations {
			if declaration.Name == "Apply" && declaration.Kind == "function" {
				production = declaration
				break
			}
		}
	}
	if production.SymbolID == "" {
		t.Fatal("production Apply declaration was not captured")
	}
	internalCall := strings.Index(internalTestSource, "Apply() }")
	if internalCall < 0 {
		t.Fatal("internal test fixture has no Apply call")
	}
	internalCall += strings.Index(internalTestSource[internalCall:], "Apply")
	externalCall := strings.Index(externalTestSource, "widget.Apply")
	if externalCall < 0 {
		t.Fatal("external test fixture has no qualified Apply call")
	}
	externalCall += len("widget.")
	byteRange := func(start, end int) model.ByteRange {
		return model.ByteRange{StartByte: int64(start), EndByte: int64(end)}
	}

	inputsByKey := make(map[model.Hash]PackageInput, len(analysis.Packages))
	for index := range analysis.Packages {
		facts := &analysis.Packages[index]
		switch facts.Variant {
		case VariantInternalTest:
			facts.Uses = append(facts.Uses,
				UseFact{SymbolID: production.SymbolID, Path: "widget.go", Name: "Apply", Range: production.NameRange},
				UseFact{SymbolID: production.SymbolID, Path: "widget_test.go", Name: "Apply", Range: byteRange(internalCall, internalCall+len("Apply"))},
			)
			inputsByKey[facts.Key] = internalInput
		case VariantExternalTest:
			facts.Uses = append(facts.Uses, UseFact{
				SymbolID: production.SymbolID, Path: "widget_external_test.go", Name: "Apply",
				Range: byteRange(externalCall, externalCall+len("Apply")),
			})
			inputsByKey[facts.Key] = externalInput
		case VariantProduction:
			inputsByKey[facts.Key] = productionInput
		}
	}
	if err := appendTestReferences(analysis.Packages, inputsByKey); err != nil {
		t.Fatal(err)
	}

	var testRefCount int
	for _, facts := range analysis.Packages {
		for _, relation := range facts.Relations {
			if relation.Kind != RelationTestRef {
				continue
			}
			testRefCount++
			if relation.TargetSymbolID == nil || *relation.TargetSymbolID != production.SymbolID {
				t.Errorf("TEST_REF lost its production target: %+v", relation)
			}
			if relation.EvidenceKind != EvidenceTypeResolved || !strings.Contains(relation.Reason, "not execution or coverage") {
				t.Errorf("TEST_REF overstates its evidence: %+v", relation)
			}
			if relation.SitePath == "widget.go" {
				t.Error("ordinary production-file reference in an internal-test variant became a TEST_REF")
			}
		}
	}
	if testRefCount != 2 {
		t.Fatalf("expected one internal and one external static test reference, got %d", testRefCount)
	}

	relationOccurrences, err := aggregateRelationOccurrences(analysis.Packages)
	if err != nil {
		t.Fatal(err)
	}
	if len(relationOccurrences) != 2 {
		t.Fatalf("expected two test-source relation occurrences, got %+v", relationOccurrences)
	}
	for _, occurrence := range relationOccurrences {
		if occurrence.Kind != RelationTestRef || occurrence.TargetOccurrenceID == nil || occurrence.TargetSymbolID != nil {
			t.Errorf("captured production target was not linked to its source occurrence: %+v", occurrence)
		}
	}
}

func TestAnalyzeLoadedPackagesEmitsTypedTestReferencesAcrossVariants(t *testing.T) {
	const modulePath = "example.test/coverage"
	root := t.TempDir()
	productionSource := []byte("package coverage\nfunc Apply(value int) int { return value + 1 }\nfunc callApply() int { return Apply(1) }\n")
	internalTestSource := []byte("package coverage\nimport \"testing\"\nfunc TestApply(t *testing.T) { if Apply(2) != 3 { t.Fatal(\"unexpected result\") } }\n")
	externalTestSource := []byte("package coverage_test\nimport (\"testing\"; coverage \"example.test/coverage\")\nfunc TestPublicAPI(t *testing.T) { if coverage.Apply(2) != 3 { t.Fatal(\"unexpected result\") } }\n")
	sourceByPath := map[string][]byte{"coverage_test.go": internalTestSource, "coverage_external_test.go": externalTestSource}
	productionInput := packageInput(modulePath, "coverage.go", string(productionSource), VariantProduction)
	productionInput.ModuleKey = modulePath
	internalInput := PackageInput{
		GenerationID: testGeneration, ProfileID: testProfile, ImportPath: modulePath,
		ModuleKey: modulePath, Variant: VariantInternalTest, MaxFileBytes: DefaultMaxFileBytes,
		Files: []CapturedFile{{Path: "coverage.go", Source: productionSource}, {Path: "coverage_test.go", Source: internalTestSource}},
	}
	externalInput := PackageInput{
		GenerationID: testGeneration, ProfileID: testProfile, ImportPath: modulePath + "_test",
		ModuleKey: modulePath, Variant: VariantExternalTest, MaxFileBytes: DefaultMaxFileBytes,
		Files: []CapturedFile{{Path: "coverage_external_test.go", Source: externalTestSource}},
	}

	productionPackage := typedFixturePackage(t, root, productionInput, importer.Default(), nil)
	internalPackage := typedFixturePackage(t, root, internalInput, importer.Default(), nil)
	externalPackage := typedFixturePackage(t, root, externalInput, fixtureImporter{
		fallback: importer.Default(), packages: map[string]*types.Package{modulePath: productionPackage.Types},
	}, map[string]*packages.Package{modulePath: productionPackage})
	loaded := []LoadedPackageInput{
		{Input: productionInput, Package: productionPackage},
		{Input: internalInput, Package: internalPackage},
		{Input: externalInput, Package: externalPackage},
	}
	run := &LoadRun{
		ID: model.UUID("cccccccc-cccc-4ccc-8ccc-cccccccccccc"), ProfileID: testProfile,
		DependencyDigest: testProfile, Status: LoadComplete,
		typesRegistry:      make(map[*types.Package]struct{}, len(loaded)),
		capturedPathByFile: make(map[string]string), capturedSourceHashes: make(map[string]model.Hash),
	}
	for _, item := range loaded {
		run.packages = append(run.packages, item.Package)
		run.typesRegistry[item.Package.Types] = struct{}{}
		for _, file := range item.Input.Files {
			absolute := filepath.Join(root, filepath.FromSlash(file.Path))
			run.capturedPathByFile[loadPathKey(absolute)] = file.Path
			run.capturedSourceHashes[file.Path] = model.SHA256(file.Source)
		}
	}

	analysis, err := AnalyzeLoadedPackages(loaded, run)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Coverage.Status != CoverageSupportedScopeComplete || analysis.Coverage.TestReferenceCount != 2 {
		t.Fatalf("typed static test references or scoped completeness are incorrect: status=%s refs=%d gaps=%+v", analysis.Coverage.Status, analysis.Coverage.TestReferenceCount, analysis.Coverage.Gaps)
	}
	if len(analysis.LoadDiagnostics) != 0 {
		t.Fatalf("unexpected loader diagnostics: %+v", analysis.LoadDiagnostics)
	}
	for _, facts := range analysis.Packages {
		for _, relation := range facts.Relations {
			if relation.Kind != RelationTestRef {
				continue
			}
			if facts.Variant == VariantProduction || !strings.HasSuffix(relation.SitePath, "_test.go") || relation.TargetSymbolID == nil || relation.EvidenceKind != EvidenceTypeResolved {
				t.Errorf("TEST_REF escaped its typed test-source boundary: variant=%s relation=%+v", facts.Variant, relation)
			}
			if err := relation.Validate(sourceByPath[relation.SitePath]); err != nil {
				t.Errorf("TEST_REF is not bound to captured bytes: %v", err)
			}
		}
	}
	for _, occurrence := range analysis.RelationOccurrences {
		if occurrence.Kind == RelationTestRef && (occurrence.TargetOccurrenceID == nil || occurrence.TargetSymbolID != nil) {
			t.Errorf("selected production target was not linked to a declaration occurrence: %+v", occurrence)
		}
	}
}

type fixtureImporter struct {
	fallback types.Importer
	packages map[string]*types.Package
}

func (i fixtureImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := i.packages[path]; ok {
		return pkg, nil
	}
	return i.fallback.Import(path)
}

func typedFixturePackage(t *testing.T, root string, input PackageInput, typeImporter types.Importer, imports map[string]*packages.Package) *packages.Package {
	t.Helper()
	fset := token.NewFileSet()
	syntaxFiles := make([]*ast.File, 0, len(input.Files))
	compiledFiles := make([]string, 0, len(input.Files))
	for _, file := range input.Files {
		absolute := filepath.Join(root, filepath.FromSlash(file.Path))
		parsed, err := parser.ParseFile(fset, absolute, file.Source, parser.ParseComments|parser.AllErrors)
		if err != nil {
			t.Fatalf("parse fixture %s: %v", file.Path, err)
		}
		syntaxFiles = append(syntaxFiles, parsed)
		compiledFiles = append(compiledFiles, absolute)
	}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object), Implicits: make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection), Scopes: make(map[ast.Node]*types.Scope),
		Instances: make(map[*ast.Ident]types.Instance), FileVersions: make(map[*ast.File]string),
	}
	typedPackage, err := (&types.Config{Importer: typeImporter}).Check(input.ImportPath, fset, syntaxFiles, info)
	if err != nil {
		t.Fatalf("type-check fixture package %s: %v", input.ImportPath, err)
	}
	return &packages.Package{
		ID: input.ImportPath, Name: syntaxFiles[0].Name.Name, PkgPath: input.ImportPath,
		CompiledGoFiles: compiledFiles, Fset: fset, Imports: imports,
		Syntax: syntaxFiles, Types: typedPackage, TypesInfo: info,
	}
}

func coverageHasGap(coverage AnalysisCoverage, code string) bool {
	for _, gap := range coverage.Gaps {
		if gap.Code == code {
			return true
		}
	}
	return false
}
