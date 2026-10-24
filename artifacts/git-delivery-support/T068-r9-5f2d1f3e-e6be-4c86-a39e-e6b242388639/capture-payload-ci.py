from __future__ import annotations

import hashlib
import json
import subprocess
import sys
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit

sys.path.insert(0, str(Path.cwd().resolve()))
from tools.spec_check import github_observer as observer

ROOT = Path.cwd()
SUPPORT = Path("artifacts/git-delivery-support/T068-r9-5f2d1f3e-e6be-4c86-a39e-e6b242388639")
DELIVERY = "5f2d1f3e-e6be-4c86-a39e-e6b242388639"
TASK = "T068"
REVISION = 7
BASE_SHA = "649d338f17432df527428a4c29b9a5936617a599"
PAYLOAD_SHA = "18191d974ebe671555ef6d088f4a85aec7b3c7fd"
PLAN_SHA = "d07558f7060fbeb97804ffd34aea8491fd96e2ff547dc521d6ceaaa6657f84f3"
BRANCH_REFS = ("refs/heads/main", "refs/heads/codex/t068-polyglot-matrix")
SCOPES = {
    "branch": {
        "branch": "codex/t068-polyglot-matrix",
        "run_id": 40051636804,
        "check_suite_id": 108562167027,
        "jobs": {
            "actions/bootstrap/ubuntu": (120745377342, "bootstrap (ubuntu-latest)"),
            "actions/bootstrap/windows": (120745377306, "bootstrap (windows-latest)"),
            "actions/spec-contract/ubuntu": (120745377140, "spec-contract (ubuntu-latest)"),
            "actions/spec-contract/windows": (120745377386, "spec-contract (windows-latest)"),
            "actions/explorer-contract/ubuntu": (120745377397, "explorer-contract (ubuntu-latest)"),
            "actions/explorer-contract/windows": (120745377308, "explorer-contract (windows-latest)"),
        },
    },
    "main": {
        "branch": "main",
        "run_id": 40051647031,
        "check_suite_id": 108562192486,
        "jobs": {
            "actions/bootstrap/ubuntu": (120745407554, "bootstrap (ubuntu-latest)"),
            "actions/bootstrap/windows": (120745407525, "bootstrap (windows-latest)"),
            "actions/spec-contract/ubuntu": (120745407557, "spec-contract (ubuntu-latest)"),
            "actions/spec-contract/windows": (120745407397, "spec-contract (windows-latest)"),
            "actions/explorer-contract/ubuntu": (120745407530, "explorer-contract (ubuntu-latest)"),
            "actions/explorer-contract/windows": (120745407518, "explorer-contract (windows-latest)"),
        },
    },
}


def utc() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def refs() -> dict[str, str]:
    proc = subprocess.run(
        ["git", "ls-remote", "--refs", "origin", *BRANCH_REFS],
        cwd=ROOT, capture_output=True, check=False,
    )
    if proc.returncode:
        raise RuntimeError(f"git ls-remote failed ({proc.returncode})")
    return {ref: sha for sha, ref in
            (line.split("\t", 1) for line in proc.stdout.decode("ascii", "strict").splitlines())}


