from __future__ import annotations

import hashlib
import json
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

ROOT = Path.cwd().resolve()
TASK, REVISION = "T068", 1
DELIVERY = "36570740-c893-45b5-8f0e-f68653195b6a"
BASE = "12547b660521fecb9592ae40cd786a13c25f63bb"
SUPPORT = f"artifacts/git-delivery-support/T068-r1-{DELIVERY}"
ATTEMPT = f"{SUPPORT}/local-checks/allocation/attempt1"
PLAN_REF = f"{SUPPORT}/allocation-retry2/check-plan-canonical-r1.json"
PLAN_SHA = "3c471d1a8af7ff7224962cd38d654eaec12469f828ab2b09f9c906f4b5b0c035"
FIXTURE_REF = "artifacts/preflight/T068/polyglot-acceptance-plan-r1.json"
T067 = "b0e123c8-1a67-47f3-9360-9f8b6ec57bd4"
T067_LIVE = "ba0cc8e3-9651-47e8-a836-64dc8076ea04"
T067_PUB = "artifacts/git-delivery/operations/a8d725e2-97a0-400d-a1fc-9f97ddfe7c91/publication.json"

if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from artifacts.preflight.T108.foundation_seal import _live_receipt_errors, _verified_archive_identity
from tools.ci.delivery_policy import _canonical_json
from tools.spec_check.archive import verify_archive
from tools.spec_check.spec import _effective_status, _evidence_fields, _section, _table_with_headers

OUT = ROOT / ATTEMPT
RECORDS = OUT / "records"
if OUT.exists():
    raise SystemExit("allocation preflight attempt1 exists; preserving it")
