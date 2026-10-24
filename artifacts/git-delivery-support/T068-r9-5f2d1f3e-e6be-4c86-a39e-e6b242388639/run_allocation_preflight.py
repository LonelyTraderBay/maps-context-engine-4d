from __future__ import annotations

import fnmatch
import hashlib
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

sys.stdout.reconfigure(encoding="utf-8")
ROOT = Path.cwd().resolve()
sys.path.insert(0, str(ROOT))

DELIVERY = "5f2d1f3e-e6be-4c86-a39e-e6b242388639"
TASK = "T068"
REVISION = 7
BASE = "649d338f17432df527428a4c29b9a5936617a599"
ATTEMPT = int(sys.argv[1]) if len(sys.argv) > 1 else 1
if ATTEMPT < 1:
    raise SystemExit("attempt number must be positive")
SUPPORT = f"artifacts/git-delivery-support/T068-r9-{DELIVERY}"
OUT = ROOT / SUPPORT / f"local-checks/allocation/attempt-{ATTEMPT}"
RECORDS = OUT / "records"
PLAN_REF = f"{SUPPORT}/check-plan-canonical.json"
PLAN_SHA = "d07558f7060fbeb97804ffd34aea8491fd96e2ff547dc521d6ceaaa6657f84f3"
ALLOWED = {
    "Maps-Context-Engine-4D_SPEC.md",
    "tests/acceptance/polyglot_benchmark_test.go",
    ".gitattributes",
}
EXPECTED_DOMAINS = [
    "typescript", "frontend-backend", "python", "python-notebook", "sql",
    "http-schema-proto", "events", "state-machine", "federation", "temporal-diff",
    "trading", "commerce", "ai-lineage", "architecture", "benchmark",
]

if (ROOT / ".git").resolve() != (ROOT / ".git").resolve(strict=True):
    raise SystemExit("repository root does not resolve safely")
if RECORDS.exists():
    raise SystemExit(f"refusing to overwrite existing attempt: {RECORDS}")
RECORDS.mkdir(parents=True)


def utc() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha_file(path: Path) -> str:
    return sha(path.read_bytes())


