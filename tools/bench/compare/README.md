# Offline A/B/C context benchmark harness

This package validates a deterministic synthetic corpus, imports usage JSONL produced by an independently authorized host runner, and writes an auditable JSON report. It does not call an LLM, model provider, network service, or billing API. It records measurements; it does not run benchmark tasks or decide whether a model's code change is correct.

The checked-in corpus contains 30 tasks across three synthetic Go repositories, with scenarios for local rename, interface/field/type changes, behavior bugs, contract preservation, dirty worktrees, missing dependencies, and large fanout. Each repository and task setup is fingerprinted. Text fingerprints normalize CRLF to LF so the same fixtures validate on Windows and Unix; binary bytes are left untouched. Corpus validation checks the fixture contents before importing any measurements.

## Commands

Validate the corpus and show its exact source-file SHA-256:

```powershell
go run ./tools/bench/compare/cmd validate -suite ./tools/bench/compare/testdata/corpus.json
```

Import host-collected JSONL and create a report at a new path:

```powershell
go run ./tools/bench/compare/cmd import `
  -suite ./tools/bench/compare/testdata/corpus.json `
  -usage ./usage.jsonl `
  -out ./benchmark-report.json
```

The output path must not already exist. Import is offline and will reject unknown fields, duplicate JSON keys, mismatched fixture/revision/dependency/setup fingerprints, duplicate samples, invalid retries, and variant order that does not match the recorded seed.

## Usage JSONL

The first record is one `run_header`. Every later record is an `attempt` or a `cost` observation. See `RunHeader`, `UsageAttempt`, and `CostObservation` in this package for the exact versioned field names.

The header binds suite/run IDs, seed, repeat count, model/version/reasoning, and SHA-256 identities for prompt, tool permissions, and budget. These identities must stay the same for A, B, and C. If fewer than three repeats are run, record a reason. The runner should also record its OS, filesystem, Go version, and hardware profile when the host can provide them.

Each attempt records the same task/repository/repeat, variant order index, starting fixture revision, dependency fingerprint, setup fingerprint, status, acceptance result, omissions, false edits, and context-proxy measurements. Retries use sequential attempt numbers starting at one. A failed attempt needs a reason. At most one accepted attempt is allowed, and it must be the final retry. Token fields must be supplied together or omitted together.

The shuffle is deterministic per seed, task ID, and repeat using `MCE4D-SPLITMIX64-FY1`; the report verifies the observed A/B/C ordering. Each task run is comparable only when all three variants are present.

`cost` records separate index/update stages (`index_full`, `index_noop_update`, `index_change_update`) from query latency/freshness/output observations. Index and query metrics are not converted to model tokens. The harness reports duration median/p95, measured CPU and bytes, storage and process-tree maxima, query freshness and output state, plus index-update cost per accepted task run. Missing optional host measurements remain absent rather than being inferred.

## Interpreting the report

- Token totals include every recorded attempt, including failed attempts and retries. Tokens per accepted task run divide by accepted task runs.
- Token savings are paired by task and repeat using A as baseline and C as optimized. Incomplete groups are excluded and counted; zero baselines are reported separately.
- Context-proxy bytes, lines, files, and read calls remain separate from actual host token telemetry. Proxy measurements are never translated into estimated tokens.
- If host token data is absent, token status is `NOT_RUN`; if only some attempts have it, token totals are withheld with `INCOMPLETE` status.
- The checked-in corpus is synthetic. Its results do not prove production performance or correctness on real repositories. T039 is the separate benchmark execution task; this harness does not execute it.
