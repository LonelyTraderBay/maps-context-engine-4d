from __future__ import annotations

import hashlib
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path


ROOT = Path.cwd().resolve()
DELIVERY = "784ed9da-3a4e-4198-909f-d96b1efe6865"
TASK = "T062"
REVISION = 1
BASE_SHA = "d6f3c306875e788e333c2035c5006bbbb213b1f3"
SUPPORT = Path(f"artifacts/git-delivery-support/T062-r1-{DELIVERY}")
ATTEMPT = int(sys.argv[1]) if len(sys.argv) > 1 else 1
if ATTEMPT < 1 or ATTEMPT > 9:
    raise SystemExit("attempt must be between 1 and 9")
OUTPUT = SUPPORT / f"local-checks/payload/attempt-{ATTEMPT}"
PLAN = SUPPORT / "check-plan-canonical.json"
PLAN_SHA = "58dc7f851ccdd86fa87f2bdcd3b794b82087d1e92b332f17a20ab1f63deb1a7e"

CHECKS = [
    ("delivery-plan", ["python", "-B", "tools/ci/delivery_policy.py", "check-plan", "--plan", PLAN.as_posix(), "--workflow", ".github/workflows/bootstrap.yml", "--sha256", PLAN_SHA]),
    ("bootstrap-source", ["python", "-B", "tools/ci/bootstrap_check.py"]),
    ("bootstrap-fixtures", ["python", "-B", "tools/ci/test_bootstrap_check.py"]),
    ("spec-check", ["python", "-B", "tools/spec_check/check.py"]),
    ("spec-check-fixtures", ["python", "-B", "tools/spec_check/test_spec_check.py"]),
    ("delivery-policy-fixtures", ["python", "-B", "tools/ci/test_delivery_policy.py"]),
    ("checkpoint-digest-fixtures", ["python", "-B", "tools/ci/test_checkpoint_digest.py"]),
    ("go-format", ["python", "-B", "tools/ci/check_gofmt.py"]),
    ("focused-go-tests", ["go", "test", "./internal/history", "./tests/contract"]),
    ("all-go-tests", ["go", "test", "./..."]),
    ("go-vet", ["go", "vet", "./..."]),
    ("go-build", ["go", "build", "./..."]),
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


started = now()
head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, check=True, stdout=subprocess.PIPE).stdout.decode().strip()
if head != BASE_SHA:
    raise SystemExit(f"expected allocation base {BASE_SHA}, observed {head}")

results: list[dict] = []
for index, (check_id, argv) in enumerate(CHECKS, start=1):
    check_started = now()
    result = subprocess.run(argv, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
    check_completed = now()
    suffix = "PASS" if result.returncode == 0 else "FAIL"
    log_ref = OUTPUT / f"{index:02d}-{check_id}.log"
    write(log_ref, result.stdout)
    results.append(
        {
            "argv": argv,
            "check_id": check_id,
            "completed_at_utc": check_completed,
            "delivery_id": DELIVERY,
            "exit_code": result.returncode,
            "format_version": 1,
            "kind": "local_check_execution",
            "log_ref": log_ref.as_posix(),
            "log_sha256": hashlib.sha256(result.stdout).hexdigest(),
            "result": suffix,
            "started_at_utc": check_started,
            "task_id": TASK,
            "task_revision": REVISION,
        }
    )

code_refs = [
    "internal/history/records_scxml.go",
    "internal/history/records_state_approval.go",
    "internal/history/records_state_test.go",
    "artifacts/preflight/T062/state-machine-fixture-plan.json",
]
code_hashes = {ref: hashlib.sha256((ROOT / ref).read_bytes()).hexdigest() for ref in code_refs}
summary = {
    "base_sha": BASE_SHA,
    "check_plan_ref": PLAN.as_posix(),
    "check_plan_sha256": PLAN_SHA,
    "checks": results,
    "code_file_sha256": code_hashes,
    "completed_at_utc": now(),
    "delivery_id": DELIVERY,
    "failed_count": sum(item["result"] != "PASS" for item in results),
    "format_version": 1,
    "kind": "payload_local_check_summary",
    "passed_count": sum(item["result"] == "PASS" for item in results),
    "result": "PASS" if all(item["result"] == "PASS" for item in results) else "FAIL",
    "started_at_utc": started,
    "task_id": TASK,
    "task_revision": REVISION,
}
encoded = canonical(summary)
write(OUTPUT / "summary.json", encoded)
print(
    json.dumps(
        {
            "result": summary["result"],
            "passed_count": summary["passed_count"],
            "failed_count": summary["failed_count"],
            "summary_ref": (OUTPUT / "summary.json").as_posix(),
            "summary_sha256": hashlib.sha256(encoded).hexdigest(),
        },
        sort_keys=True,
    )
)
if summary["failed_count"]:
    raise SystemExit(1)