def write_json(path: Path, value: object) -> str:
    data = (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    path.write_bytes(data)
    return sha(data)


def run(check_id: str, command_ref: str, argv: list[str], assertion=None) -> dict[str, object]:
    started = utc()
    begun = time.monotonic()
    try:
        if assertion is None:
            proc = subprocess.run(argv, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
            raw = proc.stdout
            code = proc.returncode
        else:
            raw, code = assertion()
        result = "PASS" if code == 0 else "FAIL"
    except Exception as exc:  # Preserve a real failing gate record.
        raw = f"FAIL: {type(exc).__name__}: {exc}\n".encode("utf-8")
        code = 1
        result = "FAIL"
    ended = utc()
    base = f"{SUPPORT}/local-checks/allocation/attempt-{ATTEMPT}/records/{check_id}"
    log_rel, record_rel = base + ".log", base + ".json"
    log_path = ROOT / log_rel
    log_path.write_bytes(raw)
    record = {
        "argv": argv,
        "check_id": check_id,
        "command_ref": command_ref,
        "cwd": "<repository-root>",
        "delivery_id": DELIVERY,
        "elapsed_seconds": round(time.monotonic() - begun, 3),
        "ended_at_utc": ended,
        "exit_code": code,
        "format_version": 1,
        "kind": "local_check_execution",
        "log_ref": log_rel,
        "log_sha256": sha(raw),
        "record_ref": record_rel,
        "result": result,
        "started_at_utc": started,
        "task_id": TASK,
        "task_revision": REVISION,
    }
    record_sha = write_json(ROOT / record_rel, record)
    return {
        "check_id": check_id,
        "record_ref": record_rel,
        "sha256": record_sha,
        "result": result,
        "exit_code": code,
        "log_ref": log_rel,
        "log_sha256": sha(raw),
        "output": raw.decode("utf-8", "replace")[-1400:],
    }


python = sys.executable
checks: list[dict[str, object]] = []
checks.append(run(
    "delivery-plan",
    "python -B tools/ci/delivery_policy.py check-plan",
    [python, "-B", "tools/ci/delivery_policy.py", "check-plan"],
))
checks.append(run(
    "spec-check",
    "python -B tools/spec_check/check.py",
    [python, "-B", "tools/spec_check/check.py"],
))
checks.append(run(
    "spec-fixtures",
    "python -B tools/spec_check/test_spec_check.py",
    [python, "-B", "tools/spec_check/test_spec_check.py"],
))
checks.append(run("git-diff-check", "git diff --check", ["git", "diff", "--check"]))


def fixture_assertion() -> tuple[bytes, int]:
    path = ROOT / "artifacts/preflight/T068/polyglot-acceptance-plan-r2.json"
    fixture = json.loads(path.read_text(encoding="utf-8"))
    cases = fixture["cases"]
    ids = [item["id"] for item in cases]
    domains = [item["domain"] for item in cases]
    cohort = fixture["cohort"]
    assert fixture["kind"] == "polyglot_acceptance_plan"
    assert ids == [f"C{number:03d}" for number in range(55, 70)]
    assert domains == EXPECTED_DOMAINS
    assert cohort["minimum_repositories"] == 3 and cohort["minimum_tasks"] >= 30
    assert cohort["repeats"] == 3 and cohort["randomization_seed"] == 68055
    assert cohort["modes"] == ["baseline", "full", "incremental"]
    assert cohort["repositories"] == ["alpha", "beta", "gamma"]
    assert cohort["real_repository_result"] == "NOT_RUN"
    assert "independently authored" in fixture["oracle"] and "never its own oracle" in fixture["oracle"]
    assert fixture["report_contract"]["correctness_failure_may_not_be_offset_by_context_savings"] is True
    assert fixture["report_contract"]["token_savings_claim_without_host_receipt"] == "FORBIDDEN"
    assert fixture["report_contract"]["token_usage_status_without_host_receipt"] == "NOT_RUN"
    return f"PASS: {len(cases)} fixture cases and domain bindings match C055-C069.\n".encode(), 0


checks.append(run(
    "fixture-plan-json",
    "inline: verify T068 fixture identities, domains, cohort and report contract",
    [python, "<inline fixture assertions>"],
    fixture_assertion,
))


def allocation_assertion() -> tuple[bytes, int]:
    base = ROOT / SUPPORT
    selection = json.loads((base / "selection.json").read_text(encoding="utf-8"))
    mode = json.loads((base / "task-mode.json").read_text(encoding="utf-8"))
    dependency = json.loads((base / "dependency-review.json").read_text(encoding="utf-8"))
    allocation = json.loads((base / "allocation.json").read_text(encoding="utf-8"))
    current = json.loads((base / "allocation-current.json").read_text(encoding="utf-8"))
    lineage = json.loads((base / "assignment-lineage.json").read_text(encoding="utf-8"))
    plan_data = (ROOT / PLAN_REF).read_bytes()
    replay = json.loads((base / "selection-chain-replay.json").read_text(encoding="utf-8"))
    assignment_ref = selection["assignment_ref"]
    assignment_sha = sha_file(ROOT / assignment_ref)
    assert selection["task_id"] == mode["task_id"] == allocation["task_id"] == current["task_id"] == TASK
    assert selection["task_revision"] == mode["task_revision"] == allocation["task_revision"] == current["task_revision"] == REVISION
    assert selection["active_delivery_id"] == mode["delivery_id"] == allocation["delivery_id"] == current["delivery_id"] == DELIVERY
    assert selection["assignment_ref"] == allocation["assignment_ref"] == current["assignment_ref"]
    assert allocation == current and allocation["base_sha"] == BASE
    assert allocation["check_plan_ref"] == PLAN_REF and allocation["check_plan_sha256"] == sha(plan_data) == PLAN_SHA
    assert allocation["fixture_plan_ref"] == "artifacts/preflight/T068/polyglot-acceptance-plan-r2.json"
    assert allocation["fixture_plan_sha256"] == sha_file(ROOT / allocation["fixture_plan_ref"])
    assert dependency["result"] == "READY" and len(dependency["dependencies"]) == 5
    assert {item["task_id"] for item in dependency["dependencies"]} == {f"T{n:03d}" for n in range(63, 68)}
    assert all(item["effective_status"] == "DONE" for item in dependency["dependencies"])
    assert set(mode["change_budget"]["allowed_paths"]) == set(allocation["scope_paths"])
    assert ALLOWED | {f"{SUPPORT}/**"} == set(allocation["scope_paths"])
    assert lineage["task_id"] == TASK and lineage["source_assignments"]
    assert any(item["assignment_ref"] == assignment_ref and item["sha256"] == assignment_sha for item in lineage["source_assignments"])
    assert replay["verdict"] == "UNKNOWN" and replay["errors"] == []
    assert replay["task_id"] == TASK and replay["task_revision"] == REVISION
    assert replay["typed_selection_count"] == 7 and len(replay["selector_revisions"]) == 7
    assert replay["typed_lineage_refs"][-1] == f"{SUPPORT}/assignment-lineage.json"
    assert replay["archive_closure"] == replay["archive_publication_readback"] == "NOT_RUN"
    return (
        f"PASS: allocation, selection, lineage and dependencies match; plan_sha256={PLAN_SHA}; "
        f"selector replay=UNKNOWN with {len(replay['errors'])} errors and 7 revisions.\n"
    ).encode(), 0


checks.append(run(
    "allocation-artifacts",
    "inline: verify T068 r9 allocation/plan/dependency/lineage/replay bindings",
    [python, "<inline allocation assertions>"],
    allocation_assertion,
))


def scope_and_base_assertion() -> tuple[bytes, int]:
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip()
    refs = subprocess.run(
        ["git", "ls-remote", "origin", "refs/heads/main", "refs/heads/codex/t068-polyglot-matrix"],
        cwd=ROOT, capture_output=True, text=True, check=True,
    ).stdout
    observed = {line.split("\t", 1)[1]: line.split("\t", 1)[0] for line in refs.splitlines() if "\t" in line}
    assert head == BASE, f"HEAD changed before allocation commit: {head}"
    expected_refs = {"refs/heads/main", "refs/heads/codex/t068-polyglot-matrix"}
    assert set(observed) == expected_refs and set(observed.values()) == {BASE}, observed
    changed = subprocess.run(["git", "diff", "HEAD", "--name-only"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.splitlines()
    violations = [path for path in changed if path not in ALLOWED and not fnmatch.fnmatchcase(path, f"{SUPPORT}/**")]
    assert changed and not violations, {"changed_paths": changed, "violations": violations}
    status = subprocess.run(["git", "status", "--porcelain", "--untracked-files=all"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.splitlines()
    current_support = [line[3:] for line in status if line.startswith("?? ") and line[3:].startswith(SUPPORT + "/")]
    assert current_support and all(fnmatch.fnmatchcase(path, f"{SUPPORT}/**") for path in current_support)
    outside_untracked_count = sum(1 for line in status if line.startswith("?? ") and not line[3:].startswith(SUPPORT + "/"))
    observation = {
        "base_sha": BASE,
        "checked_at_utc": utc(),
        "delivery_id": DELIVERY,
        "format_version": 1,
        "head_sha": head,
        "kind": "allocation_base_revalidation",
        "remote": "origin",
        "remote_refs": observed,
        "scope_paths": sorted(ALLOWED | {f"{SUPPORT}/**"}),
        "task_id": TASK,
        "task_revision": REVISION,
        "tracked_changed_paths": changed,
        "current_delivery_untracked_paths": current_support,
        "preexisting_untracked_paths_outside_current_delivery": outside_untracked_count,
    }
    ref = ROOT / SUPPORT / "allocation-revalidation.json"
    revalidation_sha = write_json(ref, observation)
    output = (
        "PASS: HEAD and both origin refs match allocation base " + BASE + ".\n"
        "git ls-remote output:\n" + refs
        + f"Current delivery paths checked against allocation scope; {outside_untracked_count} pre-existing untracked paths outside this delivery were left untouched.\n"
    )
    return output.encode("utf-8"), 0


checks.append(run(
    "scope-and-base",
    "git ls-remote origin refs/heads/main refs/heads/codex/t068-polyglot-matrix; inline changed-path scope assertions",
    ["git", "ls-remote", "origin", "refs/heads/main", "refs/heads/codex/t068-polyglot-matrix"],
    scope_and_base_assertion,
))

failed = [item for item in checks if item["result"] != "PASS"]
summary = {
    "base_sha": BASE,
    "check_refs": [{"record_ref": item["record_ref"], "sha256": item["sha256"]} for item in checks],
    "check_sha256": [item["sha256"] for item in checks],
    "delivery_id": DELIVERY,
    "failed_count": len(failed),
    "format_version": 1,
    "kind": "local_check_summary",
    "passed_count": len(checks) - len(failed),
    "result": "FAIL" if failed else "PASS",
    "task_id": TASK,
    "task_revision": REVISION,
}
summary_ref = f"{SUPPORT}/local-checks/allocation/attempt-{ATTEMPT}/summary.json"
summary_sha = write_json(ROOT / summary_ref, summary)
allocation_check = {
    "base_sha": BASE,
    "check_plan_ref": PLAN_REF,
    "check_plan_sha256": PLAN_SHA,
    "check_refs": [{"record_ref": item["record_ref"], "sha256": item["sha256"]} for item in checks],
    "checked_at_utc": utc(),
    "delivery_id": DELIVERY,
    "failed_count": len(failed),
    "format_version": 1,
    "kind": "allocation_check",
    "passed_count": len(checks) - len(failed),
    "result": "FAIL" if failed else "PASS",
    "summary_ref": summary_ref,
    "summary_sha256": summary_sha,
    "task_id": TASK,
    "task_revision": REVISION,
}
allocation_check_ref = f"{SUPPORT}/allocation-check.json"
allocation_check_sha = write_json(ROOT / allocation_check_ref, allocation_check)
preflight = {
    "allocation_check_ref": allocation_check_ref,
    "allocation_check_sha256": allocation_check_sha,
    "base_sha": BASE,
    "check_plan_ref": PLAN_REF,
    "check_plan_sha256": PLAN_SHA,
    "checked_at_utc": utc(),
    "delivery_id": DELIVERY,
    "failed_count": len(failed),
    "format_version": 1,
    "kind": "allocation_preflight",
    "passed_count": len(checks) - len(failed),
    "result": "FAIL" if failed else "PASS",
    "scope_paths": sorted(ALLOWED | {f"{SUPPORT}/**"}),
    "task_id": TASK,
    "task_revision": REVISION,
}
preflight_ref = f"{SUPPORT}/allocation-preflight.json"
preflight_sha = write_json(ROOT / preflight_ref, preflight)
print(json.dumps({
    "delivery_id": DELIVERY,
    "result": allocation_check["result"],
    "passed": allocation_check["passed_count"],
    "failed": allocation_check["failed_count"],
    "summary_ref": summary_ref,
    "summary_sha256": summary_sha,
    "allocation_check_ref": allocation_check_ref,
    "allocation_check_sha256": allocation_check_sha,
    "preflight_ref": preflight_ref,
    "preflight_sha256": preflight_sha,
    "checks": [{"id": item["check_id"], "result": item["result"], "exit": item["exit_code"], "output": item["output"]} for item in checks],
}, ensure_ascii=False, indent=2))
if failed:
    raise SystemExit(1)
