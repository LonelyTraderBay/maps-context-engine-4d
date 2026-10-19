# Self-index benchmark

`BenchmarkAnalyzePinnedPackage` clones the current `HEAD` into a temporary
directory, verifies the detached target revision, then repeatedly scans its
inventory and analyzes the pinned `internal/analysis/golang` package with syntax
facts. The benchmark reports elapsed time, package source bytes, inventory
files, package files, declarations, and Go allocations. It logs both the target
revision and the analyzer source revision. The result does not claim a public
CLI index command or repository-wide typed analysis.

Run from a clean, pinned checkout with the project's Go toolchain and cached
modules available:

```powershell
go test ./tools/bench/selfindex -run '^$' -bench '^BenchmarkAnalyzePinnedPackage$' -benchmem -benchtime=1x -v
```

The acceptance harness separately copies the compiled test binary to a
temporary state directory, records its SHA-256 and source revisions, then runs
the copy against a detached snapshot. It compares top-level declarations and
byte ranges against `go list` plus an independent `go/parser` walk. When the
engine path is forced to fail, the fallback uses only those Go tools. All
generated request/result files and the candidate binary live outside the target
snapshot.
