"""Các kiểm tra chéo tối thiểu cho các record governance S21."""

from __future__ import annotations

import hashlib
import json
import re
from fractions import Fraction
from pathlib import Path, PurePosixPath, PureWindowsPath
from typing import Any
from urllib.parse import quote, urlsplit
from uuid import UUID

from .ci_order import (
    OrderingError,
    parse_instant,
    select_latest_actions_attempts,
    select_latest_actions_generation,
    select_latest_native_records,
)
from .schema import SchemaError, strict_json_loads


SHA256 = re.compile(r"^[0-9a-f]{64}$")
GIT_SHA = re.compile(r"^(?:[0-9a-f]{40}|[0-9a-f]{64})$")
SKIP_RESULTS = {"skipped", "neutral", "cancelled", "timed_out", "action_required", "pending", "in_progress"}


def _parse_time(value: Any) -> tuple[int, Fraction] | None:
    if not isinstance(value, str):
        return None
    try:
        return parse_instant(value)
    except (OrderingError, TypeError, ValueError):
        return None


def _check_run_api_url_matches(url: Any, *, host: Any, owner: Any, name: Any, record_id: Any) -> bool:
    if not all(isinstance(value, str) and value for value in (url, host, owner, name, record_id)):
        return False
    try:
        parsed = urlsplit(url)
        if parsed.scheme != "https" or parsed.username or parsed.password or parsed.port is not None:
            return False
    except ValueError:
        return False
    if parsed.query or parsed.fragment:
        return False
    if host == "github.com":
        expected_host, api_prefix = "api.github.com", "/repos"
    else:
        expected_host, api_prefix = host.lower(), "/api/v3/repos"
    expected_path = (
        f"{api_prefix}/{quote(owner, safe='')}/{quote(name, safe='')}"
        f"/check-runs/{record_id}"
    )
    return parsed.hostname is not None and parsed.hostname.lower() == expected_host and parsed.path == expected_path


def _safe_relative_path(value: Any) -> bool:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        return False
    if PurePosixPath(value).is_absolute() or PureWindowsPath(value).is_absolute():
        return False
    if PureWindowsPath(value).drive:
        return False
    return all(part not in {"", ".", ".."} for part in value.split("/"))


def _typed_equal(left: Any, right: Any) -> bool:
    """Compare decoded JSON structures without bool/int coercion."""
    if type(left) is not type(right):
        return False
    if isinstance(left, dict):
        return left.keys() == right.keys() and all(_typed_equal(left[key], right[key]) for key in left)
    if isinstance(left, list):
        return len(left) == len(right) and all(_typed_equal(a, b) for a, b in zip(left, right))
    return left == right


def _canonical(value: Any) -> bytes:
    """Canonical JSON primitive used for the governance plan digest."""
    if value is None or isinstance(value, (str, bool)):
        try:
            return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        except UnicodeEncodeError as exc:
            raise ValueError("JSON string chứa surrogate") from exc
    if isinstance(value, int) and not isinstance(value, bool):
        if abs(value) > 9007199254740991:
            raise ValueError("JSON integer vượt miền safe integer")
        return str(value).encode("ascii")
    if isinstance(value, float):
        raise ValueError("MCE-MANIFEST-JSON1 không nhận fraction/exponent")
    if isinstance(value, list):
        return b"[" + b",".join(_canonical(item) for item in value) + b"]"
    if isinstance(value, dict):
        if not all(isinstance(key, str) for key in value):
            raise ValueError("JSON key phải là chuỗi")
        pairs = []
        for key in sorted(value):
            pairs.append(_canonical(key) + b":" + _canonical(value[key]))
        return b"{" + b",".join(pairs) + b"}"
    raise ValueError(f"Giá trị không thuộc primitive JSON: {type(value).__name__}")


def validate_semantics(kind: str, record: Any) -> list[str]:
    """Validate invariants độc lập với shape; không xác nhận nguồn ngoài."""
    errors: list[str] = []
    if not isinstance(record, dict):
        return ["record governance phải là object"]

    def error(message: str) -> None:
        errors.append(message)

    if "check_plan_sha256" in record and isinstance(record.get("check_plan_ref"), dict):
        plan = record["check_plan_ref"]
        expected = record["check_plan_sha256"]
        if isinstance(expected, str) and SHA256.fullmatch(expected):
            try:
                actual = hashlib.sha256(_canonical(plan)).hexdigest()
                if actual != expected:
                    error("check_plan_sha256 không khớp MCE-MANIFEST-JSON1(check_plan_ref)")
            except (TypeError, ValueError) as exc:
                error(f"check plan không canonical được: {exc}")

    checks = record.get("checks")
    if isinstance(checks, list):
        ids: set[str] = set()
        identities: set[tuple[Any, ...]] = set()
        if not checks:
            error("check plan không được rỗng")
        for index, check in enumerate(checks):
            if not isinstance(check, dict):
                continue
            check_id = check.get("id")
            if isinstance(check_id, str):
                if check_id in ids:
                    error(f"checks[{index}].id bị lặp: {check_id}")
                ids.add(check_id)
            events = check.get("eligible_events")
            if not isinstance(events, list):
                events = [check.get("event")]
            for event in events:
                identity_parts = (check.get("kind"), check.get("context"), check.get("source_app"),
                                  check.get("workflow_path"), check.get("job_name"), check.get("matrix"), event)
                if all(part is not None for part in identity_parts):
                    try:
                        identity = tuple(_canonical(part) for part in identity_parts)
                    except (TypeError, ValueError) as exc:
                        error(f"checks[{index}] native identity không canonical: {exc}")
                        continue
                    if identity in identities:
                        error(f"checks[{index}] trùng native execution identity với eligible event {event}")
                    identities.add(identity)

    if kind == "GitCheckPlan":
        if record.get("required_set_status") != "VERIFIED":
            error("required_set_status phải VERIFIED trước payload")
        if not record.get("canary_ref"):
            error("canary_ref không được rỗng")
        for key in ("local_check_refs", "closing_local_reuse_refs"):
            refs = record.get(key)
            if isinstance(refs, list):
                if any(not isinstance(ref, str) or not _safe_relative_path(ref) for ref in refs):
                    error(f"{key} phải chứa private relative locator an toàn")
                elif len(refs) != len(set(refs)):
                    error(f"{key} có ref trùng")
        if isinstance(record.get("checks"), list):
            plan_commands: set[str] = set()
            for index, check in enumerate(record["checks"]):
                if not isinstance(check, dict):
                    error(f"checks[{index}] phải là object")
                    continue
                if not check.get("command_refs"):
                    error(f"checks[{index}] thiếu command_refs; check rỗng không thể PASS")
                context = check.get("context")
                commands = check.get("command_refs")
                command_text = " ".join(commands) if isinstance(commands, list) and all(isinstance(x, str) for x in commands) else ""
                if isinstance(commands, list):
                    plan_commands.update(command for command in commands if isinstance(command, str))
                if (isinstance(context, str) and "closing" in context.lower()) or "closing" in command_text.lower():
                    error(f"checks[{index}] tạo phụ thuộc vào CI của closing đang chạy")
            local_refs = record.get("local_check_refs")
            if isinstance(local_refs, list) and len(local_refs) != len(plan_commands):
                error("local_check_refs phải có đúng một slot cho mỗi command duy nhất trong required checks")

    if kind == "GitDeliveryAttempt":
        try:
            delivery_uuid = UUID(record.get("delivery_id", ""))
            if str(delivery_uuid) != record.get("delivery_id"):
                raise ValueError("UUID không chuẩn hóa lowercase")
        except (ValueError, TypeError, AttributeError):
            error("delivery_id phải là UUID")
        ordered = record.get("ordered_commits")
        if isinstance(ordered, list):
            if any(not isinstance(item, dict) for item in ordered):
                error("ordered_commits chỉ chứa object commit")
            shas = [item.get("sha") for item in ordered if isinstance(item, dict)]
            if (any(not isinstance(sha, str) for sha in shas)
                    or len(shas) != len(set(sha for sha in shas if isinstance(sha, str)))):
                error("ordered_commits chứa SHA trùng")
            payload = record.get("verified_payload_sha")
            if shas and payload != shas[-1]:
                error("verified_payload_sha phải là commit cuối của ordered_commits")
            if record.get("closing_sha") in shas:
                error("ordered_commits không được chứa closing C")
            for sha in shas:
                if not isinstance(sha, str) or not GIT_SHA.fullmatch(sha):
                    error("ordered_commits phải chứa SHA Git đầy đủ")
                    break
        if record.get("phase") == "closing_proposed" and record.get("closing_parent_sha") != record.get("verified_payload_sha"):
            error("closing_parent_sha phải bằng verified_payload_sha P")
        if record.get("payload_mode") == "verified_noop":
            if not isinstance(ordered, list) or len(ordered) != 1:
                error("verified_noop chỉ được giữ một historical payload P")
            if record.get("noop_reason_ref") is None:
                error("verified_noop cần retained reason reference")
        elif (record.get("phase") == "closing_proposed" and record.get("payload_mode") == "changes"
              and (not isinstance(ordered, list) or not ordered)):
            error("payload_mode=changes cần ordered payload commit")
        marker = record.get("closing_marker")
        if marker is not None and marker != f"MCE4D-Delivery-ID: {record.get('delivery_id')}":
            error("closing_marker không khớp delivery_id bất biến")

    if kind == "GitEvidenceArchive":
        files = record.get("files")
        if isinstance(files, list):
            paths: set[str] = set()
            manifest_path = record.get("manifest_path", "manifest.json")
            archive_path = record.get("archive_path")
            for index, item in enumerate(files):
                if not isinstance(item, dict):
                    error(f"files[{index}] phải là object")
                    continue
                path = item.get("path")
                if not _safe_relative_path(path):
                    error(f"files[{index}].path phải là relative path không traversal")
                elif path in paths:
                    error(f"files[{index}].path bị lặp: {path}")
                else:
                    paths.add(path)
                if path == manifest_path or (archive_path is not None and path == archive_path):
                    error("manifest/archive không được tự liệt kê trong manifest")
                if not isinstance(item.get("sha256"), str) or not SHA256.fullmatch(item["sha256"]):
                    error(f"files[{index}].sha256 không hợp lệ")
                if not isinstance(item.get("byte_length"), int) or isinstance(item.get("byte_length"), bool) or item["byte_length"] < 0:
                    error(f"files[{index}].byte_length không phải số nguyên không âm")

    if kind == "GitEvidenceFile":
        if not _safe_relative_path(record.get("path")):
            error("path evidence phải là relative path không traversal")
        if not isinstance(record.get("sha256"), str) or not SHA256.fullmatch(record["sha256"]):
            error("sha256 evidence không hợp lệ")

    if kind == "GitObservedCheck":
        conclusion = record.get("conclusion")
        native = record.get("native_conclusion")
        if isinstance(conclusion, str) and conclusion.lower() in SKIP_RESULTS:
            error("skipped/neutral/pending không được coi là required tests PASS")
        if isinstance(native, str) and native.lower() in SKIP_RESULTS:
            error("native skipped/neutral/pending không được coi là required tests PASS")
        requested = record.get("requested_sha")
        tested = record.get("tested_checkout_sha")
        if requested and tested and requested != tested:
            error("tested_checkout_sha sai lệch với requested_sha")
        attempt = record.get("attempt")
        if isinstance(attempt, bool) or not isinstance(attempt, int) or attempt < 1:
            error("attempt native phải là số nguyên dương, không nhận bool/float")
        if record.get("status") == "completed" and isinstance(record.get("conclusion"), str) and record.get("conclusion") in SKIP_RESULTS:
            error("check terminal skipped/neutral không được coi là PASS")
        if record.get("conclusion") == "success":
            for key in ("execution_verified", "execution_evidence_ref"):
                if not record.get(key):
                    error(f"native success thiếu {key}")
            refs = record.get("evidence_refs")
            if not isinstance(refs, list) or not refs:
                error("native success thiếu retained evidence_refs")

    if kind == "GitDeliveryObservation":
        phase = record.get("phase")
        result = record.get("result")
        if phase == "merge":
            if record.get("destination_observation") is not None:
                error("merge observation không được khai push destination")
            if not isinstance(record.get("merge_group_binding"), dict):
                error("merge phase cần GitMergeGroupBinding")
        elif isinstance(phase, str) and phase in {"payload", "closing"}:
            destination = record.get("destination_observation")
            if not isinstance(destination, dict):
                error("push receipt cần destination observation riêng của phase")
            elif destination.get("candidate_sha") != record.get("subject_sha"):
                error("destination candidate_sha không khớp subject")
            if record.get("merge_group_binding") is not None:
                error("push observation không được mượn merge-group binding")
        if result == "PASS":
            if record.get("binding_verified") is not True or record.get("required_set_verified") is not True:
                error("observation PASS cần binding và expected-set proof dạng boolean true")
            if record.get("observed_remote_sha") != record.get("subject_sha"):
                error("observation PASS remote SHA phải bằng subject SHA")
            checks = record.get("checks")
            if not isinstance(checks, list) or not checks:
                error("observation PASS cần required checks thực")
            else:
                for index, check in enumerate(checks):
                    if not isinstance(check, dict):
                        error(f"checks[{index}] phải là object")
                        continue
                    if (check.get("status") != "completed" or check.get("conclusion") != "success"
                            or check.get("tested_checkout_sha") != record.get("subject_sha")):
                        error(f"checks[{index}] chưa terminal success trên subject checkout")
                    if check.get("execution_verified") is not True or not check.get("source_evidence_ref") or not check.get("execution_evidence_ref"):
                        error(f"checks[{index}] thiếu native source/execution proof")
            if not isinstance(record.get("collection_evidence_ref"), str) or not record["collection_evidence_ref"]:
                error("observation PASS cần retained collection evidence")

    if kind == "GitTaskDeliverySelection":
        active = record.get("active_delivery_id")
        allocated = _parse_time(record.get("allocated_at_utc"))
        updated = _parse_time(record.get("updated_at_utc"))
        if active is None and record.get("allocated_at_utc") is not None:
            error("active_delivery_id null thì allocated_at_utc phải null")
        if active is not None:
            if allocated is None:
                error("active delivery phải giữ allocated_at_utc thực")
            if updated is None:
                error("selection active cần updated_at_utc hợp lệ")
            if updated is not None and allocated is not None and allocated > updated:
                error("allocated_at_utc không được sau updated_at_utc")
        previous = _parse_time(record.get("previous_selection_updated_at_utc"))
        if previous and allocated and previous > allocated:
            error("selection trước cập nhật sau thời điểm allocate hiện tại")

    if kind == "GitExecutionCheckpoint":
        command_state = record.get("command_state")
        if record.get("command_record_ref") is None:
            if record.get("operation_id") is not None:
                error("checkpoint chưa có command record không được giữ operation_id")
        elif isinstance(command_state, str) and command_state in {"NOT_STARTED", "RUNNING", "SETTLING", "SUCCEEDED", "FAILED", "UNKNOWN"} and record.get("operation_id") is None:
            error(f"checkpoint {command_state} có command record nhưng thiếu operation_id")
        if command_state == "NOT_STARTED" and record.get("command_record_ref") is None:
            error("checkpoint NOT_STARTED allocated phải giữ command_record_ref")

    if kind == "GitCommandLifecycle":
        started = _parse_time(record.get("started_at_utc"))
        completed = _parse_time(record.get("completed_at_utc"))
        if started and completed and completed < started:
            error("completed_at_utc không được trước started_at_utc")
        retry = record.get("retry_index")
        if isinstance(retry, bool) or not isinstance(retry, int) or not 0 <= retry <= 9007199254740991:
            error("retry_index phải là safe integer không âm, không nhận bool/float")
        contract = record.get("contract")
        result = record.get("result")
        if isinstance(contract, dict) and isinstance(result, dict):
            result_started = _parse_time(result.get("started_at_utc"))
            result_completed = _parse_time(result.get("completed_at_utc"))
            deadline = _parse_time(contract.get("deadline_at_utc"))
            if deadline is None:
                error("command contract cần deadline RFC3339 có timezone")
            if result_started and result_completed and result_completed < result_started:
                error("command result hoàn tất trước lúc bắt đầu")
            if deadline and result_started and result_started > deadline:
                error("command bắt đầu sau deadline đã freeze")
            if result_started is None and result.get("final_status") not in {"NOT_STARTED", "RUNNING", "UNKNOWN"}:
                error("terminal command result cần started_at_utc")
            if result_completed is not None and result_started is None:
                error("command có completed_at_utc nhưng thiếu started_at_utc")
            if record.get("command_state") == "SUCCEEDED" and (
                    result.get("final_status") != "SUCCEEDED" or result.get("termination_reason") != "completed"
                    or result.get("completed_at_utc") is None):
                error("command_state SUCCEEDED cần terminal result SUCCEEDED/completed")

    if kind == "GitPushOperationEvidence":
        retry = record.get("retry_index")
        if isinstance(retry, bool) or not isinstance(retry, int) or not 0 <= retry <= 9007199254740991:
            error("push retry_index phải là safe integer không âm")
        lifecycle_start = _parse_time(record.get("lifecycle_started_at_utc"))
        deadline = _parse_time(record.get("deadline_at_utc"))
        push_start = _parse_time(record.get("push_started_at_utc"))
        push_end = _parse_time(record.get("push_completed_at_utc"))
        if not all(item is not None for item in (lifecycle_start, deadline, push_start, push_end)):
            error("push lifecycle cần đủ instant RFC3339 hợp lệ")
        elif not lifecycle_start <= push_start <= push_end <= deadline:
            error("push chronology phải nằm trong deadline lifecycle")
        events = record.get("intervening_events")
        if isinstance(events, list) and events:
            error("push freshness mất hiệu lực sau wait/interruption/config/other push")
        preflight = record.get("preflight")
        postflight = record.get("postflight")
        if isinstance(preflight, dict) and isinstance(postflight, dict):
            if preflight.get("query_id") == postflight.get("query_id"):
                error("preflight và postflight phải là hai query identity riêng")
            for label, query in (("preflight", preflight), ("postflight", postflight)):
                query_retry = query.get("retry_index")
                if (query.get("lifecycle_id") != record.get("lifecycle_id")
                        or isinstance(query_retry, bool) or not isinstance(query_retry, int) or query_retry != retry
                        or query.get("candidate_sha") != record.get("candidate_sha")):
                    error(f"{label} phải bind đúng lifecycle/retry/candidate")
            pre_start = _parse_time(preflight.get("started_at_utc"))
            pre_end = _parse_time(preflight.get("completed_at_utc"))
            post_start = _parse_time(postflight.get("started_at_utc"))
            post_end = _parse_time(postflight.get("completed_at_utc"))
            if pre_start is None or pre_end is None or not pre_start <= pre_end <= push_start:
                error("preflight query phải hoàn tất trước push")
            if post_start is None or post_end is None or not push_end <= post_start <= post_end:
                error("postflight query phải hoàn tất sau push")
            if postflight.get("observed_remote_sha") != record.get("candidate_sha"):
                error("postflight remote SHA phải bằng candidate")
            if not _typed_equal(preflight.get("repository"), postflight.get("repository")):
                error("preflight/postflight repository identity phải bằng nhau")
            if preflight.get("remote_config_digest") != postflight.get("remote_config_digest"):
                error("preflight/postflight remote config phải bằng nhau")

    if kind == "GitLiveVerification":
        # Đây là guard shape/logic cục bộ; live proof vẫn cần observer độc lập.
        started = _parse_time(record.get("started_at_utc"))
        completed = _parse_time(record.get("completed_at_utc"))
        if started is None or completed is None or completed < started:
            error("live verification cần invocation interval RFC3339 hợp lệ")
        if record.get("subject_sha") != record.get("observed_remote_sha"):
            error("live remote SHA phải khớp closing subject")
        publication = record.get("archive_publication")
        if not isinstance(publication, dict):
            error("live verification cần publication đã pin trước query")
        else:
            published = _parse_time(publication.get("published_at_utc"))
            readback = _parse_time(publication.get("readback_completed_at_utc"))
            if published is None or readback is None or readback < published:
                error("archive publication cần publish→readback chronology")
            if started is not None and readback is not None and started < readback:
                error("live invocation bắt đầu trước archive publication/readback")
            if completed is not None and readback is not None and completed < readback:
                error("live verdict hoàn tất trước archive readback")
        files = record.get("files")
        if isinstance(files, list):
            paths: set[str] = set()
            for index, item in enumerate(files):
                if not isinstance(item, dict):
                    error(f"live files[{index}] phải là object")
                    continue
                path = item.get("path")
                if not _safe_relative_path(path):
                    error(f"live files[{index}] path không an toàn")
                elif path in paths:
                    error(f"live files[{index}] path bị lặp")
                else:
                    paths.add(path)
                length = item.get("byte_length")
                if isinstance(length, bool) or not isinstance(length, int) or length < 0:
                    error(f"live files[{index}] byte_length phải là integer không âm")
                digest = item.get("sha256")
                if not isinstance(digest, str) or SHA256.fullmatch(digest) is None:
                    error(f"live files[{index}] sha256 không hợp lệ")

    return errors