class PublicGitHubApiClient(observer.GitHubApiClient):
    def __init__(self, output_root: Path, logs: dict[int, bytes], scope: dict):
        super().__init__("github.com", output_root)
        self._logs = logs
        self._scope = scope
        self._nonce = uuid.uuid4().hex

    def paginate(self, *, endpoint, requested_sha, path, params, group_id,
                 workflow_run_id=None, workflow_run_attempt=None):
        fresh_params = dict(params)
        fresh_params["fresh"] = f"{self._nonce}-{uuid.uuid4().hex}"
        if endpoint == "check_runs":
            path = f"repos/LonelyTraderBay/maps-context-engine-4d/check-suites/{self._scope['check_suite_id']}/check-runs"
            fresh_params = {"fresh": fresh_params["fresh"]}
        elif endpoint == "workflow_runs":
            fresh_params["branch"] = self._scope["branch"]
        return super().paginate(
            endpoint=endpoint, requested_sha=requested_sha, path=path,
            params=fresh_params, group_id=group_id,
            workflow_run_id=workflow_run_id,
            workflow_run_attempt=workflow_run_attempt,
        )

    def _request(self, requested_url):
        parts = urlsplit(requested_url)
        if parts.scheme != "https" or parts.netloc != "api.github.com":
            raise observer.GitHubObserverError("API URL escaped api.github.com")
        request = urllib.request.Request(
            requested_url,
            headers={
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
                "Cache-Control": "no-cache, no-store",
                "Pragma": "no-cache",
                "User-Agent": "MCE4D-payload-CI-evidence",
            },
        )
        started = utc()
        try:
            response = urllib.request.urlopen(request, timeout=45)
            body_bytes = response.read(observer.MAX_API_BODY + 1)
            status, response_headers = response.status, response.headers
        except Exception as exc:
            raise observer.GitHubObserverError(f"GitHub public API request failed: {type(exc).__name__}") from exc
        completed = utc()
        if status != 200 or len(body_bytes) > observer.MAX_API_BODY:
            raise observer.GitHubObserverError(f"GitHub API response not acceptable: status={status}, bytes={len(body_bytes)}")
        try:
            observer.strict_json_loads(body_bytes)
        except observer.SchemaError as exc:
            raise observer.GitHubObserverError("GitHub API returned invalid JSON") from exc
        headers = {key: response_headers[key] for key in (
            "content-type", "date", "etag", "link", "x-github-api-version-selected",
            "x-github-request-id", "x-ratelimit-limit", "x-ratelimit-remaining",
            "x-ratelimit-reset", "x-ratelimit-resource",
        ) if response_headers.get(key) is not None}
        return headers, body_bytes, observer.strict_json_loads(body_bytes), started, completed


