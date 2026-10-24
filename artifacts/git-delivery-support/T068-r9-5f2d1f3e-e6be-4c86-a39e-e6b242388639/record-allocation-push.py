from __future__ import annotations

import hashlib
import json
import re
import shutil
import subprocess
import sys
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlsplit
from urllib.request import Request, urlopen

sys.stdout.reconfigure(encoding="utf-8")
ROOT = Path.cwd().resolve()
DELIVERY = "5f2d1f3e-e6be-4c86-a39e-e6b242388639"
TASK = "T068"
REVISION = 7
CANDIDATE = "ad50d225bb16f3600bc9b73ae0ec45c84adfbca6"
BASE = "649d338f17432df527428a4c29b9a5936617a599"
OWNER, REPOSITORY = "LonelyTraderBay", "maps-context-engine-4d"
TASK_BRANCH = "codex/t068-polyglot-matrix"
SUPPORT = f"artifacts/git-delivery-support/T068-r9-{DELIVERY}"
ASSIGNMENT = f"{SUPPORT}/authority/task-start.txt"
PLAN_REF = f"{SUPPORT}/check-plan-canonical.json"
ALLOCATION_REF = f"{SUPPORT}/allocation-current.json"
SELECTION_REF = f"{SUPPORT}/selection.json"
PREFLIGHT_REF = f"{SUPPORT}/allocation-preflight.json"
ALLOC_CHECK_REF = f"{SUPPORT}/allocation-check.json"
API_ROOT = "https://api.github.com"
FORBIDDEN_CREDENTIAL_URL = re.compile(r"(?i)(https?://[^/@\s:]+:[^/@\s]+@)")


