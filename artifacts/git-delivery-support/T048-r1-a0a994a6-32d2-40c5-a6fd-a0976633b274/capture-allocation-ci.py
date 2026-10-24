from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
import sys
import uuid
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd().resolve(strict=True)
sys.path.insert(0, str(ROOT))
from tools.spec_check import github_observer as observer

DELIVERY = "a0a994a6-32d2-40c5-a6fd-a0976633b274"
TASK = "T048"
REVISION = 1
BASE_SHA = "5293c93ffe014f8f81f2b827d5a409927705f263"
CANDIDATE_SHA = "14bb29b289de3c89091606565340969bad42c673"
TASK_BRANCH = "codex/t003-d089-regressions"
BRANCHES = {
    "refs/heads/main": "main",
    f"refs/heads/{TASK_BRANCH}": TASK_BRANCH,
}
SUPPORT = Path("artifacts/git-delivery-support") / f"T048-r1-{DELIVERY}"
PLAN_PATH = ROOT / SUPPORT / "check-plan-canonical.json"
PLAN_SHA256 = "8e5eb83b44e17fa863ef0bcf6fabe39ae85b8368e2bb6f4693e26ec21c83f95a"
GH = str(ROOT / "work/tooling/github-cli-2.103.2/bin/gh.exe")
OUTPUT_REL = SUPPORT / "archive-view/ci/allocation-A-attempt-2"
OUTPUT_ROOT_ENV = "MCE4D_CI_OUTPUT_ROOT"
API_JOB_REF = re.compile(r"/actions/runs/([0-9]+)/job/([0-9]+)(?:\?.*)?$")


