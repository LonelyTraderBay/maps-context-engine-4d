from __future__ import annotations

import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd()
SUPPORT = ROOT / "artifacts/git-delivery-support/T034-r4-bd1c82a0-cbbf-4487-880e-0e85a0672c8d"
OUT = SUPPORT / "archive-view/operations/payload-code-1"
EXPECTED_BASE = "d70b5cc76490d9cf60457869e77b0cffd19ad06e"
EXPECTED_HEAD = "6827429e6360741ceb8a42db94a5b4b0778b4991"


def run(*args: str) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(["git", *args], cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def require_success(result: subprocess.CompletedProcess[bytes], operation: str) -> str:
    output = result.stdout.decode("utf-8", "replace")
    if result.returncode:
        raise SystemExit(f"{operation} failed ({result.returncode}): {output}")
    return output.strip()


if run("rev-parse", "HEAD").stdout.decode().strip() != EXPECTED_HEAD:
    raise SystemExit("push requires exact T034 r4 code payload HEAD")
if run("branch", "--show-current").stdout.decode().strip() != "main":
    raise SystemExit("push requires main")
if run("status", "--porcelain", "--untracked-files=no").stdout.strip():
    raise SystemExit("push requires a clean tracked worktree")
if OUT.exists():
    raise SystemExit("push operation evidence already exists; preserve and inspect")
OUT.mkdir(parents=True)

started = now()
preflight = require_success(run("ls-remote", "origin", "refs/heads/main"), "remote preflight")
preflight_sha = preflight.split()[0] if preflight else ""
if preflight_sha != EXPECTED_BASE:
    raise SystemExit(f"remote main changed before push: {preflight_sha!r}; expected {EXPECTED_BASE}")
if run("merge-base", "--is-ancestor", preflight_sha, EXPECTED_HEAD).returncode:
    raise SystemExit("local payload is not a fast-forward from remote main")

push_result = run("push", "--porcelain", "origin", "main")
push_output = push_result.stdout.decode("utf-8", "replace")
(OUT / "push-output.txt").write_text(push_output, encoding="utf-8")
if push_result.returncode:
    raise SystemExit(f"normal push failed ({push_result.returncode}): {push_output}")
postflight = require_success(run("ls-remote", "origin", "refs/heads/main"), "remote postflight")
postflight_sha = postflight.split()[0] if postflight else ""
if postflight_sha != EXPECTED_HEAD:
    raise SystemExit(f"remote main did not advance to payload SHA: {postflight_sha!r}")

record = {
    "format_version": 1, "kind": "push_operation", "task_id": "T034", "task_revision": 4,
    "delivery_id": "bd1c82a0-cbbf-4487-880e-0e85a0672c8d", "phase": "payload-code",
    "remote": "origin", "ref": "refs/heads/main", "started_at_utc": started,
    "completed_at_utc": now(), "expected_base_sha": EXPECTED_BASE, "payload_sha": EXPECTED_HEAD,
    "preflight_remote_sha": preflight_sha, "postflight_remote_sha": postflight_sha,
    "normal_fast_forward_push": True, "force_push": False, "result": "PASS",
    "push_output_ref": f"{OUT.relative_to(ROOT).as_posix()}/push-output.txt",
}
(OUT / "push-operation.json").write_text(json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n", encoding="utf-8")
print(json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=False))
