package contract_test

import (
	"crypto/sha256"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestT107IndependentPackageLoadsDoNotShareObjectIdentity(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, filepath.Join(dir, "go.mod"), "module example.test/t107fixture\n\ngo 1.27.0\n")
	sourcePath := filepath.Join(dir, "model.go")
	writeFixtureFile(t, sourcePath, "package fixture\ntype Item struct { Name string }\n")
	load := func() *types.TypeName {
		t.Helper()
		config := &packages.Config{
			Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
			Dir:  dir,
		}
		loaded, err := packages.Load(config, ".")
		if err != nil {
			t.Fatalf("packages.Load: %v", err)
		}
		if len(loaded) != 1 || len(loaded[0].Errors) != 0 || loaded[0].Types == nil {
			t.Fatalf("unexpected package load: %#v", loaded)
		}
		object, ok := loaded[0].Types.Scope().Lookup("Item").(*types.TypeName)
		if !ok {
			t.Fatal("Item type was not loaded")
		}
		return object
	}
	first := load()
	second := load()
	if first == second {
		t.Fatal("object identity from independent packages.Load calls must not be mixed")
	}
	if !types.Identical(first.Type().Underlying(), second.Type().Underlying()) {
		t.Fatal("matching source declarations should have the same underlying structure")
	}
}

func TestT107SourceBytesMustMatchTheAnalyzedSnapshot(t *testing.T) {
	captured := []byte("package fixture\nfunc Original() {}\n")
	file, err := parser.ParseFile(token.NewFileSet(), "model.go", captured, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse captured source: %v", err)
	}
	if got := topLevelFunctionName(file); got != "Original" {
		t.Fatalf("captured function=%q", got)
	}

	changed := []byte("package fixture\nfunc Replaced() {}\n")
	changedFile, err := parser.ParseFile(token.NewFileSet(), "model.go", changed, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse changed source: %v", err)
	}
	if string(captured) == string(changed) || topLevelFunctionName(changedFile) == topLevelFunctionName(file) {
		t.Fatal("changed source must be distinguishable from the captured package input")
	}
	if status := sourceSnapshotStatus(captured, changed); status != "partial" {
		t.Fatalf("mismatched source status=%q", status)
	}
	if status := sourceSnapshotStatus(captured, captured); status != "complete" {
		t.Fatalf("matching source status=%q", status)
	}
}

func sourceSnapshotStatus(captured, parsed []byte) string {
	if sha256.Sum256(captured) != sha256.Sum256(parsed) {
		return "partial"
	}
	return "complete"
}

func topLevelFunctionName(file *ast.File) string {
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			return function.Name.Name
		}
	}
	return ""
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}
