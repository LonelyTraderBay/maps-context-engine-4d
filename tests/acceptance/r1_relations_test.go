package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	golanganalysis "github.com/LonelyTraderBay/maps-context-engine-4d/internal/analysis/golang"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

func TestR1PointerAliasDoesNotInventDataFlowRelations(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go toolchain is unavailable for captured-source acceptance")
	}
	root := t.TempDir()
	moduleCache := filepath.Join(root, "module-cache")
	scratch := filepath.Join(root, "scratch")
	for _, directory := range []string{moduleCache, scratch} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	goMod := []byte("module example.test/r1-alias\n\ngo 1.27.0\n")
	source := []byte(`package aliasfixture
type Cell struct { Value int }
func Read(cell *Cell) int { return cell.Value }
func Update(cell *Cell, pointer *int, next int) {
	previous := cell.Value
	cell.Value = next
	*pointer += previous
}
`)
	for path, data := range map[string][]byte{"go.mod": goMod, "main.go": source} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	input := golanganalysis.PackageInput{
		GenerationID: model.UUID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		ProfileID:    model.SHA256([]byte("r1-alias-profile")),
		ImportPath:   "example.test/r1-alias", ModuleKey: "example.test/r1-alias",
		Variant: golanganalysis.VariantProduction, MaxFileBytes: golanganalysis.DefaultMaxFileBytes,
		Files: []golanganalysis.CapturedFile{{Path: "main.go", Source: source}},
	}
	request := golanganalysis.LoadRequest{
		SnapshotRoot: root, ModuleRoot: ".", Patterns: []string{"."},
		CapturedFiles: map[string][]byte{"go.mod": goMod, "main.go": source},
		CaptureHashes: map[string]model.Hash{"go.mod": model.SHA256(goMod), "main.go": model.SHA256(source)},
		Profile: golanganalysis.LoadProfile{
			GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ModuleRoots: []string{"."},
			ToolchainDigest: model.SHA256([]byte("r1-alias-toolchain")), ProviderDigest: model.SHA256([]byte("r1-alias-provider")),
			ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoBinary: goBinary,
			GoVersion: runtime.Version(), GoBinarySHA256: fileSHA256(t, goBinary),
			ModuleCacheRoot: moduleCache, ScratchRoot: scratch,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run, err := golanganalysis.LoadTrustedPackages(ctx, request)
	if err != nil {
		t.Fatalf("load captured pointer-alias fixture: %v", err)
	}
	if run.Status != golanganalysis.LoadComplete || len(run.Packages()) != 1 {
		t.Fatalf("captured alias fixture load status=%s packages=%d diagnostics=%+v", run.Status, len(run.Packages()), run.Diagnostics)
	}
	input.ProfileID = run.ProfileID
	analysis, err := golanganalysis.AnalyzeLoadedPackages([]golanganalysis.LoadedPackageInput{{Input: input, Package: run.Packages()[0]}}, run)
	if err != nil {
		t.Fatalf("analyze captured pointer-alias fixture: %v", err)
	}
	for _, facts := range analysis.Packages {
		for _, relation := range facts.Relations {
			if relation.Kind == golanganalysis.RelationReads || relation.Kind == golanganalysis.RelationWrites {
				t.Fatalf("unproven pointer alias produced a data-flow relation: %+v", relation)
			}
		}
	}
	if got := model.SHA256(source); request.CaptureHashes["main.go"] != got {
		t.Fatalf("fixture source identity changed: got=%s want=%s", got, request.CaptureHashes["main.go"])
	}
}
