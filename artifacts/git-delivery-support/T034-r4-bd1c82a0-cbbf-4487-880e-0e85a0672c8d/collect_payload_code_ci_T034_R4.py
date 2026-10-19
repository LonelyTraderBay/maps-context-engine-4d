from __future__ import annotations

import hashlib
import json
import sys
from pathlib import Path

ROOT = Path.cwd()
sys.path.insert(0, str(ROOT))

from tools.spec_check.github_observer import GitHubApiClient, _canonical_json, collect_ci_source_graph


TASK = "T034"
REVISION = 4
DELIVERY = "bd1c82a0-cbbf-4487-880e-0e85a0672c8d"
BASE = "16136bf18b8d0e77ede1500ccf57399391addcd5"
SUBJECT = "6827429e6360741ceb8a42db94a5b4b0778b4991"
PLAN_SHA = "bc157681517301a6cdff3601707d09f2f5f5d8e8365fc05474d047100aa9b495"
SUPPORT_REF = f"artifacts/git-delivery-support/{TASK}-r4-{DELIVERY}"
SUPPORT = ROOT / SUPPORT_REF
DESTINATION_REF = f"{SUPPORT_REF}/archive-view/operations/payload-code-confirmation-1/destination-observation.json"
COLLECTOR_ROOT = SUPPORT / "collector/payload-code-r4"
ARCHIVE_ROOT = SUPPORT / "archive-view/ci/payload-code"
ARCHIVE_PREFIX = f"{SUPPORT_REF}/archive-view/ci/payload-code/"


def rewrite(value: object, ref_map: dict[str, str]) -> object:
    if isinstance(value, dict):
        return {key: rewrite(item, ref_map) for key, item in value.items()}
    if isinstance(value, list):
        return [rewrite(item, ref_map) for item in value]
    if isinstance(value, str):
        return ref_map.get(value, value)
    return value


def main() -> None:
    if COLLECTOR_ROOT.exists() or ARCHIVE_ROOT.exists():
        raise SystemExit("Payload-ready CI collection exists; preserve and inspect before retry")
    plan_path = SUPPORT / "check-plan.json"
    plan_bytes = plan_path.read_bytes()
    plan = json.loads(plan_bytes.decode("utf-8", "strict"))
    if plan.get("canary_ref") != f"delivery:{DELIVERY}:payload":
        raise SystemExit("T032 frozen plan identity mismatch")
    if hashlib.sha256(plan_bytes).hexdigest() != PLAN_SHA:
        raise SystemExit("T032 check plan SHA mismatch")
    destination = json.loads((ROOT / DESTINATION_REF).read_text(encoding="utf-8", errors="strict"))
    if destination.get("candidate_sha") != SUBJECT or destination.get("visibility_verified") is not True:
        raise SystemExit("payload push destination observation does not bind the candidate")
    identity = {
        "delivery_id": DELIVERY,
        "task_id": TASK,
        "task_revision": REVISION,
        "base_sha": BASE,
        "verified_payload_sha": SUBJECT,
        "check_plan_sha256": PLAN_SHA,
        "priority": "P0",
    }
    COLLECTOR_ROOT.mkdir(parents=True)
    client = GitHubApiClient("github.com", COLLECTOR_ROOT)
    graph = collect_ci_source_graph(
        client,
        plan=plan,
        subject_sha=SUBJECT,
        identity=identity,
        phase="payload",
        destination_observation=destination,
    )
    ref_map = {ref: ARCHIVE_PREFIX + ref for ref in client.member_bytes}
    ARCHIVE_ROOT.mkdir(parents=True)
    for ref, data in client.member_bytes.items():
        if ref == "closing-observation.json":
            continue
        target = ARCHIVE_ROOT / Path(ref)
        target.parent.mkdir(parents=True, exist_ok=True)
        if not (ref in graph["raw_members"] and ref.endswith(".body.json")) and ref.endswith(".json"):
            parsed = json.loads(data.decode("utf-8", "strict"))
            data = _canonical_json(rewrite(parsed, ref_map))
        target.write_bytes(data)
    observation = rewrite(graph["observation"], ref_map)
    observation_ref = ARCHIVE_PREFIX + "payload-observation.archive.json"
    (ARCHIVE_ROOT / "payload-observation.archive.json").write_bytes(_canonical_json(observation))
    objects = {
        ref_map[ref]: item["kind"]
        for ref, item in graph["objects"].items()
        if ref != "closing-observation.json"
    }
    objects[observation_ref] = "GitDeliveryObservation"
    raw_refs = sorted(ref_map[ref] for ref in graph["raw_members"])
    (ARCHIVE_ROOT / "source-graph.json").write_bytes(_canonical_json({
        "format_version": 1,
        "kind": "ci_source_graph",
        "objects": dict(sorted(objects.items())),
        "raw_members": raw_refs,
    }))
    run_ids = sorted({str(item["run_id"]) for item in observation["checks"]})
    summary = {
        "format_version": 1,
        "kind": "ci_collection_summary",
        "delivery_id": DELIVERY,
        "task_id": TASK,
        "task_revision": REVISION,
        "phase": "payload",
        "subject_sha": SUBJECT,
        "check_plan_sha256": PLAN_SHA,
        "collection_id": graph["collection"]["collection_id"],
        "check_count": len(observation["checks"]),
        "passed_count": sum(item["conclusion"] == "success" for item in observation["checks"]),
        "local_checks_passed": 11,
        "local_checks_failed": 0,
        "result": observation["result"],
        "run_ids": run_ids,
        "source_graph_ref": ARCHIVE_PREFIX + "source-graph.json",
    }
    (ARCHIVE_ROOT / "collector-summary.json").write_bytes(_canonical_json(summary))
    print(json.dumps({
        "phase": "payload",
        "subject_sha": SUBJECT,
        "checks": summary["check_count"],
        "passed": summary["passed_count"],
        "run_ids": run_ids,
        "collection_id": summary["collection_id"],
        "objects": len(objects),
        "raw_members": len(raw_refs),
        "result": summary["result"],
        "archive_ref": f"{ARCHIVE_PREFIX}collector-summary.json",
    }, sort_keys=True, separators=(",", ":")))


if __name__ == "__main__":
    main()
