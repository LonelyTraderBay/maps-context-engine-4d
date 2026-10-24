from __future__ import annotations

import hashlib
import json
import subprocess
import sys
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

sys.path.insert(0, str(Path.cwd().resolve()))
from tools.spec_check import github_observer as observer

ROOT = Path.cwd().resolve(strict=True)
DELIVERY = "5f2d1f3e-e6be-4c86-a39e-e6b242388639"
TASK = "T068"
REVISION = 7
BASE_SHA = "649d338f17432df527428a4c29b9a5936617a599"
CANDIDATE_SHA = "ad50d225bb16f3600bc9b73ae0ec45c84adfbca6"
SUPPORT = Path("artifacts/git-delivery-support") / f"T068-r9-{DELIVERY}"
CI_ROOT = ROOT / SUPPORT / "archive-view/ci/allocation-main-ad50d225"
LOG_DIR = CI_ROOT / "logs"
PLAN_PATH = ROOT / SUPPORT / "check-plan-canonical.json"
BRANCHES = ("refs/heads/main", "refs/heads/codex/t068-polyglot-matrix")
EXPECTED_IDS = {
    "actions/bootstrap/ubuntu",
    "actions/bootstrap/windows",
    "actions/spec-contract/ubuntu",
    "actions/spec-contract/windows",
    "actions/explorer-contract/ubuntu",
    "actions/explorer-contract/windows",
}
EXPECTED_JOBS = {
    "actions/bootstrap/ubuntu": 120739011893,
    "actions/bootstrap/windows": 120739012047,
    "actions/spec-contract/windows": 120739012087,
    "actions/spec-contract/ubuntu": 120739012104,
    "actions/explorer-contract/windows": 120739012050,
    "actions/explorer-contract/ubuntu": 120739012099,
}


def utc() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def refs() -> dict[str, str]:
    proc = subprocess.run(
        ["git", "ls-remote", "--refs", "origin", *BRANCHES],
        cwd=ROOT, capture_output=True, check=False,
    )
    if proc.returncode:
        raise RuntimeError(f"git ls-remote failed ({proc.returncode})")
    result = {}
    for line in proc.stdout.decode("ascii", "strict").splitlines():
        digest, ref = line.split("\t", 1)
        result[ref] = digest
    return result


class PublicGitHubApiClient(observer.GitHubApiClient):
    def __init__(self, output_root: Path, logs: dict[int, bytes]):
        super().__init__("github.com", output_root)
        self._logs = logs
        self._nonce = uuid.uuid4().hex

    def _store(self, ref: str, data: bytes, *args, **kwargs) -> None:
        # The built-in collector also constructs a generic payload observation.
        # Allocation CI is retained here as source/check evidence only.
        if ref == "closing-observation.json":
            return
        return super()._store(ref, data, *args, **kwargs)

    def paginate(self, *, endpoint, requested_sha, path, params, group_id,
                 workflow_run_id=None, workflow_run_attempt=None):
        fresh_params = dict(params)
        fresh_params["fresh"] = f"{self._nonce}-{uuid.uuid4().hex}"
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
                "User-Agent": "MCE4D-allocation-evidence",
            },
        )
        started = utc()
        try:
            response = urllib.request.urlopen(request, timeout=45)
            body_bytes = response.read(observer.MAX_API_BODY + 1)
            status = response.status
            response_headers = response.headers
        except Exception as exc:
            raise observer.GitHubObserverError(
                f"GitHub public API request failed: {type(exc).__name__}"
            ) from exc
        completed = utc()
        if status != 200 or len(body_bytes) > observer.MAX_API_BODY:
            raise observer.GitHubObserverError(
                f"GitHub API response not acceptable: status={status}, bytes={len(body_bytes)}"
            )
        try:
            body = observer.strict_json_loads(body_bytes)
        except observer.SchemaError as exc:
            raise observer.GitHubObserverError("GitHub API returned invalid JSON") from exc
        headers = {}
        for key in (
            "content-type", "date", "etag", "link",
            "x-github-api-version-selected", "x-github-request-id",
            "x-ratelimit-limit", "x-ratelimit-remaining",
            "x-ratelimit-reset", "x-ratelimit-resource",
        ):
            value = response_headers.get(key)
            if value is not None:
                headers[key] = value
        return headers, body_bytes, body, started, completed