def main() -> int:
    if len(sys.argv) != 2 or sys.argv[1] not in SCOPES:
        raise RuntimeError("usage: capture-payload-ci.py branch|main")
    label = sys.argv[1]
    scope = SCOPES[label]
    if refs() != {ref: PAYLOAD_SHA for ref in BRANCH_REFS}:
        raise RuntimeError("pre-collection remote refs do not both equal payload SHA")
    plan_path = ROOT / SUPPORT / "check-plan-canonical.json"
    plan = json.loads(plan_path.read_text(encoding="utf-8"))
    if digest(observer._canonical_json(plan)) != PLAN_SHA:
        raise RuntimeError("canonical check plan digest changed")
    output_root = ROOT / SUPPORT / "archive-view" / "ci" / f"payload-{label}-P3-attempt4"
    logs: dict[int, bytes] = {}
    for check_id, (job_id, job_name) in scope["jobs"].items():
        log_path = output_root / "input-logs" / "jobs" / f"{job_id}.txt"
        log = log_path.read_bytes()
        if not log:
            raise RuntimeError(f"missing downloaded job log for {check_id}")
        logs[job_id] = log
    client = PublicGitHubApiClient(output_root, logs, scope)
    observer._command_log = lambda gh, run_id, job_id: client._logs[int(job_id)]
    operation_name = (
        "payload-branch-5c6b03c4-7503-4019-8a68-0e25bceebb27" if label == "branch"
        else "payload-main-37ede495-d44c-49c0-ad63-b99c2e42d100"
    )
    operation_root = ROOT / SUPPORT / "archive-view" / "operations" / operation_name
    destination = json.loads((operation_root / "destination-observation.json").read_text(encoding="utf-8"))
    identity = {
        "delivery_id": DELIVERY, "task_id": TASK, "task_revision": REVISION,
        "assignment_ref": f"{SUPPORT.as_posix()}/authority/task-start.txt",
        "base_sha": BASE_SHA, "verified_payload_sha": PAYLOAD_SHA,
        "check_plan_sha256": PLAN_SHA,
    }
    graph = observer.collect_ci_source_graph(
        client, plan=plan, subject_sha=PAYLOAD_SHA,
        identity=identity, phase="payload", destination_observation=destination,
        gh="connector-logs",
    )
    checks = graph["observation"]["checks"]
    expected_ids = set(scope["jobs"])
    if {item["check_id"] for item in checks} != expected_ids or len(checks) != 6:
        raise RuntimeError("selected CI set does not equal all six required check IDs")
    if any(item["run_id"] != str(scope["run_id"]) for item in checks):
        raise RuntimeError("selected check source is not the scope's exact workflow run")
    if any(item["status"] != "completed" or item["conclusion"] != "success"
           or item["reported_head_sha"] != PAYLOAD_SHA
           or item["tested_checkout_sha"] != PAYLOAD_SHA
           or not item["execution_verified"] for item in checks):
        raise RuntimeError("one or more required checks are not successful exact-SHA executions")
    after_refs = refs()
    if after_refs != {ref: PAYLOAD_SHA for ref in BRANCH_REFS}:
        raise RuntimeError("remote refs moved during CI collection")
    report = {
        "format_version": 1, "kind": "payload_ci_evidence", "result": "PASS",
        "task_id": TASK, "task_revision": REVISION, "delivery_id": DELIVERY,
        "phase": "payload", "scope": label, "branch": scope["branch"],
        "workflow_run_id": str(scope["run_id"]), "check_suite_id": str(scope["check_suite_id"]),
        "base_sha": BASE_SHA, "candidate_sha": PAYLOAD_SHA,
        "check_plan_ref": f"{SUPPORT.as_posix()}/check-plan-canonical.json",
        "check_plan_sha256": PLAN_SHA,
        "destination_observation_ref": f"{SUPPORT.as_posix()}/archive-view/operations/{operation_name}/destination-observation.json",
        "pre_collection_remote_refs": {ref: PAYLOAD_SHA for ref in BRANCH_REFS},
        "post_collection_remote_refs": after_refs,
        "collection_ref": "collection.json", "observation_ref": "closing-observation.json",
        "check_count": len(checks),
        "checks": [{
            "check_id": item["check_id"], "context": item["context"],
            "reported_head_sha": item["reported_head_sha"], "tested_checkout_sha": item["tested_checkout_sha"],
            "status": item["status"], "conclusion": item["conclusion"],
            "run_id": item["run_id"], "job_id": item["job_id"], "attempt": item["attempt"],
            "evidence_refs": item["evidence_refs"],
        } for item in sorted(checks, key=lambda item: item["check_id"])],
        "started_at_utc": graph["started_at_utc"],
        "completed_at_utc": graph["completed_collection_at_utc"],
    }
    (output_root / "payload-ci-evidence.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    files = []
    for path in sorted(item for item in output_root.rglob("*") if item.is_file() and item.name != "manifest.json"):
        data = path.read_bytes()
        files.append({"path": path.relative_to(output_root).as_posix(), "byte_length": len(data), "sha256": digest(data)})
    manifest = {
        "format_version": 1, "kind": "payload_ci_manifest", "scope": label,
        "candidate_sha": PAYLOAD_SHA, "workflow_run_id": str(scope["run_id"]),
        "check_suite_id": str(scope["check_suite_id"]), "files": files,
        "manifest_created_at_utc": utc(),
    }
    (output_root / "manifest.json").write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"result": "PASS", "scope": label, "candidate_sha": PAYLOAD_SHA,
                      "workflow_run_id": scope["run_id"], "check_suite_id": scope["check_suite_id"],
                      "check_count": len(checks), "collection_id": graph["collection"]["collection_id"],
                      "manifest_files": len(files)}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