def _same_fields(left: dict[str, Any], right: dict[str, Any], fields: tuple[str, ...]) -> bool:
    return all(_typed_equal(left.get(field), right.get(field)) for field in fields)


def _nonblank_text(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


def _validate_commit_snapshot(value: Any, *, owner: str) -> list[str]:
    errors: list[str] = []
    if not isinstance(value, dict):
        return [f"{owner}: Git parents projection root phải là object"]
    expected = {"kind", "sha", "parents", "subject", "body", "changed_paths"}
    if set(value) != expected or value.get("kind") != "git_commit_snapshot":
        errors.append(f"{owner}: Git parents projection thiếu kind/fields hoặc có field lạ")
    if not isinstance(value.get("sha"), str) or GIT_SHA.fullmatch(value["sha"]) is None:
        errors.append(f"{owner}: Git snapshot sha không hợp lệ")
    parents = value.get("parents")
    if not isinstance(parents, list) or any(not isinstance(parent, str) or GIT_SHA.fullmatch(parent) is None for parent in parents):
        errors.append(f"{owner}: Git snapshot parents phải là mảng SHA đầy đủ")
    elif len(parents) != len(set(parents)):
        errors.append(f"{owner}: Git snapshot parents bị lặp")
    if not isinstance(value.get("subject"), str) or not value["subject"]:
        errors.append(f"{owner}: Git snapshot subject rỗng/sai type")
    if not isinstance(value.get("body"), str):
        errors.append(f"{owner}: Git snapshot body phải là string")
    paths = value.get("changed_paths")
    if not isinstance(paths, list) or not paths or any(not _safe_relative_path(path) for path in paths):
        errors.append(f"{owner}: Git snapshot changed_paths phải là mảng path relative an toàn")
    elif len(paths) != len(set(paths)):
        errors.append(f"{owner}: Git snapshot changed_paths bị lặp")
    return errors


def _validate_ci_page(endpoint: Any, value: Any, *, owner: str) -> tuple[list[str], list[dict[str, Any]]]:
    errors: list[str] = []
    if isinstance(value, dict) and value.get("kind") == "github_api_page":
        if value.get("endpoint") != endpoint:
            errors.append(f"{owner}: envelope endpoint không khớp collection page")
        value = value.get("body")
    if endpoint == "commit_statuses":
        if not isinstance(value, list) or any(not isinstance(item, dict) for item in value):
            return [f"{owner}: commit_statuses raw root phải là array of objects"], []
        return [], value
    if endpoint not in {"check_runs", "workflow_runs", "workflow_run_attempt"}:
        return [f"{owner}: CI endpoint không được hỗ trợ"], []
    if not isinstance(value, dict):
        return [f"{owner}: {endpoint} raw root phải là object"], []
    key = {"check_runs": "check_runs", "workflow_runs": "workflow_runs",
           "workflow_run_attempt": "jobs"}[endpoint]
    total = value.get("total_count")
    rows = value.get(key)
    if isinstance(total, bool) or not isinstance(total, int) or total < 0:
        errors.append(f"{owner}: {endpoint}.total_count phải là integer không âm")
    if not isinstance(rows, list) or any(not isinstance(item, dict) for item in rows):
        errors.append(f"{owner}: {endpoint}.{key} phải là array of objects")
        rows = []
    return errors, rows


def _validate_reason_member(
    value: Any,
    *,
    role: str,
    identity: dict[str, Any],
    owner: str,
) -> list[str]:
    errors: list[str] = []
    if not isinstance(value, dict):
        return [f"{owner}: {role} root phải là object"]
    required_by_role = {
        "priority_rationale": {"format_version", "kind", "assignment_ref", "task_id",
                               "task_revision", "delivery_id", "priority", "reason"},
        "shared_invariant": {"format_version", "kind", "assignment_ref", "task_ids", "invariant"},
        "breaking_decision": {"format_version", "kind", "assignment_ref", "task_id",
                              "task_revision", "delivery_id", "approved_decision_ref", "decision"},
        "verified_noop_reason": {"format_version", "kind", "assignment_ref", "task_id",
                                 "task_revision", "delivery_id", "verified_payload_sha", "reason"},
    }
    required = required_by_role.get(role)
    if required is None:
        return [f"{owner}: retained reason role không được hỗ trợ"]
    if set(value) != required or value.get("format_version") != 1 or value.get("kind") != role:
        errors.append(f"{owner}: {role} member thiếu kind/fields hoặc có field lạ")
    identity_fields = {
        "priority_rationale": ("assignment_ref", "task_id", "task_revision", "delivery_id"),
        "shared_invariant": ("assignment_ref",),
        "breaking_decision": ("assignment_ref", "task_id", "task_revision", "delivery_id"),
        "verified_noop_reason": ("assignment_ref", "task_id", "task_revision", "delivery_id"),
    }
    for field in identity_fields[role]:
        if field in identity and not _typed_equal(value.get(field), identity[field]):
            errors.append(f"{owner}: {role} lệch {field}")
    if role == "priority_rationale":
        if value.get("priority") != identity.get("priority"):
            errors.append(f"{owner}: priority rationale lệch priority")
        if not _nonblank_text(value.get("reason")):
            errors.append(f"{owner}: priority rationale cần reason không trắng")
    elif role == "shared_invariant":
        task_ids = value.get("task_ids")
        if not isinstance(task_ids, list) or not all(isinstance(item, str) for item in task_ids):
            errors.append(f"{owner}: shared invariant task_ids sai type")
        elif not _typed_equal(task_ids, identity.get("task_ids")):
            errors.append(f"{owner}: shared invariant task_ids lệch scope")
        if not _nonblank_text(value.get("invariant")):
            errors.append(f"{owner}: shared invariant cần văn bản không trắng")
    elif role == "breaking_decision":
        if not _nonblank_text(value.get("approved_decision_ref")) or not _nonblank_text(value.get("decision")):
            errors.append(f"{owner}: breaking decision cần ref được duyệt và quyết định dạng text")
    elif role == "verified_noop_reason":
        if value.get("verified_payload_sha") != identity.get("verified_payload_sha"):
            errors.append(f"{owner}: no-op reason lệch verified payload SHA")
        if not _nonblank_text(value.get("reason")):
            errors.append(f"{owner}: no-op reason cần văn bản không trắng")
    return errors


def _payload_observation_parent(base_sha: Any, ordered_commits: Any, payload_mode: Any) -> Any:
    """Return the direct parent of the payload SHA represented by an observation."""
    if payload_mode == "verified_noop":
        return base_sha
    if isinstance(ordered_commits, list) and len(ordered_commits) > 1:
        previous_commit = ordered_commits[-2]
        if isinstance(previous_commit, dict):
            return previous_commit.get("sha")
    return base_sha


def _observation_link_errors(
    owner: str,
    observation: dict[str, Any],
    plan: dict[str, Any],
    records: dict[str, tuple[str, dict[str, Any]]],
    raw_members: dict[str, Any],
    raw_member_bytes: dict[str, bytes] | None = None,
    *,
    expected_phase: str,
    expected_parent: str | None,
    expected_identity: dict[str, Any] | None = None,
) -> list[str]:
    """Bind a PASS observation to the frozen plan and its typed source graph."""
    errors: list[str] = []

    def error(message: str) -> None:
        errors.append(f"{owner}: {message}")

    if not isinstance(plan, dict) or not isinstance(observation, dict):
        return [f"{owner}: plan/observation root phải là object"]
    repository = plan.get("repository")
    if not isinstance(repository, dict):
        return [f"{owner}: plan.repository phải là object"]
    try:
        plan_sha = hashlib.sha256(_canonical(plan)).hexdigest()
    except (TypeError, ValueError) as exc:
        return [f"{owner}: check plan không canonical được: {exc}"]
    subject = observation.get("subject_sha")
    if observation.get("phase") != expected_phase:
        error(f"phase phải là {expected_phase}")
    if observation.get("parent_sha") != expected_parent:
        error("parent_sha không khớp phase parent đã chọn")
    if observation.get("check_plan_sha256") != plan_sha:
        error("check_plan_sha256 không khớp plan")
    if observation.get("result") != "PASS":
        error("phase được dùng làm proof đóng task phải có result PASS")
    if observation.get("observed_remote_sha") != subject:
        error("observed_remote_sha không khớp subject")
    if observation.get("binding_verified") is not True or observation.get("required_set_verified") is not True:
        error("PASS cần binding và required-set boolean true")

    def resolve(ref: Any, kind: str, field: str) -> dict[str, Any] | None:
        if not isinstance(ref, str) or ref not in records:
            error(f"{field} trỏ tới evidence không tồn tại")
            return None
        actual_kind, value = records[ref]
        if actual_kind != kind:
            error(f"{field} trỏ sai kind: cần {kind}, nhận {actual_kind}")
            return None
        return value

    expected_checks = plan.get("checks")
    observed_checks = observation.get("checks")
    if not isinstance(expected_checks, list) or not expected_checks:
        error("plan checks rỗng hoặc sai shape")
        expected_checks = []
    if not isinstance(observed_checks, list) or len(observed_checks) != len(expected_checks):
        error("số required checks không khớp plan")
        observed_checks = observed_checks if isinstance(observed_checks, list) else []
    expected_by_id = {
        item.get("id"): item for item in expected_checks
        if isinstance(item, dict) and isinstance(item.get("id"), str)
    }
    observed_by_id: dict[str, dict[str, Any]] = {}
    for index, check in enumerate(observed_checks):
        if not isinstance(check, dict):
            error(f"checks[{index}] không phải object")
            continue
        check_id = check.get("check_id")
        if not isinstance(check_id, str) or check_id in observed_by_id:
            error(f"checks[{index}] thiếu identity hoặc bị lặp")
            continue
        observed_by_id[check_id] = check
    if set(observed_by_id) != set(expected_by_id):
        error("check_id set không đúng bằng frozen required set")

    used_refs: set[str] = set()
    destination = observation.get("destination_observation")
    if not isinstance(destination, dict):
        error("thiếu destination observation của phase")
    else:
        if not _typed_equal(destination.get("repository"), repository):
            error("destination repository không khớp check plan")
        if destination.get("candidate_sha") != subject:
            error("destination candidate_sha không khớp subject")
        if destination.get("identity_verified") is not True or destination.get("visibility_verified") is not True:
            error("destination chưa xác minh repository identity/visibility")
        operation_ref = destination.get("operation_evidence_ref")
        operation = resolve(operation_ref, "GitPushOperationEvidence", "destination.operation_evidence_ref")
        if operation is not None:
            used_refs.add(operation_ref)
            preflight = operation.get("preflight")
            postflight = operation.get("postflight")
            if not isinstance(preflight, dict) or not isinstance(postflight, dict):
                error("push operation preflight/postflight phải là object")
                preflight = preflight if isinstance(preflight, dict) else {}
                postflight = postflight if isinstance(postflight, dict) else {}
            if (operation.get("push_operation_id") != destination.get("push_operation_id")
                    or operation.get("candidate_sha") != subject):
                error("push operation id/candidate không khớp destination")
            if operation.get("credential_class") != plan.get("credential_class"):
                error("push operation credential class không khớp frozen plan")
            if not _typed_equal(postflight.get("repository"), repository):
                error("push postflight repository không khớp check plan")
            if (postflight.get("observed_remote_sha") != subject
                    or preflight.get("candidate_sha") != subject):
                error("push operation không bind preflight/postflight với subject")
            if (preflight.get("remote_config_digest") != destination.get("local_remote_config_digest")
                    or postflight.get("remote_config_digest") != destination.get("local_remote_config_digest")
                    or operation.get("push_started_at_utc") != destination.get("push_started_at_utc")):
                error("destination remote config/push start không khớp push operation")
            if _parse_time(destination.get("observed_at_utc")) != _parse_time(preflight.get("completed_at_utc")):
                error("destination observed_at phải bằng completion của preflight query")
            for query_name, query in (("preflight", preflight), ("postflight", postflight)):
                raw_ref = query.get("raw_api_ref")
                raw_query = raw_members.get(raw_ref) if isinstance(raw_ref, str) else None
                query_fields = {
                    "query_id", "lifecycle_id", "retry_index", "candidate_sha", "repository",
                    "remote_config_digest", "started_at_utc", "completed_at_utc", "observed_remote_sha",
                }
                if not isinstance(raw_query, dict):
                    error(f"{query_name} raw query snapshot phải là object retained")
                elif set(raw_query) != query_fields or not _same_fields(raw_query, query, tuple(query_fields)):
                    error(f"{query_name} raw query snapshot lệch/sai shape so với projection")
            command_ref = operation.get("command_record_ref")
            command = resolve(command_ref, "GitCommandLifecycle", "push.command_record_ref")
            if command is not None:
                used_refs.add(command_ref)
                identity = expected_identity if isinstance(expected_identity, dict) else {}
                for field in ("delivery_id", "task_id", "task_revision", "assignment_ref"):
                    if field in identity and not _typed_equal(command.get(field), identity[field]):
                        error(f"command lifecycle lệch {field}")
                if (command.get("operation_id") != operation.get("push_operation_id")
                        or command.get("lifecycle_id") != operation.get("lifecycle_id")
                        or not _typed_equal(command.get("retry_index"), operation.get("retry_index"))):
                    error("command lifecycle operation/lifecycle/retry không khớp push operation")
                if command.get("delivery_id") != observation.get("delivery_id"):
                    error("common command delivery_id không khớp phase receipt")
                if command.get("command_state") != "SUCCEEDED":
                    error("push PASS cần common command_state SUCCEEDED")
                contract = command.get("contract")
                result = command.get("result")
                if not isinstance(contract, dict) or not isinstance(result, dict):
                    error("common command contract/result phải là object")
                else:
                    argv = contract.get("argv")
                    expected_ref = repository.get("ref")
                    remote_name = repository.get("remote_name")
                    target_refspec = f"{subject}:{expected_ref}" if isinstance(subject, str) and isinstance(expected_ref, str) else None
                    forbidden = {"--force", "-f", "--force-with-lease", "--all", "--mirror", "--tags", "--delete", "--no-verify", "--dry-run"}
                    if (contract.get("executable") != "git" or not isinstance(argv, list)
                            or any(not isinstance(arg, str) or not arg for arg in argv)):
                        error("push command phải pin executable=git và argv dạng string")
                    elif (len(argv) != 4 or argv[0] != "push" or argv[1] != "--porcelain"
                          or argv[2] != remote_name or argv[3] != target_refspec
                          or any(arg in forbidden or arg.startswith("+") for arg in argv)):
                        error("Git argv không phải push thường explicit đúng remote/ref/SHA")
                    if contract.get("deadline_at_utc") != operation.get("deadline_at_utc"):
                        error("command contract deadline khác push lifecycle")
                    command_started = _parse_time(result.get("started_at_utc"))
                    command_completed = _parse_time(result.get("completed_at_utc"))
                    command_deadline = _parse_time(contract.get("deadline_at_utc"))
                    postflight_end = _parse_time(postflight.get("completed_at_utc"))
                    lifecycle_start = _parse_time(operation.get("lifecycle_started_at_utc"))
                    push_start = _parse_time(operation.get("push_started_at_utc"))
                    if (result.get("final_status") != "SUCCEEDED"
                            or result.get("termination_reason") != "completed"
                            or result.get("command_exit_status") != 0
                            or result.get("cleanup_status") not in {"NOT_REQUIRED", "SUCCEEDED"}
                            or command_started is None or command_completed is None
                            or command_deadline is None or postflight_end is None
                            or lifecycle_start is None or command_started < lifecycle_start
                            or command_started != push_start
                            or command_completed < postflight_end
                            or command_completed > command_deadline):
                        error("push command result phải SUCCEEDED/exit0/cleanup và trong deadline")

    collection_ref = observation.get("collection_evidence_ref")
    collection = resolve(collection_ref, "GitCICollectionEvidence", "collection_evidence_ref")
    page_refs: set[str] = set()
    pages: list[dict[str, Any]] = []
    rows_by_endpoint: dict[str, list[dict[str, Any]]] = {}
    rows_by_page_ref: dict[str, list[dict[str, Any]]] = {}
    rows_by_page_group: dict[tuple[Any, ...], list[dict[str, Any]]] = {}
    totals_by_page_group: dict[tuple[Any, ...], set[int]] = {}
    page_envelopes: dict[str, dict[str, Any]] = {}
    workflow_rows: list[dict[str, Any]] = []
    if collection is not None:
        used_refs.add(collection_ref)
        if (collection.get("host") != repository.get("host")
                or collection.get("repository_id") != repository.get("repository_id")
                or collection.get("subject_sha") != subject
                or collection.get("check_plan_sha256") != plan_sha):
            error("CI collection host/repository/subject/plan không khớp")
        raw_pages = collection.get("pages")
        endpoints: set[str] = set()
        page_query_ids: set[str] = set()
        if not isinstance(raw_pages, list) or not raw_pages:
            error("CI collection cần retained API pages")
        else:
            pages = [page for page in raw_pages if isinstance(page, dict)]
            grouped_pages: dict[tuple[Any, ...], list[dict[str, Any]]] = {}
            for page in pages:
                query_id = page.get("query_id")
                if not isinstance(query_id, str):
                    error("CI collection query_id phải là string")
                elif query_id in page_query_ids:
                    error("CI collection query_id bị lặp")
                else:
                    page_query_ids.add(query_id)
                page_ref = page.get("raw_page_ref")
                if isinstance(page_ref, str):
                    page_refs.add(page_ref)
                    used_refs.add(page_ref)
                endpoint = page.get("endpoint")
                if isinstance(endpoint, str):
                    endpoints.add(endpoint)
                raw_page = raw_members.get(page_ref) if isinstance(page_ref, str) else None
                raw_rows: list[dict[str, Any]] = []
                if raw_page is None:
                    error(f"collection page raw member chưa được resolve: {page_ref!r}")
                else:
                    page_errors, raw_rows = _validate_ci_page(endpoint, raw_page, owner=f"collection page {page_ref}")
                    errors.extend(page_errors)
                    if isinstance(raw_page, dict) and raw_page.get("kind") == "github_api_page":
                        expected_fields = {
                            "format_version", "kind", "query_id", "endpoint", "requested_sha",
                            "page_number", "next_page_number", "requested_url", "next_url",
                            "request_started_at_utc", "response_received_at_utc", "http_status",
                            "response_headers", "body_ref", "body_sha256", "body_byte_length", "body",
                        }
                        if set(raw_page) != expected_fields or raw_page.get("format_version") != 1:
                            error(f"collection page {page_ref} envelope thiếu fields hoặc có field lạ")
                        if (raw_page.get("query_id") != page.get("query_id")
                                or raw_page.get("requested_sha") != page.get("requested_sha")
                                or raw_page.get("page_number") != page.get("page_number")
                                or raw_page.get("next_page_number") != page.get("next_page_number")):
                            error(f"collection page {page_ref} envelope lệch query identity/pagination")
                        if raw_page.get("http_status") != 200:
                            error(f"collection page {page_ref} không phải HTTP 200")
                        body_ref = raw_page.get("body_ref")
                        if not _safe_relative_path(body_ref):
                            error(f"collection page {page_ref} body_ref không an toàn")
                        else:
                            used_refs.add(body_ref)
                            if raw_members.get(body_ref) is None or not _typed_equal(
                                    raw_members.get(body_ref), raw_page.get("body")):
                                error(f"collection page {page_ref} parsed body lệch retained raw response")
                        if (not isinstance(raw_page.get("body_sha256"), str)
                                or SHA256.fullmatch(raw_page["body_sha256"]) is None
                                or isinstance(raw_page.get("body_byte_length"), bool)
                                or not isinstance(raw_page.get("body_byte_length"), int)
                                or raw_page["body_byte_length"] < 0):
                            error(f"collection page {page_ref} raw body length/SHA sai shape")
                        request_start = _parse_time(raw_page.get("request_started_at_utc"))
                        response_end = _parse_time(raw_page.get("response_received_at_utc"))
                        if request_start is None or response_end is None or response_end < request_start:
                            error(f"collection page {page_ref} request interval không hợp lệ")
                        collection_start = _parse_time(collection.get("started_at_utc"))
                        collection_end = _parse_time(collection.get("completed_at_utc"))
                        if ((collection_start is not None and request_start is not None and request_start < collection_start)
                                or (collection_end is not None and response_end is not None and response_end > collection_end)):
                            error(f"collection page {page_ref} nằm ngoài collection invocation")
                        headers = raw_page.get("response_headers")
                        if (not isinstance(headers, dict) or any(
                                not isinstance(key, str) or not isinstance(value, str)
                                for key, value in headers.items())):
                            error(f"collection page {page_ref} response_headers sai shape")
                        if not isinstance(raw_page.get("requested_url"), str):
                            error(f"collection page {page_ref} thiếu requested_url")
                        next_url = raw_page.get("next_url")
                        if next_url is not None and not isinstance(next_url, str):
                            error(f"collection page {page_ref} next_url sai type")
                        if page.get("next_page_number") is None and next_url is not None:
                            error(f"collection page cuối vẫn có Link next")
                        if page.get("next_page_number") is not None and next_url is None:
                            error(f"collection page thiếu Link next dù khai báo trang sau")
                        if isinstance(page_ref, str):
                            page_envelopes[page_ref] = raw_page
                        if raw_member_bytes is not None:
                            page_bytes = raw_member_bytes.get(page_ref) if isinstance(page_ref, str) else None
                            body_bytes = raw_member_bytes.get(body_ref) if isinstance(body_ref, str) else None
                            if not isinstance(page_bytes, bytes) or not isinstance(body_bytes, bytes):
                                error(f"collection page {page_ref} thiếu exact envelope/body bytes")
                            else:
                                try:
                                    decoded_page = strict_json_loads(page_bytes)
                                    decoded_body = strict_json_loads(body_bytes)
                                except SchemaError:
                                    error(f"collection page {page_ref} raw envelope/body JSON không strict")
                                else:
                                    if not _typed_equal(decoded_page, raw_page):
                                        error(f"collection page {page_ref} envelope object lệch raw bytes")
                                    if not _typed_equal(decoded_body, raw_page.get("body")):
                                        error(f"collection page {page_ref} body object lệch exact raw bytes")
                                    if (len(body_bytes) != raw_page.get("body_byte_length")
                                            or hashlib.sha256(body_bytes).hexdigest() != raw_page.get("body_sha256")):
                                        error(f"collection page {page_ref} body bytes lệch declared length/SHA")
                    if isinstance(endpoint, str):
                        rows_by_endpoint.setdefault(endpoint, []).extend(raw_rows)
                        if isinstance(page_ref, str):
                            rows_by_page_ref[page_ref] = raw_rows
                if page.get("requested_sha") != subject:
                    error("collection page requested_sha không khớp subject")
                run_id = page.get("workflow_run_id")
                run_attempt = page.get("workflow_run_attempt")
                page_number = page.get("page_number")
                if (not isinstance(endpoint, str)
                        or (run_id is not None and not isinstance(run_id, str))
                        or (run_attempt is not None and (isinstance(run_attempt, bool) or not isinstance(run_attempt, int)))
                        or isinstance(page_number, bool) or not isinstance(page_number, int)):
                    error("CI collection pagination identity/page_number sai type")
                    continue
                if ((endpoint == "workflow_run_attempt" and (run_id is None or run_attempt is None))
                        or (endpoint != "workflow_run_attempt" and (run_id is not None or run_attempt is not None))):
                    error("CI collection run/attempt chỉ được gắn với workflow_run_attempt pages")
                identity = (endpoint, run_id, run_attempt)
                grouped_pages.setdefault(identity, []).append(page)
                rows_by_page_group.setdefault(identity, []).extend(raw_rows)
                if isinstance(raw_page, dict):
                    page_body = raw_page.get("body") if raw_page.get("kind") == "github_api_page" else raw_page
                    total = page_body.get("total_count") if isinstance(page_body, dict) else None
                    if isinstance(total, int) and not isinstance(total, bool):
                        totals_by_page_group.setdefault(identity, set()).add(total)
            for identity, grouped in grouped_pages.items():
                ordered = sorted(grouped, key=lambda item: item.get("page_number", -1))
                numbers = [item.get("page_number") for item in ordered]
                if numbers != list(range(1, len(numbers) + 1)):
                    error(f"collection pagination thiếu/trùng trang cho {identity[0]}")
                for index, page in enumerate(ordered):
                    expected_next = numbers[index + 1] if index + 1 < len(numbers) else None
                    if page.get("next_page_number") != expected_next:
                        error(f"collection next_page_number sai cho {identity[0]}")
                    envelope = page_envelopes.get(page.get("raw_page_ref"))
                    if envelope is not None:
                        following = (page_envelopes.get(ordered[index + 1].get("raw_page_ref"))
                                     if index + 1 < len(ordered) else None)
                        if expected_next is None:
                            if envelope.get("next_url") is not None:
                                error(f"collection {identity[0]} final page có Link next")
                        elif following is None or envelope.get("next_url") != following.get("requested_url"):
                            error(f"collection {identity[0]} Link next không trỏ đúng trang kế tiếp")
            for identity, totals in totals_by_page_group.items():
                if len(totals) != 1:
                    error(f"collection {identity[0]} total_count thay đổi giữa pages")
                elif len(rows_by_page_group.get(identity, [])) != next(iter(totals)):
                    error(f"collection {identity[0]} rows không khớp total_count")
            if "check_runs" not in endpoints:
                error("CI collection thiếu endpoint check_runs")
            actions_checks = [item for item in observed_by_id.values() if item.get("kind") == "check_run"]
            if actions_checks and not {"workflow_runs", "workflow_run_attempt"}.issubset(endpoints):
                error("Actions collection cần workflow_runs và workflow_run_attempt")
            for check in actions_checks:
                if not any(page.get("endpoint") == "workflow_run_attempt"
                           and page.get("workflow_run_id") == check.get("run_id")
                           and page.get("workflow_run_attempt") == check.get("attempt")
                           for page in pages):
                    error(f"check {check.get('check_id')} thiếu page đúng workflow run/attempt")
            if actions_checks:
                workflow_rows = rows_by_endpoint.get("workflow_runs", [])
                checks_by_workflow: dict[tuple[str, str], list[dict[str, Any]]] = {}
                for check in actions_checks:
                    workflow_path = check.get("workflow_path")
                    event = check.get("event")
                    if isinstance(workflow_path, str) and isinstance(event, str):
                        checks_by_workflow.setdefault((workflow_path, event), []).append(check)
                job_candidates: list[dict[str, Any]] = []
                for workflow_identity, workflow_checks in checks_by_workflow.items():
                    workflow_path, event = workflow_identity
                    generations = [
                        {
                            "workflow_identity": row.get("path"),
                            "event": row.get("event"),
                            "subject_sha": row.get("head_sha"),
                            "run_id": row.get("id"),
                            "created_at_utc": row.get("created_at"),
                            "collection_complete": True,
                        }
                        for row in workflow_rows
                        if row.get("path") == workflow_path and row.get("event") == event
                        and row.get("head_sha") == subject
                    ]
                    selected_generation = select_latest_actions_generation(
                        generations, workflow_identity=workflow_path, event=event, subject_sha=subject,
                    )
                    if selected_generation.get("verdict") != "SELECTED":
                        error(f"Actions latest workflow generation chưa xác định: {selected_generation.get('reason')}")
                        continue
                    selected_run_id = str(selected_generation["run"].get("run_id"))
                    for check in workflow_checks:
                        if str(check.get("run_id")) != selected_run_id:
                            error(f"check {check.get('check_id')} không thuộc latest workflow generation")
                    attempt_pages = [
                        page for page in pages
                        if page.get("endpoint") == "workflow_run_attempt"
                        and page.get("workflow_run_id") == selected_run_id
                    ]
                    context_to_check = {
                        check.get("context"): check for check in workflow_checks
                        if isinstance(check.get("context"), str)
                    }
                    for page in attempt_pages:
                        page_ref = page.get("raw_page_ref")
                        for job in rows_by_page_ref.get(page_ref, []):
                            check = context_to_check.get(job.get("name"))
                            if check is None or job.get("head_sha") != subject:
                                continue
                            job_candidates.append({
                                "run_id": selected_run_id,
                                "attempt": page.get("workflow_run_attempt"),
                                "check_identity": check.get("check_id"),
                                "id": job.get("id"),
                            })
                    selected_attempts = select_latest_actions_attempts(
                        job_candidates, selected_run_id=selected_run_id,
                        required_job_identities={
                            check.get("check_id") for check in workflow_checks
                            if isinstance(check.get("check_id"), str)
                        },
                    )
                    if selected_attempts.get("verdict") != "SELECTED":
                        error(f"Actions latest per-job attempt chưa xác định: {selected_attempts.get('reason')}")
                    else:
                        for check in workflow_checks:
                            selected_job = selected_attempts["attempts"].get(check.get("check_id"))
                            if (selected_job is None
                                    or str(selected_job.get("id")) != str(check.get("job_id"))
                                    or selected_job.get("attempt") != check.get("attempt")
                                    or selected_run_id != str(check.get("run_id"))):
                                error(f"check {check.get('check_id')} không phải latest job attempt của latest workflow")
        collection_start = _parse_time(collection.get("started_at_utc"))
        collection_end = _parse_time(collection.get("completed_at_utc"))
        observed_at = _parse_time(observation.get("observed_at_utc"))
        if collection_start is None or collection_end is None or collection_end < collection_start:
            error("CI collection chronology không hợp lệ")
        if observed_at is None or (collection_end is not None and observed_at < collection_end):
            error("observation phải sau khi collection hoàn tất")

    for check_id, expected in expected_by_id.items():
        check = observed_by_id.get(check_id)
        if check is None:
            continue
        expected_fields = ("kind", "context", "source_app", "workflow_path", "job_name", "matrix")
        if not _same_fields(expected, check, expected_fields):
            error(f"check {check_id} native identity khác plan")
        eligible_events = expected.get("eligible_events")
        if not isinstance(eligible_events, list) or check.get("event") not in eligible_events:
            error(f"check {check_id} event không thuộc eligible_events")
        if (check.get("status") != "completed" or check.get("conclusion") != "success"
                or check.get("execution_verified") is not True
                or check.get("reported_head_sha") != subject
                or check.get("tested_checkout_sha") != subject):
            error(f"check {check_id} chưa chứng minh success trên subject checkout")
        source_ref = check.get("source_evidence_ref")
        source = resolve(source_ref, "GitCheckSourceEvidence", f"check {check_id}.source_evidence_ref")
        if source is not None:
            used_refs.add(source_ref)
            source_fields = ("source_app", "context", "workflow_path", "job_name", "matrix", "event", "reported_head_sha")
            if not _same_fields(source, check, source_fields):
                error(f"check {check_id} native source fields lệch observed check")
            if (source.get("run_id") != check.get("run_id") or source.get("job_id") != check.get("job_id")
                    or source.get("attempt") != check.get("attempt")):
                error(f"check {check_id} run/job/attempt lệch native source")
            if source.get("host") != repository.get("host") or source.get("repository_id") != repository.get("repository_id"):
                error(f"check {check_id} source repository identity lệch plan")
            raw_api_ref = source.get("raw_api_ref")
            if isinstance(raw_api_ref, str):
                used_refs.add(raw_api_ref)
                if collection is not None and raw_api_ref not in page_refs:
                    error(f"check {check_id} raw_api_ref không thuộc CI collection pages")
            if source.get("native_status") != "completed" or source.get("native_conclusion") != "success":
                error(f"check {check_id} native source không phải completed/success")
            expected_source_kinds = ({"actions_check_run", "external_check_run"}
                                     if check.get("kind") == "check_run" else {"commit_status"})
            if source.get("source_kind") not in expected_source_kinds:
                error(f"check {check_id} source_kind không khớp native check kind")
            if source.get("workflow_head_sha") != subject or (
                    source.get("job_head_sha") is not None and source.get("job_head_sha") != subject):
                error(f"check {check_id} workflow/job head SHA không khớp subject")
            collection_end = _parse_time(collection.get("completed_at_utc")) if collection is not None else None
            run_created = _parse_time(check.get("run_created_at_utc"))
            if collection_end is not None and run_created is not None and run_created > collection_end:
                error(f"check {check_id} run created sau collection completion")
            source_kind = source.get("source_kind")
            if source_kind in {"actions_check_run", "external_check_run"}:
                raw_api_ref = source.get("raw_api_ref")
                native_row = next((row for row in rows_by_page_ref.get(raw_api_ref, [])
                                   if str(row.get("id")) == source.get("record_id")), None)
                if native_row is None:
                    error(f"check {check_id} raw_api_ref không chứa matching check_run row")
                else:
                    app = native_row.get("app")
                    app_id = app.get("id") if isinstance(app, dict) else None
                    source_app = check.get("source_app")
                    expected_app_id = source_app.rsplit(":", 1)[-1] if isinstance(source_app, str) else None
                    workflow_row = next((row for row in workflow_rows
                                         if str(row.get("id")) == str(check.get("run_id"))), None)
                    if (native_row.get("name") != check.get("context")
                            or native_row.get("head_sha") != subject
                            or native_row.get("status") != "completed"
                            or native_row.get("conclusion") != "success"
                            or str(app_id) != expected_app_id
                            or native_row.get("details_url") != source.get("job_check_run_url")):
                        error(f"check {check_id} raw check_run fields lệch selected source")
                    check_suite = native_row.get("check_suite")
                    native_suite_id = check_suite.get("id") if isinstance(check_suite, dict) else None
                    workflow_suite_id = workflow_row.get("check_suite_id") if workflow_row else None
                    if (native_suite_id is None or workflow_suite_id is None
                            or source.get("check_suite_id") != str(native_suite_id)
                            or str(native_suite_id) != str(workflow_suite_id)):
                        error(f"check {check_id} check_suite_id không khớp raw check_run/workflow_run")
                    try:
                        raw_created = _parse_time(workflow_row.get("created_at")) if workflow_row else None
                        source_created = _parse_time(source.get("created_at_utc"))
                        if raw_created is None or raw_created != source_created:
                            error(f"check {check_id} source creation instant lệch native workflow_run.created_at")
                    except (TypeError, ValueError):
                        error(f"check {check_id} source creation instant sai")
                run_job_match = re.search(
                    r"/actions/runs/([1-9][0-9]*)/job/([1-9][0-9]*)$",
                    source.get("job_check_run_url", "") if isinstance(source.get("job_check_run_url"), str) else "",
                )
                if (run_job_match is None or run_job_match.group(1) != str(check.get("run_id"))
                        or run_job_match.group(2) != str(check.get("job_id"))):
                    error(f"check {check_id} job URL không bind run/job IDs đã chọn")
                matching_native_rows: list[dict[str, Any]] = []
                for row in rows_by_endpoint.get("check_runs", []):
                    row_app = row.get("app")
                    row_app_id = row_app.get("id") if isinstance(row_app, dict) else None
                    details_url = row.get("details_url")
                    match_url = re.search(
                        r"/actions/runs/([1-9][0-9]*)/job/([1-9][0-9]*)$",
                        details_url if isinstance(details_url, str) else "",
                    )
                    row_workflow = next(
                        (item for item in workflow_rows
                         if match_url is not None and str(item.get("id")) == match_url.group(1)),
                        None,
                    )
                    if (row.get("name") == check.get("context") and row.get("head_sha") == subject
                            and str(row_app_id) == expected_app_id and row_workflow is not None
                            and row_workflow.get("path") == check.get("workflow_path")
                            and row_workflow.get("event") == check.get("event")):
                        matching_native_rows.append({
                            "source_app": source_app, "context": check.get("context"),
                            "workflow_path": check.get("workflow_path"), "job_name": check.get("job_name"),
                            "event": check.get("event"), "subject_sha": subject,
                            "id": row.get("id"), "created_at_utc": row_workflow.get("created_at"),
                            "status": row.get("status"), "conclusion": row.get("conclusion"),
                            "sha": row.get("head_sha"),
                        })
                latest_native = select_latest_native_records(matching_native_rows, source_kind="check_run")
                if latest_native.get("verdict") != "SELECTED":
                    error(f"check {check_id} latest native check-run chưa xác định: {latest_native.get('reason')}")
                elif (not latest_native.get("records")
                      or str(next(iter(latest_native["records"].values())).get("id")) != str(source.get("record_id"))):
                    error(f"check {check_id} selected source không phải latest native check-run")
                workflow_row = next((row for row in workflow_rows
                                     if str(row.get("id")) == check.get("run_id")), None)
                if workflow_row is None:
                    error(f"check {check_id} không có matching raw workflow_run row")
                elif (workflow_row.get("head_sha") != subject
                      or workflow_row.get("event") != check.get("event")
                      or workflow_row.get("status") != "completed"
                      or workflow_row.get("conclusion") != "success"
                      or workflow_row.get("run_attempt") != check.get("attempt")
                      or workflow_row.get("path") != check.get("workflow_path")):
                    error(f"check {check_id} raw workflow_run fields lệch selected attempt")
                job_page = next((page for page in pages if page.get("endpoint") == "workflow_run_attempt"
                                 and page.get("workflow_run_id") == check.get("run_id")
                                 and page.get("workflow_run_attempt") == check.get("attempt")), None)
                job_page_ref = job_page.get("raw_page_ref") if job_page is not None else None
                job_row = next((row for row in rows_by_page_ref.get(job_page_ref, [])
                                if str(row.get("id")) == check.get("job_id")), None)
                if job_row is None:
                    error(f"check {check_id} không có matching raw job row")
                elif (job_row.get("name") != check.get("context")
                      or job_row.get("head_sha") != subject
                      or job_row.get("conclusion") != "success"):
                    error(f"check {check_id} raw job fields lệch selected execution")
                elif (str(job_row.get("run_id")) != str(check.get("run_id"))
                      or (job_row.get("run_attempt") is not None
                          and job_row.get("run_attempt") != check.get("attempt"))):
                    error(f"check {check_id} raw job run/attempt không khớp endpoint scope")
                elif native_row is not None:
                    job_check_url = job_row.get("check_run_url")
                    if (str(job_row.get("id")) != source.get("record_id")
                            or job_check_url != native_row.get("url")
                            or not _check_run_api_url_matches(
                                job_check_url, host=repository.get("host"),
                                owner=repository.get("owner"), name=repository.get("name"),
                                record_id=source.get("record_id"))
                            or job_row.get("html_url") != source.get("job_check_run_url")):
                        error(f"check {check_id} job.check_run_url không bind native check ID/host/repository")
        execution_ref = check.get("execution_evidence_ref")
        execution = resolve(execution_ref, "GitCheckExecutionEvidence", f"check {check_id}.execution_evidence_ref")
        if execution is not None:
            used_refs.add(execution_ref)
            if (execution.get("check_id") != check_id or execution.get("subject_sha") != subject
                    or execution.get("tested_checkout_sha") != subject
                    or execution.get("source_evidence_ref") != source_ref):
                error(f"check {check_id} execution identity/source/SHA lệch")
            commands = expected.get("command_refs")
            results = execution.get("command_results")
            result_rows = [row for row in results if isinstance(row, dict)] if isinstance(results, list) else []
            by_command = {row.get("command_ref"): row for row in result_rows if isinstance(row.get("command_ref"), str)}
            if (not isinstance(commands, list) or any(not isinstance(item, str) for item in commands)
                    or len(result_rows) != len(by_command) or set(by_command) != set(commands)):
                error(f"check {check_id} execution command set không đúng plan")
            for command in commands if isinstance(commands, list) else []:
                result = by_command.get(command)
                if (result is None or result.get("revision") != subject or result.get("result") != "PASS"):
                    error(f"check {check_id} command chưa PASS trên subject: {command}")
                elif isinstance(result.get("log_ref"), str):
                    used_refs.add(result["log_ref"])
        evidence_refs = check.get("evidence_refs")
        expected_evidence_refs = {item for item in (source_ref, execution_ref) if isinstance(item, str)}
        if source is not None and isinstance(source.get("raw_api_ref"), str):
            expected_evidence_refs.add(source["raw_api_ref"])
            raw_page = raw_members.get(source["raw_api_ref"])
            if isinstance(raw_page, dict) and raw_page.get("kind") == "github_api_page":
                body_ref = raw_page.get("body_ref")
                if isinstance(body_ref, str):
                    expected_evidence_refs.add(body_ref)
        if execution is not None and isinstance(execution.get("command_results"), list):
            expected_evidence_refs.update(
                row["log_ref"] for row in execution["command_results"]
                if isinstance(row, dict) and isinstance(row.get("log_ref"), str)
            )
        if (not isinstance(evidence_refs, list)
                or any(not isinstance(item, str) for item in evidence_refs)
                or len(evidence_refs) != len(set(item for item in evidence_refs if isinstance(item, str)))
                or set(item for item in evidence_refs if isinstance(item, str)) != expected_evidence_refs):
            error(f"check {check_id} evidence_refs phải khớp source/execution/raw page/body/log members")
        else:
            used_refs.update(evidence_refs)

    return errors + [f"{owner}:required-member:{ref}" for ref in sorted(used_refs) if not _safe_relative_path(ref)]


def validate_delivery_links(objects: Any, *, raw_members: Any = None, raw_member_bytes: Any = None) -> dict[str, Any]:
    """Evaluate typed delivery links without promoting static records to DONE.

    ``objects`` maps private relative evidence refs to ``{kind, value}`` entries.
    GitHub API freshness, archive byte readback and secured caller identity remain
    external observer responsibilities, so a clean result is still UNKNOWN.
    """
    if not isinstance(objects, dict):
        return {"verdict": "REJECT", "errors": ["bundle.objects phải là object ref→typed record"], "unknowns": []}
    if raw_members is None:
        raw_members = {}
    if not isinstance(raw_members, dict):
        return {"verdict": "REJECT", "errors": ["bundle.raw_members phải là object ref→decoded JSON"], "unknowns": []}
    if raw_member_bytes is not None and not isinstance(raw_member_bytes, dict):
        return {"verdict": "REJECT", "errors": ["bundle.raw_member_bytes phải là object ref→bytes"], "unknowns": []}
    if isinstance(raw_member_bytes, dict) and any(
            not _safe_relative_path(ref) or not isinstance(data, bytes)
            for ref, data in raw_member_bytes.items()):
        return {"verdict": "REJECT", "errors": ["bundle.raw_member_bytes chứa locator không an toàn hoặc giá trị non-bytes"], "unknowns": []}
    records: dict[str, tuple[str, dict[str, Any]]] = {}
    errors: list[str] = []
    for raw_ref in raw_members:
        if not _safe_relative_path(raw_ref):
            errors.append(f"Raw evidence ref không phải locator relative an toàn: {raw_ref!r}")
    unknowns = ["Static evaluator không query GitHub và không tự xác minh byte readback của archive"]
    from .schema import SchemaError, extract_governance_schema, validate_schema
    spec_path = Path(__file__).resolve().parents[2] / "Maps-Context-Engine-4D_SPEC.md"
    try:
        root_schema = extract_governance_schema(spec_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, SchemaError) as exc:
        return {"verdict": "REJECT", "errors": [f"Không nạp được schema S21: {exc}"], "unknowns": []}
    for ref, entry in objects.items():
        if not _safe_relative_path(ref):
            errors.append(f"Evidence ref không phải locator relative an toàn: {ref!r}")
            continue
        if not isinstance(entry, dict) or not isinstance(entry.get("kind"), str) or not isinstance(entry.get("value"), dict):
            errors.append(f"Evidence entry phải có kind và object value: {ref}")
            continue
        kind, value = entry["kind"], entry["value"]
        definition = root_schema.get("$defs", {}).get(kind)
        if not isinstance(definition, dict):
            errors.append(f"{ref}: kind governance không được khai báo: {kind}")
            continue
        for schema_error in validate_schema(value, definition, root_schema=root_schema):
            errors.append(f"{ref}: {schema_error}")
        errors.extend(f"{ref}: {semantic_error}" for semantic_error in validate_semantics(kind, value))
        records[ref] = (kind, value)

    def resolve(ref: Any, expected_kind: str, owner: str) -> dict[str, Any] | None:
        if not isinstance(ref, str) or not ref:
            errors.append(f"{owner} thiếu reference tới {expected_kind}")
            return None
        target = records.get(ref)
        if target is None:
            errors.append(f"{owner} trỏ tới evidence không tồn tại: {ref}")
            return None
        kind, value = target
        if kind != expected_kind:
            errors.append(f"{owner} trỏ sai kind: cần {expected_kind}, nhận {kind}")
            return None
        return value

    def record_for(ref: Any) -> tuple[str, dict[str, Any]] | None:
        """Look up a typed reference only after confirming it can be a map key."""
        return records.get(ref) if isinstance(ref, str) else None

    selection_records = {
        ref: value for ref, (kind, value) in records.items()
        if kind == "GitTaskDeliverySelection"
    }
    selection_versions: dict[tuple[Any, Any, int], str] = {}
    for selection_ref, selection in selection_records.items():
        revision = selection.get("task_revision")
        task_id, assignment = selection.get("task_id"), selection.get("assignment_ref")
        if (isinstance(revision, int) and not isinstance(revision, bool)
                and isinstance(task_id, str) and isinstance(assignment, str)):
            key = (task_id, assignment, revision)
            prior_ref = selection_versions.get(key)
            if prior_ref is not None:
                errors.append(f"selection revision bị trùng/conflict: {prior_ref} và {selection_ref}")
            selection_versions[key] = selection_ref

    def validate_selection(ref: str, selection: dict[str, Any]) -> None:
        task_id = selection.get("task_id")
        assignment = selection.get("assignment_ref")
        revision = selection.get("task_revision")
        active = selection.get("active_delivery_id")
        previous_ref = selection.get("previous_selection_ref")
        invalidations = selection.get("invalidation_refs")
        if isinstance(revision, bool) or not isinstance(revision, int) or revision < 1:
            return  # S21 schema validation above supplies the typed failure.
        if revision == 1:
            if previous_ref is not None or selection.get("supersedes_delivery_id") is not None:
                errors.append(f"{ref}: revision 1 không được có previous/supersedes selection")
            if invalidations != []:
                errors.append(f"{ref}: revision 1 không được có invalidation refs")
            return
        if not isinstance(previous_ref, str) or not previous_ref:
            errors.append(f"{ref}: revision sau 1 cần previous_selection_ref")
            return
        previous_entry = record_for(previous_ref)
        if previous_entry is None or previous_entry[0] != "GitTaskDeliverySelection":
            errors.append(f"{ref}: previous selection phải resolve retained typed selection")
            return
        previous = previous_entry[1]
        old_active = previous.get("active_delivery_id")
        if (previous.get("task_id") != task_id or previous.get("assignment_ref") != assignment
                or isinstance(previous.get("task_revision"), bool)
                or previous.get("task_revision") != revision - 1):
            errors.append(f"{ref}: previous selection phải cùng task/assignment và revision liền trước")
        if not isinstance(old_active, str) or not old_active:
            errors.append(f"{ref}: previous selection phải giữ active delivery đã supersede")
        if selection.get("supersedes_delivery_id") != old_active:
            errors.append(f"{ref}: supersedes_delivery_id phải bằng active delivery trước")
        if active is not None and active == old_active:
            errors.append(f"{ref}: revision mới phải cấp UUID khác selection trước")
        if not isinstance(invalidations, list) or not invalidations:
            errors.append(f"{ref}: supersede cần ít nhất một retained invalidation reason")
            return
        if any(not isinstance(item, str) or not _safe_relative_path(item) for item in invalidations):
            errors.append(f"{ref}: invalidation refs phải là locator relative an toàn")
            return
        if len(invalidations) != len(set(invalidations)):
            errors.append(f"{ref}: invalidation refs bị lặp")
        if selection.get("reason_ref") not in invalidations:
            errors.append(f"{ref}: reason_ref phải là một invalidation reason đã giữ lại")
        prior_updated = _parse_time(previous.get("updated_at_utc"))
        allocated = _parse_time(selection.get("allocated_at_utc"))
        updated = _parse_time(selection.get("updated_at_utc"))
        if prior_updated is None or updated is None or updated < prior_updated:
            errors.append(f"{ref}: selection history có thời gian cập nhật sai thứ tự")
        if active is not None and (allocated is None or (prior_updated is not None and allocated < prior_updated)):
            errors.append(f"{ref}: allocation mới phải sau snapshot selection trước")
        for reason_ref in invalidations:
            reason = raw_members.get(reason_ref)
            if not isinstance(reason, dict):
                errors.append(f"{ref}: invalidation reason thiếu retained object tại {reason_ref}")
                continue
            if (reason.get("kind") != "delivery_selection_invalidation"
                    or reason.get("format_version") != 1
                    or reason.get("task_id") != task_id
                    or isinstance(reason.get("previous_task_revision"), bool)
                    or reason.get("previous_task_revision") != revision - 1
                    or isinstance(reason.get("task_revision"), bool)
                    or reason.get("task_revision") != revision
                    or reason.get("previous_delivery_id") != old_active
                    or reason.get("active_delivery_id") != active
                    or not _nonblank_text(reason.get("reason"))
                    or not isinstance(reason.get("invalidated_refs"), list)
                    or not reason.get("invalidated_refs")):
                errors.append(f"{ref}: invalidation reason {reason_ref} sai identity/shape")
            invalidated_refs = reason.get("invalidated_refs")
            if isinstance(invalidated_refs, list):
                if (any(not isinstance(item, str) or not _safe_relative_path(item)
                        or (item not in raw_members and item not in records) for item in invalidated_refs)
                        or len(invalidated_refs) != len(set(item for item in invalidated_refs if isinstance(item, str)))):
                    errors.append(f"{ref}: invalidated refs phải là unique retained raw/typed members")
            reason_time = _parse_time(reason.get("recorded_at_utc"))
            if reason_time is None or (allocated is not None and reason_time > allocated):
                errors.append(f"{ref}: invalidation reason phải được ghi trước allocation revision mới")

    for selection_ref, selection in selection_records.items():
        validate_selection(selection_ref, selection)

    def require_selection_history(selection_ref: Any, required_members: set[str]) -> None:
        visited: set[str] = set()
        current_ref = selection_ref
        for _ in range(109):
            if not isinstance(current_ref, str):
                return
            if current_ref in visited:
                errors.append(f"selection history loop tại {current_ref}")
                return
            visited.add(current_ref)
            required_members.add(current_ref)
            selection_entry = record_for(current_ref)
            if selection_entry is None or selection_entry[0] != "GitTaskDeliverySelection":
                return
            selection = selection_entry[1]
            invalidations = selection.get("invalidation_refs")
            if isinstance(invalidations, list):
                for reason_ref in invalidations:
                    if isinstance(reason_ref, str) and _safe_relative_path(reason_ref):
                        required_members.add(reason_ref)
            current_ref = selection.get("previous_selection_ref")
            if current_ref is None:
                return
        errors.append(f"selection history vượt giới hạn 109 revision tại {selection_ref}")

    def same_identity(left: dict[str, Any], right: dict[str, Any], owner: str) -> None:
        fields = ("delivery_id", "task_id", "task_revision", "assignment_ref")
        for field in fields:
            if field in left and field in right:
                if not _typed_equal(left.get(field), right.get(field)):
                    errors.append(f"{owner} lệch {field}")

    def validate_commit_record(
        commit_value: dict[str, Any],
        *,
        phase: str,
        owner: str,
        identity: dict[str, Any],
        expected_sha: Any,
        expected_parent: Any,
        check_parent: bool = True,
    ) -> None:
        historical_noop = phase == "payload" and identity.get("payload_mode") == "verified_noop"
        scope_ref = commit_value.get("scope_ref")
        scope = resolve(scope_ref, "GitCommitScopeEvidence", owner + ".scope_ref")
        snapshot_ref = commit_value.get("git_evidence_ref")
        if not isinstance(snapshot_ref, str) or snapshot_ref not in archive_paths_for_validation:
            errors.append(f"{owner}: Git snapshot phải là raw retained archive member")
        snapshot = raw_members.get(snapshot_ref) if isinstance(snapshot_ref, str) else None
        errors.extend(_validate_commit_snapshot(snapshot, owner=owner + ".git_evidence_ref"))
        if isinstance(snapshot, dict):
            for field in ("sha", "subject", "body"):
                if not _typed_equal(snapshot.get(field), commit_value.get(field)):
                    errors.append(f"{owner}: wrapper GitCommit {field} lệch actual Git snapshot")
            if expected_sha is not None and snapshot.get("sha") != expected_sha:
                errors.append(f"{owner}: actual snapshot SHA lệch expected commit")
            parents = snapshot.get("parents")
            wanted_parents = [] if expected_parent is None else [expected_parent]
            if check_parent and isinstance(parents, list) and parents != wanted_parents:
                errors.append(f"{owner}: actual Git parents không khớp ordered ancestry")
        if scope is None:
            return
        scope_phase = scope.get("phase")
        if (scope_phase != phase
                and not (historical_noop and scope_phase in {"payload", "closing"})):
            errors.append(f"{owner}: commit scope phase không khớp")
        task_ids = scope.get("task_ids")
        task_id = identity.get("task_id")
        if (not isinstance(task_ids, list) or not task_ids
                or (historical_noop and len(task_ids) != 1)
                or (not historical_noop and task_id not in task_ids)):
            errors.append(f"{owner}: commit scope phải chứa task identity")
        if isinstance(task_ids, list) and len(task_ids) > 1:
            if not isinstance(scope.get("shared_invariant_ref"), str):
                errors.append(f"{owner}: multi-task commit cần shared invariant retained")
        if scope.get("priority") not in {"P0", "P1", "P2", "P3"}:
            errors.append(f"{owner}: commit scope priority không hợp lệ")
        subject = commit_value.get("subject")
        scope_task = task_ids[0] if isinstance(task_ids, list) and task_ids else task_id
        if (not isinstance(subject, str) or f"[{scope.get('priority')}]" not in subject
                or f"[{scope_task}]" not in subject):
            errors.append(f"{owner}: actual commit subject phải khớp task/priority của scope")
        if isinstance(snapshot, dict) and not _typed_equal(scope.get("changed_paths"), snapshot.get("changed_paths")):
            errors.append(f"{owner}: scope changed_paths lệch actual Git snapshot")
        priority_ref = scope.get("priority_reason_ref")
        if isinstance(priority_ref, str):
            priority_identity = {"priority": scope.get("priority")} if historical_noop else dict(identity)
            priority_identity["priority"] = scope.get("priority")
            reason = raw_members.get(priority_ref)
            errors.extend(_validate_reason_member(reason, role="priority_rationale",
                                                  identity=priority_identity, owner=owner + ".priority_reason"))
            if (historical_noop and isinstance(reason, dict)
                    and reason.get("task_id") not in task_ids):
                errors.append(f"{owner}: historical priority rationale lệch task trong scope")
        elif scope.get("priority") != identity.get("priority"):
            if not historical_noop:
                errors.append(f"{owner}: priority khác task cần priority rationale retained")
        shared_ref = scope.get("shared_invariant_ref")
        if isinstance(shared_ref, str):
            shared_identity = {"task_ids": task_ids} if historical_noop else dict(identity)
            shared_identity["task_ids"] = task_ids
            errors.extend(_validate_reason_member(raw_members.get(shared_ref), role="shared_invariant",
                                                  identity=shared_identity, owner=owner + ".shared_invariant"))
        breaking_ref = scope.get("breaking_decision_ref")
        is_breaking = (isinstance(subject, str) and re.match(
            r"^(?:feat|fix|test|docs|build|refactor|chore|ci|perf|revert)(?:\([a-z0-9._/-]+\))?!:",
            subject,
        ) is not None) or any(
            line.startswith("BREAKING CHANGE:") for line in str(commit_value.get("body", "")).splitlines()
        )
        if isinstance(breaking_ref, str):
            breaking_identity = {} if historical_noop else identity
            errors.extend(_validate_reason_member(raw_members.get(breaking_ref), role="breaking_decision",
                                                  identity=breaking_identity, owner=owner + ".breaking_decision"))
        elif is_breaking:
            errors.append(f"{owner}: breaking commit cần breaking decision retained")
        from tools.ci.delivery_policy import validate_commit_message
        message_phase = scope_phase if historical_noop else phase
        message_task = scope_task if historical_noop else task_id
        message_priority = scope.get("priority")
        message_errors = validate_commit_message(
            str(subject or "") + "\n\n" + str(commit_value.get("body", "")),
            phase=message_phase, task_id=message_task, priority=message_priority,
        )
        errors.extend(f"{owner}: commit message: {message_error}" for message_error in message_errors)
        if historical_noop:
            if identity.get("base_sha") != commit_value.get("sha"):
                errors.append(f"{owner}: verified_noop payload P phải bằng base_sha")
            if scope_phase == "closing":
                old_delivery = next((line.partition(":")[2].strip()
                                     for line in str(commit_value.get("body", "")).splitlines()
                                     if line.startswith("MCE4D-Delivery-ID:")), None)
                if old_delivery == identity.get("delivery_id"):
                    errors.append(f"{owner}: historical closing P phải giữ UUID khác delivery hiện tại")
        if phase == "closing" and (not isinstance(snapshot, dict)
                                    or snapshot.get("changed_paths") != ["Maps-Context-Engine-4D_SPEC.md"]):
            errors.append(f"{owner}: closing snapshot chỉ được đổi leaf của SPEC")

    archive_paths_for_validation: set[str] = set()
    for entry in records.values():
        if entry[0] == "GitEvidenceArchive":
            files = entry[1].get("files")
            if isinstance(files, list):
                archive_paths_for_validation.update(
                    item["path"] for item in files
                    if isinstance(item, dict) and isinstance(item.get("path"), str)
                )

    attempt_refs: dict[str, dict[str, Any]] = {}
    for ref, (kind, attempt) in records.items():
        if kind != "GitDeliveryAttempt":
            continue
        attempt_refs[ref] = attempt
        try:
            parsed_delivery_id = UUID(attempt.get("delivery_id", ""))
            if str(parsed_delivery_id) != attempt.get("delivery_id"):
                raise ValueError("UUID không ở dạng lowercase chuẩn")
        except (ValueError, TypeError, AttributeError):
            errors.append(f"{ref}: delivery_id không phải UUID")
        if attempt.get("closing_marker") != f"MCE4D-Delivery-ID: {attempt.get('delivery_id')}":
            errors.append(f"{ref}: closing_marker không khớp delivery_id")
        if attempt.get("payload_mode") == "verified_noop":
            noop_ref = attempt.get("noop_reason_ref")
            noop_identity = dict(attempt)
            noop_identity["verified_payload_sha"] = attempt.get("verified_payload_sha")
            errors.extend(_validate_reason_member(
                raw_members.get(noop_ref) if isinstance(noop_ref, str) else None,
                role="verified_noop_reason", identity=noop_identity, owner=ref + ".noop_reason_ref",
            ))
        elif attempt.get("noop_reason_ref") is not None:
            errors.append(f"{ref}: payload_mode=changes không được trỏ verified_noop_reason")
        plan = resolve(attempt.get("check_plan_ref"), "GitCheckPlan", ref)
        if plan is not None:
            try:
                digest = hashlib.sha256(_canonical(plan)).hexdigest()
                if digest != attempt.get("check_plan_sha256"):
                    errors.append(f"{ref}: check_plan_sha256 không khớp plan bytes canonical")
            except (TypeError, ValueError) as exc:
                errors.append(f"{ref}: check plan không canonical được: {exc}")
            expected_canary = f"delivery:{attempt.get('delivery_id')}:payload"
            if plan.get("canary_ref") != expected_canary:
                errors.append(f"{ref}: check-plan canary_ref không bind delivery payload")
            planned_local = plan.get("local_check_refs")
            attempt_local = attempt.get("local_evidence_refs")
            if not isinstance(planned_local, list) or not isinstance(attempt_local, list):
                errors.append(f"{ref}: local evidence refs phải là mảng")
            elif (any(not isinstance(item, str) for item in planned_local + attempt_local)
                  or len(planned_local) != len(set(planned_local))
                  or len(attempt_local) != len(set(attempt_local))
                  or set(planned_local) != set(attempt_local)):
                errors.append(f"{ref}: local evidence refs phải khớp đúng check plan")
            else:
                planned_commands = [
                    command
                    for check in plan.get("checks", [])
                    if isinstance(check, dict) and isinstance(check.get("command_refs"), list)
                    for command in check["command_refs"]
                    if isinstance(command, str)
                ]
                required_local_commands = set(planned_commands)
                observed_local_commands: set[str] = set()
                for local_ref in attempt_local:
                    local = resolve(local_ref, "GitLocalCheckEvidence", ref + ".local_evidence_refs")
                    if local is None:
                        continue
                    if (local.get("task_id") != attempt.get("task_id")
                            or not _typed_equal(local.get("task_revision"), attempt.get("task_revision"))
                            or local.get("delivery_id") != attempt.get("delivery_id")
                            or local.get("phase") != "payload"
                            or local.get("revision") != attempt.get("verified_payload_sha")
                            or local.get("result") != "PASS"
                            or local.get("execution_verified") is not True):
                        errors.append(f"{ref}: local evidence {local_ref} không bind P/task/delivery/PASS")
                    if local.get("command_ref") not in planned_commands:
                        errors.append(f"{ref}: local evidence {local_ref} command ngoài check plan")
                    local_command = local.get("command_ref")
                    if isinstance(local_command, str):
                        if local_command in observed_local_commands:
                            errors.append(f"{ref}: local evidence lặp command {local_command}")
                        observed_local_commands.add(local_command)
                if observed_local_commands != required_local_commands:
                    errors.append(f"{ref}: local evidence command set phải khớp mọi command duy nhất của plan")
            if isinstance(planned_local, list) and isinstance(attempt_local, list):
                closing_reuse = plan.get("closing_local_reuse_refs", [])
                if not isinstance(closing_reuse, list) or any(
                        not isinstance(item, str) or item not in attempt_local for item in closing_reuse):
                    errors.append(f"{ref}: closing_local_reuse_refs phải thuộc local evidence đã lưu")
        if attempt.get("phase") == "closing_proposed":
            payload = resolve(attempt.get("payload_observation_ref"), "GitDeliveryObservation", ref)
            verified_payload_sha = attempt.get("verified_payload_sha")
            commits = attempt.get("ordered_commits")
            if not isinstance(commits, list) or not commits:
                errors.append(f"{ref}: closing_proposed cần ordered payload commits")
            else:
                commit_shas = [item.get("sha") for item in commits if isinstance(item, dict)]
                if not commit_shas or commit_shas[-1] != verified_payload_sha:
                    errors.append(f"{ref}: ordered payload cuối phải bằng verified_payload_sha")
                if (any(not isinstance(sha, str) for sha in commit_shas)
                        or len(commit_shas) != len(set(sha for sha in commit_shas if isinstance(sha, str)))):
                    errors.append(f"{ref}: ordered payload SHA thiếu/sai type hoặc bị lặp")
                for commit_item in commits:
                    if not isinstance(commit_item, dict):
                        continue
                    subject = commit_item.get("subject")
                    if (attempt.get("payload_mode") != "verified_noop"
                            and (not isinstance(subject, str)
                                 or f"[{attempt.get('priority')}]" not in subject
                                 or f"[{attempt.get('task_id')}]" not in subject)):
                        errors.append(f"{ref}: ordered payload commit phải bind priority/task")
                    body = commit_item.get("body")
                    if (attempt.get("payload_mode") != "verified_noop" and isinstance(body, str)
                            and any(line.startswith("MCE4D-") for line in body.splitlines())):
                        errors.append(f"{ref}: payload commit không được có closing trailers")
                payload_parent = attempt.get("base_sha")
                for commit_index, commit_item in enumerate(commits):
                    if isinstance(commit_item, dict):
                        validate_commit_record(
                            commit_item, phase="payload", owner=f"{ref}.ordered_commits[{commit_index}]",
                            identity=attempt, expected_sha=commit_item.get("sha"), expected_parent=payload_parent,
                            check_parent=attempt.get("payload_mode") != "verified_noop",
                        )
                        if attempt.get("payload_mode") != "verified_noop":
                            payload_parent = commit_item.get("sha")
            if attempt.get("closing_parent_sha") != verified_payload_sha:
                errors.append(f"{ref}: closing parent phải bằng P")
            if payload is not None:
                same_identity(attempt, payload, ref + ".payload")
                if payload.get("phase") != "payload":
                    errors.append(f"{ref}: payload_observation_ref trỏ sai phase")
                if payload.get("subject_sha") != verified_payload_sha:
                    errors.append(f"{ref}: payload observation subject không bằng P")
                if payload.get("check_plan_sha256") != attempt.get("check_plan_sha256"):
                    errors.append(f"{ref}: payload observation dùng check plan khác")
                if payload.get("result") != "PASS" or payload.get("observed_remote_sha") != verified_payload_sha:
                    errors.append(f"{ref}: payload observation chưa chứng minh P đã push và required checks PASS")
                if payload.get("check_plan_sha256") != attempt.get("check_plan_sha256"):
                    errors.append(f"{ref}: payload observation plan hash lệch attempt")
                if plan is not None:
                    errors.extend(_observation_link_errors(
                        ref + ".payload", payload, plan, records, raw_members,
                        raw_member_bytes=raw_member_bytes,
                        expected_phase="payload",
                        expected_parent=_payload_observation_parent(
                            attempt.get("base_sha"), commits, attempt.get("payload_mode"),
                        ),
                        expected_identity=attempt,
                    ))
            if not isinstance(attempt.get("closing_marker"), str) or not attempt["closing_marker"].strip():
                errors.append(f"{ref}: closing marker rỗng")
        elif attempt.get("phase") == "payload_pending":
            if attempt.get("verified_payload_sha") is not None or attempt.get("payload_observation_ref") is not None:
                errors.append(f"{ref}: payload_pending không được tự ghi P hoặc observation tương lai")
        else:
            errors.append(f"{ref}: phase không hợp lệ")

    archive_refs: dict[str, dict[str, Any]] = {}
    for ref, (kind, archive) in records.items():
        if kind != "GitEvidenceArchive":
            continue
        archive_refs[ref] = archive
        attempt = resolve(archive.get("attempt_ref"), "GitDeliveryAttempt", ref)
        selection = resolve(archive.get("selection_ref"), "GitTaskDeliverySelection", ref)
        payload = resolve(archive.get("payload_observation_ref"), "GitDeliveryObservation", ref)
        closing = resolve(archive.get("closing_observation_ref"), "GitDeliveryObservation", ref)
        commit = resolve(archive.get("closing_commit_ref"), "GitCommit", ref)
        plan = resolve(attempt.get("check_plan_ref"), "GitCheckPlan", ref) if attempt is not None else None
        members = archive.get("files")
        member_paths = {
            item.get("path") for item in members
            if isinstance(item, dict) and isinstance(item.get("path"), str)
        } if isinstance(members, list) else set()
        for pointer_field in ("attempt_ref", "selection_ref", "payload_observation_ref",
                              "closing_observation_ref", "closing_commit_ref", "git_evidence_ref"):
            pointer = archive.get(pointer_field)
            if not isinstance(pointer, str) or pointer not in member_paths:
                errors.append(f"{ref}: {pointer_field} không thuộc member set của archive")
        if attempt is not None:
            same_identity(archive, attempt, ref + ".attempt")
            if attempt.get("phase") != "closing_proposed":
                errors.append(f"{ref}: archive chỉ đóng được attempt closing_proposed")
            if archive.get("verified_payload_sha") != attempt.get("verified_payload_sha"):
                errors.append(f"{ref}: archive payload SHA lệch attempt")
            if archive.get("check_plan_sha256") != attempt.get("check_plan_sha256"):
                errors.append(f"{ref}: archive plan SHA lệch attempt")
            ordered_payload = attempt.get("ordered_commits")
            if any(item.get("sha") == archive.get("closing_sha")
                   for item in (ordered_payload if isinstance(ordered_payload, list) else [])
                   if isinstance(item, dict)):
                errors.append(f"{ref}: closing SHA không được nằm trong ordered payload commits")
        if selection is not None:
            for field in ("task_id", "task_revision", "assignment_ref"):
                if not _typed_equal(archive.get(field), selection.get(field)):
                    errors.append(f"{ref}.selection lệch {field}")
            if not _typed_equal(selection.get("active_delivery_id"), archive.get("delivery_id")):
                errors.append(f"{ref}: selector active_delivery_id không khớp archive")
            revision = selection.get("task_revision")
            if isinstance(revision, int) and not isinstance(revision, bool):
                latest = max((candidate.get("task_revision", 0)
                              for candidate in selection_records.values()
                              if candidate.get("task_id") == selection.get("task_id")
                              and candidate.get("assignment_ref") == selection.get("assignment_ref")
                              and isinstance(candidate.get("task_revision"), int)
                              and not isinstance(candidate.get("task_revision"), bool)), default=revision)
                if revision != latest:
                    errors.append(f"{ref}: archive selector không phải current revision {latest}")
        if payload is not None:
            same_identity(archive, payload, ref + ".payload")
            if payload.get("phase") != "payload" or payload.get("subject_sha") != archive.get("verified_payload_sha"):
                errors.append(f"{ref}: archive payload pointer trỏ sai phase/SHA")
        if closing is not None:
            same_identity(archive, closing, ref + ".closing")
            if closing.get("phase") != "closing" or closing.get("subject_sha") != archive.get("closing_sha"):
                errors.append(f"{ref}: archive closing pointer trỏ sai phase/SHA")
            if closing.get("parent_sha") != archive.get("verified_payload_sha"):
                errors.append(f"{ref}: closing observation parent không bằng P")
            if closing.get("check_plan_sha256") != archive.get("check_plan_sha256"):
                errors.append(f"{ref}: closing observation dùng check plan khác")
            if plan is not None:
                errors.extend(_observation_link_errors(
                    ref + ".closing", closing, plan, records, raw_members,
                    raw_member_bytes=raw_member_bytes,
                    expected_phase="closing", expected_parent=archive.get("verified_payload_sha"),
                    expected_identity=attempt if attempt is not None else archive,
                ))
        if archive.get("payload_observation_ref") == archive.get("closing_observation_ref"):
            errors.append(f"{ref}: payload/closing observations phải là hai nguồn khác nhau")
        if commit is not None:
            validate_commit_record(
                commit, phase="closing", owner=ref + ".closing_commit",
                identity=attempt if attempt is not None else archive,
                expected_sha=archive.get("closing_sha"),
                expected_parent=archive.get("verified_payload_sha"),
            )
            if commit.get("sha") != archive.get("closing_sha"):
                errors.append(f"{ref}: closing_commit_ref không khớp closing_sha")
            if (not isinstance(commit.get("scope_ref"), str) or commit.get("scope_ref") not in member_paths
                    or not isinstance(commit.get("git_evidence_ref"), str) or commit.get("git_evidence_ref") not in member_paths):
                errors.append(f"{ref}: closing commit scope/Git evidence không thuộc archive")
            subject = commit.get("subject")
            attempt_priority = attempt.get("priority") if attempt is not None else None
            if not isinstance(subject, str) or f"[{attempt_priority}]" not in subject or f"[{archive.get('task_id')}]" not in subject:
                errors.append(f"{ref}: closing commit subject phải bind priority/task")
            body = commit.get("body")
            trailers = [line for line in body.splitlines() if line.startswith("MCE4D-")] if isinstance(body, str) else []
            expected_trailers = [
                f"MCE4D-Delivery-ID: {archive.get('delivery_id')}",
                "MCE4D-Delivery-Phase: closing",
            ]
            if trailers != expected_trailers:
                errors.append(f"{ref}: closing commit phải có đúng delivery ID/phase trailers")

        # Archive publication is a retained closure of every relative evidence
        # locator reachable from the typed records that prove this delivery.
        required_members: set[str] = set()

        def require_member(value: Any) -> None:
            if isinstance(value, str) and _safe_relative_path(value):
                required_members.add(value)

        def require_scope_reasons(scope_ref: Any) -> None:
            require_member(scope_ref)
            scope_record = records.get(scope_ref) if isinstance(scope_ref, str) else None
            if scope_record is None or scope_record[0] != "GitCommitScopeEvidence":
                return
            scope = scope_record[1]
            for field in ("priority_reason_ref", "shared_invariant_ref", "breaking_decision_ref"):
                require_member(scope.get(field))

        require_selection_history(archive.get("selection_ref"), required_members)

        for pointer_field in ("attempt_ref", "selection_ref", "payload_observation_ref",
                              "closing_observation_ref", "closing_commit_ref", "git_evidence_ref"):
            require_member(archive.get(pointer_field))
        if attempt is not None:
            require_member(archive.get("attempt_ref"))
            require_member(attempt.get("check_plan_ref"))
            require_member(attempt.get("noop_reason_ref"))
            local_evidence_refs = attempt.get("local_evidence_refs")
            for evidence_ref in local_evidence_refs if isinstance(local_evidence_refs, list) else []:
                require_member(evidence_ref)
                local_record = record_for(evidence_ref)
                if local_record and local_record[0] == "GitLocalCheckEvidence":
                    log_refs = local_record[1].get("log_refs")
                    for log_ref in log_refs if isinstance(log_refs, list) else []:
                        require_member(log_ref)
            ordered_commits = attempt.get("ordered_commits")
            for payload_commit in ordered_commits if isinstance(ordered_commits, list) else []:
                if isinstance(payload_commit, dict):
                    require_scope_reasons(payload_commit.get("scope_ref"))
                    require_member(payload_commit.get("git_evidence_ref"))
        if plan is not None:
            plan_local_refs = plan.get("local_check_refs")
            for evidence_ref in plan_local_refs if isinstance(plan_local_refs, list) else []:
                require_member(evidence_ref)
        for observation, observation_ref in ((payload, archive.get("payload_observation_ref")),
                                             (closing, archive.get("closing_observation_ref"))):
            require_member(observation_ref)
            if observation is None:
                continue
            require_member(observation.get("collection_evidence_ref"))
            observed_items = observation.get("checks")
            for observed_check in observed_items if isinstance(observed_items, list) else []:
                if not isinstance(observed_check, dict):
                    continue
                require_member(observed_check.get("source_evidence_ref"))
                require_member(observed_check.get("execution_evidence_ref"))
                check_evidence_refs = observed_check.get("evidence_refs")
                for evidence_ref in check_evidence_refs if isinstance(check_evidence_refs, list) else []:
                    require_member(evidence_ref)
                source_ref = observed_check.get("source_evidence_ref")
                source_record = record_for(source_ref)
                if source_record and source_record[0] == "GitCheckSourceEvidence":
                    require_member(source_record[1].get("raw_api_ref"))
                execution_ref = observed_check.get("execution_evidence_ref")
                execution_record = record_for(execution_ref)
                if execution_record and execution_record[0] == "GitCheckExecutionEvidence":
                    for command_result in execution_record[1].get("command_results", []):
                        if isinstance(command_result, dict):
                            require_member(command_result.get("log_ref"))
            destination = observation.get("destination_observation")
            if isinstance(destination, dict):
                operation_ref = destination.get("operation_evidence_ref")
                require_member(operation_ref)
                operation_record = record_for(operation_ref)
                if operation_record and operation_record[0] == "GitPushOperationEvidence":
                    operation = operation_record[1]
                    require_member(operation.get("command_record_ref"))
                    for query_name in ("preflight", "postflight"):
                        query = operation.get(query_name)
                        if isinstance(query, dict):
                            require_member(query.get("raw_api_ref"))
            collection_ref = observation.get("collection_evidence_ref")
            collection_record = record_for(collection_ref)
            if collection_record and collection_record[0] == "GitCICollectionEvidence":
                collection_pages = collection_record[1].get("pages")
                for page in collection_pages if isinstance(collection_pages, list) else []:
                    if isinstance(page, dict):
                        require_member(page.get("raw_page_ref"))
                        page_ref = page.get("raw_page_ref")
                        raw_page = raw_members.get(page_ref) if isinstance(page_ref, str) else None
                        if isinstance(raw_page, dict) and raw_page.get("kind") == "github_api_page":
                            require_member(raw_page.get("body_ref"))
        if commit is not None:
            require_scope_reasons(commit.get("scope_ref"))
            require_member(commit.get("git_evidence_ref"))
        missing_members = sorted(required_members - member_paths)
        if missing_members:
            errors.append(f"{ref}: archive thiếu evidence member: {missing_members[:8]}")

    for live_ref, (kind, verification) in records.items():
        if kind != "GitLiveVerification":
            continue
        matching_archives = [
            (ref, archive) for ref, archive in archive_refs.items()
            if archive.get("delivery_id") == verification.get("delivery_id")
            and archive.get("task_id") == verification.get("task_id")
            and archive.get("task_revision") == verification.get("task_revision")
            and archive.get("assignment_ref") == verification.get("assignment_ref")
        ]
        if len(matching_archives) != 1:
            errors.append(f"{live_ref}: cần đúng một archive sealed cùng delivery/task/revision")
            continue
        archive_ref, archive = matching_archives[0]
        attempt = resolve(archive.get("attempt_ref"), "GitDeliveryAttempt", live_ref + ".archive.attempt_ref")
        selection = resolve(archive.get("selection_ref"), "GitTaskDeliverySelection", live_ref + ".archive.selection_ref")
        plan = resolve(attempt.get("check_plan_ref"), "GitCheckPlan", live_ref + ".plan") if attempt is not None else None
        observation = resolve(verification.get("closing_observation_ref"), "GitDeliveryObservation", live_ref + ".closing_observation_ref")
        collection = resolve(verification.get("collection_evidence_ref"), "GitCICollectionEvidence", live_ref + ".collection_evidence_ref")
        if selection is not None:
            if (selection.get("active_delivery_id") != verification.get("delivery_id")
                    or selection.get("task_revision") != verification.get("task_revision")):
                errors.append(f"{live_ref}: live verification không bind current selector")
        if attempt is not None:
            for field in ("assignment_ref", "task_id", "task_revision", "delivery_id"):
                if not _typed_equal(verification.get(field), attempt.get(field)):
                    errors.append(f"{live_ref}: live identity lệch attempt {field}")
            if verification.get("subject_sha") != archive.get("closing_sha"):
                errors.append(f"{live_ref}: live subject phải bằng archived closing_sha")
            if verification.get("check_plan_sha256") != attempt.get("check_plan_sha256"):
                errors.append(f"{live_ref}: live check-plan hash lệch attempt/archive")
        if archive.get("archive_path") is not None:
            expected_archive_path = archive.get("archive_path")
        else:
            expected_archive_path = f"artifacts/git-delivery/{archive.get('delivery_id')}"
        publication = verification.get("archive_publication")
        if isinstance(publication, dict):
            if publication.get("planned_archive_path") != expected_archive_path:
                errors.append(f"{live_ref}: publication path không khớp current archive")
            try:
                manifest_sha = hashlib.sha256(_canonical(archive)).hexdigest()
            except (TypeError, ValueError):
                manifest_sha = None
            if manifest_sha is not None and publication.get("archive_manifest_sha256") != manifest_sha:
                errors.append(f"{live_ref}: publication manifest hash không khớp canonical archive")
            published = _parse_time(publication.get("published_at_utc"))
            readback = _parse_time(publication.get("readback_completed_at_utc"))
            started = _parse_time(verification.get("started_at_utc"))
            completed = _parse_time(verification.get("completed_at_utc"))
            if (published is None or readback is None or published > readback
                    or started is None or started < readback
                    or completed is None or completed < started):
                errors.append(f"{live_ref}: live invocation phải bắt đầu sau publication+readback")
        if collection is not None:
            if (collection.get("subject_sha") != verification.get("subject_sha")
                    or collection.get("check_plan_sha256") != verification.get("check_plan_sha256")):
                errors.append(f"{live_ref}: fresh CI collection lệch live subject/plan")
        if observation is not None and plan is not None and attempt is not None:
            if (observation.get("phase") != "closing"
                    or observation.get("delivery_id") != verification.get("delivery_id")
                    or observation.get("subject_sha") != verification.get("subject_sha")
                    or observation.get("collection_evidence_ref") != verification.get("collection_evidence_ref")):
                errors.append(f"{live_ref}: fresh closing observation lệch invocation")
            errors.extend(_observation_link_errors(
                live_ref + ".closing", observation, plan, records, raw_members,
                raw_member_bytes=raw_member_bytes,
                expected_phase="closing", expected_parent=archive.get("verified_payload_sha"),
                expected_identity=attempt,
            ))
        files = verification.get("files")
        file_paths = {item.get("path") for item in files if isinstance(item, dict)} if isinstance(files, list) else set()
        required_live_refs = {
            verification.get("closing_observation_ref"),
            verification.get("collection_evidence_ref"),
            verification.get("git_evidence_ref"),
        }
        if isinstance(verification.get("current_pr_ref"), str):
            required_live_refs.add(verification["current_pr_ref"])
        if collection is not None:
            for page in collection.get("pages", []):
                if isinstance(page, dict):
                    page_ref = page.get("raw_page_ref")
                    required_live_refs.add(page_ref)
                    page_value = raw_members.get(page_ref) if isinstance(page_ref, str) else None
                    if isinstance(page_value, dict) and page_value.get("kind") == "github_api_page":
                        required_live_refs.add(page_value.get("body_ref"))
        if observation is not None:
            for check in observation.get("checks", []):
                if isinstance(check, dict):
                    required_live_refs.update((check.get("source_evidence_ref"), check.get("execution_evidence_ref")))
                    required_live_refs.update(check.get("evidence_refs", []) if isinstance(check.get("evidence_refs"), list) else [])
        required_live_refs.discard(None)
        if any(not isinstance(ref, str) or ref not in file_paths for ref in required_live_refs):
            errors.append(f"{live_ref}: files manifest thiếu source graph của fresh invocation")
        if isinstance(files, list) and len(file_paths) != len(files):
            errors.append(f"{live_ref}: files manifest chứa path trùng")
        unknowns.append(f"{live_ref}: freshness GitHub, archive seal/readback bytes và secured caller identity cần observer thật")

    seen_markers: dict[str, str] = {}
    for ref, attempt in attempt_refs.items():
        marker = attempt.get("closing_marker")
        if isinstance(marker, str):
            previous = seen_markers.get(marker)
            if previous is not None and previous != ref:
                errors.append(f"closing marker không unique giữa {previous} và {ref}")
            seen_markers[marker] = ref
    if errors:
        verdict = "REJECT"
    else:
        verdict = "UNKNOWN"
        unknowns.append("Bundle tĩnh không đủ điều kiện để công nhận effective DONE")
    return {"verdict": verdict, "errors": errors, "unknowns": unknowns, "archive_count": len(archive_refs), "attempt_count": len(attempt_refs)}
