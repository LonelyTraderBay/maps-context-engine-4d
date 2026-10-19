from __future__ import annotations

import hashlib
import json
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd()
SUPPORT_REF = "artifacts/git-delivery-support/T034-r4-bd1c82a0-cbbf-4487-880e-0e85a0672c8d"
SUPPORT = ROOT / SUPPORT_REF
PLAN = SUPPORT / "check-plan.json"
PLAN_SHA = "bc157681517301a6cdff3601707d09f2f5f5d8e8365fc05474d047100aa9b495"
TASK = "T034"
REVISION = 4
DELIVERY = "bd1c82a0-cbbf-4487-880e-0e85a0672c8d"
BASE = "ea8763c63e711e5ed87fc3bf7307d11ba286a709"
EXPECTED_HEAD = "d70b5cc76490d9cf60457869e77b0cffd19ad06e"
OUT = SUPPORT / "local-checks/allocation/attempt-1"


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def git(*args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=ROOT, text=True, encoding="utf-8").strip()


if git("rev-parse", "HEAD") != EXPECTED_HEAD or git("rev-parse", "HEAD^") != BASE:
    raise SystemExit("allocation checks require exact T035 commit with direct T033 closing parent")
if git("branch", "--show-current") != "main":
    raise SystemExit("allocation checks require the authorized main branch")
if git("status", "--porcelain", "--untracked-files=no"):
    raise SystemExit("tracked worktree must be clean at the frozen allocation commit")
tree = git("rev-parse", "HEAD^{tree}")
if git("write-tree") != tree:
    raise SystemExit("index tree differs from the allocation commit")
plan_bytes = PLAN.read_bytes()
if digest(plan_bytes) != PLAN_SHA:
    raise SystemExit("frozen T035 check plan SHA mismatch")

commands = [
    ("bootstrap_check", ["python", "-B", "tools/ci/bootstrap_check.py"]),
    ("test_bootstrap_check", ["python", "-B", "tools/ci/test_bootstrap_check.py"]),
    ("delivery_policy_plan", ["python", "-B", "tools/ci/delivery_policy.py", "check-plan",
                               "--plan", str(PLAN), "--sha256", PLAN_SHA]),
    ("spec_check", ["python", "-B", "tools/spec_check/check.py"]),
    ("test_spec_check", ["python", "-B", "tools/spec_check/test_spec_check.py"]),
    ("test_delivery_policy", ["python", "-B", "tools/ci/test_delivery_policy.py"]),
    ("test_checkpoint_digest", ["python", "-B", "tools/ci/test_checkpoint_digest.py"]),
    ("go_format", ["python", "-B", "tools/ci/check_gofmt.py"]),
    ("go_test_all", ["go", "test", "./..."]),
    ("go_vet_all", ["go", "vet", "./..."]),
    ("go_build_all", ["go", "build", "./..."]),
]
if (OUT / "summary.json").exists() or any(OUT.glob("*.log")):
    raise SystemExit("allocation attempt-1 already contains output; preserve and inspect")
OUT.mkdir(parents=True, exist_ok=True)
checks: list[dict] = []
for name, argv in commands:
    started = now()
    clock = time.perf_counter()
    try:
        result = subprocess.run(argv, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=1800, check=False)
        output = result.stdout
        exit_code = result.returncode
    except subprocess.TimeoutExpired as exc:
        output = exc.stdout or b""
        exit_code = 124
    except OSError as exc:
        output = str(exc).encode("utf-8", "replace")
        exit_code = 127
    duration = round(time.perf_counter() - clock, 3)
    completed = now()
    log_ref = (OUT / f"{name}.log").relative_to(SUPPORT).as_posix()
    (SUPPORT / log_ref).write_bytes(output)
    checks.append({
        "name": name,
        "argv": argv,
        "started_at_utc": started,
        "completed_at_utc": completed,
        "duration_seconds": duration,
        "exit_code": exit_code,
        "log": log_ref,
        "log_sha256": digest(output),
    })
    print(f"{'PASS' if exit_code == 0 else 'FAIL'} {len(checks)}/{len(commands)} {name} ({duration}s)", flush=True)
    if exit_code:
        print(output.decode("utf-8", "replace")[-6000:], flush=True)
        break

if git("rev-parse", "HEAD") != EXPECTED_HEAD or git("rev-parse", "HEAD^{tree}") != tree or git("write-tree") != tree:
    raise SystemExit("allocation candidate/tree/index changed during checks")
if git("status", "--porcelain", "--untracked-files=no"):
    raise SystemExit("tracked worktree changed during allocation checks")
passed = sum(item["exit_code"] == 0 for item in checks)
summary = {
    "format_version": 1,
    "kind": "local_check_summary",
    "delivery_id": DELIVERY,
    "task_id": TASK,
    "task_revision": REVISION,
    "phase": "allocation",
    "base_sha": BASE,
    "candidate_sha": EXPECTED_HEAD,
    "candidate_tree_sha": tree,
    "check_plan_sha256": PLAN_SHA,
    "created_at_utc": now(),
    "check_count": len(commands),
    "executed_checks": len(checks),
    "passed_checks": passed,
    "failed_checks": len(checks) - passed,
    "result": "PASS" if len(checks) == len(commands) and passed == len(commands) else "FAIL",
    "checkout_binding": {"tracked_worktree_clean": True, "preexisting_untracked_paths_preserved": True},
    "checks": checks,
}
summary_path = OUT / "summary.json"
summary_bytes = (json.dumps(summary, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n").encode("utf-8")
summary_path.write_bytes(summary_bytes)
receipt = {key: value for key, value in summary.items() if key != "checks"}
receipt.update({
    "kind": "local_check_receipt",
    "runner_summary_ref": f"{SUPPORT_REF}/local-checks/allocation/attempt-1/summary.json",
    "runner_summary_sha256": digest(summary_bytes),
    "checks": checks,
})
(OUT / "allocation-receipt.json").write_text(
    json.dumps(receipt, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n", encoding="utf-8"
)
print(json.dumps({"result": summary["result"], "passed": passed, "total": len(commands),
                  "candidate_sha": EXPECTED_HEAD, "tree_sha": tree,
                  "summary_sha256": digest(summary_bytes)}, sort_keys=True, separators=(",", ":")), flush=True)
sys.exit(0 if summary["result"] == "PASS" else 1)
