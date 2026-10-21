from __future__ import annotations

import hashlib
import json
import subprocess
from pathlib import Path

ROOT = Path.cwd().resolve()
OWNER = "LonelyTraderBay"
REPO = "maps-context-engine-4d"
RUN_ID = 39588010911
SHA = "a342bd77ba607565c8a8411b9177d55253e7a4a1"
DELIVERY = "784ed9da-3a4e-4198-909f-d96b1efe6865"
SUPPORT = Path(f"artifacts/git-delivery-support/T062-r1-{DELIVERY}")
BASE = SUPPORT / "archive-view/ci/payload-candidate"
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
    path = ROOT / path
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        if path.read_bytes() != data:
            raise SystemExit(f"preserving conflicting evidence: {path}")
        return
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()


def api(path: str) -> tuple[dict, bytes]:
    result = subprocess.run(["gh", "api", path], cwd=ROOT, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    value = json.loads(result.stdout)
    return value, canonical(value)


run, run_bytes = api(f"repos/{OWNER}/{REPO}/actions/runs/{RUN_ID}")
if run.get("head_sha") != SHA or run.get("event") != "push" or run.get("id") != RUN_ID:
    raise SystemExit("candidate Actions run identity mismatch")
if run.get("status") != "completed" or run.get("conclusion") != "success":
    raise SystemExit("candidate Actions run is not 6/6 successful")
write(BASE / "run-api.json", run_bytes)

attempts: list[dict] = []
for number in range(1, int(run["run_attempt"]) + 1):
    jobs, jobs_bytes = api(f"repos/{OWNER}/{REPO}/actions/runs/{RUN_ID}/attempts/{number}/jobs?per_page=100")
    rows = jobs.get("jobs", [])
    if jobs.get("total_count") != 6 or {row.get("name") for row in rows} != EXPECTED_JOBS:
        raise SystemExit(f"attempt {number} matrix mismatch")
    if any(row.get("status") != "completed" for row in rows):
        raise SystemExit(f"attempt {number} has unfinished jobs")
    jobs_ref = BASE / f"attempt-{number}-jobs-api.json"
    write(jobs_ref, jobs_bytes)
    sorted_rows = sorted(rows, key=lambda row: row["name"])
    attempts.append({
        "attempt": number,
        "conclusion": "success" if all(row.get("conclusion") == "success" for row in sorted_rows) else "failure",
        "jobs": [
            {
                "name": row.get("name"),
                "conclusion": row.get("conclusion"),
                "html_url": row.get("html_url"),
                "steps": [{"name": step.get("name"), "conclusion": step.get("conclusion")} for step in row.get("steps", [])],
            }
            for row in sorted_rows
        ],
        "jobs_ref": jobs_ref.as_posix(),
        "jobs_sha256": hashlib.sha256(jobs_bytes).hexdigest(),
    })
if attempts[-1]["conclusion"] != "success":
    raise SystemExit("latest candidate attempt is not 6/6 PASS")

summary = {
    "format_version": 1,
    "kind": "payload_candidate_ci_attempt_receipt",
    "task_id": "T062",
    "task_revision": 1,
    "delivery_id": DELIVERY,
    "subject_sha": SHA,
    "workflow_run_id": RUN_ID,
    "workflow_url": run.get("html_url"),
    "event": run.get("event"),
    "latest_attempt": run.get("run_attempt"),
    "result": "PASS(6/6)",
    "attempts": attempts,
    "run_api_ref": (BASE / "run-api.json").as_posix(),
    "run_api_sha256": hashlib.sha256(run_bytes).hexdigest(),
}
summary_bytes = canonical(summary)
summary_ref = BASE / "collector-summary.json"
write(summary_ref, summary_bytes)
preflight = {
    "attempts": attempts,
    "delivery_id": DELIVERY,
    "format_version": 1,
    "kind": "payload_candidate_ci",
    "result": "PASS",
    "run_id": RUN_ID,
    "run_url": run.get("html_url"),
    "sha": SHA,
    "task_id": "T062",
    "task_revision": 1,
}
preflight_bytes = canonical(preflight)
write(Path("artifacts/preflight/T062/payload-candidate-ci.json"), preflight_bytes)
print(json.dumps({
    "result": "PASS(6/6)",
    "run_id": RUN_ID,
    "sha": SHA,
    "attempts": len(attempts),
    "collector_ref": summary_ref.as_posix(),
    "collector_sha256": hashlib.sha256(summary_bytes).hexdigest(),
    "preflight_ref": "artifacts/preflight/T062/payload-candidate-ci.json",
    "preflight_sha256": hashlib.sha256(preflight_bytes).hexdigest(),
}, sort_keys=True))
