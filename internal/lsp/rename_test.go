package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestValidateWorkspaceEditMapsEveryFileToCapturedByteRanges(t *testing.T) {
	files := map[string][]byte{
		"a.go": []byte("package p\nvar old = 1\n"),
		"b.go": []byte("package p\nvar ref = old\n"),
	}
	workspace, _ := testValidatedWorkspace(t, files)
	uriA, _ := pathToFileURI(filepath.Join(workspace.rootPath, "a.go"))
	uriB, _ := pathToFileURI(filepath.Join(workspace.rootPath, "b.go"))
	raw, err := json.Marshal(map[string]any{"changes": map[string]any{
		uriB: []any{map[string]any{"range": testRange(1, 10, 13), "newText": "renamed"}},
		uriA: []any{map[string]any{"range": testRange(1, 4, 7), "newText": "renamed"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	edits, err := validateWorkspaceEdit(context.Background(), raw, workspace, PositionUTF16, "old", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 || edits[0].edit.Path != "a.go" || edits[1].edit.Path != "b.go" {
		t.Fatalf("edits are not complete and deterministic: %+v", edits)
	}
	if edits[0].edit.Range != (model.ByteRange{StartByte: 14, EndByte: 17}) || edits[1].edit.Range != (model.ByteRange{StartByte: 20, EndByte: 23}) {
		t.Fatalf("byte ranges=%+v, want [14,17), [20,23)", []model.ByteRange{edits[0].edit.Range, edits[1].edit.Range})
	}
	for _, edit := range edits {
		if edit.edit.Version != 1 || edit.edit.ContentHash != model.SHA256(files[edit.edit.Path]) || edit.edit.NewText != "renamed" {
			t.Errorf("edit is not bound to captured version/hash: %+v", edit.edit)
		}
	}
}

func TestValidateWorkspaceEditRejectsScopeVersionResourceAndOverlap(t *testing.T) {
	workspace, _ := testValidatedWorkspace(t, map[string][]byte{"a.go": []byte("package p\nvar old = 1\n")})
	uri, _ := pathToFileURI(filepath.Join(workspace.rootPath, "a.go"))
	outside := "file:///outside.go"
	cases := []struct {
		name    string
		raw     any
		want    error
		newName string
	}{
		{"outside file", map[string]any{"changes": map[string]any{outside: []any{map[string]any{"range": testRange(0, 0, 1), "newText": "x"}}}}, ErrCapturedWorkspace, ""},
		{"overlap", map[string]any{"changes": map[string]any{uri: []any{
			map[string]any{"range": testRange(1, 4, 7), "newText": "renamed"},
			map[string]any{"range": testRange(1, 4, 7), "newText": "renamed"},
		}}}, ErrRenameConflict, "renamed"},
		{"resource move", map[string]any{"documentChanges": []any{map[string]any{"kind": "rename", "oldUri": uri, "newUri": uri + ".new"}}}, ErrRenameUnsupported, ""},
		{"version mismatch", map[string]any{"documentChanges": []any{map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": 2}, "edits": []any{map[string]any{"range": testRange(1, 4, 7), "newText": "x"}},
		}}}, ErrCapturedWorkspace, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			newName := test.newName
			if newName == "" {
				newName = "x"
			}
			_, err = validateWorkspaceEdit(context.Background(), raw, workspace, PositionUTF16, "old", newName)
			if !errors.Is(err, test.want) {
				t.Fatalf("validateWorkspaceEdit error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestValidateWorkspaceEditRejectsFileOutsideEffectiveModuleRoots(t *testing.T) {
	files := map[string][]byte{
		"module/a.go": []byte("package p\nvar old = 1\n"),
		"other/b.go":  []byte("package p\nvar old = 2\n"),
	}
	workspace, _ := testValidatedWorkspace(t, files)
	workspace.moduleRoots = []string{"module"}
	uri, _ := pathToFileURI(filepath.Join(workspace.rootPath, "other", "b.go"))
	raw, err := json.Marshal(map[string]any{"changes": map[string]any{
		uri: []any{map[string]any{"range": testRange(1, 4, 7), "newText": "renamed"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateWorkspaceEdit(context.Background(), raw, workspace, PositionUTF16, "old", "renamed"); !errors.Is(err, ErrCapturedWorkspace) {
		t.Fatalf("edit outside effective module roots error=%v", err)
	}
}

func TestValidateWorkspaceEditRejectsDuplicateJSONMembers(t *testing.T) {
	workspace, _ := testValidatedWorkspace(t, map[string][]byte{"a.go": []byte("package p\nvar old = 1\n")})
	uri, _ := pathToFileURI(filepath.Join(workspace.rootPath, "a.go"))
	raw := json.RawMessage(`{"changes":{},"changes":{"` + uri + `":[{"range":{"start":{"line":1,"character":4},"end":{"line":1,"character":7}},"newText":"renamed"}]}}`)
	if _, err := validateWorkspaceEdit(context.Background(), raw, workspace, PositionUTF16, "old", "renamed"); !errors.Is(err, ErrRenameUnsupported) {
		t.Fatalf("duplicate WorkspaceEdit member error=%v", err)
	}
}

func TestValidateWorkspaceEditRejectsLiteralAndTagEdits(t *testing.T) {
	source := []byte("package p\ntype Record struct { Exported string `json:\"Exported\"` }\nvar label = \"Exported\"\n")
	workspace, _ := testValidatedWorkspace(t, map[string][]byte{"record.go": source})
	uri, _ := pathToFileURI(filepath.Join(workspace.rootPath, "record.go"))
	for _, test := range []struct {
		name  string
		start int64
	}{
		{name: "struct tag", start: int64(strings.Index(string(source), "`json:\"") + len("`json:\""))},
		{name: "string literal", start: int64(strings.LastIndex(string(source), "Exported"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			start, err := ByteOffsetToPosition(source, test.start, PositionUTF16)
			if err != nil {
				t.Fatal(err)
			}
			end, err := ByteOffsetToPosition(source, test.start+int64(len("Exported")), PositionUTF16)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"changes": map[string]any{
				uri: []any{map[string]any{"range": Range{Start: start, End: end}, "newText": "Renamed"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validateWorkspaceEdit(context.Background(), raw, workspace, PositionUTF16, "Exported", "Renamed"); !errors.Is(err, ErrRenameUnsupported) {
				t.Fatalf("non-identifier edit error=%v, want unsupported", err)
			}
		})
	}
}

func TestIdentifierRangeAtRequiresAnExactUTF8Boundary(t *testing.T) {
	source := []byte("package p\nvar café = 1\n")
	start := int64(strings.Index(string(source), "café"))
	byteRange, name, err := IdentifierRangeAt(source, "unicode.go", start+3)
	if err != nil || name != "café" || byteRange != (model.ByteRange{StartByte: start, EndByte: start + int64(len("café"))}) {
		t.Fatalf("valid identifier offset resolved to range=%+v name=%q err=%v", byteRange, name, err)
	}
	if _, _, err := IdentifierRangeAt(source, "unicode.go", start+4); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("offset inside a UTF-8 code point error=%v, want invalid position", err)
	}
}

func TestSnapshotVerificationRejectsManifestChangeAndMutation(t *testing.T) {
	files := map[string][]byte{"a.go": []byte("package p\n")}
	workspace, snapshot := testValidatedWorkspace(t, files)
	changed := append([]SnapshotFile(nil), snapshot.Files...)
	changed[0].ContentHash = model.SHA256([]byte("different"))
	if err := workspace.verify(context.Background(), changed, 2*1024*1024, 4*1024*1024); !errors.Is(err, ErrCapturedWorkspace) {
		t.Fatalf("changed manifest error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.rootPath, "a.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.verify(context.Background(), snapshot.Files, 2*1024*1024, 4*1024*1024); !errors.Is(err, ErrCapturedWorkspace) {
		t.Fatalf("changed source error=%v", err)
	}
}

func TestVerifyLSPModuleClosureRequiresCapturedModulesWithoutReplace(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string][]byte
		want  error
	}{
		{
			name: "valid captured module",
			files: map[string][]byte{
				"go.mod": []byte("module example.com/p\n\ngo 1.27.0\n"),
				"a.go":   []byte("package p\n"),
			},
		},
		{
			name:  "missing go.mod",
			files: map[string][]byte{"a.go": []byte("package p\n")},
			want:  ErrInvalidLSPProfile,
		},
		{
			name: "local replace",
			files: map[string][]byte{
				"go.mod": []byte("module example.com/p\n\ngo 1.27.0\n\nreplace example.com/dep => ../dep\n"),
				"a.go":   []byte("package p\n"),
			},
			want: ErrInvalidLSPProfile,
		},
		{
			name: "nested module outside effective roots",
			files: map[string][]byte{
				"go.mod":        []byte("module example.com/p\n\ngo 1.27.0\n"),
				"nested/go.mod": []byte("module example.com/p/nested\n\ngo 1.27.0\n"),
				"a.go":          []byte("package p\n"),
			},
			want: ErrInvalidLSPProfile,
		},
		{
			name: "vendor metadata is outside module closure",
			files: map[string][]byte{
				"go.mod":                []byte("module example.com/p\n\ngo 1.27.0\n"),
				"vendor/example/go.mod": []byte("module example.com/vendor\n\ngo 1.27.0\n"),
				"a.go":                  []byte("package p\n"),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, _ := testValidatedWorkspace(t, test.files)
			err := verifyLSPModuleClosure(context.Background(), workspace, GoProfile{ModuleRoots: []string{"."}})
			if test.want == nil && err != nil {
				t.Fatalf("valid module closure error=%v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("module closure error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestRenameSiteWarningsAndPackageRenameBoundary(t *testing.T) {
	implicit := []byte("package p\ntype Record struct { Exported string }\n")
	offset := int64(strings.Index(string(implicit), "Exported"))
	warnings, packageName := assessRenameSite(implicit, "record.go", offset)
	if packageName || !reflect.DeepEqual(warnings, []string{"RUNTIME_STRING_LOOKUP_REVIEW_REQUIRED", "IMPLICIT_SERIALIZATION_NAME_CHANGE"}) {
		t.Fatalf("implicit JSON field analysis warnings=%v package=%v", warnings, packageName)
	}
	explicit := []byte("package p\ntype Record struct { Exported string `json:\"api_name\"` }\n")
	warnings, packageName = assessRenameSite(explicit, "record.go", int64(strings.Index(string(explicit), "Exported")))
	if packageName || !reflect.DeepEqual(warnings, []string{"RUNTIME_STRING_LOOKUP_REVIEW_REQUIRED"}) {
		t.Fatalf("explicit JSON field analysis warnings=%v package=%v", warnings, packageName)
	}
	packageSource := []byte("package p\nvar value int\n")
	warnings, packageName = assessRenameSite(packageSource, "record.go", int64(strings.Index(string(packageSource), "package ")+len("package ")))
	if !packageName {
		t.Fatalf("package declaration rename was not rejected: %v", warnings)
	}
}

func TestRenameIdentifierValidation(t *testing.T) {
	for _, value := range []string{"NewName", "café", "变量"} {
		if !tokenIsIdentifier(value) {
			t.Errorf("valid Go identifier rejected: %q", value)
		}
	}
	for _, value := range []string{"", "_", "package", "new-name", "a b"} {
		if tokenIsIdentifier(value) {
			t.Errorf("invalid Go identifier accepted: %q", value)
		}
	}
}

func TestSourcePathMustBelongToEffectiveModuleRoot(t *testing.T) {
	for _, test := range []struct {
		path  string
		roots []string
		want  bool
	}{
		{path: "module/pkg/file.go", roots: []string{"module"}, want: true},
		{path: "module.go", roots: []string{"."}, want: true},
		{path: "other/pkg/file.go", roots: []string{"module"}, want: false},
		{path: "../module/file.go", roots: []string{"."}, want: false},
	} {
		if got := sourcePathInModuleRoots(test.path, test.roots); got != test.want {
			t.Errorf("sourcePathInModuleRoots(%q, %v)=%v, want %v", test.path, test.roots, got, test.want)
		}
	}
}

func testValidatedWorkspace(t *testing.T, values map[string][]byte) (*validatedWorkspace, WorkspaceSnapshot) {
	t.Helper()
	identity := resultTestIdentity(t)
	rootPath := t.TempDir()
	snapshot := WorkspaceSnapshot{Root: rootPath, Complete: true, Scope: identity.Scope, Files: make([]SnapshotFile, 0, len(values))}
	for relative, content := range values {
		path := filepath.Join(rootPath, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot.Files = append(snapshot.Files, SnapshotFile{Path: relative, ContentHash: model.SHA256(content)})
	}
	profile := GoProfile{ID: identity.Scope.Members[0].ProfileID, ModuleRoots: []string{"."}, MaxFileBytes: 2 * 1024 * 1024, MaxSourceBytes: 4 * 1024 * 1024}
	workspace, err := validateWorkspaceSnapshot(context.Background(), snapshot, profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.close() })
	return workspace, snapshot
}

func testRange(line, start, end uint32) Range {
	return Range{Start: Position{Line: line, Character: start}, End: Position{Line: line, Character: end}}
}