RECORDS.mkdir(parents=True)


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def save_json(path: Path, value: dict[str, Any]) -> str:
    data = (json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n").encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()
    return digest(data)


def assert_allocation_artifacts() -> None:
    spec_path = ROOT / "Maps-Context-Engine-4D_SPEC.md"
    spec = spec_path.read_text(encoding="utf-8")
    table = _table_with_headers(_section(spec, "## S19 — Bảng triển khai duy nhất") or "", "Task", "Gate", "Status", "Evidence")
    assert table is not None
    rows = {row["Task"].strip(): row for row in table.rows}
    assert all(_effective_status(rows[task]) == "DONE" for task in ("T063", "T064", "T065", "T066", "T067"))
    assert _effective_status(rows["T068"]) == "IN_PROGRESS"
    assert sum(_effective_status(row) == "DONE" for row in rows.values()) == 73
    evidence = _evidence_fields(rows["T068"]["Evidence"].strip())
    assert evidence["delivery"] == DELIVERY and evidence["base_sha"] == BASE
    assert evidence["plan"] == PLAN_SHA and evidence["plan_sha256"] == PLAN_SHA
    assert evidence["allocation_CI"] == "PENDING" and evidence["allocation_commit_sha"] == "PENDING"
    assert all(evidence[key] != "PENDING" for key in (
        "assignment_ref", "assignment_sha256", "selection_reason_ref", "selection_reason_sha256",
        "selector_ref", "selector_sha256", "task_mode_ref", "task_mode_sha256",
        "dependency_review_ref", "dependency_review_sha256", "allocation_ref", "allocation_sha256",
        "fixture_plan_ref", "fixture_plan_sha256",
    ))
    assert evidence["dependency_review_ref"] == f"{SUPPORT}/allocation-retry2/dependency-review.json"
    assert evidence["fixture_plan_ref"] == FIXTURE_REF
    review_path = ROOT / evidence["dependency_review_ref"]
    review = json.loads(review_path.read_text(encoding="utf-8"))
    assert review["result"] == "READY"
    assert [item["task_id"] for item in review["dependencies"]] == ["T063", "T064", "T065", "T066", "T067"]
    assert all(item["effective_status"] == "DONE" for item in review["dependencies"])
    assert review["normalization_note"]["task_id"] == "T065"
    assert review["normalization_note"]["archived_bytes_modified"] is False
    fixture = json.loads((ROOT / FIXTURE_REF).read_text(encoding="utf-8"))
    assert [case["id"] for case in fixture["cases"]] == [f"C{number:03}" for number in range(55, 70)]
    assert fixture["cohort"]["minimum_tasks"] == 30 and fixture["cohort"]["minimum_repositories"] == 3
    plan = json.loads((ROOT / PLAN_REF).read_bytes())
    assert plan["canary_ref"] == f"delivery:{DELIVERY}:payload"
    assert digest(_canonical_json(plan)) == PLAN_SHA
    t067_archive = verify_archive(ROOT, T067, spec_path=spec_path)
    t067_identity = _verified_archive_identity(ROOT, T067, "T067")
    assert t067_archive["kind"] == "archive_publication"
    assert t067_identity["closing_sha"] == BASE
    live_path = ROOT / f"artifacts/git-live/{T067}/{T067_LIVE}/live-verification.json"
    live = json.loads(live_path.read_text(encoding="utf-8"))
    assert not _live_receipt_errors(live, spec_text=spec, expected_verification_id=T067_LIVE, root=ROOT)
    assert live["subject_sha"] == BASE and live["observed_remote_sha"] == BASE
    s22 = spec[spec.index("## S22 —"):spec.index("\n## S23 —")]
    assert 'active_task: T068' in s22 and 'next_task: T068' in s22
    assert f'last_live_verification_id: "{T067_LIVE}"' in s22


checks: list[dict[str, Any]] = []


def run(check_id: str, argv: list[str], assertion: Callable[[], None] | None = None) -> None:
    started = now()
    tick = time.monotonic()
    if assertion:
        stored_argv = ["python", f"<inline {check_id} assertions>"]
        try:
            assertion()
            code, raw = 0, b"PASS\n"
        except Exception as exc:
            code, raw = 1, f"FAIL: {type(exc).__name__}: {exc}\n".encode("utf-8")
    else:
        stored_argv = argv
        result = subprocess.run(argv, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        code, raw = result.returncode, result.stdout
    ended = now()
    result_label = "PASS" if code == 0 else "FAIL"
    log_ref = f"{ATTEMPT}/records/{check_id}.log"
    record_ref = f"{ATTEMPT}/records/{check_id}.json"
    (ROOT / log_ref).write_bytes(raw)
    record = {
        "format_version": 1,
        "kind": "local_check_execution",
        "task_id": TASK,
        "task_revision": REVISION,
        "delivery_id": DELIVERY,
        "check_id": check_id,
        "argv": stored_argv,
        "cwd": "<repository-root>",
        "started_at_utc": started,
        "ended_at_utc": ended,
        "elapsed_seconds": round(time.monotonic() - tick, 3),
        "exit_code": code,
        "result": result_label,
        "log_ref": log_ref,
        "log_sha256": digest(raw),
        "record_ref": record_ref,
    }
    record_sha = save_json(ROOT / record_ref, record)
    checks.append({"check_id": check_id, "record_ref": record_ref, "sha256": record_sha,
                   "result": result_label, "exit_code": code, "log_ref": log_ref,
                   "log_sha256": digest(raw), "output": raw.decode("utf-8", "replace")[-700:]})


run("delivery-plan", ["python", "-B", "tools/ci/delivery_policy.py", "check-plan", "--plan", PLAN_REF,
                      "--workflow", ".github/workflows/bootstrap.yml", "--sha256", PLAN_SHA])
run("spec-check", ["python", "-B", "tools/spec_check/check.py"])
run("spec-fixtures", ["python", "-B", "tools/spec_check/test_spec_check.py"])
run("delivery-policy-fixtures", ["python", "-B", "tools/ci/test_delivery_policy.py"])
def assert_fixture_plan() -> None:
    fixture = json.loads((ROOT / FIXTURE_REF).read_text(encoding="utf-8"))
    assert fixture["kind"] == "polyglot_acceptance_plan"
    assert [item["id"] for item in fixture["cases"]] == [f"C{i:03}" for i in range(55, 70)]
    assert fixture["cohort"]["minimum_tasks"] == 30 and fixture["cohort"]["minimum_repositories"] == 3
    assert fixture["report_contract"]["token_usage_status_without_host_receipt"] == "NOT_RUN"


run("fixture-plan", ["python", "<inline fixture assertions>"], assert_fixture_plan)
run("allocation-artifacts", ["python", "<inline allocation consistency assertions>"], assert_allocation_artifacts)
run("git-diff-check", ["git", "diff", "--check"])

failed = [item for item in checks if item["result"] != "PASS"]
summary = {
    "format_version": 1,
    "kind": "local_check_summary",
    "task_id": TASK,
    "task_revision": REVISION,
    "delivery_id": DELIVERY,
    "base_sha": BASE,
    "result": "FAIL" if failed else "PASS",
    "passed_count": len(checks) - len(failed),
    "failed_count": len(failed),
    "check_refs": [{"record_ref": item["record_ref"], "sha256": item["sha256"]} for item in checks],
}
summary_ref = f"{ATTEMPT}/summary.json"
summary_sha = save_json(ROOT / summary_ref, summary)
check_doc = {
    "format_version": 1,
    "kind": "allocation_check",
    "task_id": TASK,
    "task_revision": REVISION,
    "delivery_id": DELIVERY,
    "base_sha": BASE,
    "check_plan_ref": PLAN_REF,
    "check_plan_sha256": PLAN_SHA,
    "checked_at_utc": now(),
    "result": summary["result"],
    "passed_count": summary["passed_count"],
    "failed_count": summary["failed_count"],
    "summary_ref": summary_ref,
    "summary_sha256": summary_sha,
    "check_refs": [{"record_ref": item["record_ref"], "sha256": item["sha256"]} for item in checks],
}
check_ref = f"{SUPPORT}/allocation-check-r1.json"
check_sha = save_json(ROOT / check_ref, check_doc)
preflight = {
    "format_version": 1,
    "kind": "allocation_preflight",
    "task_id": TASK,
    "task_revision": REVISION,
    "delivery_id": DELIVERY,
    "base_sha": BASE,
    "check_plan_ref": PLAN_REF,
    "check_plan_sha256": PLAN_SHA,
    "allocation_check_ref": check_ref,
    "allocation_check_sha256": check_sha,
    "checked_at_utc": now(),
    "result": check_doc["result"],
    "passed_count": check_doc["passed_count"],
    "failed_count": check_doc["failed_count"],
    "scope_paths": json.loads((ROOT / f"{SUPPORT}/allocation-retry2/allocation.json").read_text(encoding="utf-8"))["scope_paths"],
}
preflight_ref = f"{SUPPORT}/allocation-preflight-r1.json"
preflight_sha = save_json(ROOT / preflight_ref, preflight)
print(json.dumps({
    "result": check_doc["result"], "passed": check_doc["passed_count"], "failed": check_doc["failed_count"],
    "summary_ref": summary_ref, "summary_sha256": summary_sha,
    "allocation_check_ref": check_ref, "allocation_check_sha256": check_sha,
    "preflight_ref": preflight_ref, "preflight_sha256": preflight_sha,
    "checks": [{"id": item["check_id"], "result": item["result"], "exit": item["exit_code"], "output": item["output"]} for item in checks],
}, ensure_ascii=True, sort_keys=True))
if failed:
    raise SystemExit(1)
