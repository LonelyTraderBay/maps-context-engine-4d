"""Fresh GitHub Actions collection and invocation-bound delivery verification.

Reads use the authenticated ``gh`` client, retain bounded response bodies and
response metadata, follow actual ``Link`` pagination, and fail closed when a
native check cannot be mapped to one current plan identity.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import subprocess
import threading
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlsplit

from .archive import ArchiveError, _canonical, _load_index, _read_bounded, verify_archive
from .ci_order import OrderingError, select_latest_actions_attempts, select_latest_actions_generation, select_latest_native_records
from .schema import SchemaError, extract_governance_schema, strict_json_loads, validate_schema
from .semantics import validate_delivery_links, validate_semantics


MAX_API_BODY = 10 * 1024 * 1024
MAX_LOG_BYTES = 10 * 1024 * 1024
MAX_LIVE_FILES = 4096
MAX_LIVE_BYTES = 100 * 1024 * 1024
MAX_TOTAL_API_CALLS = 1000
MAX_PAGES_PER_QUERY = 1000
MAX_RELEVANT_RUNS = 1000
MAX_RUN_ATTEMPTS = 50
MAX_API_STDERR_BYTES = 64 * 1024
MAX_API_RESPONSE_BYTES = MAX_API_BODY + 64 * 1024
_TOKEN = re.compile(r"(?i)\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b")
_RUN_JOB = re.compile(r"/actions/runs/([1-9][0-9]*)/job/([1-9][0-9]*)$")
_ANSI = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")
_LAST_INVOCATION_ROOT: Path | None = None


class GitHubObserverError(RuntimeError):
    """The external live query is incomplete, ambiguous or unsafe to claim PASS."""


def _merge_evidence_members(label: str, *sources: dict[str, Any]) -> dict[str, Any]:
    """Merge retained member maps without allowing one source to replace another."""
    merged: dict[str, Any] = {}
    for source in sources:
        collisions = sorted(merged.keys() & source.keys())
        if collisions:
            raise GitHubObserverError(
                f"{label} evidence member collision: {collisions[:8]}"
            )
        merged.update(source)
    return merged


def _run_bounded_process(
    command: list[str], *, env: dict[str, str], timeout: int,
    stdout_limit: int, stderr_limit: int,
) -> tuple[int, bytes, bytes]:
    """Drain child pipes concurrently and stop the child as soon as a cap is exceeded."""
    try:
        process = subprocess.Popen(
            command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, env=env,
        )
    except OSError as exc:
        raise GitHubObserverError(f"không chạy được GitHub CLI: {type(exc).__name__}") from exc
    if process.stdout is None or process.stderr is None:
        process.kill()
        process.wait()
        raise GitHubObserverError("không mở được pipe GitHub CLI")

    overflow = threading.Event()
    reader_errors: list[BaseException] = []
    buffers = {"stdout": bytearray(), "stderr": bytearray()}
    limits = {"stdout": stdout_limit, "stderr": stderr_limit}

    def drain(name: str, stream: Any) -> None:
        try:
            while chunk := stream.read(64 * 1024):
                remaining = limits[name] - len(buffers[name])
                if len(chunk) > remaining:
                    if remaining > 0:
                        buffers[name].extend(chunk[:remaining])
                    overflow.set()
                    try:
                        process.kill()
                    except OSError:
                        pass
                    continue
                buffers[name].extend(chunk)
        except OSError as exc:
            reader_errors.append(exc)
            overflow.set()
            try:
                process.kill()
            except OSError:
                pass
        finally:
            try:
                stream.close()
            except OSError as exc:
                reader_errors.append(exc)
                overflow.set()

    readers = [
        threading.Thread(target=drain, args=("stdout", process.stdout), daemon=True),
        threading.Thread(target=drain, args=("stderr", process.stderr), daemon=True),
    ]
    for reader in readers:
        reader.start()
    timed_out = False
    try:
        process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        process.kill()
        process.wait()
    for reader in readers:
        reader.join()
    if reader_errors:
        raise GitHubObserverError("không thu đầy đủ GitHub CLI stdout/stderr") from reader_errors[0]
    if overflow.is_set():
        raise GitHubObserverError("GitHub CLI stdout/stderr vượt giới hạn đọc trong lúc thu thập")
    if timed_out:
        raise GitHubObserverError(f"gh api không hoàn tất trong {timeout}s")
    return process.returncode, bytes(buffers["stdout"]), bytes(buffers["stderr"])


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def _iso_time(value: Any) -> datetime | None:
    if not isinstance(value, str):
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo is not None else None


def _digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _redact(data: bytes) -> bytes:
    text = data.decode("utf-8", "replace")
    text = _TOKEN.sub("[REDACTED_GITHUB_TOKEN]", text)
    text = re.sub(r"(?im)^\s*authorization\s*[:=].*$", "Authorization: [REDACTED]", text)
    text = re.sub(r"(?i)([?&](?:token|access_token|signature|sig)=)[^&\s]+", r"\1[REDACTED]", text)
    return text.encode("utf-8")


def _write_member(root: Path, ref: str, data: bytes, *, limit: int = MAX_API_BODY) -> None:
    if not ref or ref.startswith("/") or "\\" in ref or any(part in {"", ".", ".."} for part in ref.split("/")):
        raise GitHubObserverError(f"live member path không an toàn: {ref!r}")
    if len(data) > limit:
        raise GitHubObserverError(f"live member vượt {limit} byte: {ref}")
    current = root
    for part in ref.split("/")[:-1]:
        current = current / part
        if current.exists():
            info = current.lstat()
            if current.is_symlink() or not current.is_dir() or getattr(info, "st_file_attributes", 0) & 0x400:
                raise GitHubObserverError(f"live parent không an toàn: {current}")
        else:
            current.mkdir()
    target = current / ref.split("/")[-1]
    try:
        with target.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    except FileExistsError as exc:
        raise GitHubObserverError(f"live member đã tồn tại trong invocation mới: {ref}") from exc


def _canonical_json(value: Any) -> bytes:
    return _canonical(value)


def _parse_links(value: str | None) -> dict[str, str]:
    result: dict[str, str] = {}
    if not value:
        return result
    for link in re.finditer(r'<([^>]+)>\s*;\s*rel="([^"]+)"', value):
        relation = link.group(2)
        if relation in result:
            raise GitHubObserverError(f"GitHub Link header lặp relation {relation}")
        result[relation] = link.group(1)
    return result


def _closing_commit_snapshot(commit_body: dict[str, Any], *, expected_sha: str, payload_sha: str) -> dict[str, Any]:
    parents = commit_body.get("parents")
    commit_detail = commit_body.get("commit")
    files = commit_body.get("files")
    message = commit_detail.get("message") if isinstance(commit_detail, dict) else None
    if (commit_body.get("sha") != expected_sha
            or not isinstance(parents, list) or len(parents) != 1 or not isinstance(parents[0], dict)
            or parents[0].get("sha") != payload_sha
            or not isinstance(message, str)
            or not isinstance(files, list) or len(files) >= 300
            or any(not isinstance(item, dict) or not isinstance(item.get("filename"), str) for item in files)):
        raise GitHubObserverError("fresh closing commit source thiếu exact parent/message/file graph")
    subject_line, separator, remainder = message.partition("\n")
    subject = subject_line.removesuffix("\r")
    body = remainder
    if separator:
        # GitHub REST retains the blank line that separates the subject from
        # the body; `git show %b` omits that separator. Remove exactly one
        # newline so any additional leading body newline remains meaningful.
        if body.startswith("\r\n"):
            body = body[2:]
        elif body.startswith("\n"):
            body = body[1:]
    return {
        "kind": "git_commit_snapshot", "sha": expected_sha,
        "parents": [item["sha"] for item in parents],
        "subject": subject,
        # REST exposes the message separator in commit.message; the retained
        # GitCommitSnapshot stores subject separately and `%b` here.
        "body": body,
        "changed_paths": sorted(item["filename"] for item in files),
    }


def _payload_commit_parent(identity: dict[str, Any], *, subject_sha: str) -> str:
    """Return the parent expected by a payload observation, failing closed on gaps."""
    base_sha = identity.get("base_sha")
    if not isinstance(base_sha, str) or re.fullmatch(r"[0-9a-f]{40}", base_sha) is None:
        raise GitHubObserverError("payload identity thiếu base SHA hợp lệ")
    if identity.get("verified_payload_sha") != subject_sha:
        raise GitHubObserverError("payload identity không khớp subject SHA")

    payload_mode = identity.get("payload_mode")
    if payload_mode == "verified_noop":
        if subject_sha != base_sha:
            raise GitHubObserverError("verified no-op payload phải giữ subject tại base SHA")
        return base_sha
    if payload_mode != "changes":
        raise GitHubObserverError("payload identity thiếu mode changes/verified_noop")

    ordered_commits = identity.get("ordered_commits")
    if not isinstance(ordered_commits, list) or not ordered_commits:
        raise GitHubObserverError("payload identity thiếu ordered_commits")
    last_commit = ordered_commits[-1]
    if not isinstance(last_commit, dict) or last_commit.get("sha") != subject_sha:
        raise GitHubObserverError("ordered_commits không kết thúc tại payload subject SHA")

    if len(ordered_commits) == 1:
        parent_sha = base_sha
    else:
        previous_commit = ordered_commits[-2]
        parent_sha = previous_commit.get("sha") if isinstance(previous_commit, dict) else None
    if not isinstance(parent_sha, str) or re.fullmatch(r"[0-9a-f]{40}", parent_sha) is None:
        raise GitHubObserverError("ordered_commits thiếu direct parent SHA hợp lệ")
    return parent_sha


class GitHubApiClient:
    """Small bounded GitHub REST reader; never exposes its credential to evidence."""

    def __init__(self, host: str, output_root: Path, *, gh: str = "gh"):
        if host != "github.com":
            raise GitHubObserverError(f"REST host chưa được xác minh cho live observer: {host}")
        if not isinstance(gh, str) or not gh.strip():
            raise GitHubObserverError("GitHub CLI executable không hợp lệ")
        self.host = host
        self.output_root = output_root
        self.gh = gh
        self.collection_pages: list[dict[str, Any]] = []
        self.raw_members: dict[str, Any] = {}
        self.member_bytes: dict[str, bytes] = {}
        self.total_bytes = 0
        self.query_count = 0
        self.started_at_utc = _utc_now()

    def _store(self, ref: str, data: bytes, *, limit: int = MAX_API_BODY) -> None:
        if ref in self.member_bytes:
            raise GitHubObserverError(f"live evidence member đã tồn tại trong invocation: {ref}")
        if len(self.member_bytes) >= MAX_LIVE_FILES:
            raise GitHubObserverError(f"live evidence vượt {MAX_LIVE_FILES} files")
        if self.total_bytes + len(data) > MAX_LIVE_BYTES:
            raise GitHubObserverError(f"live evidence vượt {MAX_LIVE_BYTES} bytes")
        _write_member(self.output_root, ref, data, limit=limit)
        self.member_bytes[ref] = data
        self.total_bytes += len(data)

    def _request(self, requested_url: str) -> tuple[dict[str, str], bytes, Any, str, str]:
        if self.query_count >= MAX_TOTAL_API_CALLS:
            raise GitHubObserverError(f"live GitHub API calls vượt giới hạn {MAX_TOTAL_API_CALLS}")
        parsed = urlsplit(requested_url)
        if (parsed.scheme != "https" or parsed.netloc != "api.github.com" or parsed.username
                or parsed.password or parsed.fragment
                or not (parsed.path.startswith("/repos/") or parsed.path == "/user")):
            raise GitHubObserverError("URL REST không thuộc API host/repository đã chọn")
        relative = requested_url.removeprefix("https://api.github.com/")
        env = dict(os.environ)
        env.update({"GH_PROMPT_DISABLED": "1", "GH_PAGER": "cat", "NO_COLOR": "1"})
        start = _utc_now()
        return_code, response_bytes, stderr_bytes = _run_bounded_process(
            [self.gh, "api", "--include", "--hostname", self.host, relative,
             "-H", "Accept: application/vnd.github+json",
             "-H", "X-GitHub-Api-Version: 2022-11-28"],
            env=env, timeout=60, stdout_limit=MAX_API_RESPONSE_BYTES,
            stderr_limit=MAX_API_STDERR_BYTES,
        )
        end = _utc_now()
        if return_code:
            detail = _redact(stderr_bytes).decode("utf-8", "replace")[-700:].strip()
            raise GitHubObserverError(f"gh api thất bại ({return_code}): {detail}")
        split = response_bytes.find(b"\r\n\r\n")
        delimiter = 4
        if split < 0:
            split = response_bytes.find(b"\n\n")
            delimiter = 2
        if split < 0:
            raise GitHubObserverError("gh api --include thiếu HTTP header/body boundary")
        header_bytes = response_bytes[:split]
        body_bytes = response_bytes[split + delimiter:]
        header_lines = header_bytes.decode("utf-8", "strict").splitlines()
        if not header_lines or not re.match(r"^HTTP/[^ ]+ 200(?: |$)", header_lines[0]):
            status = header_lines[0] if header_lines else "missing status"
            raise GitHubObserverError(f"GitHub API trả status không PASS: {status}")
        headers: dict[str, str] = {}
        for line in header_lines[1:]:
            name, separator, value = line.partition(":")
            if separator:
                key = name.strip().casefold()
                if key == "link" or key in {
                    "content-type", "date", "etag", "x-github-api-version-selected",
                    "x-github-request-id", "x-ratelimit-limit", "x-ratelimit-remaining",
                    "x-ratelimit-reset", "x-ratelimit-resource",
                }:
                    headers[key] = value.strip()
        if len(body_bytes) > MAX_API_BODY:
            raise GitHubObserverError("GitHub API JSON body vượt 10 MiB")
        try:
            body = strict_json_loads(body_bytes)
        except SchemaError as exc:
            raise GitHubObserverError(f"GitHub API body JSON nghiêm ngặt không hợp lệ: {exc}") from exc
        if _iso_time(start) is None or _iso_time(end) is None or _iso_time(end) < _iso_time(start):
            raise GitHubObserverError("local request interval không hợp lệ")
        return headers, body_bytes, body, start, end

    def _capture_page(
        self,
        *,
        endpoint: str,
        requested_sha: str,
        requested_url: str,
        page_number: int,
        group_id: str,
        workflow_run_id: str | None = None,
        workflow_run_attempt: int | None = None,
    ) -> dict[str, Any]:
        headers, body_bytes, body, started, completed = self._request(requested_url)
        self.query_count += 1
        query_id = str(uuid.uuid4())
        next_url = _parse_links(headers.get("link")).get("next")
        next_page = None
        if next_url is not None:
            next_parts = urlsplit(next_url)
            requested_parts = urlsplit(requested_url)
            query = parse_qsl(next_parts.query, keep_blank_values=True)
            requested_query = parse_qsl(requested_parts.query, keep_blank_values=True)
            next_pages = [value for key, value in query if key == "page"]
            requested_non_page = sorted((key, value) for key, value in requested_query if key != "page")
            next_non_page = sorted((key, value) for key, value in query if key != "page")
            try:
                if len(next_pages) != 1:
                    raise ValueError("page must occur once")
                next_page = int(next_pages[0])
            except (TypeError, ValueError) as exc:
                raise GitHubObserverError("GitHub Link next không chứa page số") from exc
            if (next_parts.scheme != "https" or next_parts.netloc != "api.github.com"
                    or next_parts.username or next_parts.password or next_parts.fragment
                    or next_parts.path != requested_parts.path or next_page != page_number + 1
                    or next_non_page != requested_non_page):
                raise GitHubObserverError("GitHub Link next đổi host/path/filter hoặc nhảy trang")
        body_ref = f"pages/{group_id}-page-{page_number}-{query_id}.body.json"
        page_ref = f"pages/{group_id}-page-{page_number}-{query_id}.json"
        self._store(body_ref, body_bytes)
        body_record = {
            "format_version": 1, "kind": "github_api_page", "query_id": query_id,
            "endpoint": endpoint, "requested_sha": requested_sha,
            "page_number": page_number, "next_page_number": next_page,
            "requested_url": requested_url, "next_url": next_url,
            "request_started_at_utc": started, "response_received_at_utc": completed,
            "http_status": 200, "response_headers": headers,
            "body_ref": body_ref, "body_sha256": _digest(body_bytes),
            "body_byte_length": len(body_bytes), "body": body,
        }
        envelope_bytes = _canonical_json(body_record)
        self._store(page_ref, envelope_bytes)
        self.raw_members[body_ref] = body
        self.raw_members[page_ref] = body_record
        page_entry = {
            "query_id": query_id, "endpoint": endpoint, "requested_sha": requested_sha,
            "page_number": page_number, "next_page_number": next_page,
            "raw_page_ref": page_ref, "workflow_run_id": workflow_run_id,
            "workflow_run_attempt": workflow_run_attempt,
        }
        self.collection_pages.append(page_entry)
        return {"page": page_entry, "body": body, "envelope": body_record}

    def paginate(
        self,
        *,
        endpoint: str,
        requested_sha: str,
        path: str,
        params: dict[str, str],
        group_id: str,
        workflow_run_id: str | None = None,
        workflow_run_attempt: int | None = None,
    ) -> list[dict[str, Any]]:
        rows: list[dict[str, Any]] = []
        page_number = 1
        next_url: str | None = None
        expected_total: int | None = None
        while True:
            if page_number > MAX_PAGES_PER_QUERY:
                raise GitHubObserverError(f"GitHub pagination vượt {MAX_PAGES_PER_QUERY} pages")
            query_params = dict(params)
            query_params["per_page"] = "100"
            query_params["page"] = str(page_number)
            requested_url = next_url or f"https://api.github.com/{path}?{urlencode(query_params)}"
            captured = self._capture_page(
                endpoint=endpoint, requested_sha=requested_sha, requested_url=requested_url,
                page_number=page_number, group_id=group_id,
                workflow_run_id=workflow_run_id, workflow_run_attempt=workflow_run_attempt,
            )
            body = captured["body"]
            key = {"check_runs": "check_runs", "workflow_runs": "workflow_runs",
                   "workflow_run_attempt": "jobs"}.get(endpoint)
            if endpoint == "commit_statuses":
                page_rows = body if isinstance(body, list) else None
            else:
                page_rows = body.get(key) if isinstance(body, dict) and isinstance(key, str) else None
                total = body.get("total_count") if isinstance(body, dict) else None
                if isinstance(total, bool) or not isinstance(total, int) or total < 0:
                    raise GitHubObserverError(f"{endpoint} thiếu total_count integer")
                if expected_total is None:
                    expected_total = total
                elif expected_total != total:
                    raise GitHubObserverError(f"{endpoint} total_count thay đổi giữa pages")
            if not isinstance(page_rows, list) or any(not isinstance(row, dict) for row in page_rows):
                raise GitHubObserverError(f"{endpoint} page rows sai shape")
            rows.extend(page_rows)
            next_url = captured["envelope"].get("next_url")
            if next_url is None:
                break
            page_number += 1
        if expected_total is not None and len(rows) != expected_total:
            raise GitHubObserverError(f"{endpoint} tổng rows {len(rows)} khác total_count {expected_total}")
        return rows

    def snapshot(self, *, path: str, query: dict[str, str], group_id: str) -> dict[str, Any]:
        url = f"https://api.github.com/{path}"
        if query:
            url += "?" + urlencode(query)
        headers, body_bytes, body, started, completed = self._request(url)
        self.query_count += 1
        body_ref = f"api/{group_id}.body.json"
        envelope_ref = f"api/{group_id}.json"
        self._store(body_ref, body_bytes)
        envelope = {
            "format_version": 1, "kind": "github_api_snapshot", "requested_url": url,
            "request_started_at_utc": started, "response_received_at_utc": completed,
            "http_status": 200, "response_headers": headers,
            "body_ref": body_ref, "body_sha256": _digest(body_bytes),
            "body_byte_length": len(body_bytes), "body": body,
        }
        envelope_bytes = _canonical_json(envelope)
        self._store(envelope_ref, envelope_bytes)
        self.raw_members[body_ref] = body
        self.raw_members[envelope_ref] = envelope
        return {"body": body, "envelope": envelope, "body_ref": body_ref, "ref": envelope_ref}


def _require_list(payload: Any, name: str) -> list[dict[str, Any]]:
    if not isinstance(payload, list) or any(not isinstance(item, dict) for item in payload):
        raise GitHubObserverError(f"{name} phải là array object")
    return payload


def _matrix_matches(expected: Any, job: dict[str, Any]) -> bool:
    if not isinstance(expected, dict) or not expected:
        return False
    labels = job.get("labels")
    if not isinstance(labels, list) or any(not isinstance(item, str) for item in labels):
        return False
    for key, value in expected.items():
        # GitHub's jobs API exposes runner labels, not the original matrix JSON.
        # This adapter supports an ``os`` matrix only and fails closed otherwise.
        if key != "os" or not isinstance(value, str) or value not in labels:
            return False
    return True


def _command_log(gh: str, run_id: str, job_id: str) -> bytes:
    try:
        return_code, stdout, stderr = _run_bounded_process(
            [gh, "run", "view", run_id, "--job", job_id, "--log"],
            env={**os.environ, "GH_PROMPT_DISABLED": "1", "GH_PAGER": "cat", "NO_COLOR": "1"},
            timeout=90, stdout_limit=MAX_LOG_BYTES, stderr_limit=MAX_API_STDERR_BYTES,
        )
    except GitHubObserverError as exc:
        raise GitHubObserverError(f"GitHub job log query không hoàn tất: {type(exc).__name__}") from exc
    if return_code:
        detail = _redact(stderr).decode("utf-8", "replace")[-500:].strip()
        raise GitHubObserverError(f"GitHub job log query thất bại: {detail}")
    logs = _redact(stdout)
    logs = _ANSI.sub("", logs.decode("utf-8", "replace")).encode("utf-8")
    if not logs or len(logs) > MAX_LOG_BYTES:
        raise GitHubObserverError("GitHub job log rỗng hoặc vượt 10 MiB")
    return logs


def collect_ci_source_graph(
    client: GitHubApiClient,
    *,
    plan: dict[str, Any],
    subject_sha: str,
    identity: dict[str, Any],
    phase: str = "closing",
    destination_observation: dict[str, Any] | None = None,
    pull_request_binding_ref: str | None = None,
    observation_ref: str = "closing-observation.json",
    gh: str = "gh",
) -> dict[str, Any]:
    """Collect every page and construct current required check/source/execution records."""
    repository = plan.get("repository")
    checks_plan = plan.get("checks")
    if not isinstance(repository, dict) or not isinstance(checks_plan, list) or not checks_plan:
        raise GitHubObserverError("frozen plan thiếu repository/checks")
    if repository.get("host") != client.host or not isinstance(repository.get("repository_id"), int):
        raise GitHubObserverError("plan repository identity khác observer host")
    owner, name = repository.get("owner"), repository.get("name")
    if not isinstance(owner, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", owner):
        raise GitHubObserverError("plan owner không an toàn cho GitHub REST")
    if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+", name):
        raise GitHubObserverError("plan repository name không an toàn")
    checkrun_plans = [item for item in checks_plan if isinstance(item, dict) and item.get("kind") == "check_run"]
    if len(checkrun_plans) != len(checks_plan):
        raise GitHubObserverError("adapter hiện chỉ hỗ trợ native Actions check_run; status/other kind giữ UNKNOWN")
    if any(not isinstance(item.get("eligible_events"), list) or not item["eligible_events"] for item in checkrun_plans):
        raise GitHubObserverError("check plan thiếu eligible_events")
    if plan.get("pull_request_flow") is not None or plan.get("merge_checks"):
        raise GitHubObserverError("PR/merge flow cần adapter binding riêng; invocation này giữ UNKNOWN")

    base = f"repos/{owner}/{name}"
    started = client.started_at_utc
    checks = client.paginate(
        endpoint="check_runs", requested_sha=subject_sha,
        path=f"{base}/commits/{subject_sha}/check-runs", params={}, group_id="check-runs",
    )
    statuses = client.paginate(
        endpoint="commit_statuses", requested_sha=subject_sha,
        path=f"{base}/commits/{subject_sha}/statuses", params={}, group_id="commit-statuses",
    )
    if statuses:
        # No status is required by the frozen current plan. Still retained and
        # decoded above; unsupported extra status candidates must not replace it.
        pass
    workflow_runs = client.paginate(
        endpoint="workflow_runs", requested_sha=subject_sha,
        path=f"{base}/actions/runs", params={"head_sha": subject_sha}, group_id="workflow-runs",
    )
    workflow_by_id: dict[str, dict[str, Any]] = {}
    for row in workflow_runs:
        run_id = row.get("id")
        if isinstance(run_id, (int, str)) and not isinstance(run_id, bool):
            key = str(run_id)
            prior = workflow_by_id.get(key)
            if prior is not None and prior != row:
                raise GitHubObserverError("workflow run ID bị duplicate với raw fields khác nhau")
            workflow_by_id[key] = row

    relevant_workflows: list[dict[str, Any]] = []
    planned_workflow_events = {
        (item.get("workflow_path"), event)
        for item in checkrun_plans
        for event in item.get("eligible_events", [])
        if isinstance(item.get("workflow_path"), str) and isinstance(event, str)
    }
    for run in workflow_by_id.values():
        if ((run.get("path"), run.get("event")) in planned_workflow_events
                and run.get("head_sha") == subject_sha):
            relevant_workflows.append(run)
    if len(relevant_workflows) > MAX_RELEVANT_RUNS:
        raise GitHubObserverError("workflow generation count vượt bounded observer limit")

    jobs_by_run_attempt: dict[tuple[str, int], list[dict[str, Any]]] = {}
    jobs_page_by_ref: dict[str, list[dict[str, Any]]] = {}
    for run in relevant_workflows:
        run_id = str(run.get("id"))
        attempt_count = run.get("run_attempt")
        if isinstance(attempt_count, bool) or not isinstance(attempt_count, int) or not 1 <= attempt_count <= MAX_RUN_ATTEMPTS:
            raise GitHubObserverError(f"workflow run {run_id} run_attempt chưa bounded/valid")
        for attempt in range(1, attempt_count + 1):
            rows = client.paginate(
                endpoint="workflow_run_attempt", requested_sha=subject_sha,
                path=f"{base}/actions/runs/{run_id}/attempts/{attempt}/jobs", params={},
                group_id=f"jobs-{run_id}-attempt-{attempt}",
                workflow_run_id=run_id, workflow_run_attempt=attempt,
            )
            jobs_by_run_attempt[(run_id, attempt)] = rows
            # Keep the actual page member mapping for the static source evaluator.
            group_pages = [page for page in client.collection_pages
                           if page.get("endpoint") == "workflow_run_attempt"
                           and page.get("workflow_run_id") == run_id
                           and page.get("workflow_run_attempt") == attempt]
            for page in group_pages:
                page_value = client.raw_members.get(page.get("raw_page_ref"))
                body = page_value.get("body") if isinstance(page_value, dict) else None
                if isinstance(body, dict) and isinstance(body.get("jobs"), list):
                    jobs_page_by_ref[page["raw_page_ref"]] = body["jobs"]

    payload_sha = identity.get("verified_payload_sha")
    if not isinstance(payload_sha, str):
        raise GitHubObserverError("archived attempt thiếu verified payload P")
    check_plan_sha = hashlib.sha256(_canonical_json(plan)).hexdigest()
    collection_id = str(uuid.uuid4())
    completed_collection = _utc_now()
    collection = {
        "format_version": 1, "kind": "ci_collection", "collection_id": collection_id,
        "host": client.host, "repository_id": repository["repository_id"],
        "subject_sha": subject_sha, "check_plan_sha256": check_plan_sha,
        "started_at_utc": started, "completed_at_utc": completed_collection,
        "pages": list(client.collection_pages),
    }
    client._store("collection.json", _canonical_json(collection))

    objects: dict[str, dict[str, Any]] = {"collection.json": {"kind": "GitCICollectionEvidence", "value": collection}}
    raw = dict(client.raw_members)
    observed: list[dict[str, Any]] = []
    workflow_by_id_text = workflow_by_id

    for expected in checkrun_plans:
        check_id = expected.get("id")
        context = expected.get("context")
        source_app = expected.get("source_app")
        workflow_path = expected.get("workflow_path")
        job_name = expected.get("job_name")
        matrix = expected.get("matrix")
        app_match = re.fullmatch(r"github\.com:app:([1-9][0-9]*)", source_app or "")
        if (not all(isinstance(value, str) and value for value in (check_id, context, source_app, workflow_path, job_name))
                or app_match is None):
            raise GitHubObserverError(f"plan check {check_id!r} thiếu source identity đã hỗ trợ")
        expected_app_id = app_match.group(1)
        if expected.get("checkout_binding") != "head" or expected.get("reported_sha_binding") != "subject":
            raise GitHubObserverError(f"check {check_id} cần checkout=head và reported_sha=subject adapter")
        candidate_rows: list[dict[str, Any]] = []
        candidate_meta: dict[str, tuple[dict[str, Any], dict[str, Any], str]] = {}
        for checkrun in checks:
            if checkrun.get("name") != context or checkrun.get("head_sha") != subject_sha:
                continue
            app = checkrun.get("app")
            app_id = app.get("id") if isinstance(app, dict) else None
            details = checkrun.get("details_url")
            match = _RUN_JOB.search(details) if isinstance(details, str) else None
            if match is None:
                raise GitHubObserverError(f"candidate {context} check-run không pin Actions run/job URL")
            run_id, job_id = match.group(1), match.group(2)
            run = workflow_by_id_text.get(run_id)
            if run is None:
                raise GitHubObserverError(f"candidate {context} thiếu raw workflow-run source")
            if (str(app_id) != expected_app_id or run.get("path") != workflow_path
                    or run.get("event") not in expected["eligible_events"]
                    or run.get("head_sha") != subject_sha):
                raise GitHubObserverError(f"candidate {context} có native identity mâu thuẫn với frozen plan")
            created = run.get("created_at")
            if not isinstance(created, str):
                raise GitHubObserverError(f"workflow run {run_id} thiếu created_at")
            candidate_rows.append({
                "source_app": source_app, "context": context, "workflow_path": workflow_path,
                "job_name": job_name, "event": run["event"], "subject_sha": subject_sha,
                "id": checkrun.get("id"), "created_at_utc": created,
                "status": checkrun.get("status"), "conclusion": checkrun.get("conclusion"),
                "sha": checkrun.get("head_sha"),
            })
            candidate_meta[str(checkrun.get("id"))] = (checkrun, run, job_id)
        latest = select_latest_native_records(candidate_rows, source_kind="check_run")
        if latest.get("verdict") != "SELECTED" or not latest.get("records"):
            raise GitHubObserverError(f"check {check_id} latest native source UNKNOWN: {latest.get('reason')}")
        if len(latest["records"]) != 1:
            raise GitHubObserverError(f"check {check_id} có nhiều event identity candidate; không đoán")
        selected_native = next(iter(latest["records"].values()))
        checkrun, run, job_id = candidate_meta[str(selected_native["id"])]
        run_id = str(run.get("id"))
        event = run.get("event")
        generation_rows = [
            {"workflow_identity": row.get("path"), "event": row.get("event"),
             "subject_sha": row.get("head_sha"), "run_id": row.get("id"),
             "created_at_utc": row.get("created_at"), "collection_complete": True}
            for row in relevant_workflows
            if row.get("path") == workflow_path and row.get("event") == event and row.get("head_sha") == subject_sha
        ]
        generation = select_latest_actions_generation(
            generation_rows, workflow_identity=workflow_path, event=event, subject_sha=subject_sha,
        )
        if generation.get("verdict") != "SELECTED" or str(generation["run"].get("run_id")) != run_id:
            raise GitHubObserverError(f"check {check_id} không thuộc newest workflow generation")

        group_checks = [item for item in checkrun_plans
                        if item.get("workflow_path") == workflow_path]
        job_candidates: list[dict[str, Any]] = []
        job_rows_by_id: dict[str, dict[str, Any]] = {}
        for (candidate_run_id, attempt), rows in jobs_by_run_attempt.items():
            if candidate_run_id != run_id:
                continue
            for job in rows:
                if job.get("name") not in {item.get("context") for item in group_checks}:
                    continue
                if job.get("head_sha") != subject_sha:
                    raise GitHubObserverError(f"job {job.get('id')} checkout SHA khác closing subject")
                job_candidates.append({"run_id": candidate_run_id, "attempt": attempt,
                                       "check_identity": next(item["id"] for item in group_checks
                                                              if item["context"] == job.get("name")),
                                       "id": job.get("id")})
                job_rows_by_id[f"{attempt}:{job.get('id')}"] = job
        selected_attempts = select_latest_actions_attempts(
            job_candidates, selected_run_id=run_id,
            required_job_identities={item["id"] for item in group_checks},
        )
        if selected_attempts.get("verdict") != "SELECTED":
            raise GitHubObserverError(f"workflow {run_id} latest per-job attempt UNKNOWN: {selected_attempts.get('reason')}")
        selected_job = selected_attempts["attempts"].get(check_id)
        if selected_job is None or str(selected_job.get("id")) != job_id:
            raise GitHubObserverError(f"check {check_id} raw check-run không bind latest actual job attempt")
        attempt_number = selected_job["attempt"]
        job = job_rows_by_id.get(f"{attempt_number}:{job_id}")
        if job is None:
            raise GitHubObserverError(f"check {check_id} latest job raw source thiếu")
        if (checkrun.get("status") != "completed" or checkrun.get("conclusion") != "success"
                or job.get("status") != "completed" or job.get("conclusion") != "success"):
            raise GitHubObserverError(f"check {check_id} đang pending hoặc latest native kết quả không PASS")
        if not _matrix_matches(matrix, job):
            raise GitHubObserverError(f"check {check_id} không thể chứng minh matrix từ runner labels")
        suite = checkrun.get("check_suite")
        if not isinstance(suite, dict) or str(suite.get("id")) != str(run.get("check_suite_id")):
            raise GitHubObserverError(f"check {check_id} check_suite không khớp workflow run")

        raw_page_ref = None
        for page in client.collection_pages:
            if page.get("endpoint") != "check_runs":
                continue
            envelope = client.raw_members.get(page.get("raw_page_ref"))
            body = envelope.get("body") if isinstance(envelope, dict) else None
            page_checks = body.get("check_runs") if isinstance(body, dict) else None
            if any(str(item.get("id")) == str(checkrun.get("id"))
                   for item in _require_list(page_checks, "check_runs")):
                raw_page_ref = page["raw_page_ref"]
                break
        if raw_page_ref is None:
            raise GitHubObserverError(f"check {check_id} raw check-run page ref không được giữ")
        source_ref = f"checks/{check_id}/source.json"
        execution_ref = f"checks/{check_id}/execution.json"
        log_ref = f"logs/jobs/{job_id}.txt"
        log_bytes = _command_log(gh, run_id, job_id)
        log_text = log_bytes.decode("utf-8", "replace")
        command_results = []
        for command in expected.get("command_refs", []):
            if not isinstance(command, str) or command not in log_text:
                raise GitHubObserverError(f"check {check_id} job log không chứng minh command đã chạy: {command}")
            command_results.append({"command_ref": command, "revision": subject_sha,
                                   "result": "PASS", "log_ref": log_ref})
        if re.search(r"Process completed with exit code (?!0(?:\D|$))[0-9]+", log_text):
            raise GitHubObserverError(f"check {check_id} job log có command exit khác 0")
        client._store(log_ref, log_bytes, limit=MAX_LOG_BYTES)
        source = {
            "format_version": 1, "kind": "check_source", "source_kind": "actions_check_run",
            "host": client.host, "repository_id": repository["repository_id"],
            "record_id": str(checkrun.get("id")), "run_id": run_id, "job_id": str(job_id),
            "attempt": attempt_number, "created_at_utc": run["created_at"],
            "source_app": source_app, "context": context, "workflow_path": workflow_path,
            "job_name": job_name, "matrix": matrix, "event": event,
            "reported_head_sha": checkrun.get("head_sha"), "native_status": checkrun.get("status"),
            "native_conclusion": checkrun.get("conclusion"), "raw_api_ref": raw_page_ref,
            "check_suite_id": str(suite["id"]), "workflow_head_sha": run.get("head_sha"),
            "job_head_sha": job.get("head_sha"), "job_check_run_url": job.get("html_url"),
        }
        execution = {
            "format_version": 1, "kind": "check_execution", "check_id": check_id,
            "subject_sha": subject_sha, "tested_checkout_sha": subject_sha,
            "source_evidence_ref": source_ref, "command_results": command_results,
        }
        source_bytes = _canonical_json(source)
        execution_bytes = _canonical_json(execution)
        client._store(source_ref, source_bytes)
        client._store(execution_ref, execution_bytes)
        objects[source_ref] = {"kind": "GitCheckSourceEvidence", "value": source}
        objects[execution_ref] = {"kind": "GitCheckExecutionEvidence", "value": execution}
        raw_page = client.raw_members.get(raw_page_ref)
        body_ref = raw_page.get("body_ref") if isinstance(raw_page, dict) else None
        evidence_refs = [source_ref, execution_ref, raw_page_ref, body_ref, log_ref]
        if any(not isinstance(ref, str) for ref in evidence_refs):
            raise GitHubObserverError(f"check {check_id} missing evidence locator")
        observed.append({
            "check_id": check_id, "kind": expected["kind"], "context": context,
            "source_app": source_app, "workflow_path": workflow_path, "job_name": job_name,
            "matrix": matrix, "event": event, "run_id": run_id, "job_id": str(job_id),
            "attempt": attempt_number, "reported_head_sha": checkrun.get("head_sha"),
            "tested_checkout_sha": subject_sha, "pr_head_sha": None, "pr_base_sha": None,
            "tested_merge_sha": None, "status": checkrun.get("status"),
            "conclusion": checkrun.get("conclusion"), "execution_verified": True,
            "url": checkrun.get("html_url"), "evidence_refs": evidence_refs,
            "run_created_at_utc": run["created_at"], "source_evidence_ref": source_ref,
            "execution_evidence_ref": execution_ref,
        })

    if phase not in {"payload", "closing"}:
        raise GitHubObserverError(f"phase không được hỗ trợ: {phase}")
    commit_parent = (
        _payload_commit_parent(identity, subject_sha=subject_sha)
        if phase == "payload" else identity.get("verified_payload_sha")
    )
    if not isinstance(commit_parent, str):
        raise GitHubObserverError(f"identity thiếu parent SHA cho phase {phase}")
    observation = {
        "format_version": 1, "delivery_id": identity["delivery_id"], "phase": phase,
        "subject_sha": subject_sha, "parent_sha": commit_parent,
        "check_plan_sha256": check_plan_sha, "observed_remote_sha": subject_sha,
        "observed_at_utc": _utc_now(), "binding_verified": True,
        "required_set_verified": len(observed) == len(checkrun_plans), "checks": observed,
        "result": "PASS", "blocker": None, "merge_group_binding": None,
        "destination_observation": destination_observation,
        "pull_request_binding_ref": pull_request_binding_ref,
        "collection_evidence_ref": "collection.json",
    }
    if not observation["required_set_verified"]:
        raise GitHubObserverError("native check mapping không phủ đúng frozen set")
    observation_bytes = _canonical_json(observation)
    client._store(observation_ref, observation_bytes)
    objects[observation_ref] = {"kind": "GitDeliveryObservation", "value": observation}
    return {"collection": collection, "observation": observation, "objects": objects,
            "raw_members": raw, "member_bytes": client.member_bytes,
            "started_at_utc": started, "completed_collection_at_utc": completed_collection}


def _current_tracker_identity(spec_path: Path, task_id: str) -> tuple[str, int, str]:
    text = spec_path.read_text(encoding="utf-8", errors="strict")
    lines = text.splitlines()
    section_starts = [index for index, line in enumerate(lines) if line.startswith("## S19 ")]
    if len(section_starts) != 1:
        raise GitHubObserverError("SPEC phải có đúng một section S19")
    start = section_starts[0] + 1
    end = next((index for index in range(start, len(lines)) if lines[index].startswith("## ")), len(lines))
    rows = [line for line in lines[start:end] if line.startswith(f"| {task_id} |")]
    if len(rows) != 1:
        raise GitHubObserverError(f"S19 phải có đúng một hàng {task_id}")
    parts = rows[0].split("|")
    if len(parts) < 2 or parts[0].strip() or parts[1].strip() != task_id:
        raise GitHubObserverError(f"S19 hàng {task_id} sai cấu trúc cột")
    fields = parts[1:]
    if fields and not fields[-1].strip():
        fields = fields[:-1]
    if len(fields) != 9 or fields[0].strip() != task_id:
        raise GitHubObserverError(f"S19 hàng {task_id} sai cấu trúc cột")
    evidence = fields[8].strip()
    revision = re.search(r"(?:^|;)task_revision=([1-9][0-9]*)(?:;|$)", evidence)
    delivery = re.search(r"(?:^|;)delivery=([0-9a-f-]{36})(?:;|$)", evidence)
    if revision is None or delivery is None:
        raise GitHubObserverError("S19 current row không mang task_revision/active delivery")
    return task_id, int(revision.group(1)), delivery.group(1)


def _verify_publication_projection(repo_root: Path, delivery_id: str, projection_path: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    root = repo_root.resolve(strict=True)
    operations = Path(os.path.abspath(root / "artifacts" / "git-delivery" / "operations"))
    candidate = Path(os.path.abspath(projection_path))
    try:
        relative = candidate.relative_to(operations)
    except ValueError as exc:
        raise GitHubObserverError("publication projection phải nằm trong operations/<UUID>/publication.json") from exc
    if len(relative.parts) != 2 or relative.name != "publication.json":
        raise GitHubObserverError("publication projection phải nằm trong operations/<UUID>/publication.json")
    try:
        operation_id = str(uuid.UUID(relative.parts[0]))
    except ValueError as exc:
        raise GitHubObserverError("publication projection operation directory phải là UUID") from exc
    if operation_id != relative.parts[0]:
        raise GitHubObserverError("publication projection operation UUID phải canonical lowercase")
    current = root
    for component in ("artifacts", "git-delivery", "operations", operation_id, "publication.json"):
        current = current / component
        try:
            info = current.lstat()
        except OSError as exc:
            raise GitHubObserverError(f"publication projection path không đọc được: {type(exc).__name__}") from exc
        if current.is_symlink() or getattr(info, "st_file_attributes", 0) & 0x400:
            raise GitHubObserverError("publication projection path chứa symlink/reparse point")
        if component != "publication.json" and not current.is_dir():
            raise GitHubObserverError("publication projection ancestor phải là directory thật")
        if component == "publication.json" and not current.is_file():
            raise GitHubObserverError("publication projection phải là regular file")
    archive_projection = verify_archive(repo_root, delivery_id)
    try:
        projection = strict_json_loads(_read_bounded(candidate))
    except (ArchiveError, SchemaError) as exc:
        raise GitHubObserverError(f"publication projection không đọc được: {exc}") from exc
    required = {"format_version", "kind", "planned_archive_path", "archive_manifest_sha256",
                "archive_seal_sha256", "published_at_utc", "readback_completed_at_utc"}
    if not isinstance(projection, dict) or set(projection) != required:
        raise GitHubObserverError("publication projection thiếu fields hoặc có field lạ")
    for field in ("planned_archive_path", "archive_manifest_sha256", "archive_seal_sha256"):
        if projection.get(field) != archive_projection.get(field):
            raise GitHubObserverError(f"publication projection lệch final readback ở {field}")
    published, readback = _iso_time(projection.get("published_at_utc")), _iso_time(projection.get("readback_completed_at_utc"))
    if published is None or readback is None or readback < published:
        raise GitHubObserverError("publication projection thiếu actual publish/readback interval")
    if (projection.get("kind") != "archive_publication" or projection.get("format_version") != 1
            or projection.get("planned_archive_path") != f"artifacts/git-delivery/{delivery_id}"):
        raise GitHubObserverError("publication projection sai archive identity")
    return projection, archive_projection


def _validate_live_verification_record(
    verification: dict[str, Any], schema: dict[str, Any],
) -> list[str]:
    errors = validate_schema(
        verification, schema["$defs"]["GitLiveVerification"], root_schema=schema,
    )
    errors.extend(validate_semantics("GitLiveVerification", verification))
    return errors


def verify_live_delivery(
    repo_root: Path,
    delivery_id: str,
    publication_projection_path: Path,
    *,
    gh: str = "gh",
) -> dict[str, Any]:
    """Read back the sealed archive, query fresh GitHub state and retain the live proof.

    Any incomplete API collection or ambiguous plan mapping raises an error and
    leaves its partial invocation directory intact for diagnosis.  A retry gets
    a new invocation UUID and never rewrites prior evidence.
    """
    root = repo_root.resolve(strict=True)
    projection_path = Path(publication_projection_path)
    if not projection_path.is_absolute():
        projection_path = root / projection_path
    projection, archive_projection = _verify_publication_projection(root, delivery_id, projection_path)
    archive_dir = root / "artifacts" / "git-delivery" / delivery_id
    archive = strict_json_loads(_read_bounded(archive_dir / "manifest.json"))
    seal = strict_json_loads(_read_bounded(archive_dir / "seal.json"))
    if not isinstance(archive, dict) or not isinstance(seal, dict):
        raise GitHubObserverError("sealed archive manifest/seal sai shape")
    objects, raw_members, archive_member_bytes = _load_index(archive_dir, archive)
    attempt_ref = archive.get("attempt_ref")
    plan_ref = objects.get(attempt_ref, {}).get("value", {}).get("check_plan_ref")
    attempt = objects.get(attempt_ref, {}).get("value")
    plan = objects.get(plan_ref, {}).get("value")
    selection = objects.get(archive.get("selection_ref"), {}).get("value")
    closing_commit = objects.get(archive.get("closing_commit_ref"), {}).get("value")
    if not all(isinstance(value, dict) for value in (attempt, plan, selection, closing_commit)):
        raise GitHubObserverError("archive thiếu attempt/plan/selector/closing GitCommit typed sources")
    task_id, task_revision, active_id = _current_tracker_identity(root / "Maps-Context-Engine-4D_SPEC.md", archive["task_id"])
    if (task_id != archive.get("task_id") or task_revision != archive.get("task_revision")
            or active_id != delivery_id or selection.get("active_delivery_id") != delivery_id
            or selection.get("task_revision") != task_revision):
        raise GitHubObserverError("S19/selector hiện hành không trỏ đúng sealed delivery")
    head_result = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root, capture_output=True, check=False)
    branch_result = subprocess.run(["git", "branch", "--show-current"], cwd=root, capture_output=True, check=False)
    diff_result = subprocess.run(["git", "diff", "--quiet"], cwd=root, capture_output=True, check=False)
    staged_result = subprocess.run(["git", "diff", "--cached", "--quiet"], cwd=root, capture_output=True, check=False)
    if any(item.returncode for item in (head_result, branch_result, diff_result, staged_result)):
        raise GitHubObserverError("local HEAD/branch/working-tree/index không được chứng minh sạch")
    local_sha = head_result.stdout.decode("ascii", "strict").strip()
    branch = branch_result.stdout.decode("utf-8", "strict").strip()
    expected_sha = archive.get("closing_sha")
    if local_sha != expected_sha or branch != "main":
        raise GitHubObserverError("observer chỉ chạy khi checkout main đúng closing C của archive")
    repository = plan.get("repository")
    if not isinstance(repository, dict) or repository.get("host") != "github.com":
        raise GitHubObserverError("current plan repository chưa được hỗ trợ")
    owner, name = repository.get("owner"), repository.get("name")
    host = repository.get("host")
    invocation_id = str(uuid.uuid4())
    live_parent = root / "artifacts" / "git-live" / delivery_id
    if live_parent.exists():
        info = live_parent.lstat()
        if live_parent.is_symlink() or not live_parent.is_dir() or getattr(info, "st_file_attributes", 0) & 0x400:
            raise GitHubObserverError("live root delivery không phải directory thường")
    else:
        live_parent.mkdir(parents=True)
    output_root = live_parent / invocation_id
    output_root.mkdir(exist_ok=False)
    global _LAST_INVOCATION_ROOT
    _LAST_INVOCATION_ROOT = output_root
    client = GitHubApiClient(host, output_root, gh=gh)
    started = client.started_at_utc

    repo_snapshot = client.snapshot(path=f"repos/{owner}/{name}", query={}, group_id="repository-before")
    repo_value = repo_snapshot["body"]
    full_name = repo_value.get("full_name") if isinstance(repo_value, dict) else None
    if (not isinstance(repo_value, dict) or repo_value.get("id") != repository.get("repository_id")
            or not isinstance(full_name, str) or full_name.casefold() != f"{owner}/{name}".casefold()
            or repo_value.get("visibility") != repository.get("visibility")
            or repo_value.get("archived") is True or repo_value.get("disabled") is True):
        raise GitHubObserverError("repository ID/name/visibility/availability khác frozen plan")
    user_snapshot = client.snapshot(path="user", query={}, group_id="authenticated-user")
    user = user_snapshot["body"]
    if not isinstance(user, dict) or isinstance(user.get("id"), bool) or not isinstance(user.get("id"), int) or not isinstance(user.get("login"), str):
        raise GitHubObserverError("authenticated GitHub identity chưa được xác minh")
    user_record = {"format_version": 1, "kind": "authenticated_user_projection",
                   "id": user["id"], "login": user["login"], "type": user.get("type")}
    user_bytes = _canonical_json(user_record)
    client._store("identity/actor.json", user_bytes)
    actual_ref = str(repository.get("ref", ""))
    ref_prefix = "refs/heads/"
    if not actual_ref.startswith(ref_prefix):
        raise GitHubObserverError("plan ref live chỉ hỗ trợ refs/heads/*")
    branch_name = actual_ref[len(ref_prefix):]
    from urllib.parse import quote

    encoded_branch = "/".join(quote(part, safe="") for part in branch_name.split("/"))
    ref_before = client.snapshot(path=f"repos/{owner}/{name}/git/ref/heads/{encoded_branch}",
                                 query={}, group_id="ref-before")
    ref_body = ref_before["body"]
    ref_object = ref_body.get("object") if isinstance(ref_body, dict) else None
    remote_sha = ref_object.get("sha") if isinstance(ref_object, dict) else None
    if remote_sha != expected_sha:
        raise GitHubObserverError("remote branch trước live CI không bằng closing C")
    commit_source = client.snapshot(path=f"repos/{owner}/{name}/commits/{expected_sha}",
                                    query={}, group_id="closing-commit")
    commit_body = commit_source["body"]
    if not isinstance(commit_body, dict) or commit_body.get("sha") != expected_sha:
        raise GitHubObserverError("fresh GitHub commit source không khớp closing C")
    actual_snapshot = _closing_commit_snapshot(
        commit_body, expected_sha=expected_sha,
        payload_sha=archive.get("verified_payload_sha"),
    )
    archived_git_ref = closing_commit.get("git_evidence_ref")
    archived_git = raw_members.get(archived_git_ref) if isinstance(archived_git_ref, str) else None
    if (not isinstance(archived_git, dict)
            or any(actual_snapshot.get(key) != archived_git.get(key)
                   for key in ("sha", "parents", "subject", "body", "changed_paths"))):
        raise GitHubObserverError("fresh GitHub commit projection lệch retained closing Git evidence")
    git_ref = "live/git/closing-commit-snapshot.json"
    git_bytes = _canonical_json(actual_snapshot)
    client._store(git_ref, git_bytes)
    client.raw_members[git_ref] = actual_snapshot

    # Current commit PR associations are queried explicitly. A nonempty set needs
    # its own frozen PR flow and source-parity adapter before it can be accepted.
    pr_snapshot = client.snapshot(
        path=f"repos/{owner}/{name}/commits/{expected_sha}/pulls",
        query={"per_page": "100", "page": "1"}, group_id="current-prs",
    )
    if _parse_links(pr_snapshot["envelope"].get("response_headers", {}).get("link")).get("next") is not None:
        raise GitHubObserverError("current commit có >100 PR associations; pagination bổ sung chưa được đóng")
    prs = _require_list(pr_snapshot["body"], "current commit pull requests")
    if prs:
        raise GitHubObserverError("current commit có PR binding; frozen plan hiện không cho PR flow")

    live_observation_ref = "live/closing-observation.json"
    graph = collect_ci_source_graph(
        client, plan=plan, subject_sha=expected_sha, identity=attempt,
        phase="closing", destination_observation=objects.get(archive.get("closing_observation_ref"), {}).get("value", {}).get("destination_observation"),
        pull_request_binding_ref=None, observation_ref=live_observation_ref, gh=gh,
    )
    # Read the branch again after all CI/log/API sources to detect movement during
    # collection. A changed or temporarily unreadable ref produces no PASS.
    ref_after = client.snapshot(path=f"repos/{owner}/{name}/git/ref/heads/{encoded_branch}",
                                query={}, group_id="ref-after")
    after_body = ref_after["body"]
    after_object = after_body.get("object") if isinstance(after_body, dict) else None
    final_sha = after_object.get("sha") if isinstance(after_object, dict) else None
    if final_sha != expected_sha:
        raise GitHubObserverError("remote branch di chuyển trong live verification")

    live_completed = _utc_now()
    members = dict(client.member_bytes)
    live_files = [{"path": ref, "byte_length": len(data), "sha256": _digest(data)}
                  for ref, data in sorted(members.items())]
    verification = {
        "format_version": 1, "kind": "live_verification", "verification_id": invocation_id,
        "assignment_ref": attempt["assignment_ref"], "task_id": attempt["task_id"],
        "task_revision": attempt["task_revision"], "delivery_id": delivery_id,
        "check_plan_sha256": attempt["check_plan_sha256"], "subject_sha": expected_sha,
        "observed_remote_sha": final_sha, "started_at_utc": started,
        "completed_at_utc": live_completed,
        "closing_observation_ref": live_observation_ref,
        "collection_evidence_ref": "collection.json", "git_evidence_ref": git_ref,
        "current_pr_ref": None, "files": live_files,
        "archive_publication": {
            "format_version": 1, "kind": "archive_publication",
            "planned_archive_path": projection["planned_archive_path"],
            "archive_manifest_sha256": projection["archive_manifest_sha256"],
            "archive_seal_sha256": projection["archive_seal_sha256"],
            "published_at_utc": projection["published_at_utc"],
            "readback_completed_at_utc": projection["readback_completed_at_utc"],
        },
    }
    governance_schema = extract_governance_schema(
        (root / "Maps-Context-Engine-4D_SPEC.md").read_text(encoding="utf-8")
    )
    verification_errors = _validate_live_verification_record(verification, governance_schema)
    if verification_errors:
        raise GitHubObserverError("live verification record schema/semantics REJECT: " + "; ".join(verification_errors[:8]))
    combined_objects = _merge_evidence_members(
        "typed", objects, graph["objects"],
        {"live-verification.json": {"kind": "GitLiveVerification", "value": verification}},
    )
    # The live Git snapshot is raw retained evidence, not a typed governance object.
    combined_raw = _merge_evidence_members("raw", raw_members, graph["raw_members"])
    combined_member_bytes = _merge_evidence_members(
        "archive/live byte", archive_member_bytes, client.member_bytes,
    )
    linked = validate_delivery_links(
        combined_objects, raw_members=combined_raw, raw_member_bytes=combined_member_bytes,
    )
    if linked["errors"]:
        raise GitHubObserverError("fresh live graph bị static parity reject: " + "; ".join(linked["errors"][:10]))
    verification_bytes = _canonical_json(verification)
    _write_member(output_root, "live-verification.json", verification_bytes)
    # Read every file and the final record back after flush. The verification
    # record itself is deliberately outside its own files hash list.
    for ref, expected_bytes in members.items():
        actual = _read_bounded(output_root / ref, limit=max(len(expected_bytes), MAX_LOG_BYTES))
        if actual != expected_bytes:
            raise GitHubObserverError(f"live evidence readback byte mismatch: {ref}")
    if _read_bounded(output_root / "live-verification.json") != verification_bytes:
        raise GitHubObserverError("live verification record readback không khớp")
    return {
        "result": "PASS", "verification_id": invocation_id, "delivery_id": delivery_id,
        "task_id": attempt["task_id"], "task_revision": attempt["task_revision"],
        "subject_sha": expected_sha, "observed_remote_sha": final_sha,
        "repository_id": repository["repository_id"], "actor": user_record["login"],
        "check_count": len(graph["observation"]["checks"]), "page_count": len(client.collection_pages),
        "archive_manifest_sha256": archive_projection["archive_manifest_sha256"],
        "live_record": str(output_root / "live-verification.json"),
        "static_verdict": linked["verdict"], "completed_at_utc": live_completed,
    }