def main() -> int:
    if refs() != {ref: CANDIDATE_SHA for ref in BRANCHES}:
        raise RuntimeError("pre-collection refs do not both equal allocation SHA")
    plan = json.loads(PLAN_PATH.read_text(encoding="utf-8"))
    if sha(observer._canonical_json(plan)) != "d07558f7060fbeb97804ffd34aea8491fd96e2ff547dc521d6ceaaa6657f84f3":
        raise RuntimeError("canonical check plan digest changed")
    logs = {}
    for check_id, job_id in EXPECTED_JOBS.items():
        path = LOG_DIR / f"{job_id}.txt"
        data = path.read_bytes()
        if not data:
            raise RuntimeError(f"missing job log bytes for {check_id}")
        logs[job_id] = data
    invocation = str(uuid.uuid4())
    output_rel = SUPPORT / "ci-a7-2"
    output_root = ROOT / output_rel
    output_root.mkdir(parents=True, exist_ok=False)
    client = PublicGitHubApiClient(output_root, logs)
    observer._command_log = lambda gh, run_id, job_id: client._logs[int(job_id)]
    identity = {
        "delivery_id": DELIVERY,
        "task_id": TASK,
        "task_revision": REVISION,
        "assignment_ref": f"{SUPPORT.as_posix()}/authority/task-start.txt",
        "base_sha": BASE_SHA,
        "verified_payload_sha": CANDIDATE_SHA,
        "check_plan_sha256": "d07558f7060fbeb97804ffd34aea8491fd96e2ff547dc521d6ceaaa6657f84f3",
    }
    graph = observer.collect_ci_source_graph(
        client, plan=plan, subject_sha=CANDIDATE_SHA,
        identity=identity, phase="payload", gh="not-used",
    )
    checks = graph["observation"]["checks"]
    found_ids = {item["check_id"] for item in checks}
    if found_ids != EXPECTED_IDS or len(checks) != 6:
        raise RuntimeError(f"required set mismatch: {sorted(found_ids)}")
    if any(
        item["status"] != "completed"
        or item["conclusion"] != "success"
        or item["reported_head_sha"] != CANDIDATE_SHA
        or item["tested_checkout_sha"] != CANDIDATE_SHA
        or not item["execution_verified"]
        for item in checks
    ):
        raise RuntimeError("one or more selected CI checks are not exact-SHA successful executions")
    after_refs = refs()
    if after_refs != {ref: CANDIDATE_SHA for ref in BRANCHES}:
        raise RuntimeError("remote refs moved during CI collection")
    collection = graph["collection"]
    report = {
        "format_version": 1,
        "kind": "allocation_ci_evidence",
        "result": "PASS",
        "task_id": TASK,
        "task_revision": REVISION,
        "delivery_id": DELIVERY,
        "base_sha": BASE_SHA,
        "candidate_sha": CANDIDATE_SHA,
        "check_plan_ref": f"{SUPPORT.as_posix()}/check-plan-canonical.json",
        "check_plan_sha256": "d07558f7060fbeb97804ffd34aea8491fd96e2ff547dc521d6ceaaa6657f84f3",
        "pre_collection_remote_refs": {key: CANDIDATE_SHA for key in BRANCHES},
        "post_collection_remote_refs": after_refs,
        "collection_ref": "collection.json",
        "checks": [
            {
                "check_id": item["check_id"],
                "context": item["context"],
                "reported_head_sha": item["reported_head_sha"],
                "tested_checkout_sha": item["tested_checkout_sha"],
                "status": item["status"],
                "conclusion": item["conclusion"],
                "run_id": item["run_id"],
                "job_id": item["job_id"],
                "attempt": item["attempt"],
                "evidence_refs": item["evidence_refs"],
            }
            for item in sorted(checks, key=lambda item: item["check_id"])
        ],
        "started_at_utc": graph["started_at_utc"],
        "completed_at_utc": graph["completed_collection_at_utc"],
    }
    report_path = output_root / "allocation-ci-evidence.json"
    report_bytes = (json.dumps(report, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    report_path.write_bytes(report_bytes)
    files = []
    for path in sorted(item for item in output_root.rglob("*") if item.is_file()):
        relative = path.relative_to(output_root).as_posix()
        data = path.read_bytes()
        files.append({"path": relative, "byte_length": len(data), "sha256": sha(data)})
    manifest = {
        "format_version": 1,
        "kind": "allocation_ci_manifest",
        "invocation_id": invocation,
        "candidate_sha": CANDIDATE_SHA,
        "files": files,
        "manifest_created_at_utc": utc(),
    }
    manifest_path = output_root / "manifest.json"
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({
        "result": "PASS",
        "candidate_sha": CANDIDATE_SHA,
        "check_count": len(checks),
        "collection_id": collection["collection_id"],
        "invocation_id": invocation,
        "output_root": output_rel.as_posix(),
        "report_sha256": sha(report_bytes),
        "manifest_sha256": sha(manifest_path.read_bytes()),
        "response_date_headers": sorted({
            item.get("date")
            for item in (value for value in client.raw_members.values() if isinstance(value, dict))
            for item in [item.get("response_headers", {})]
            if isinstance(item, dict) and item.get("date")
        }),
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
