from __future__ import annotations

import hashlib
import json
import subprocess
from pathlib import Path

ROOT = Path.cwd()
SUPPORT = Path("artifacts/git-delivery-support/T068-r9-5f2d1f3e-e6be-4c86-a39e-e6b242388639")
SHA = "18191d974ebe671555ef6d088f4a85aec7b3c7fd"
BRANCH_REFS = ("refs/heads/main", "refs/heads/codex/t068-polyglot-matrix")
SCOPES = {
    "branch": {
        "branch": "codex/t068-polyglot-matrix", "run_id": "40051636804",
        "check_suite_id": "108562167027",
        "operation": "payload-branch-5c6b03c4-7503-4019-8a68-0e25bceebb27",
    },
    "main": {
        "branch": "main", "run_id": "40051647031",
        "check_suite_id": "108562192486",
        "operation": "payload-main-37ede495-d44c-49c0-ad63-b99c2e42d100",
    },
}


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def refs() -> dict[str, str]:
    proc = subprocess.run(["git", "ls-remote", "--refs", "origin", *BRANCH_REFS],
                          cwd=ROOT, capture_output=True, check=True)
    return {ref: value for value, ref in
            (line.split("\t", 1) for line in proc.stdout.decode("ascii", "strict").splitlines())}


def read_json(path: Path) -> tuple[dict, bytes]:
    data = path.read_bytes()
    value = json.loads(data.decode("utf-8", "strict"))
    if not isinstance(value, dict):
        raise RuntimeError(f"not a JSON object: {path}")
    return value, data


def main() -> int:
    before_refs = refs()
    if before_refs != {ref: SHA for ref in BRANCH_REFS}:
        raise RuntimeError("remote branch and main refs do not both equal exact payload SHA")
    evidence = []
    for scope, expected in SCOPES.items():
        root = SUPPORT / "archive-view" / "ci" / f"payload-{scope}-P3-attempt4"
        report, report_bytes = read_json(root / "payload-ci-evidence.json")
        observation, _ = read_json(root / "closing-observation.json")
        collection, _ = read_json(root / "collection.json")
        manifest, _ = read_json(root / "manifest.json")
        if (report.get("result") != "PASS" or report.get("candidate_sha") != SHA
                or report.get("branch") != expected["branch"]
                or report.get("workflow_run_id") != expected["run_id"]
                or report.get("check_suite_id") != expected["check_suite_id"]
                or report.get("check_count") != 6):
            raise RuntimeError(f"{scope} report identity/result mismatch")
        checks = report.get("checks")
        if not isinstance(checks, list) or len(checks) != 6:
            raise RuntimeError(f"{scope} required check set incomplete")
        if any(item.get("run_id") != expected["run_id"]
               or item.get("reported_head_sha") != SHA
               or item.get("tested_checkout_sha") != SHA
               or item.get("status") != "completed"
               or item.get("conclusion") != "success" for item in checks):
            raise RuntimeError(f"{scope} has non-success, wrong-run, or wrong-SHA check")
        if (observation.get("phase") != "payload" or observation.get("subject_sha") != SHA
                or observation.get("destination_observation", {}).get("candidate_sha") != SHA
                or observation.get("destination_observation", {}).get("repository", {}).get("ref")
                != f"refs/heads/{expected['branch']}"):
            raise RuntimeError(f"{scope} observation is not bound to its destination ref")
        if collection.get("subject_sha") != SHA or collection.get("check_plan_sha256") != report.get("check_plan_sha256"):
            raise RuntimeError(f"{scope} collection identity mismatch")
        for item in manifest.get("files", []):
            data = (root / item["path"]).read_bytes()
            if len(data) != item.get("byte_length") or sha(data) != item.get("sha256"):
                raise RuntimeError(f"{scope} manifest mismatch for {item.get('path')}")
        operation_root = SUPPORT / "archive-view" / "operations" / expected["operation"]
        operation, operation_bytes = read_json(operation_root / "push-operation.json")
        destination, destination_bytes = read_json(operation_root / "destination-observation.json")
        if (operation.get("candidate_sha") != SHA
                or operation.get("push_operation_id") != destination.get("push_operation_id")
                or destination.get("candidate_sha") != SHA
                or destination.get("repository", {}).get("ref") != f"refs/heads/{expected['branch']}"):
            raise RuntimeError(f"{scope} push receipt does not bind exact candidate/ref")
        evidence.append({
            "scope": scope, "branch": expected["branch"], "run_id": expected["run_id"],
            "check_suite_id": expected["check_suite_id"], "check_count": len(checks),
            "result": "PASS", "candidate_sha": SHA,
            "report_ref": f"{root.as_posix()}/payload-ci-evidence.json",
            "report_sha256": sha(report_bytes),
            "observation_ref": f"{root.as_posix()}/closing-observation.json",
            "collection_ref": f"{root.as_posix()}/collection.json",
            "manifest_ref": f"{root.as_posix()}/manifest.json",
            "manifest_file_count": len(manifest.get("files", [])),
            "push_operation_ref": f"{operation_root.as_posix()}/push-operation.json",
            "push_operation_id": operation.get("push_operation_id"),
            "push_operation_sha256": sha(operation_bytes),
            "destination_observation_ref": f"{operation_root.as_posix()}/destination-observation.json",
            "destination_observation_sha256": sha(destination_bytes),
        })
    local_path = SUPPORT / "local-checks" / "payload" / "exact-P7" / "summary.json"
    local, local_bytes = read_json(local_path)
    if local.get("result") != "PASS" or local.get("passed_count") != 13 or local.get("subject_sha") != SHA:
        raise RuntimeError("payload local-check summary is not PASS 13/13 on payload SHA")
    after_refs = refs()
    if after_refs != before_refs:
        raise RuntimeError("remote refs moved while assembling payload CI pair")
    report = {
        "format_version": 1, "kind": "payload_ci_run_pair", "task_id": "T068",
        "task_revision": 7, "delivery_id": "5f2d1f3e-e6be-4c86-a39e-e6b242388639",
        "candidate_sha": SHA, "result": "PASS", "required_checks_per_scope": 6,
        "pre_collection_remote_refs": before_refs, "post_collection_remote_refs": after_refs,
        "local_check_summary_ref": local_path.as_posix(),
        "local_check_summary_sha256": sha(local_bytes), "local_checks": "PASS(13/13)",
        "scopes": evidence,
    }
    out = ROOT / SUPPORT / "archive-view" / "ci" / "payload-run-pair-final.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"result": report["result"], "payload_sha": SHA,
                      "local": report["local_checks"],
                      "scopes": [{"scope": x["scope"], "run_id": x["run_id"],
                                  "check_suite_id": x["check_suite_id"], "checks": x["check_count"],
                                  "report_sha256": x["report_sha256"]} for x in evidence],
                      "pair_ref": out.as_posix(), "pair_sha256": sha(out.read_bytes())}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
