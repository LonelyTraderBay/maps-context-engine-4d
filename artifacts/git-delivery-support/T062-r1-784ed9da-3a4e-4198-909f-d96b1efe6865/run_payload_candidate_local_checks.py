from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd().resolve()
TASK = "T062"
DELIVERY = "784ed9da-3a4e-4198-909f-d96b1efe6865"
SOURCE_SHA = "a342bd77ba607565c8a8411b9177d55253e7a4a1"
PLAN_SHA = "58dc7f851ccdd86fa87f2bdcd3b794b82087d1e92b332f17a20ab1f63deb1a7e"
SUPPORT = Path(f"artifacts/git-delivery-support/T062-r1-{DELIVERY}")
OUTPUT = SUPPORT / "local-checks/payload-candidate/attempt-1"
PLAN = SUPPORT / "check-plan-canonical.json"
NODE_ROOT = Path(os.environ["TEMP"]) / "mce4d-t060-node-v24.22.0" / "node-v24.22.0-win-x64"
NODE = NODE_ROOT / "node.exe"
NPM_CLI = NODE_ROOT / "node_modules/npm/bin/npm-cli.js"
CHECKS = [
    ("bootstrap", ["python", "-B", "tools/ci/bootstrap_check.py"]),
    ("bootstrap-fixtures", ["python", "-B", "tools/ci/test_bootstrap_check.py"]),
    ("delivery-plan", ["python", "-B", "tools/ci/delivery_policy.py", "check-plan", "--plan", PLAN.as_posix(), "--workflow", ".github/workflows/bootstrap.yml", "--sha256", PLAN_SHA]),
    ("spec-check", ["python", "-B", "tools/spec_check/check.py"]),
    ("spec-check-fixtures", ["python", "-B", "tools/spec_check/test_spec_check.py"]),
    ("delivery-policy-fixtures", ["python", "-B", "tools/ci/test_delivery_policy.py"]),
    ("checkpoint-digest-fixtures", ["python", "-B", "tools/ci/test_checkpoint_digest.py"]),
    ("go-format", ["python", "-B", "tools/ci/check_gofmt.py"]),
    ("go-test", ["go", "test", "./..."]),
    ("go-vet", ["go", "vet", "./..."]),
    ("go-build", ["go", "build", "./..."]),
    ("explorer-install", None),
    ("explorer-contract", None),
]


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def canonical(value: object) -> bytes:
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")


def write(path: Path, data: bytes) -> None:
    path = ROOT / path
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        if path.read_bytes() != data:
            raise SystemExit(f"preserving conflicting evidence: {path}")
        return
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()


def run_check(check_id: str, argv: list[str] | None) -> subprocess.CompletedProcess[bytes]:
    env = os.environ.copy()
    if check_id == "explorer-install":
        if not NODE.is_file() or not NPM_CLI.is_file():
            return subprocess.CompletedProcess([], 127, b"Pinned Node.js 24.22.0 / npm CLI is unavailable\n")
        with tempfile.TemporaryDirectory(prefix="mce4d-t062-npmci-") as temp_name:
            temp = Path(temp_name)
            shutil.copy2(ROOT / "package.json", temp / "package.json")
            shutil.copy2(ROOT / "package-lock.json", temp / "package-lock.json")
            workspace = temp / "web/explorer"
            workspace.mkdir(parents=True)
            shutil.copy2(ROOT / "web/explorer/package.json", workspace / "package.json")
            env["PATH"] = str(NODE_ROOT) + ";" + env.get("PATH", "")
            env["npm_config_script_shell"] = str(Path(os.environ["WINDIR"]) / "System32/WindowsPowerShell/v1.0/powershell.exe")
            command = [str(NODE), str(NPM_CLI), "ci", "--ignore-scripts", "--no-audit", "--no-fund"]
            result = subprocess.run(command, cwd=temp, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False, timeout=900)
            manifest_hashes = {
                "package_json": hashlib.sha256((ROOT / "package.json").read_bytes()).hexdigest(),
                "package_lock": hashlib.sha256((ROOT / "package-lock.json").read_bytes()).hexdigest(),
                "explorer_package_json": hashlib.sha256((ROOT / "web/explorer/package.json").read_bytes()).hexdigest(),
            }
            prefix = canonical({"source_sha": SOURCE_SHA, "manifest_sha256": manifest_hashes}) + b"\n"
            return subprocess.CompletedProcess(command, result.returncode, prefix + result.stdout)
    if check_id == "explorer-contract":
        if not NODE.is_file() or not NPM_CLI.is_file():
            return subprocess.CompletedProcess([], 127, b"Pinned Node.js 24.22.0 / npm CLI is unavailable\n")
        env["PATH"] = str(NODE_ROOT) + ";" + env.get("PATH", "")
        env["npm_config_script_shell"] = str(Path(os.environ["WINDIR"]) / "System32/WindowsPowerShell/v1.0/powershell.exe")
        argv = [str(NODE), str(NPM_CLI), "run", "test:contract:ui"]
    assert argv is not None
    return subprocess.run(argv, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False, timeout=900)


