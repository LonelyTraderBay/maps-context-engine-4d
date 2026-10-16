package golang

import (
	"errors"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	testGeneration = model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	testProfile    = model.Hash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func packageInput(importPath, filePath, source string, variant PackageVariant) PackageInput {
	return PackageInput{
		GenerationID: testGeneration,
		ProfileID:    testProfile,
		ImportPath:   importPath,
		ModuleKey:    "example.test/module",
		Variant:      variant,
		MaxFileBytes: DefaultMaxFileBytes,
		Files:        []CapturedFile{{Path: filePath, Source: []byte(source)}},
	}
}

func TestAnalyzeUsesPackageScopeAndSourceRangesForIdentity(t *testing.T) {
	const source = "package same\nvar Left, Right int\nfunc Apply() {}\n"
	analysis, err := AnalyzePackages([]PackageInput{
		packageInput("example.test/first", "first/api.go", source, VariantProduction),
		packageInput("example.test/second", "second/api.go", source, VariantProduction),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := declarationsNamed(analysis.Packages[0], "Apply", "function")
	second := declarationsNamed(analysis.Packages[1], "Apply", "function")
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("same-named package declarations were not independently indexed: first=%d second=%d", len(first), len(second))
	}
	if first[0].SymbolID == second[0].SymbolID {
		t.Fatal("same-named declarations in distinct package paths shared an identity")
	}
	left := declarationsNamed(analysis.Packages[0], "Left", "var")
	right := declarationsNamed(analysis.Packages[0], "Right", "var")
	if len(left) != 1 || len(right) != 1 || left[0].SymbolID == right[0].SymbolID {
		t.Fatalf("names in one grouped declaration need separate symbol identities: left=%v right=%v", left, right)
	}
	for _, declaration := range []Declaration{first[0], second[0]} {
		sourceBytes := []byte(source)
		if err := declaration.NameRange.ValidateAgainst(sourceBytes); err != nil {
			t.Fatalf("declaration has invalid byte range: %+v: %v", declaration, err)
		}
		if got := string(sourceBytes[declaration.NameRange.StartByte:declaration.NameRange.EndByte]); got != declaration.Name {
			t.Fatalf("name range selects %q, want %q", got, declaration.Name)
		}
	}
}

func TestAnalyzeSeparatesShadowedBindingsClosuresReceiverAndNamedResult(t *testing.T) {
	const source = "package sample\n" +
		"// café keeps offsets byte-based.\n" +
		"type Base struct { ID int }\n" +
		"type Box[T any] struct { *Base; Value T }\n" +
		"func (b *Box[T]) Read(item T) (Value T) {\n" +
		"    { Value := item; _ = Value }\n" +
		"    { item := item; _ = item }\n" +
		"    first := func(item T) T { return item }\n" +
		"    second := func(item T) T { return item }\n" +
		"    Value = first(second(item))\n" +
		"    return\n" +
		"}\n"
	facts, err := AnalyzePackage(packageInput("example.test/sample", "sample/box.go", source, VariantProduction))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct{ name, kind string }{
		{"Box", "type"}, {"T", "type_parameter"}, {"Base", "field"}, {"Value", "field"},
		{"Read", "method"}, {"b", "receiver"}, {"item", "parameter"}, {"Value", "named_result"},
		{"first", "short_var_candidate"}, {"second", "short_var_candidate"},
	} {
		if len(declarationsNamed(facts, expected.name, expected.kind)) == 0 {
			t.Errorf("missing %s declaration %q", expected.kind, expected.name)
		}
	}

	closureParameters := declarationsAtKindWithName(facts, "item", "parameter")
	if len(closureParameters) != 3 {
		t.Fatalf("expected method and two closure parameters named item, got %d", len(closureParameters))
	}
	seen := map[model.Hash]struct{}{}
	seenScopes := map[model.Hash]struct{}{}
	for _, declaration := range closureParameters {
		if _, exists := seen[declaration.SymbolID]; exists {
			t.Fatalf("same-spelled parameter reused symbol identity: %+v", declaration)
		}
		seen[declaration.SymbolID] = struct{}{}
		if _, exists := seenScopes[declaration.ScopeID]; exists {
			t.Fatal("function and closure parameters were assigned the same lexical scope")
		}
		seenScopes[declaration.ScopeID] = struct{}{}
	}

	shadowed := declarationsNamed(facts, "Value", "short_var_candidate")
	result := declarationsNamed(facts, "Value", "named_result")
	field := declarationsNamed(facts, "Value", "field")
	if len(shadowed) != 1 || len(result) != 1 || len(field) != 1 {
		t.Fatalf("field, named result, and local shadow were not kept separate: local=%d result=%d field=%d", len(shadowed), len(result), len(field))
	}
	if shadowed[0].SymbolID == result[0].SymbolID || result[0].SymbolID == field[0].SymbolID || shadowed[0].SymbolID == field[0].SymbolID {
		t.Fatal("same-spelled field/result/local declarations shared identity")
	}
	if !shadowed[0].RequiresTypeResolution {
		t.Fatal("short declaration was treated as a fully resolved binding")
	}

	for _, declaration := range facts.Declarations {
		if err := declaration.NameRange.ValidateAgainst([]byte(source)); err != nil {
			t.Fatalf("invalid UTF-8 byte range for %q: %v", declaration.Name, err)
		}
		selected := string([]byte(source)[declaration.NameRange.StartByte:declaration.NameRange.EndByte])
		if selected != declaration.Name {
			t.Errorf("byte range selected %q instead of %q", selected, declaration.Name)
		}
	}
}

func TestAnalyzeKeepsTestVariantProvenanceWithoutCountingSharedOccurrenceTwice(t *testing.T) {
	const productionSource = "package widget\nfunc Apply() {}\n"
	const testSource = "package widget\nfunc TestApply() {}\n"
	const externalSource = "package widget_test\nfunc TestPublicAPI() {}\n"
	analysis, err := AnalyzePackages([]PackageInput{
		packageInput("example.test/widget", "widget.go", productionSource, VariantProduction),
		{
			GenerationID: testGeneration, ProfileID: testProfile, ImportPath: "example.test/widget",
			ModuleKey: "example.test/module", Variant: VariantInternalTest, MaxFileBytes: DefaultMaxFileBytes,
			Files: []CapturedFile{
				{Path: "widget.go", Source: []byte(productionSource)},
				{Path: "widget_test.go", Source: []byte(testSource)},
			},
		},
		packageInput("example.test/widget_test", "external_test.go", externalSource, VariantExternalTest),
	})
	if err != nil {
		t.Fatal(err)
	}
	var applyOccurrences []DeclarationOccurrence
	for _, occurrence := range analysis.Occurrences {
		if occurrence.Name == "Apply" {
			applyOccurrences = append(applyOccurrences, occurrence)
		}
	}
	if len(applyOccurrences) != 1 {
		t.Fatalf("shared production declaration should be one source occurrence, got %d", len(applyOccurrences))
	}
	occurrence := applyOccurrences[0]
	if len(occurrence.Variants) != 2 || occurrence.Variants[0] != VariantInternalTest || occurrence.Variants[1] != VariantProduction {
		t.Fatalf("shared declaration lost package-variant provenance: %v", occurrence.Variants)
	}
	if len(occurrence.SymbolIDs) != 2 || occurrence.SymbolIDs[0] == occurrence.SymbolIDs[1] {
		t.Fatalf("variant-specific symbol identities were not retained: %v", occurrence.SymbolIDs)
	}
	if len(analysis.Packages) != 3 {
		t.Fatalf("production/internal/external variants were not kept separate: %d", len(analysis.Packages))
	}
}

func TestAnalyzePreservesPartialFactsAndSanitizesParserDiagnostics(t *testing.T) {
	const secretSentinel = "sourceSecretSentinel"
	source := "package sample\nvar Visible int\nvar " + secretSentinel + " ???\n"
	facts, err := AnalyzePackage(packageInput("example.test/sample", "sample/broken.go", source, VariantProduction))
	if err != nil {
		t.Fatal(err)
	}
	if facts.SyntaxStatus != SyntaxPartial || len(declarationsNamed(facts, "Visible", "var")) != 1 {
		t.Fatalf("parser failure dropped parseable syntax facts: status=%s declarations=%v", facts.SyntaxStatus, facts.Declarations)
	}
	if len(facts.Diagnostics) == 0 {
		t.Fatal("parse error was silently ignored")
	}
	for _, diagnostic := range facts.Diagnostics {
		if strings.Contains(diagnostic.Message, secretSentinel) {
			t.Fatalf("diagnostic leaked source text: %q", diagnostic.Message)
		}
	}

	invalidUTF8 := packageInput("example.test/sample", "sample/invalid.go", "package sample\n", VariantProduction)
	invalidUTF8.Files[0].Source = []byte{'p', 'a', 'c', 'k', 'a', 'g', 'e', ' ', 'p', 0xff}
	invalidFacts, err := AnalyzePackage(invalidUTF8)
	if err != nil {
		t.Fatal(err)
	}
	if invalidFacts.Files[0].ParseStatus != ParseFailed || invalidFacts.Diagnostics[0].Code != "GO_INVALID_UTF8" {
		t.Fatalf("invalid UTF-8 was not retained as a scoped diagnostic: %+v", invalidFacts)
	}
}

func TestAnalyzeUsesExplicitImportMetadataAndHandlesOversizedFiles(t *testing.T) {
	input := packageInput("example.test/deep/path/package_name", "src/a.go", "package p\nimport alias \"example.test/other/deep\"\nimport \"example.test/default\"\nimport . \"example.test/dot\"\nfunc Use() {}\n", VariantProduction)
	facts, err := AnalyzePackage(input)
	if err != nil {
		t.Fatal(err)
	}
	if facts.ImportPath != "example.test/deep/path/package_name" {
		t.Fatalf("package identity was inferred from a folder or package clause: %q", facts.ImportPath)
	}
	if len(facts.Imports) != 3 || len(declarationsNamed(facts, "alias", "import")) != 1 {
		t.Fatalf("import syntax facts or explicit alias declaration missing: %+v", facts)
	}
	if len(declarationsNamed(facts, "other", "import")) != 0 || len(declarationsNamed(facts, "default", "import")) != 0 {
		t.Fatal("import name was incorrectly inferred from its path")
	}
	if !hasDiagnostic(facts, "GO_DOT_IMPORT_UNRESOLVED") {
		t.Fatal("dot import did not disclose its unresolved package boundary")
	}

	input.Files[0].Source = []byte("package p\n" + strings.Repeat(" ", int(DefaultMaxFileBytes)))
	oversized, err := AnalyzePackage(input)
	if err != nil {
		t.Fatal(err)
	}
	if oversized.Files[0].ParseStatus != ParseSkipped || oversized.SyntaxStatus != SyntaxPartial || !hasDiagnostic(oversized, "GO_FILE_TOO_LARGE") {
		t.Fatalf("oversized captured file was parsed or silently counted complete: %+v", oversized)
	}
}

func TestAnalyzeRejectsAmbiguousOrInconsistentPackageInputs(t *testing.T) {
	input := packageInput("example.test/sample", "sample/a.go", "package sample\n", VariantProduction)
	missingImportPath := input
	missingImportPath.ImportPath = ""
	if _, err := AnalyzePackage(missingImportPath); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing explicit import path accepted: %v", err)
	}
	duplicatePath := input
	duplicatePath.Files = append(duplicatePath.Files, duplicatePath.Files[0])
	if _, err := AnalyzePackage(duplicatePath); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate captured source path accepted: %v", err)
	}
	variant := input
	variant.Variant = VariantInternalTest
	variant.Files = []CapturedFile{{Path: "sample/a.go", Source: []byte("package sample\nvar Changed int\n")}}
	if _, err := AnalyzePackages([]PackageInput{input, variant}); !errors.Is(err, ErrVariantSourceMismatch) {
		t.Fatalf("different captured bytes for one source across variants accepted: %v", err)
	}
}

func declarationsNamed(facts PackageFacts, name, kind string) []Declaration {
	return declarationsAtKindWithName(facts, name, kind)
}

func declarationsAtKindWithName(facts PackageFacts, name, kind string) []Declaration {
	var result []Declaration
	for _, declaration := range facts.Declarations {
		if declaration.Name == name && declaration.Kind == kind {
			result = append(result, declaration)
		}
	}
	return result
}

func hasDiagnostic(facts PackageFacts, code string) bool {
	for _, diagnostic := range facts.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
