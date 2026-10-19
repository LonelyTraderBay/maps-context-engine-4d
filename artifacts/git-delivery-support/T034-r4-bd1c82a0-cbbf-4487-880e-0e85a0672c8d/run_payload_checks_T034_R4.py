from __future__ import annotations

import hashlib
import json
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd()
SUPPORT_REF = Path("artifacts/git-delivery-support/T034-r4-bd1c82a0-cbbf-4487-880e-0e85a0672c8d")
SUPPORT = ROOT / SUPPORT_REF
PLAN = SUPPORT / "check-plan.json"
PLAN_SHA = "bc157681517301a6cdff3601707d09f2f5f5d8e8365fc05474d047100aa9b495"
BASE = "16136bf18b8d0e77ede1500ccf57399391addcd5"
EXPECTED_HEAD = "6827429e6360741ceb8a42db94a5b4b0778b4991"
ATTEMPT = sys.argv[1] if len(sys.argv) > 1 else "attempt-1"
OUT = SUPPORT / "local-checks/payload" / ATTEMPT


def git(*args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=ROOT, text=True, encoding="utf-8").strip()


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


if git("rev-parse", "HEAD") != EXPECTED_HEAD or git("rev-parse", "HEAD^") != BASE:
    raise SystemExit("payload checks require the exact T034 r4 fix commit on main")
if git("branch", "--show-current") != "main":
    raise SystemExit("payload checks require the authorized main branch")
if git("status", "--porcelain", "--untracked-files=no") or git("write-tree") != git("rev-parse", "HEAD^{tree}"):
    raise SystemExit("payload checks require a clean tracked worktree and index")
if hashlib.sha256(PLAN.read_bytes()).hexdigest() != PLAN_SHA:
    raise SystemExit("frozen T034 r4 check plan SHA mismatch")
if OUT.exists():
    raise SystemExit("payload attempt-1 already exists; preserve and inspect")
OUT.mkdir(parents=True)

commands = [
    ("bootstrap_check", ["python", "-B", "tools/ci/bootstrap_check.py"]),
    ("test_bootstrap_check", ["python", "-B", "tools/ci/test_bootstrap_check.py"]),
    ("delivery_policy_plan", ["python", "-B", "tools/ci/delivery_policy.py", "check-plan", "--plan", str(PLAN), "--sha256", PLAN_SHA]),
    ("spec_check", ["python", "-B", "tools/spec_check/check.py"]),
    ("test_spec_check", ["python", "-B", "tools/spec_check/test_spec_check.py"]),
    ("test_delivery_policy", ["python", "-B", "tools/ci/test_delivery_policy.py"]),
    ("test_checkpoint_digest", ["python", "-B", "tools/ci/test_checkpoint_digest.py"]),
    ("go_format", ["python", "-B", "tools/ci/check_gofmt.py"]),
    ("go_test_all", ["go", "test", "./..."]),
    ("go_vet_all", ["go", "vet", "./..."]),
    ("go_build_all", ["go", "build", "./..."]),
]
checks: list[dict] = []
for index, (name, argv) in enumerate(commands, 1):
    started = now()
    tick = time.perf_counter()
    try:
        result = subprocess.run(argv, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=1800, check=False)
        output, exit_code = result.stdout, result.returncode
    except subprocess.TimeoutExpired as exc:
        output, exit_code = exc.stdout or b"", 124
    except OSError as exc:
        output, exit_code = str(exc).encode("utf-8", "replace"), 127
    completed = now()
    duration = round(time.perf_counter() - tick, 3)
    log = OUT / f"{name}.log"
    log.write_bytes(output)
    checks.append({
        "name": name, "argv": argv, "started_at_utc": started, "completed_at_utc": completed,
        "duration_seconds": duration, "exit_code": exit_code,
        "log": log.relative_to(SUPPORT).as_posix(), "log_sha256": hashlib.sha256(output).hexdigest(),
    })
    print(f"{'PASS' if exit_code == 0 else 'FAIL'} {index}/{len(commands)} {name} ({duration}s)", flush=True)
    if exit_code:
        print(output.decode("utf-8", "replace")[-6000:], flush=True)
        break

tree = git("rev-parse", "HEAD^{tree}")
summary = {
    "format_version": 1, "kind": "local_check_summary", "task_id": "T034", "task_revision": 4,
    "delivery_id": "bd1c82a0-cbbf-4487-880e-0e85a0672c8d", "phase": "payload",
    "base_sha": BASE, "candidate_sha": EXPECTED_HEAD, "candidate_tree_sha": tree,
    "check_plan_sha256": PLAN_SHA, "created_at_utc": now(), "check_count": len(commands),
    "executed_checks": len(checks), "passed_checks": sum(check["exit_code"] == 0 for check in checks),
    "failed_checks": sum(check["exit_code"] != 0 for check in checks),
    "result": "PASS" if len(checks) == len(commands) and all(check["exit_code"] == 0 for check in checks) else "FAIL",
    "checkout_binding": {"tracked_worktree_clean": True, "preexisting_untracked_paths_preserved": True},
    "checks": checks,
}
summary_bytes = (json.dumps(summary, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n").encode("utf-8")
(OUT / "summary.json").write_bytes(summary_bytes)
receipt = {key: value for key, value in summary.items() if key != "checks"}
receipt.update({
    "kind": "local_check_receipt", "runner_summary_ref": f"{SUPPORT_REF.as_posix()}/local-checks/payload/{ATTEMPT}/summary.json",
    "runner_summary_sha256": hashlib.sha256(summary_bytes).hexdigest(), "checks": checks,
})
(OUT / "receipt.json").write_text(json.dumps(receipt, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n", encoding="utf-8")
print(json.dumps({"result": summary["result"], "passed": summary["passed_checks"], "total": len(commands),
                  "candidate_sha": EXPECTED_HEAD, "tree_sha": tree,
                  "summary_sha256": hashlib.sha256(summary_bytes).hexdigest()}, sort_keys=True, separators=(",", ":")), flush=True)
sys.exit(0 if summary["result"] == "PASS" else 1)
