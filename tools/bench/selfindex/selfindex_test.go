package selfindex

import (
	"context"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func BenchmarkAnalyzePinnedPackage(b *testing.B) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("resolve benchmark source path")
	}
	sourceRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	root, targetRevision := benchmarkPinnedSnapshot(b, sourceRoot)
	analyzerRevision := benchmarkGit(b, sourceRoot, "log", "-1", "--format=%H", targetRevision, "--", "internal/analysis/golang")
	baseline, err := AnalyzePinnedPackage(context.Background(), root)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("SELF_INDEX_BENCH target_revision=%s analyzer_source_revision=%s package=%s", targetRevision, analyzerRevision, baseline.PackageImportPath)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := AnalyzePinnedPackage(context.Background(), root); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(baseline.SourceBytes), "pkg-B/op")
	b.ReportMetric(float64(baseline.InventoryFileCount), "inventory-files/op")
	b.ReportMetric(float64(len(baseline.Files)), "pkg-files/op")
	b.ReportMetric(float64(len(baseline.EngineDeclarations)), "symbols/op")
}

func benchmarkPinnedSnapshot(b *testing.B, sourceRoot string) (string, string) {
	b.Helper()
	git, err := osexec.LookPath("git")
	if err != nil {
		b.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		b.Fatal(err)
	}
	revision := benchmarkGitWithBinary(b, git, sourceRoot, "rev-parse", "HEAD")
	targetRoot := filepath.Join(b.TempDir(), "target")
	clone := osexec.Command(git, "clone", "--quiet", "--no-hardlinks", "--no-checkout", "--", sourceRoot, targetRoot)
	if output, err := clone.CombinedOutput(); err != nil {
		b.Fatalf("clone benchmark target: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if runtime.GOOS == "windows" {
		configureLongPaths := osexec.Command(git, "-C", targetRoot, "config", "core.longpaths", "true")
		if output, err := configureLongPaths.CombinedOutput(); err != nil {
			b.Fatalf("configure benchmark target long paths: %v: %s", err, strings.TrimSpace(string(output)))
		}
	}
	checkout := osexec.Command(git, "-C", targetRoot, "checkout", "--quiet", "--detach", revision)
	if output, err := checkout.CombinedOutput(); err != nil {
		b.Fatalf("checkout benchmark target: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if got := benchmarkGitWithBinary(b, git, targetRoot, "rev-parse", "HEAD"); got != revision {
		b.Fatalf("benchmark target revision mismatch: got=%s want=%s", got, revision)
	}
	return targetRoot, revision
}

func benchmarkGit(b *testing.B, root string, args ...string) string {
	b.Helper()
	git, err := osexec.LookPath("git")
	if err != nil {
		b.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		b.Fatal(err)
	}
	return benchmarkGitWithBinary(b, git, root, args...)
}

func benchmarkGitWithBinary(b *testing.B, git, root string, args ...string) string {
	b.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	command := osexec.Command(git, commandArgs...)
	output, err := command.Output()
	if err != nil {
		b.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}