head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, check=True, stdout=subprocess.PIPE).stdout.decode("ascii").strip()
if head != SOURCE_SHA:
    raise SystemExit(f"expected candidate {SOURCE_SHA}, observed {head}")
plan_bytes = (ROOT / PLAN).read_bytes()
plan = json.loads(plan_bytes)
if hashlib.sha256(plan_bytes).hexdigest() != PLAN_SHA:
    raise SystemExit("frozen plan hash mismatch")
expected = [ref.rsplit("/", 1)[-1].removesuffix(".json") for ref in plan["local_check_refs"]]
if expected != [item[0] for item in CHECKS]:
    raise SystemExit(f"frozen local check order mismatch: {expected}")

started = now()
checks: list[dict] = []
for index, (check_id, argv) in enumerate(CHECKS, start=1):
    print(f"START {check_id}", flush=True)
    check_started = now()
    tick = time.monotonic()
    result = run_check(check_id, argv)
    completed = now()
    log_ref = OUTPUT / f"{index:02d}-{check_id}.log"
    write(log_ref, result.stdout)
    checks.append(
        {
            "argv": argv,
            "check_id": check_id,
            "completed_at_utc": completed,
            "delivery_id": DELIVERY,
            "exit_code": result.returncode,
            "format_version": 1,
            "kind": "local_check_execution",
            "log_ref": log_ref.as_posix(),
            "log_sha256": hashlib.sha256(result.stdout).hexdigest(),
            "result": "PASS" if result.returncode == 0 else "FAIL",
            "started_at_utc": check_started,
            "task_id": TASK,
            "task_revision": 1,
            "elapsed_seconds": round(time.monotonic() - tick, 3),
        }
    )
    print(f"END {check_id}: {'PASS' if result.returncode == 0 else 'FAIL'}", flush=True)

code_refs = [
    "internal/history/records_scxml.go",
    "internal/history/records_state_approval.go",
    "internal/history/records_state_test.go",
    "artifacts/preflight/T062/state-machine-fixture-plan.json",
]
code_hashes = {ref: hashlib.sha256((ROOT / ref).read_bytes()).hexdigest() for ref in code_refs}
summary = {
    "base_sha": "d6f3c306875e788e333c2035c5006bbbb213b1f3",
    "check_plan_ref": PLAN.as_posix(),
    "check_plan_sha256": PLAN_SHA,
    "checks": checks,
    "code_file_sha256": code_hashes,
    "completed_at_utc": now(),
    "delivery_id": DELIVERY,
    "failed_count": sum(item["result"] != "PASS" for item in checks),
    "format_version": 1,
    "kind": "payload_candidate_local_check_summary",
    "passed_count": sum(item["result"] == "PASS" for item in checks),
    "result": "PASS" if all(item["result"] == "PASS" for item in checks) else "FAIL",
    "source_sha": SOURCE_SHA,
    "started_at_utc": started,
    "task_id": TASK,
    "task_revision": 1,
}
data = canonical(summary)
write(OUTPUT / "summary.json", data)
attempt = {
    "delivery_id": DELIVERY,
    "format_version": 1,
    "kind": "local_check_attempt",
    "phase": "payload_candidate",
    "plan_sha256": PLAN_SHA,
    "revision": SOURCE_SHA,
    "result": summary["result"],
    "passed_checks": summary["passed_count"],
    "failed_checks": summary["failed_count"],
    "total_checks": len(checks),
    "summary_ref": (OUTPUT / "summary.json").as_posix(),
    "summary_sha256": hashlib.sha256(data).hexdigest(),
    "task_id": TASK,
    "task_revision": 1,
}
write(OUTPUT / "attempt.json", canonical(attempt))
print(json.dumps(attempt, ensure_ascii=False, sort_keys=True), flush=True)
if summary["failed_count"]:
    raise SystemExit(1)