def utc() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def parse_utc(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n", encoding="utf-8", newline="")


def git(args: list[str], *, check: bool = True) -> str:
    process = subprocess.run(["git", *args], cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, encoding="utf-8", errors="replace", timeout=60)
    if check and process.returncode:
        raise RuntimeError(f"git {args[0]} failed ({process.returncode})")
    return process.stdout.strip()


def config_values(key: str) -> list[str]:
    process = subprocess.run(["git", "config", "--get-all", key], cwd=ROOT, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, text=True, encoding="utf-8", errors="replace", timeout=15)
    if process.returncode not in (0, 1):
        raise RuntimeError(f"git config query failed for {key}")
    return process.stdout.splitlines()


def sanitize_remote(value: str) -> str:
    parsed = urlsplit(value)
    if parsed.scheme != "https" or parsed.hostname != "github.com" or parsed.username or parsed.password:
        raise RuntimeError("origin is not a credential-free GitHub HTTPS remote")
    if parsed.path.rstrip("/") != f"/{OWNER}/{REPOSITORY}.git":
        raise RuntimeError("origin does not match the selected repository")
    return f"https://github.com/{OWNER}/{REPOSITORY}.git"


def remote_snapshot() -> tuple[dict[str, object], str]:
    urls = config_values("remote.origin.url")
    push_urls = config_values("remote.origin.pushurl") or urls
    if len(urls) != 1 or not push_urls:
        raise RuntimeError("origin remote configuration is missing or ambiguous")
    branch_key = f"branch.{TASK_BRANCH}"
    snapshot: dict[str, object] = {
        "format_version": 1,
        "kind": "remote_config_snapshot",
        "remote_name": "origin",
        "remote_urls_sanitized": [sanitize_remote(value) for value in urls],
        "push_urls_sanitized": [sanitize_remote(value) for value in push_urls],
        "fetch_refspecs": config_values("remote.origin.fetch"),
        "task_branch": TASK_BRANCH,
        "task_branch_tracking_remote": config_values(branch_key + ".remote"),
        "task_branch_tracking_merge": config_values(branch_key + ".merge"),
        "git_version": git(["--version"]),
    }
    canonical = json.dumps(snapshot, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return snapshot, sha(canonical)


def api_get(path: str) -> dict[str, object]:
    url = API_ROOT + "/" + path + "?fresh=" + uuid.uuid4().hex
    started = utc()
    request = Request(url, headers={
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "MCE4D-T068-delivery-observer",
        "Cache-Control": "no-cache, no-store",
        "Pragma": "no-cache",
    })
    try:
        with urlopen(request, timeout=30) as response:
            completed = utc()
            if urlsplit(response.geturl()).netloc != "api.github.com":
                raise RuntimeError("GitHub API redirected outside api.github.com")
            body_bytes = response.read(1024 * 1024 + 1)
            if response.status != 200 or len(body_bytes) > 1024 * 1024:
                raise RuntimeError(f"GitHub API returned invalid status/size: {response.status}/{len(body_bytes)}")
            headers = {}
            for key, value in response.headers.items():
                folded = key.casefold()
                if folded in {"content-type", "date", "etag", "x-github-api-version-selected", "x-github-request-id"}:
                    headers[folded] = value.strip()
            body = json.loads(body_bytes.decode("utf-8", "strict"))
    except (HTTPError, URLError, TimeoutError, OSError) as exc:
        raise RuntimeError(f"GitHub API request failed: {type(exc).__name__}") from exc
    return {
        "requested_url": url,
        "request_started_at_utc": started,
        "response_received_at_utc": completed,
        "http_status": 200,
        "response_headers": headers,
        "body_byte_length": len(body_bytes),
        "body_sha256": sha(body_bytes),
        "body_raw_utf8": body_bytes.decode("utf-8", "strict"),
        "body": body,
    }


def query_destination(ref: str, lifecycle_id: str, retry: int, remote_digest: str, raw_path: Path):
    started = utc()
    repo_query = api_get(f"repos/{OWNER}/{REPOSITORY}")
    ref_name = ref.removeprefix("refs/heads/")
    ref_query = api_get(f"repos/{OWNER}/{REPOSITORY}/git/ref/heads/{quote(ref_name, safe='/')}")
    completed = utc()
    repo = repo_query["body"]
    ref_value = ref_query["body"]
    if not isinstance(repo, dict) or not isinstance(ref_value, dict):
        raise RuntimeError("GitHub API identity/ref response has the wrong shape")
    owner_login = repo.get("owner", {}).get("login") if isinstance(repo.get("owner"), dict) else None
    if (repo.get("id") != 1421031011 or repo.get("full_name") != f"{OWNER}/{REPOSITORY}"
            or owner_login != OWNER or repo.get("visibility") != "public"
            or repo.get("archived") is True or repo.get("disabled") is True):
        raise RuntimeError("GitHub API identity/visibility does not match the assigned public repository")
    obj = ref_value.get("object") if isinstance(ref_value.get("object"), dict) else {}
    observed_sha = obj.get("sha")
    if not isinstance(observed_sha, str) or not re.fullmatch(r"[0-9a-f]{40}", observed_sha):
        raise RuntimeError("GitHub API ref response lacks a full commit SHA")
    repository_identity = {
        "host": "github.com",
        "name": REPOSITORY,
        "owner": OWNER,
        "publication_authority_ref": ASSIGNMENT,
        "ref": ref,
        "remote_name": "origin",
        "remote_url_sanitized": f"https://github.com/{OWNER}/{REPOSITORY}.git",
        "repository_id": 1421031011,
        "visibility": "public",
    }
    raw_http = {
        "format_version": 1,
        "kind": "github_push_api_snapshot",
        "lifecycle_id": lifecycle_id,
        "retry_index": retry,
        "candidate_sha": CANDIDATE,
        "remote_config_digest": remote_digest,
        "repository": repository_identity,
        "observed_remote_sha": observed_sha,
        "started_at_utc": started,
        "completed_at_utc": completed,
        "repository_api": repo_query,
        "ref_api": ref_query,
    }
    write_json(raw_path, raw_http)
    return repository_identity, observed_sha, started, completed


def redact(text: str) -> str:
    return FORBIDDEN_CREDENTIAL_URL.sub("https://[REDACTED]@", text)


def main() -> None:
    if len(sys.argv) not in {2, 3} or sys.argv[1] not in {"main", "branch"}:
        raise SystemExit("usage: record-allocation-push.py main|branch [retry-index]")
    label = sys.argv[1]
    retry_index = int(sys.argv[2]) if len(sys.argv) == 3 else 0
    if retry_index < 0:
        raise SystemExit("retry-index must be non-negative")
    ref = "refs/heads/main" if label == "main" else f"refs/heads/{TASK_BRANCH}"
    if git(["rev-parse", "HEAD"]) != CANDIDATE:
        raise RuntimeError("HEAD is not the allocated commit SHA")
    if git(["rev-parse", "HEAD^"]) != BASE:
        raise RuntimeError("allocation commit parent is not the verified base")
    merge = subprocess.run(["git", "merge-base", "--is-ancestor", BASE, CANDIDATE], cwd=ROOT,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20)
    if merge.returncode != 0:
        raise RuntimeError("remote base is not an ancestor of the candidate")
    git_executable = shutil.which("git")
    if not git_executable:
        raise RuntimeError("git executable is unavailable")
    git_path = Path(git_executable).resolve(strict=True)
    git_version = git(["--version"])
    git_sha = sha(git_path.read_bytes())
    operation_id, lifecycle_id = str(uuid.uuid4()), str(uuid.uuid4())
    operation_ref = f"{SUPPORT}/archive-view/operations/allocation-{label}-{operation_id}"
    operation_dir = ROOT / operation_ref
    operation_dir.mkdir(parents=True, exist_ok=False)
    lifecycle_started = utc()
    deadline = (parse_utc(lifecycle_started) + timedelta(minutes=10)).isoformat(timespec="microseconds").replace("+00:00", "Z")
    config_before, digest = remote_snapshot()
    write_json(operation_dir / "remote-config.json", config_before)
    pre_repo, pre_sha, pre_started, pre_completed = query_destination(
        ref, lifecycle_id, retry_index, digest, operation_dir / "preflight-api.json",
    )
    config_after_pre, digest_after_pre = remote_snapshot()
    if config_after_pre != config_before or digest_after_pre != digest:
        raise RuntimeError("origin configuration changed during preflight")
    if pre_sha not in {BASE, CANDIDATE}:
        raise RuntimeError(f"preflight ref is neither base nor candidate; refusing push: {pre_sha}")
    preflight_id = str(uuid.uuid4())
    preflight = {
        "query_id": preflight_id,
        "lifecycle_id": lifecycle_id,
        "retry_index": retry_index,
        "candidate_sha": CANDIDATE,
        "repository": pre_repo,
        "remote_config_digest": digest,
        "started_at_utc": pre_started,
        "completed_at_utc": pre_completed,
        "observed_remote_sha": pre_sha,
        "raw_api_ref": f"{operation_ref}/preflight.json",
    }
    write_json(operation_dir / "preflight.json", preflight)

    command_started = utc()
    argv = ["push", "--porcelain", "origin", f"{CANDIDATE}:{ref}"]
    command_env = {**dict(__import__("os").environ), "GIT_TERMINAL_PROMPT": "0", "GH_PROMPT_DISABLED": "1"}
    try:
        process = subprocess.run([str(git_path), *argv], cwd=ROOT, env=command_env,
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 text=True, encoding="utf-8", errors="replace", timeout=300)
        push_completed = utc()
        exit_code = process.returncode
        output = redact(process.stdout)
        termination = "completed"
    except subprocess.TimeoutExpired as exc:
        push_completed = utc()
        exit_code = None
        output = redact((exc.stdout or b"").decode("utf-8", "replace") if isinstance(exc.stdout, bytes) else (exc.stdout or ""))
        termination = "timeout"

    post_api_ok = True
    try:
        post_repo, post_sha, post_started, post_completed = query_destination(
            ref, lifecycle_id, retry_index, digest, operation_dir / "postflight-api.json",
        )
    except Exception as exc:
        post_api_ok = False
        post_repo = pre_repo
        post_sha = None
        post_started = utc()
        post_completed = utc()
        write_json(operation_dir / "postflight-api.json", {
            "format_version": 1, "kind": "github_push_api_error", "error_type": type(exc).__name__,
            "started_at_utc": post_started, "completed_at_utc": post_completed,
        })
    postflight_id = str(uuid.uuid4())
    postflight = {
        "query_id": postflight_id,
        "lifecycle_id": lifecycle_id,
        "retry_index": retry_index,
        "candidate_sha": CANDIDATE,
        "repository": post_repo,
        "remote_config_digest": digest,
        "started_at_utc": post_started,
        "completed_at_utc": post_completed,
        "observed_remote_sha": post_sha,
        "raw_api_ref": f"{operation_ref}/postflight.json",
    }
    write_json(operation_dir / "postflight.json", postflight)
    config_after, digest_after = remote_snapshot()
    lifecycle_completed = utc()

    log_text = (
        f"command=git {' '.join(argv)}\n"
        f"preflight_remote_sha={pre_sha}\n"
        f"push_started_at_utc={command_started}\n"
        f"push_process_completed_at_utc={push_completed}\n"
        f"postflight_remote_sha={post_sha if post_sha is not None else 'UNKNOWN'}\n"
        f"postflight_completed_at_utc={post_completed}\n"
        f"lifecycle_completed_at_utc={lifecycle_completed}\n"
        f"exit_code={exit_code}\n"
        f"{output.rstrip()}\n"
    )
    (operation_dir / "command.log").write_text(log_text, encoding="utf-8", newline="")
    write_json(operation_dir / "git-tool.json", {
        "format_version": 1,
        "kind": "toolchain_source",
        "name": "git",
        "path": "git",
        "version": git_version,
        "sha256": git_sha,
        "observed_at_utc": lifecycle_started,
    })
    cleanup_ref = f"{operation_ref}/cleanup-plan.json"
    write_json(operation_dir / "cleanup-plan.json", {
        "format_version": 1,
        "kind": "cleanup_plan",
        "reason": "git push created no temporary files or cache data",
        "required": False,
    })

    successful = (exit_code == 0 and post_api_ok and post_sha == CANDIDATE
                  and config_after == config_before and digest_after == digest
                  and termination == "completed")
    command = {
        "format_version": 1,
        "kind": "command_lifecycle",
        "operation_id": operation_id,
        "lifecycle_id": lifecycle_id,
        "retry_index": retry_index,
        "assignment_ref": ASSIGNMENT,
        "task_id": TASK,
        "task_revision": REVISION,
        "delivery_id": DELIVERY,
        "command_state": "SUCCEEDED" if successful else "FAILED",
        "contract": {
            "command_id": f"push-T068-r9-allocation-{label}",
            "task_mode": "implement",
            "owner": "Codex",
            "baseline_ref": f"git:{ref}={pre_sha}",
            "executable": "git",
            "argv": argv,
            "cwd": ".",
            "tool_versions": {"git": git_version},
            "command_source_ref": f"{operation_ref}/git-tool.json",
            "input_refs": [PLAN_REF, ALLOCATION_REF, SELECTION_REF, PREFLIGHT_REF, ALLOC_CHECK_REF],
            "contracts": ["PUSH-FRESH-01", "S53_AW_08"],
            "fingerprints": {
                "remote_config_sha256": digest,
                "candidate_sha": hashlib.sha256(CANDIDATE.encode("ascii")).hexdigest(),
            },
            "expected_writes": [],
            "temporary_roots": [],
            "cache_roots": [],
            "log_roots": [operation_ref],
            "download_roots": [],
            "network_scope": ["api.github.com", "github.com"],
            "child_processes": [],
            "side_effect_class": "external_service",
            "permission_ref": ASSIGNMENT,
            "storage_mode": "bounded_small",
            "volumes": [],
            "budget_refs": [],
            "retention_ref": f"artifacts/git-delivery/{DELIVERY}",
            "deadline_at_utc": deadline,
            "monitor_interval_ms": 1000,
            "run_id": str(uuid.uuid4()),
            "output_root": operation_ref,
            "cleanup_plan_ref": cleanup_ref,
            "cleanup_permission_ref": None,
        },
        "result": {
            "started_at_utc": command_started,
            "completed_at_utc": lifecycle_completed,
            "command_exit_status": exit_code,
            "termination_reason": termination,
            "cleanup_status": "NOT_REQUIRED",
            "final_status": "SUCCEEDED" if successful else "FAILED",
            "outputs_created": [],
            "artifact_refs": [f"{operation_ref}/command.log"],
        },
    }
    write_json(operation_dir / "command.json", command)
    operation = {
        "format_version": 1,
        "kind": "push_operation",
        "push_operation_id": operation_id,
        "lifecycle_id": lifecycle_id,
        "retry_index": retry_index,
        "candidate_sha": CANDIDATE,
        "credential_class": "local_user",
        "command_record_ref": f"{operation_ref}/command.json",
        "lifecycle_started_at_utc": lifecycle_started,
        "deadline_at_utc": deadline,
        "push_started_at_utc": command_started,
        "push_completed_at_utc": push_completed,
        "intervening_events": [],
        "preflight": preflight,
        "postflight": postflight,
    }
    write_json(operation_dir / "push-operation.json", operation)
    destination = {
        "format_version": 1,
        "push_operation_id": operation_id,
        "candidate_sha": CANDIDATE,
        "repository": pre_repo,
        "local_remote_config_digest": digest,
        "observed_at_utc": pre_completed,
        "push_started_at_utc": command_started,
        "identity_verified": True,
        "visibility_verified": True,
        "operation_evidence_ref": f"{operation_ref}/push-operation.json",
    }
    write_json(operation_dir / "destination-observation.json", destination)
    print(json.dumps({
        "result": "PASS" if successful else "FAIL",
        "task_id": TASK,
        "task_revision": REVISION,
        "delivery_id": DELIVERY,
        "operation_id": operation_id,
        "ref": ref,
        "candidate_sha": CANDIDATE,
        "remote_sha_before": pre_sha,
        "remote_sha_after": post_sha,
        "push_started_at_utc": command_started,
        "push_process_completed_at_utc": push_completed,
        "postflight_completed_at_utc": post_completed,
        "lifecycle_completed_at_utc": lifecycle_completed,
        "output_ref": operation_ref,
        "push_output": output.strip(),
    }, ensure_ascii=False, indent=2))
    if not successful:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
