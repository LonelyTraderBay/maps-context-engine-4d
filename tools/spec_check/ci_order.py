"""Chuẩn hóa và chọn candidate CI theo identity native, instant và attempt."""

from __future__ import annotations

import re
from datetime import date
from fractions import Fraction
from typing import Any


TIME = re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(Z|[+-]\d{2}:\d{2})$"
)


class OrderingError(ValueError):
    """Nguồn CI không đủ chuẩn để chọn duy nhất candidate mới nhất."""


def parse_instant(value: Any) -> tuple[int, Fraction]:
    """Return UTC whole seconds and an exact fractional second."""
    if not isinstance(value, str):
        raise OrderingError("timestamp không phải chuỗi")
    match = TIME.fullmatch(value)
    if match is None:
        raise OrderingError(f"timestamp RFC3339 thiếu timezone hoặc không hợp lệ: {value!r}")
    year, month, day, hour, minute, second = (int(match.group(i)) for i in range(1, 7))
    fraction = match.group(7) or ""
    if len(fraction) > 64:
        raise OrderingError("fractional seconds vượt giới hạn 64 chữ số")
    zone = match.group(8)
    try:
        ordinal = date(year, month, day).toordinal()
    except ValueError as exc:
        raise OrderingError(f"ngày RFC3339 không hợp lệ: {value!r}") from exc
    if hour > 23 or minute > 59 or second > 59:
        raise OrderingError(f"giờ RFC3339 không hợp lệ: {value!r}")
    seconds = ordinal * 86400 + hour * 3600 + minute * 60 + second
    if zone != "Z":
        sign = 1 if zone[0] == "+" else -1
        offset_hours = int(zone[1:3])
        offset_minutes = int(zone[4:6])
        if offset_hours > 23 or offset_minutes > 59:
            raise OrderingError(f"timezone offset không hợp lệ: {value!r}")
        seconds -= sign * (offset_hours * 3600 + offset_minutes * 60)
    subsecond = Fraction(int(fraction), 10 ** len(fraction)) if fraction else Fraction(0, 1)
    return seconds, subsecond


def _native_id(value: Any, field: str) -> int:
    if isinstance(value, bool):
        raise OrderingError(f"{field} không được là boolean")
    if isinstance(value, int):
        if value < 0:
            raise OrderingError(f"{field} phải không âm")
        return value
    if isinstance(value, str) and re.fullmatch(r"(?:0|[1-9][0-9]*)", value):
        return int(value)
    raise OrderingError(f"{field} phải là native decimal ID")


def select_latest_actions_generation(
    workflow_runs: list[dict[str, Any]],
    *,
    workflow_identity: str,
    event: str,
    subject_sha: str,
) -> dict[str, Any]:
    """Select one newest Actions workflow generation without dropping bad rows."""
    parsed: list[tuple[tuple[int, Fraction], int, dict[str, Any]]] = []
    seen_run_ids: dict[tuple[str, str, str, int], tuple[int, Fraction]] = {}
    for index, run in enumerate(workflow_runs):
        if not isinstance(run, dict):
            return {"verdict": "UNKNOWN", "reason": f"workflow run {index} không phải object"}
        try:
            instant = parse_instant(run.get("created_at_utc"))
            run_id = _native_id(run.get("run_id"), "workflow_run_id")
        except OrderingError as exc:
            return {"verdict": "UNKNOWN", "reason": str(exc)}
        key = (str(run.get("workflow_identity")), str(run.get("event")), str(run.get("subject_sha")), run_id)
        previous = seen_run_ids.get(key)
        if previous is not None and previous != instant:
            return {"verdict": "UNKNOWN", "reason": "cùng workflow run ID có creation time mâu thuẫn"}
        seen_run_ids[key] = instant
        if run.get("collection_complete") is not True:
            return {"verdict": "UNKNOWN", "reason": "workflow generation collection chưa đủ trang"}
        if (run.get("workflow_identity"), run.get("event"), run.get("subject_sha")) == (workflow_identity, event, subject_sha):
            parsed.append((instant, run_id, run))
    if not parsed:
        return {"verdict": "UNKNOWN", "reason": "không có workflow generation phù hợp"}
    parsed.sort(key=lambda item: (item[0], item[1]))
    return {"verdict": "SELECTED", "run": parsed[-1][2]}


