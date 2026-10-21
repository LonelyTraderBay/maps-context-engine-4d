from __future__ import annotations

import hashlib
import json
import subprocess
from pathlib import Path


ROOT = Path.cwd().resolve()
SUPPORT = Path("artifacts/git-delivery-support/T062-r1-784ed9da-3a4e-4198-909f-d96b1efe6865")
BASE = SUPPORT / "archive-view/ci/allocation"
OWNER = "LonelyTraderBay"
REPO = "maps-context-engine-4d"
RUN_ID = 39583224200
SHA = "d6f3c306875e788e333c2035c5006bbbb213b1f3"
EXPECTED_JOBS = {
    "bootstrap (ubuntu-latest)",
    "bootstrap (windows-latest)",
    "explorer-contract (ubuntu-latest)",
    "explorer-contract (windows-latest)",
    "spec-contract (ubuntu-latest)",
    "spec-contract (windows-latest)",
}


def canonical(value: object) -> bytes:
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")


def write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        if path.read_bytes() != data:
            raise SystemExit(f"preserving conflicting evidence: {path}")
        return
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()


def api(path: str) -> tuple[dict, bytes]:
    result = subprocess.run(
        ["gh", "api", path], cwd=ROOT, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE
    )
    value = json.loads(result.stdout)
    encoded = canonical(value)
    return value, encoded


run, run_bytes = api(f"repos/{OWNER}/{REPO}/actions/runs/{RUN_ID}")
if run.get("head_sha") != SHA or run.get("event") != "push" or run.get("run_attempt") != 2:
    raise SystemExit("allocation workflow identity or final attempt mismatch")
if run.get("status") != "completed" or run.get("conclusion") != "success":
    raise SystemExit("allocation workflow is not successful")
write(BASE / "run-api.json", run_bytes)

attempts: dict[int, dict] = {}
raw_refs: dict[int, str] = {}
for attempt in (1, 2):
    jobs, jobs_bytes = api(
        f"repos/{OWNER}/{REPO}/actions/runs/{RUN_ID}/attempts/{attempt}/jobs?per_page=100"
    )
    rows = jobs.get("jobs", [])
    if jobs.get("total_count") != 6 or {row.get("name") for row in rows} != EXPECTED_JOBS:
        raise SystemExit(f"attempt {attempt} job matrix mismatch")
    if any(row.get("status") != "completed" for row in rows):
        raise SystemExit(f"attempt {attempt} contains unfinished jobs")
    api_ref = BASE / f"attempt-{attempt}-jobs-api.json"
    write(api_ref, jobs_bytes)
    rows = sorted(rows, key=lambda row: row["name"])
    attempts[attempt] = {
        "status": "completed",
        "conclusion": "success" if all(row.get("conclusion") == "success" for row in rows) else "failure",
        "jobs": [
            {
                "name": row["name"],
                "conclusion": row.get("conclusion"),
                "html_url": row.get("html_url"),
                "steps": [
                    {"name": step.get("name"), "conclusion": step.get("conclusion")}
                    for step in row.get("steps", [])
                ],
            }
            for row in rows
        ],
        "jobs_ref": api_ref.as_posix(),
        "jobs_sha256": hashlib.sha256(jobs_bytes).hexdigest(),
    }
    raw_refs[attempt] = api_ref.as_posix()

failed = [row["name"] for row in attempts[1]["jobs"] if row["conclusion"] == "failure"]
if attempts[1]["conclusion"] != "failure" or failed != ["explorer-contract (windows-latest)"]:
    raise SystemExit("attempt 1 is not the observed single Windows Explorer failure")
if attempts[2]["conclusion"] != "success":
    raise SystemExit("attempt 2 is not 6/6 PASS")

failure_log = SUPPORT / "archive-view/ci/allocation-attempt-1-failure.log"
if not failure_log.is_file():
    raise SystemExit("attempt 1 failure log is missing")
receipt = {
    "format_version": 1,
    "kind": "allocation_ci_attempt_receipt",
    "task_id": "T062",
    "task_revision": 1,
    "delivery_id": "784ed9da-3a4e-4198-909f-d96b1efe6865",
    "subject_sha": SHA,
    "workflow_run_id": RUN_ID,
    "workflow_url": run.get("html_url"),
    "event": run.get("event"),
    "latest_attempt": run.get("run_attempt"),
    "attempt_1": {
        **attempts[1],
        "passed_jobs": 5,
        "failed_jobs": 1,
        "failed_contexts": failed,
        "failure_log_ref": failure_log.as_posix(),
        "failure_log_sha256": hashlib.sha256(failure_log.read_bytes()).hexdigest(),
    },
    "attempt_2": {
        **attempts[2],
        "passed_jobs": 6,
        "failed_jobs": 0,
        "failed_contexts": [],
    },
    "run_api_ref": (BASE / "run-api.json").as_posix(),
    "run_api_sha256": hashlib.sha256(run_bytes).hexdigest(),
}
receipt_bytes = canonical(receipt)
write(BASE / "collector-summary.json", receipt_bytes)
print(
    json.dumps(
        {
            "head_sha": SHA,
            "attempt_1": "FAIL(5/6)",
            "attempt_2": "PASS(6/6)",
            "receipt_ref": (BASE / "collector-summary.json").as_posix(),
            "receipt_sha256": hashlib.sha256(receipt_bytes).hexdigest(),
        },
        sort_keys=True,
    )
)