def utc() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def write_json(path: Path, value: object) -> bytes:
    data = (json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n").encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return data


def refs() -> dict[str, str]:
    proc = subprocess.run(
        ["git", "ls-remote", "--refs", "origin", *BRANCHES],
        cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False, timeout=30,
    )
    if proc.returncode:
        raise RuntimeError(f"git ls-remote failed ({proc.returncode})")
    observed: dict[str, str] = {}
    for line in proc.stdout.decode("ascii", "strict").splitlines():
        sha, ref = line.split("\t", 1)
        observed[ref] = sha
    return observed


def page_rows(client, endpoint: str, array_key: str) -> list[dict[str, object]]:
    result: list[dict[str, object]] = []
    for page in client.collection_pages:
        if page.get("endpoint") != endpoint:
            continue
        record = client.raw_members.get(page.get("raw_page_ref"))
        body = record.get("body") if isinstance(record, dict) else None
        rows = body.get(array_key) if isinstance(body, dict) else None
        if not isinstance(rows, list) or not all(isinstance(row, dict) for row in rows):
            raise RuntimeError(f"raw {endpoint} page has invalid {array_key}")
        result.extend(rows)
    return result


def verify_ref_run(plan: dict[str, object], client, workflow_runs, check_runs, ref: str, branch: str, out_root: Path):
    expected_checks = plan["checks"]
    workflow_path = expected_checks[0]["workflow_path"]
    matching_runs = [
        row for row in workflow_runs
        if row.get("path") == workflow_path
        and row.get("event") == "push"
        and row.get("head_sha") == CANDIDATE_SHA
        and row.get("head_branch") == branch
    ]
    if len(matching_runs) != 1:
        raise RuntimeError(f"expected one exact push workflow for {ref}; found {len(matching_runs)}")
    run = matching_runs[0]
    run_id = str(run.get("id"))
    attempt = run.get("run_attempt")
    if isinstance(attempt, bool) or not isinstance(attempt, int) or attempt < 1:
        raise RuntimeError(f"workflow {run_id} has invalid run_attempt")

    job_pages: list[dict[str, object]] = []
    for page in client.collection_pages:
        if (page.get("endpoint") == "workflow_run_attempt"
                and str(page.get("workflow_run_id")) == run_id
                and page.get("workflow_run_attempt") == attempt):
            record = client.raw_members.get(page.get("raw_page_ref"))
            body = record.get("body") if isinstance(record, dict) else None
            rows = body.get("jobs") if isinstance(body, dict) else None
            if not isinstance(rows, list) or not all(isinstance(row, dict) for row in rows):
                raise RuntimeError(f"workflow {run_id} latest attempt jobs page is invalid")
            job_pages.extend(rows)

    selected: list[dict[str, object]] = []
    for expected in expected_checks:
        context = expected["context"]
        candidates = []
        for check in check_runs:
            if check.get("name") != context or check.get("head_sha") != CANDIDATE_SHA:
                continue
            details = check.get("details_url")
            match = API_JOB_REF.search(details) if isinstance(details, str) else None
            if match and match.group(1) == run_id:
                candidates.append((check, match.group(2)))
        if len(candidates) != 1:
            raise RuntimeError(f"expected one exact check-run for {context} on {branch}; found {len(candidates)}")
        check, job_id = candidates[0]
        app = check.get("app") if isinstance(check.get("app"), dict) else {}
        suite = check.get("check_suite") if isinstance(check.get("check_suite"), dict) else {}
        if (str(app.get("id")) != expected["source_app"].rsplit(":", 1)[1]
                or check.get("status") != "completed" or check.get("conclusion") != "success"
                or str(suite.get("id")) != str(run.get("check_suite_id"))):
            raise RuntimeError(f"check-run {context} is not completed success with frozen app/suite identity")
        jobs = [job for job in job_pages if str(job.get("id")) == job_id]
        if len(jobs) != 1:
            raise RuntimeError(f"expected job {job_id} for {context} on {branch}; found {len(jobs)}")
        job = jobs[0]
        if (job.get("name") != context or job.get("head_sha") != CANDIDATE_SHA
                or job.get("status") != "completed" or job.get("conclusion") != "success"
                or not observer._matrix_matches(expected["matrix"], job)):
            raise RuntimeError(f"job {job_id} failed exact SHA/status/matrix verification for {context}")

        log = observer._command_log(GH, run_id, job_id)
        log_ref = (OUTPUT_REL / "per-ref" / branch / f"{expected['id']}.log").as_posix()
        log_path = out_root / "per-ref" / branch / f"{expected['id']}.log"
        log_path.parent.mkdir(parents=True, exist_ok=True)
        log_path.write_bytes(log)
        selected.append({
            "check_id": expected["id"], "context": context, "workflow_path": workflow_path,
            "event": run["event"], "branch": branch, "ref": ref, "subject_sha": CANDIDATE_SHA,
            "run_id": run_id, "run_attempt": attempt, "check_run_id": str(check["id"]),
            "job_id": job_id, "status": check["status"], "conclusion": check["conclusion"],
            "reported_head_sha": check["head_sha"], "tested_checkout_sha": job["head_sha"],
            "runner_name": job.get("runner_name"), "runner_group_name": job.get("runner_group_name"),
            "matrix": expected["matrix"], "log_ref": log_ref, "log_byte_length": len(log),
            "log_sha256": digest(log),
        })
    if {row["check_id"] for row in selected} != {row["id"] for row in expected_checks}:
        raise RuntimeError(f"required context set mismatch on {branch}")
    return {"ref": ref, "branch": branch, "workflow_run_id": run_id,
            "workflow_run_attempt": attempt, "head_sha": CANDIDATE_SHA,
            "created_at": run.get("created_at"), "checks": sorted(selected, key=lambda row: row["check_id"])}


def main() -> int:
    if not Path(GH).is_file():
        raise RuntimeError("pinned GitHub CLI executable is missing")
    plan = json.loads(PLAN_PATH.read_text(encoding="utf-8"))
    if digest(observer._canonical_json(plan)) != PLAN_SHA256:
        raise RuntimeError("frozen check-plan canonical SHA changed")
    before = refs()
    expected_refs = {ref: CANDIDATE_SHA for ref in BRANCHES}
    if before != expected_refs:
        raise RuntimeError(f"pre-collection refs differ from allocation SHA: {before}")
    expected_output_root = ROOT / OUTPUT_REL
    override_value = os.environ.get(OUTPUT_ROOT_ENV)
    if not override_value:
        raise RuntimeError("short, task-bound evidence output mapping is required")
    override = Path(override_value)
    if not override.is_dir() or override.resolve(strict=True) != expected_output_root.resolve(strict=True):
        raise RuntimeError("evidence output mapping does not resolve to the task allocation CI directory")
    output_root = override
    started = utc()
    client = observer.GitHubApiClient("github.com", output_root, gh=GH)
    identity = {
        "delivery_id": DELIVERY, "task_id": TASK, "task_revision": REVISION,
        "assignment_ref": (SUPPORT / "authority/task-start.txt").as_posix(),
        "base_sha": BASE_SHA, "verified_payload_sha": CANDIDATE_SHA,
        "check_plan_sha256": PLAN_SHA256,
    }
    graph = observer.collect_ci_source_graph(
        client, plan=plan, subject_sha=CANDIDATE_SHA, identity=identity,
        phase="payload", gh=GH,
    )
    primary_checks = graph["observation"].get("checks", [])
    if len(primary_checks) != len(plan["checks"]):
        raise RuntimeError("observer primary required context count mismatch")
    if any(row.get("status") != "completed" or row.get("conclusion") != "success"
           or row.get("reported_head_sha") != CANDIDATE_SHA
           or row.get("tested_checkout_sha") != CANDIDATE_SHA
           or row.get("execution_verified") is not True for row in primary_checks):
        raise RuntimeError("observer selected required context is not exact-SHA successful")

    workflows = page_rows(client, "workflow_runs", "workflow_runs")
    checks = page_rows(client, "check_runs", "check_runs")
    ref_results = [
        verify_ref_run(plan, client, workflows, checks, ref, branch, output_root)
        for ref, branch in BRANCHES.items()
    ]
    after = refs()
    if after != expected_refs:
        raise RuntimeError(f"remote refs moved during collection: {after}")
    all_checks = [check for ref_result in ref_results for check in ref_result["checks"]]
    if len(all_checks) != 12 or sum(check["conclusion"] == "success" for check in all_checks) != 12:
        raise RuntimeError("both authorized refs do not have all 12 successful check executions")

    report = {
        "format_version": 1, "kind": "allocation_ci_evidence", "result": "PASS",
        "task_id": TASK, "task_revision": REVISION, "delivery_id": DELIVERY,
        "base_sha": BASE_SHA, "candidate_sha": CANDIDATE_SHA,
        "check_plan_ref": (SUPPORT / "check-plan-canonical.json").as_posix(),
        "check_plan_sha256": PLAN_SHA256, "required_contexts_per_ref": 6,
        "pre_collection_remote_refs": before, "post_collection_remote_refs": after,
        "run_count": len(ref_results), "check_count": len(all_checks),
        "passed_count": sum(check["conclusion"] == "success" for check in all_checks),
        "ref_results": ref_results,
        "observer_source_graph": graph["observation"],
        "observer_collection_ref": "collection.json",
        "started_at_utc": started, "completed_at_utc": utc(),
    }
    report_bytes = write_json(output_root / "allocation-ci-evidence.json", report)
    files = []
    for path in sorted(item for item in output_root.rglob("*") if item.is_file() and item.name != "manifest.json"):
        data = path.read_bytes()
        files.append({"path": path.relative_to(output_root).as_posix(), "byte_length": len(data), "sha256": digest(data)})
    manifest_bytes = write_json(output_root / "manifest.json", {
        "format_version": 1, "kind": "allocation_ci_manifest", "invocation_id": str(uuid.uuid4()),
        "candidate_sha": CANDIDATE_SHA, "files": files, "manifest_created_at_utc": utc(),
    })
    print(json.dumps({
        "result": "PASS", "candidate_sha": CANDIDATE_SHA, "run_count": len(ref_results),
        "check_count": len(all_checks), "passed_count": len(all_checks),
        "run_ids": {item["ref"]: item["workflow_run_id"] for item in ref_results},
        "output_ref": OUTPUT_REL.as_posix(),
        "report_sha256": digest(report_bytes), "manifest_sha256": digest(manifest_bytes),
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
