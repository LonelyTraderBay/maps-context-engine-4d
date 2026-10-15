# AI_RULES supplement: bounded build and test storage

Edition 1.0.0. An explicit, portable supplement to the accepted project policy. Keep original Universal AI_RULES and PROJECT_BOOTSTRAP_PROMPT files unchanged. This supplement does not grant execution, cleanup, configuration, or deletion permission. Apply the actual instruction hierarchy and required approvals.

## Scope and minimal implementation

Apply before authorized builds, tests, benchmarks, generators, or other checks that can produce substantial output. Documentation-only bootstrap records the proposed controls and actual commands; it does not install hooks, change runtime configuration, implement wrappers, or run a heavy workload unless separately authorized. Use the project's existing tools and a small wrapper only where needed. Do not introduce a general cleanup daemon, global cache policy, or new dependency without demonstrated need and authority.

## Before starting

- Identify the actual command, working directory, output roots, temporary directories, caches, logs, downloads, and their backing volumes. Check free bytes on every actual destination volume, including redirected caches and temporary storage; repository drive free space alone is insufficient.
- Record a workload budget: expected peak additional bytes, retained bytes, safety reserve, maximum duration, and concurrent writers. Derive estimates from relevant runs or a bounded sample. If a heavy workload has unknown growth, destination, or budget, stop that workload and report the missing evidence; do not guess that it fits.
- Configure project-appropriate warning and blocking thresholds with explicit units. Do not hard-code universal numbers. Warn below the warning threshold. Block if free space is below the configured blocking threshold or cannot cover peak additional demand, other active jobs' reservations, and the reserve. Validate thresholds and fail closed on unreadable measurements or invalid budgets.
- For long or variable runs, recheck free space and output growth at suitable bounded intervals. On the limit, stop the owned process tree safely and preserve its failure status. A preflight check or polling cannot guarantee the disk will never fill; other writers, abrupt termination, and growth between samples remain risks.

## Ownership, paths, and parallel work

Use a unique run identity and dedicated generated-output directory where tooling supports it. Record the owner, command, allowed roots, created paths, start state, and retention limits in a small run record. A directory name, ignored status, or marker alone does not prove contents are disposable.

Resolve and validate paths against the explicitly allowed root, including every relevant ancestor. Reject symlink, junction, reparse-point, mount, or traversal escapes and ambiguous paths. Revalidate before cleanup and prevent path replacement races; if the platform cannot enforce containment safely, refuse automatic cleanup. Never traverse a link to delete its target.

Do not delete or overwrite another job's output. Isolate parallel runs; serialize tools with unavoidable shared output, or use a verified lock covering all writers. Account for the aggregate disk demand of active jobs. Shared dependency caches remain shared: use documented tooling and separate authority, never a broad recursive deletion guessed from a cache name.

## Bounded retention and cleanup

Define finite byte/count/age limits for disposable output, caches under project control, and logs. Choose limits from project needs; keep a bounded useful failure record. Success, failure, and cancellation all reach a cleanup/finalization path once owned writers stop. Preserve the original command result; report cleanup failure separately rather than returning a false success. On an uncatchable crash, leave a record for a later report-only recovery check; do not assume automatic cleanup ran.

Before removal, support a report-only/dry-run that lists exact candidates, ownership evidence, size, reason, exclusions, and expected retained data. Dry-run must not run the workload, delete files, install tools, or mutate project state. Unknown provenance, unexpected contents, live writers, missing ownership, or unsafe containment means refuse removal and report.

Only remove proven disposable output within the authorized scope. Never remove test source, fixtures, baseline/golden snapshots, Git metadata or tracked source, user data, unique results, or unrelated files. Preserve concise failure evidence such as command, exit status, failing tests or seed, and a bounded sanitized log excerpt; do not retain secrets or unbounded dumps. A requested deliverable is not disposable merely because it was generated.

Existing accumulated junk requires separate, specific cleanup consent; prevention work does not authorize retroactive cleanup. Recoverable removal still needs applicable approval. Permanent deletion that cannot be restored through normal product recovery requires approval at action time; this policy, a run marker, or standing cleanup instructions cannot bypass it. If required approval is absent, report candidates and leave them intact. Prefer prevention, supported size limits, and recoverable handling where practical; moving output on the same volume does not reclaim space.

## Verify and report

Use small synthetic disposable cases before heavy execution. Verify low-space blocking without filling the disk, budget rejection, output bounds, success/failure/cancel finalization, original exit-status propagation, simultaneous jobs, unexpected contents, and link/junction escape rejection. Test only within authorized disposable paths; do not exercise destructive cases on real project data.

Report actual commands/checks, output volumes, free space before/after, generated and retained bytes, removed candidates if approved, cleanup failures, and remaining limits. Label untested behavior UNVERIFIED. A written instruction is not an enforced guard; a passing small test is not proof against all disk-full conditions.
