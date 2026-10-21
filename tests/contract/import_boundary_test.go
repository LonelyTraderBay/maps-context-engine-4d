package contract_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/LonelyTraderBay/maps-context-engine-4d"

func TestProductionImportDirectionHasNoCycle(t *testing.T) {
	_, sourceFile, _, ok := runtimeCaller(0)
	if !ok {
		t.Fatal("cannot locate import-boundary test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	allowed := map[string]map[string]bool{
		"internal/model": {},
		"internal/contracts": {
			modulePath + "/contracts":      true,
			modulePath + "/internal/model": true,
		},
		// Telemetry is a local leaf package with no project imports.
		"internal/telemetry": {},
		"internal/indexer": {
			modulePath + "/internal/inventory":    true,
			modulePath + "/internal/model":        true,
			modulePath + "/internal/snapshot":     true,
			modulePath + "/internal/store/sqlite": true,
		},
		"internal/snapshot": {modulePath + "/internal/model": true},
		"internal/history":  {modulePath + "/internal/model": true},
		"internal/analysis/golang": {
			modulePath + "/internal/model":        true,
			modulePath + "/internal/runtime/exec": true,
		},
		"internal/search": {
			modulePath + "/internal/cursor": true,
			modulePath + "/internal/model":  true,
		},
		"internal/overview":    {modulePath + "/internal/model": true},
		"internal/contextpack": {modulePath + "/internal/model": true},
		"internal/cursor": {
			modulePath + "/contracts":        true,
			modulePath + "/internal/model":   true,
			modulePath + "/internal/runtime": true,
		},
		"internal/lsp": {
			modulePath + "/contracts":             true,
			modulePath + "/internal/cursor":       true,
			modulePath + "/internal/model":        true,
			modulePath + "/internal/store/sqlite": true,
		},
		"internal/catalog": {
			modulePath + "/internal/model":  true,
			modulePath + "/internal/policy": true,
		},
		"internal/paths":  {},
		"internal/policy": {modulePath + "/internal/paths": true},
		"internal/inventory": {
			modulePath + "/internal/paths":  true,
			modulePath + "/internal/policy": true,
		},
		"internal/runtime/config": {
			modulePath + "/contracts":      true,
			modulePath + "/internal/model": true,
		},
		"internal/runtime/maintenance": {modulePath + "/internal/runtime": true},
		// The controlled tool runner is a leaf package with no project imports.
		"internal/runtime/exec": {},
		"internal/runtime": {
			modulePath + "/internal/paths":  true,
			modulePath + "/internal/policy": true,
		},
		"internal/analysis/providers": {
			modulePath + "/contracts":      true,
			modulePath + "/internal/model": true,
			modulePath + "/internal/ports": true,
		},
		"internal/store/sqlite": {
			modulePath + "/contracts":                    true,
			modulePath + "/internal/history":             true,
			modulePath + "/internal/model":               true,
			modulePath + "/internal/runtime":             true,
			modulePath + "/internal/runtime/maintenance": true,
			// SQLite is the runtime lock consumer; the resulting graph remains acyclic.
		},
		"internal/app/config": {
			modulePath + "/internal/model":          true,
			modulePath + "/internal/runtime/config": true,
		},
		"internal/app/explorer": {
			modulePath + "/internal/catalog":      true,
			modulePath + "/internal/explorer":     true,
			modulePath + "/internal/model":        true,
			modulePath + "/internal/paths":        true,
			modulePath + "/internal/policy":       true,
			modulePath + "/internal/store/sqlite": true,
		},
		"internal/app/records": {
			modulePath + "/internal/catalog":      true,
			modulePath + "/internal/history":      true,
			modulePath + "/internal/model":        true,
			modulePath + "/internal/paths":        true,
			modulePath + "/internal/policy":       true,
			modulePath + "/internal/store/sqlite": true,
		},
		"internal/app/store": {
			modulePath + "/internal/model":        true,
			modulePath + "/internal/store/sqlite": true,
		},
		"internal/app/registration": {
			modulePath + "/contracts":        true,
			modulePath + "/internal/catalog": true,
			modulePath + "/internal/model":   true,
		},
		"internal/app/response": {
			modulePath + "/contracts":            true,
			modulePath + "/internal/contextpack": true,
			modulePath + "/internal/model":       true,
		},
		"internal/app/validate": {
			modulePath + "/internal/inventory": true,
			modulePath + "/internal/model":     true,
			modulePath + "/internal/policy":    true,
			modulePath + "/internal/runtime":   true,
			modulePath + "/internal/snapshot":  true,
		},
		"internal/app/rename": {
			modulePath + "/contracts":             true,
			modulePath + "/internal/app/response": true,
			modulePath + "/internal/app/validate": true,
			modulePath + "/internal/cursor":       true,
			modulePath + "/internal/lsp":          true,
			modulePath + "/internal/model":        true,
			modulePath + "/internal/snapshot":     true,
		},
		"internal/cli": {
			modulePath + "/contracts":               true,
			modulePath + "/internal/app/config":     true,
			modulePath + "/internal/app/explorer":   true,
			modulePath + "/internal/app/records":    true,
			modulePath + "/internal/app/store":      true,
			modulePath + "/internal/history":        true,
			modulePath + "/internal/model":          true,
			modulePath + "/internal/runtime":        true,
			modulePath + "/internal/runtime/config": true,
			modulePath + "/internal/store/sqlite":   true,
		},
		"internal/mcp": {
			modulePath + "/contracts":             true,
			modulePath + "/internal/app/response": true,
		},
		"internal/explorer": {
			modulePath + "/contracts": true,
		},
		"internal/ports": {modulePath + "/internal/model": true},
		"contracts":      {modulePath + "/internal/model": true},
		"tests/contract": {
			modulePath + "/contracts":                 true,
			modulePath + "/internal/model":            true,
			modulePath + "/internal/catalog":          true,
			modulePath + "/internal/paths":            true,
			modulePath + "/internal/policy":           true,
			modulePath + "/internal/ports":            true,
			modulePath + "/internal/runtime":          true,
			modulePath + "/internal/runtime/config":   true,
			modulePath + "/internal/app/config":       true,
			modulePath + "/internal/app/registration": true,
			modulePath + "/internal/app/store":        true,
			modulePath + "/internal/app/validate":     true,
		},
	}
	graph := make(map[string][]string)
	for _, directory := range []string{"internal", "contracts", filepath.Join("tests", "contract")} {
		base := filepath.Join(root, directory)
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			relativeDirectory, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			packagePath := filepath.ToSlash(relativeDirectory)
			allowedImports, knownPackage := allowed[packagePath]
			if !knownPackage {
				return os.ErrInvalid
			}
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if strings.HasPrefix(importPath, modulePath+"/") {
					if !allowedImports[importPath] {
						t.Errorf("%s imports forbidden project package %s", packagePath, importPath)
						continue
					}
					graph[packagePath] = append(graph[packagePath], strings.TrimPrefix(importPath, modulePath+"/"))
				}
				if packagePath == "internal/model" && strings.Contains(strings.Split(importPath, "/")[0], ".") {
					t.Errorf("pure model imports external dependency %s", importPath)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", directory, err)
		}
	}
	if hasCycle(graph) {
		t.Fatalf("project import graph contains a cycle: %#v", graph)
	}
}

func hasCycle(graph map[string][]string) bool {
	const (
		visiting = 1
		visited  = 2
	)
	state := make(map[string]int)
	var visit func(string) bool
	visit = func(node string) bool {
		if state[node] == visiting {
			return true
		}
		if state[node] == visited {
			return false
		}
		state[node] = visiting
		for _, next := range graph[node] {
			if visit(next) {
				return true
			}
		}
		state[node] = visited
		return false
	}
	for node := range graph {
		if visit(node) {
			return true
		}
	}
	return false
}

// runtimeCaller is isolated so the test can report a clear error if source paths are unavailable.
func runtimeCaller(skip int) (uintptr, string, int, bool) {
	return runtime.Caller(skip + 1)
}