def select_latest_actions_attempts(
    jobs: list[dict[str, Any]],
    *,
    selected_run_id: Any,
    required_job_identities: set[str],
) -> dict[str, Any]:
    """Choose max attempt separately per required check identity in one run."""
    run_id = _native_id(selected_run_id, "selected_run_id")
    by_identity: dict[str, list[dict[str, Any]]] = {identity: [] for identity in required_job_identities}
    seen_native: set[tuple[int, int, str]] = set()
    for index, job in enumerate(jobs):
        if not isinstance(job, dict):
            return {"verdict": "UNKNOWN", "reason": f"job {index} không phải object"}
        try:
            candidate_run = _native_id(job.get("run_id"), "workflow_run_id")
            attempt = _native_id(job.get("attempt"), "workflow_run_attempt")
        except OrderingError as exc:
            return {"verdict": "UNKNOWN", "reason": str(exc)}
        identity = job.get("check_identity")
        if not isinstance(identity, str) or not identity:
            return {"verdict": "UNKNOWN", "reason": "job thiếu check identity native"}
        if attempt < 1:
            return {"verdict": "UNKNOWN", "reason": "workflow_run_attempt phải từ 1 trở lên"}
        if candidate_run != run_id or identity not in by_identity:
            continue
        native = (candidate_run, attempt, identity)
        if native in seen_native:
            return {"verdict": "UNKNOWN", "reason": f"job attempt trùng: {identity}/{attempt}"}
        seen_native.add(native)
        by_identity[identity].append(job)
    missing = sorted(identity for identity, attempts in by_identity.items() if not attempts)
    if missing:
        return {"verdict": "UNKNOWN", "reason": "generation mới nhất thiếu required job(s): " + ", ".join(missing)}
    selected: dict[str, dict[str, Any]] = {}
    for identity, attempts in by_identity.items():
        attempts.sort(key=lambda job: _native_id(job.get("attempt"), "workflow_run_attempt"))
        selected[identity] = attempts[-1]
    return {"verdict": "SELECTED", "attempts": selected}


def select_latest_native_records(records: list[dict[str, Any]], *, source_kind: str) -> dict[str, Any]:
    """Select newest check-run/status per native identity; malformed rows block."""
    grouped: dict[tuple[str, ...], list[tuple[tuple[int, Fraction], int, dict[str, Any]]]] = {}
    identity_fields = {
        "check_run": ("source_app", "context", "workflow_path", "job_name", "event", "subject_sha"),
        "commit_status": ("source_app", "context", "event", "subject_sha"),
    }.get(source_kind)
    if identity_fields is None:
        return {"verdict": "UNKNOWN", "reason": f"source_kind không hỗ trợ: {source_kind}"}
    seen: dict[tuple[str, ...] + tuple[int], tuple[tuple[int, Fraction], str]] = {}
    for index, record in enumerate(records):
        if not isinstance(record, dict):
            return {"verdict": "UNKNOWN", "reason": f"native record {index} không phải object"}
        try:
            instant = parse_instant(record.get("created_at_utc"))
            native_id = _native_id(record.get("id"), "native check/status ID")
        except OrderingError as exc:
            return {"verdict": "UNKNOWN", "reason": str(exc)}
        if any(not isinstance(record.get(field), str) or not record[field] for field in identity_fields):
            return {"verdict": "UNKNOWN", "reason": "native identity thiếu field bắt buộc"}
        if native_id < 1:
            return {"verdict": "UNKNOWN", "reason": "native check/status ID phải dương"}
        identity = tuple(record[field] for field in identity_fields)
        candidate_key = identity + (native_id,)
        signature = repr((record.get("status"), record.get("conclusion"), record.get("sha")))
        previous = seen.get(candidate_key)
        if previous is not None and (previous[0] != instant or previous[1] != signature):
            return {"verdict": "UNKNOWN", "reason": "cùng native ID có nguồn hoặc conclusion mâu thuẫn"}
        seen[candidate_key] = (instant, signature)
        grouped.setdefault(identity, []).append((instant, native_id, record))
    selected: dict[tuple[str, ...], dict[str, Any]] = {}
    for identity, candidates in grouped.items():
        candidates.sort(key=lambda item: (item[0], item[1]))
        selected[identity] = candidates[-1][2]
    return {"verdict": "SELECTED", "records": selected}
