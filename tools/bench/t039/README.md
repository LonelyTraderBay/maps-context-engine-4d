# T039 engine component profile

Run from the repository root with the revision under test set explicitly:

```powershell
$env:MCE4D_BENCH_REVISION = (git rev-parse HEAD)
$env:MCE4D_BENCH_HARDWARE_PROFILE = 'record the measured host CPU and model'
$env:MCE4D_BENCH_FILESYSTEM_CLASS = 'record the fixture volume filesystem'
go run ./tools/bench/t039
```

The revision must be a full lowercase Git commit SHA. The command measures three existing Go engine components against generated
fixtures: warm indexed symbol search, graph-only context selection, and a full
disk enumeration/read/SHA-256/snapshot freshness comparison. It records 30
warm samples after three warmups and reports nearest-rank p50/p95. Fixture
construction and schema creation are outside the timed sections; freshness
includes all live fixture reads and hashes. Windows measurements use
`QueryPerformanceCounter` for sub-millisecond resolution. Output is JSON for
capture in a new `artifacts/bench/T039/` attempt directory.

The fixtures are not a real repository or a semantic-analysis corpus. The
search database contains 512 symbols/documents; context selection receives a
1,000-node graph; freshness scans 1,000 synthetic files totaling 20,000,000
bytes. These component measurements do not establish end-to-end indexing or
model task success. The command never invokes an LLM, network service, or
billing API. Cold OS-cache results, a full-index workflow, real repositories,
the A/B/C task run, and host token telemetry remain `NOT_RUN` when unavailable.
