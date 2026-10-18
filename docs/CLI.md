# CLI reference

The first CLI release exposes only commands backed by typed runtime services:

```text
mce4d version --json
mce4d doctor --json
mce4d config show [--json] [--alternate-json <file>] [--override <key=value>...]
mce4d config validate --input-json <file-or-dash>
mce4d config set --input-json <file-or-dash> --expected-digest <sha256-or-absent>
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
