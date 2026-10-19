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

func TestR1BuildProfilesSeparateOSArchitectureTagsAndCGO(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go toolchain is unavailable for build-profile acceptance")
	}
	root := t.TempDir()
	goMod := []byte("module example.test/r1-profile\n\ngo 1.27.0\n")
	sources := map[string][]byte{
		"go.mod":                  goMod,
		"main.go":                 []byte("package profilefixture\nfunc Active() string { return selected }\n"),
		"tagged_linux_amd64.go":   []byte("//go:build linux && amd64 && mce4d_profile\n\npackage profilefixture\nvar selected = \"linux-amd64-tagged\"\n"),
		"plain_linux_amd64.go":    []byte("//go:build linux && amd64 && !mce4d_profile\n\npackage profilefixture\nvar selected = \"linux-amd64-plain\"\n"),
		"tagged_linux_arm64.go":   []byte("//go:build linux && arm64 && mce4d_profile\n\npackage profilefixture\nvar selected = \"linux-arm64-tagged\"\n"),
		"tagged_windows_amd64.go": []byte("//go:build windows && amd64 && mce4d_profile\n\npackage profilefixture\nvar selected = \"windows-amd64-tagged\"\n"),
	}
	for path, data := range sources {
		if err := os.WriteFile(filepath.Join(root, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hashes := make(map[string]model.Hash, len(sources))
	for path, data := range sources {
		hashes[path] = model.SHA256(data)
	}
	loadProfile := func(name, goos, goarch string, tags []string, cgo bool) (*golanganalysis.LoadRun, map[string]bool) {
		t.Helper()
		moduleCache := filepath.Join(root, name, "module-cache")
		scratch := filepath.Join(root, name, "scratch")
		for _, directory := range []string{moduleCache, scratch} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatalf("prepare profile fixture directory %q: %v", directory, err)
			}
		}
		request := golanganalysis.LoadRequest{
			SnapshotRoot: root, ModuleRoot: ".", Patterns: []string{"."},
			CapturedFiles: sources, CaptureHashes: hashes,
			Profile: golanganalysis.LoadProfile{
				GOOS: goos, GOARCH: goarch, ModuleRoots: []string{"."}, BuildTags: tags, CGOEnabled: cgo,
				ToolchainDigest: model.SHA256([]byte("r1-profile-toolchain")), ProviderDigest: model.SHA256([]byte("r1-profile-provider")),
				ModuleMode: "readonly", AnalysisMode: "typed_when_available", GoBinary: goBinary,
				GoVersion: runtime.Version(), GoBinarySHA256: fileSHA256(t, goBinary),
				ModuleCacheRoot: moduleCache, ScratchRoot: scratch,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		run, err := golanganalysis.LoadTrustedPackages(ctx, request)
		if err != nil {
			t.Fatalf("load %s profile: %v", name, err)
		}
		if cgo {
			if run.Status != golanganalysis.LoadPartial || len(run.Packages()) != 0 || len(run.Diagnostics) != 1 || run.Diagnostics[0].Code != "cgo_profile_unsupported" {
				t.Fatalf("%s CGO profile status=%s packages=%d diagnostics=%+v", name, run.Status, len(run.Packages()), run.Diagnostics)
			}
			return run, nil
		}
		if run.Status != golanganalysis.LoadComplete || len(run.Packages()) != 1 {
			t.Fatalf("%s profile status=%s packages=%d diagnostics=%+v", name, run.Status, len(run.Packages()), run.Diagnostics)
		}
		active := make(map[string]bool, len(run.Packages()[0].Syntax))
		for _, syntax := range run.Packages()[0].Syntax {
			active[filepath.Base(run.Packages()[0].Fset.PositionFor(syntax.Pos(), false).Filename)] = true
		}
		return run, active
	}

	linuxTagged, linuxTaggedFiles := loadProfile("linux-tagged", "linux", "amd64", []string{"mce4d_profile"}, false)
	linuxPlain, linuxPlainFiles := loadProfile("linux-plain", "linux", "amd64", nil, false)
	linuxArm64, linuxArm64Files := loadProfile("linux-arm64", "linux", "arm64", []string{"mce4d_profile"}, false)
	windowsAMD64, windowsAMD64Files := loadProfile("windows-amd64", "windows", "amd64", []string{"mce4d_profile"}, false)
	linuxCGO, _ := loadProfile("linux-cgo", "linux", "amd64", []string{"mce4d_profile"}, true)
	for _, check := range []struct {
		name   string
		files  map[string]bool
		want   string
		absent string
	}{
		{"linux tagged", linuxTaggedFiles, "tagged_linux_amd64.go", "plain_linux_amd64.go"},
		{"linux plain", linuxPlainFiles, "plain_linux_amd64.go", "tagged_linux_amd64.go"},
		{"linux arm64", linuxArm64Files, "tagged_linux_arm64.go", "tagged_linux_amd64.go"},
		{"windows amd64", windowsAMD64Files, "tagged_windows_amd64.go", "tagged_linux_amd64.go"},
	} {
		if !check.files[check.want] || check.files[check.absent] {
			t.Errorf("%s selected files=%v; want %s and exclude %s", check.name, check.files, check.want, check.absent)
		}
	}
	if linuxTagged.ProfileID == linuxPlain.ProfileID || linuxTagged.ProfileID == linuxArm64.ProfileID ||
		linuxTagged.ProfileID == windowsAMD64.ProfileID || linuxTagged.ProfileID == linuxCGO.ProfileID {
		t.Fatalf("effective profile identity did not change across GOOS/GOARCH/tags/CGO: linux-tagged=%s linux-plain=%s linux-arm64=%s windows-amd64=%s linux-cgo=%s",
			linuxTagged.ProfileID, linuxPlain.ProfileID, linuxArm64.ProfileID, windowsAMD64.ProfileID, linuxCGO.ProfileID)
	}
}
