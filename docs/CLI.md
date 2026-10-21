# CLI reference

The first CLI release exposes only commands backed by typed runtime services:

```text
mce4d version --json
mce4d doctor --json
mce4d config show [--json] [--alternate-json <file>] [--override <key=value>...]
mce4d config validate --input-json <file-or-dash>
mce4d config set --input-json <file-or-dash> --expected-digest <sha256-or-absent>
mce4d store backup --target worktree --worktree-id <uuid> --output <owned-backup-dir> --json
mce4d store gc --target worktree --worktree-id <uuid> --keep-per-profile <count> --json [--apply]
mce4d store restore --manifest <approved-local-file> --confirm --wait-ms <bounded> --json
mce4d store recover --target worktree --worktree-id <uuid> --report-only --json
mce4d store recover --target worktree --worktree-id <uuid> --apply --confirm [--wait-ms <bounded>] --json
mce4d records list --worktree-id <uuid> --input-json <file-or-dash> --json
mce4d records read --worktree-id <uuid> --input-json <file-or-dash> --json
```

Successful commands emit one JSON result on stdout. Diagnostics go to stderr. Use
`--input-json -` to read a candidate from stdin. Candidate input is bounded to
1 MiB and must satisfy the closed RuntimeConfig schema.

`config show` resolves built-in defaults, the canonical home source, an optional
explicit alternate preview, then repeated process overrides. Its
`source_kind`, `source_digest`, and ordered `effective_sources` describe the
source layers used. Neither the alternate file nor process overrides are saved.

`config validate` and `config set` treat their input as the complete candidate.
Missing fields resolve from built-in defaults; the old home config is not merged.
Validation does not replace `runtime.json`. `config set` requires the current
canonical source digest from `config show`, or `absent` when no home source
exists. It takes the short home config lock and atomically replaces the source
only if the digest still matches. A stale digest exits with code 3 and leaves
the file unchanged.

`store backup` requires an existing worktree index. Its output directory must
be an immediate child of that worktree's `backups` directory and use a new UUID
as its name. The command takes an exclusive maintenance lease, reserves durable
volume growth, and writes a canonical manifest with database and referenced
capture-artifact hashes. If a records database exists, the manifest records a
quiesced index-and-records set; an index-only backup is labeled `single_store`.

`store gc` reports generations outside the requested retention and terminal
failed or cancelled generations. It opens the index read-only and performs no
SQLite or filesystem writes by default. Add `--apply` to collect the listed
generations; collection rechecks the plan under the worktree writer lease.

`store restore` requires an absolute local `manifest.json` path, explicit
`--confirm`, and bounded `--wait-ms`. The manifest selects its worktree, while
staging, rollback copies, journal, and replacement files stay under that
worktree's owned `maintenance` directory. Restored SQLite stores receive new
incarnations; restore blocks store opens until verification completes.

`store recover` defaults to a read-only report; `--report-only` makes that mode
explicit. `--apply --confirm` resumes or rolls back a pending restore only after
validating its preserved journal and evidence. A terminal verified journal
remains safe if the operating system reports that marker-directory flushing is
unsupported after removing the recovery marker.

`records list` and `records read` accept the closed `RecordListRequest` and
`RecordReadRequest` JSON shapes. Pass the worktree in `--worktree-id` or in
`scope.worktree_id`; if both are present they must match. Profile and generation
scope selectors are optional. The list defaults to 20 rows and accepts a
bounded `limit` up to 100 plus a continuation cursor. A cursor is tied to the
record store incarnation, record epoch, and list filters; any record mutation
invalidates it. The read command returns the selected record version's durable
summary. Both commands are read-only and have no proposal or review route.

Each summary keeps its declared status and reports an effective status and
provenance status derived from the current policy and retained index snapshot.
When a snapshot has expired or its source no longer matches, the command keeps
the record metadata, reports `needs_review` for a stale approval, and does not
reconstruct source bytes. Index absence does not remove durable records.

The canonical home path follows the SPEC: `MCE4D_HOME` when explicitly set;
otherwise `%LOCALAPPDATA%\mce4d` on Windows, `$XDG_STATE_HOME/mce4d` on Linux
when configured, or `~/.local/state/mce4d` as the Linux fallback. An override
must be an absolute path on the local filesystem.

Exit codes are 0 for success, 2 for command/input errors, 3 for invalid config
or stale preconditions, 4 for lock timeout/cancellation, 5 for storage/internal
errors, and 6 for a partial result.

## Example

```text
mce4d config validate --input-json docs/examples/runtime-config.json
mce4d config show --json
mce4d config set --input-json docs/examples/runtime-config.json --expected-digest absent
```

The Codex CLI can be inspected with `codex --version`. This release does not
write `CODEX_HOME/config.toml` or register a local MCP server automatically.
The sealed six-tool MCP transport remains available as an internal package;
the executable MCP command will appear when its typed app handlers are ready.
For a future local registration, use an isolated Codex home for the smoke test
and preserve any existing user configuration byte-for-byte.
