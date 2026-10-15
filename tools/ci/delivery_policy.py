"""Guard kế hoạch CI, commit và giới hạn diff closing của AW-08."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
import unicodedata
from pathlib import Path
from typing import Any
from uuid import UUID

ROOT = Path(__file__).resolve().parents[2]
SPEC_PATH = ROOT / "Maps-Context-Engine-4D_SPEC.md"
SPEC_RELATIVE = "Maps-Context-Engine-4D_SPEC.md"
CHECKOUT_SHA = "3d3c42e5aac5ba805825da76410c181273ba90b1"
GIT_SHA = re.compile(r"^(?:[0-9a-f]{40}|[0-9a-f]{64})$")
COMMIT_SUBJECT = re.compile(
    r"^(?P<type>feat|fix|test|docs|build|refactor|chore|ci|perf|revert)"
    r"(?:\([a-z0-9._/-]+\))?(?P<breaking>!)?: "
    r"\[(?P<priority>P[0-3])\]\[(?P<task_id>T\d{3})\] (?P<note>\S.*\S|\S)$"
)
FORBIDDEN_SKIP = re.compile(r"\[(?:skip|no)[ -]ci\]|\bskip(?:ped)?\s*(?:as|=|:)\s*pass\b", re.IGNORECASE)
TRAILER = re.compile(r"^([A-Za-z0-9][A-Za-z0-9-]*):\s*(\S.*)$")
ALLOWED_S22_KEYS = {
    "implementation_revision", "git_delivery.last_verified_payload_sha",
    "git_delivery.closing_commit_ref", "git_delivery.pending_delivery_id",
    "git_delivery.active_delivery_id", "git_delivery.task_revision",
    "git_delivery.planned_archive_path", "git_delivery.closing_parent_sha",
    "git_delivery.durable_archive_ref", "git_delivery.observed_remote_sha",
    "git_delivery.expected_check_plan_ref", "git_delivery.last_ci_result",
    "git_delivery.last_live_verification_ref", "git_delivery.last_live_verification_id",
    "git_delivery.last_ci_collection_ref", "git_delivery.pending_delivery_task",
    "git_delivery.last_delivery_evidence_ref", "execution_checkpoint",
    "execution_checkpoint.head_sha", "execution_checkpoint.working_tree_digest",
    "execution_checkpoint.index_digest", "execution_checkpoint.command_record_ref",
    "execution_checkpoint.operation_id", "execution_checkpoint.command_state",
    "execution_checkpoint.unfinished_check_refs", "execution_checkpoint.blocker_refs",
    "execution_checkpoint.next_action_ref", "execution_checkpoint.checkpointed_at_utc",
}


def _strict_json(raw: bytes) -> Any:
    if __package__:
        from tools.spec_check.schema import strict_json_loads
    else:
        sys.path.insert(0, str(ROOT))
        from tools.spec_check.schema import strict_json_loads
    return strict_json_loads(raw)


def _load_schema_definition(kind: str) -> tuple[dict[str, Any], dict[str, Any]]:
    if __package__:
        from tools.spec_check.schema import extract_governance_schema
    else:
        sys.path.insert(0, str(ROOT))
        from tools.spec_check.schema import extract_governance_schema
    schema = extract_governance_schema(SPEC_PATH.read_text(encoding="utf-8"))
    definition = schema.get("$defs", {}).get(kind)
    if not isinstance(definition, dict):
        raise ValueError(f"Không có definition S21: {kind}")
    return schema, definition


def _workflow_jobs(workflow: str) -> dict[str, dict[str, Any]]:
    """Read the deliberately small job subset used by the pinned workflow."""
    lines = workflow.replace("\r\n", "\n").splitlines()
    jobs_index = next((index for index, line in enumerate(lines) if line == "jobs:"), None)
    if jobs_index is None:
        return {}
    jobs: dict[str, dict[str, Any]] = {}
    current_name: str | None = None
    current_lines: list[str] = []
    for line in lines[jobs_index + 1:]:
        match = re.fullmatch(r"  ([A-Za-z_][A-Za-z0-9_-]*):\s*", line)
        if match:
            if current_name is not None:
                jobs[current_name] = _workflow_job_details(current_lines)
            current_name = match.group(1)
            if current_name in jobs:
                return {}
            current_lines = []
        elif current_name is not None:
            if line and not line.startswith(" "):
                jobs[current_name] = _workflow_job_details(current_lines)
                break
            current_lines.append(line)
    else:
        if current_name is not None:
            jobs[current_name] = _workflow_job_details(current_lines)
    return jobs


def _workflow_job_details(lines: list[str]) -> dict[str, Any]:
    name: str | None = None
    matrix_os: set[str] = set()
    commands: list[str] = []
    for line in lines:
        match = re.fullmatch(r"    name:\s*([^#]+?)\s*", line)
        if match:
            name = match.group(1)
        match = re.fullmatch(r"\s+os:\s*\[([^]]*)\]\s*", line)
        if match:
            matrix_os.update(item.strip().strip("'\"") for item in match.group(1).split(",") if item.strip())
        match = re.fullmatch(r"\s+run:\s*(\S.*?)\s*", line)
        if match:
            commands.append(match.group(1))
    return {"name": name, "matrix_os": matrix_os, "commands": commands}


def validate_check_plan(plan: Any, *, workflow_text: str, expected_sha256: str | None = None) -> list[str]:
    errors: list[str] = []
    if __package__:
        from tools.spec_check.schema import validate_schema
        from tools.spec_check.semantics import validate_semantics
    else:
        sys.path.insert(0, str(ROOT))
        from tools.spec_check.schema import validate_schema
        from tools.spec_check.semantics import validate_semantics
    try:
        schema, definition = _load_schema_definition("GitCheckPlan")
    except (OSError, ValueError, UnicodeDecodeError) as exc:
        return [str(exc)]
    errors.extend(validate_schema(plan, definition, root_schema=schema))
    errors.extend(validate_semantics("GitCheckPlan", plan))
    if not isinstance(plan, dict):
        return errors
    if expected_sha256:
        actual = hashlib.sha256(_canonical_json(plan)).hexdigest()
        if actual != expected_sha256:
            errors.append(f"SHA-256 plan sai: cần {expected_sha256}, thực tế {actual}")
    if plan.get("required_set_status") != "VERIFIED" or not plan.get("checks"):
        errors.append("Expected check set phải VERIFIED và không rỗng")
    canary_ref = plan.get("canary_ref")
    if not isinstance(canary_ref, str) or re.fullmatch(r"delivery:[0-9a-fA-F-]{36}:payload", canary_ref) is None:
        errors.append("canary_ref phải là locator đã freeze trước payload")
    elif plan.get("phase") == "bootstrap":
        try:
            UUID(canary_ref.split(":", 2)[1])
        except ValueError:
            errors.append("bootstrap canary_ref phải chứa delivery UUID")
    workflow = workflow_text.replace("\r\n", "\n")
    if re.search(r"(?m)^on:\s*$", workflow) is None:
        errors.append("Workflow thiếu trigger on")
    for trigger in ("  push:", "  pull_request:"):
        if re.search(rf"(?m)^{re.escape(trigger)}\s*$", workflow) is None:
            errors.append(f"Workflow thiếu trigger bắt buộc {trigger.strip()}")
    if "pull_request_target:" in workflow:
        errors.append("Workflow bootstrap không được dùng pull_request_target đặc quyền")
    if f"actions/checkout@{CHECKOUT_SHA}" not in workflow or "persist-credentials: false" not in workflow:
        errors.append("Workflow cần checkout action đã pin và tắt persist-credentials")
    if "permissions:\n  contents: read" not in workflow:
        errors.append("Workflow bootstrap cần quyền tối thiểu contents: read")
    if "secrets." in workflow or "pull_request_target:" in workflow:
        errors.append("Workflow bootstrap không được dùng secrets hoặc pull_request_target")
    if isinstance(plan.get("checks"), list):
        jobs = _workflow_jobs(workflow)
        for index, check in enumerate(plan["checks"]):
            if not isinstance(check, dict):
                continue
            context = check.get("context")
            job = check.get("job_name")
            matrix = check.get("matrix")
            os_name = matrix.get("os") if isinstance(matrix, dict) else None
            if not all(isinstance(value, str) and value for value in (context, job, os_name)):
                continue
            actual_job = jobs.get(job)
            expected_name = f"{job} (${{{{ matrix.os }}}})"
            if actual_job is None or actual_job.get("name") != expected_name:
                errors.append(f"checks[{index}] context {context!r} không map tới job {job!r}")
            elif os_name not in actual_job.get("matrix_os", set()):
                errors.append(f"checks[{index}] matrix OS {os_name!r} không được job {job!r} khai báo")
            if context != f"{job} ({os_name})":
                errors.append(f"checks[{index}] context không khớp job/matrix identity")
            if check.get("workflow_path") != ".github/workflows/bootstrap.yml":
                errors.append(f"checks[{index}] workflow_path không khớp bootstrap workflow")
            if check.get("checkout_binding") != "head" or check.get("reported_sha_binding") != "subject":
                errors.append(f"checks[{index}] phải bind checkout/reported SHA vào đúng subject")
            if not {"push", "pull_request"}.issubset(set(check.get("eligible_events", []))):
                errors.append(f"checks[{index}] phải chạy trên push và pull_request")
            if check.get("requirement_origins") != ["project"]:
                errors.append(f"checks[{index}] phải giữ project-origin requirement")
            commands = check.get("command_refs")
            if not isinstance(commands, list) or not commands:
                errors.append(f"checks[{index}] command_refs không được rỗng")
                continue
            for command in commands:
                if not isinstance(command, str) or not command.strip():
                    errors.append(f"checks[{index}] có command rỗng")
                elif actual_job is None or command not in actual_job.get("commands", []):
                    errors.append(f"Workflow không chạy command đã freeze: {command}")
                elif re.search(r"(?:^|\s)(?:echo|Write-Output)\s+['\"]?(?:PASS|ĐẠT)", command, re.IGNORECASE):
                    errors.append(f"checks[{index}] dùng lệnh no-op giả PASS")
                if isinstance(command, str) and re.search(r"gh\s+run\s+watch|check-runs|workflow_run", command, re.IGNORECASE):
                    errors.append(f"checks[{index}] không được chờ/đọc kết quả của chính workflow đang chạy")
    return errors


def _canonical_json(value: Any) -> bytes:
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
        return b"[" + b",".join(_canonical_json(child) for child in value) + b"]"
    if isinstance(value, dict):
        if not all(isinstance(key, str) for key in value):
            raise ValueError("JSON key phải là chuỗi")
        return b"{" + b",".join(_canonical_json(key) + b":" + _canonical_json(value[key]) for key in sorted(value)) + b"}"
    raise ValueError(f"Giá trị ngoài primitive JSON: {type(value).__name__}")


def validate_commit_message(message: str, *, phase: str, task_id: str, priority: str) -> list[str]:
    errors: list[str] = []
    normalized = message.replace("\r\n", "\n").rstrip("\n")
    lines = normalized.split("\n")
    subject = lines[0] if lines else ""
    match = COMMIT_SUBJECT.fullmatch(subject)
    if match is None:
        errors.append("Subject phải theo Conventional Commit + [P0–P3][Tnnn] + ghi chú")
    else:
        if match.group("task_id") != task_id:
            errors.append(f"Subject phải gắn đúng task {task_id}")
        if match.group("priority") != priority:
            errors.append(f"Subject phải giữ priority {priority}")
        note = match.group("note")
        if not any(character in "ăâđêôơưĂÂĐÊÔƠƯ" or "À" <= character <= "ỹ" for character in note):
            errors.append("Ghi chú trong subject phải có tiếng Việt có dấu")
    if FORBIDDEN_SKIP.search(normalized):
        errors.append("Commit message chứa chỉ thị bỏ qua CI hoặc gán skip thành PASS")
    if unicodedata.normalize("NFC", normalized) != normalized:
        errors.append("Commit message phải ở Unicode NFC")
    if "\r" in normalized or "\x00" in normalized:
        errors.append("Commit message chứa CR lẻ hoặc NUL")
    section_names = ("Mục đích:", "Thay đổi:", "Kiểm tra:")
    section_positions: dict[str, int] = {}
    for section in section_names:
        positions = [index for index, line in enumerate(lines[1:], start=1) if line.startswith(section)]
        if len(positions) != 1:
            errors.append(f"Body cần đúng một heading {section}")
            continue
        section_positions[section] = positions[0]
    if len(section_positions) == 3 and list(section_positions.values()) != sorted(section_positions.values()):
        errors.append("Body headings phải theo thứ tự Mục đích, Thay đổi, Kiểm tra")
    trailer_positions = [index for index, line in enumerate(lines[1:], start=1) if line.startswith("MCE4D-")]
    for section in section_names:
        start = section_positions.get(section)
        if start is None:
            continue
        later_boundaries = [position for name, position in section_positions.items() if position > start]
        later_boundaries.extend(position for position in trailer_positions if position > start)
        end = min(later_boundaries, default=len(lines))
        content_lines = lines[start:end]
        content_lines[0] = content_lines[0][len(section):]
        if not any(line.strip() for line in content_lines):
            errors.append(f"Body heading {section} cần nội dung")
    trailers = _trailers(lines)
    delivery_keys = [key for key, _ in trailers if key.startswith("MCE4D-")]
    retained_delivery_lines = [line for line in lines[1:] if line.startswith("MCE4D-")]
    parsed_delivery_lines = [f"{key}: {value}" for key, value in trailers if key.startswith("MCE4D-")]
    if retained_delivery_lines != parsed_delivery_lines:
        errors.append("MCE4D trailers phải hợp lệ, duy nhất và nằm trong trailer block cuối")
    if phase == "payload":
        if delivery_keys:
            errors.append("Payload không được chứa delivery closing trailers")
    elif phase == "closing":
        expected = {"MCE4D-Delivery-ID", "MCE4D-Delivery-Phase"}
        if set(delivery_keys) != expected or any(delivery_keys.count(key) != 1 for key in expected):
            errors.append("Closing cần đúng một Delivery-ID và một Delivery-Phase trailer")
        values = dict(trailers)
        if values.get("MCE4D-Delivery-Phase") != "closing":
            errors.append("MCE4D-Delivery-Phase phải là closing")
        try:
            UUID(values.get("MCE4D-Delivery-ID", ""))
        except (ValueError, AttributeError):
            errors.append("MCE4D-Delivery-ID phải là UUID hợp lệ")
    else:
        errors.append(f"phase không hợp lệ: {phase}")
    breaking_body_lines = [line for line in lines[1:] if line.startswith("BREAKING CHANGE:")]
    has_breaking_marker = bool(match is not None and match.group("breaking")) or bool(breaking_body_lines)
    if has_breaking_marker:
        if len(breaking_body_lines) != 1 or not breaking_body_lines[0][len("BREAKING CHANGE:"):].strip():
            errors.append("Breaking marker cần đúng một BREAKING CHANGE có mô tả")
    return errors


def _trailers(lines: list[str]) -> list[tuple[str, str]]:
    collected: list[tuple[str, str]] = []
    for line in reversed(lines[1:]):
        if not line.strip():
            if collected:
                break
            continue
        match = TRAILER.fullmatch(line.strip())
        if match is None:
            if collected:
                break
            return []
        collected.append((match.group(1), match.group(2)))
    return list(reversed(collected))


def validate_observed_checks(expected_checks: list[dict[str, Any]], observed_checks: list[dict[str, Any]], expected_sha: str) -> list[str]:
    errors: list[str] = []
    by_id: dict[str, list[dict[str, Any]]] = {}
    for check in observed_checks:
        check_id = check.get("id")
        if isinstance(check_id, str):
            by_id.setdefault(check_id, []).append(check)
    expected_ids = {item.get("id") for item in expected_checks if isinstance(item, dict)}
    if len(expected_ids) != len(expected_checks) or None in expected_ids:
        errors.append("Expected check IDs bị thiếu hoặc trùng")
    for expected in expected_checks:
        if not isinstance(expected, dict):
            continue
        check_id = expected.get("id")
        matches = by_id.get(check_id, [])
        if len(matches) != 1:
            errors.append(f"Check {check_id!r} cần đúng một observed native identity, hiện có {len(matches)}")
            continue
        actual = matches[0]
        if actual.get("context") != expected.get("context") or actual.get("source_app") != expected.get("source_app"):
            errors.append(f"Check {check_id} sai context hoặc source app")
        if actual.get("tested_checkout_sha") != expected_sha:
            errors.append(f"Check {check_id} tested checkout không khớp subject SHA")
        for field in ("kind", "workflow_path", "job_name", "matrix"):
            if field in expected and actual.get(field) != expected.get(field):
                errors.append(f"Check {check_id} sai native field {field}")
        if "eligible_events" in expected and actual.get("event") not in expected["eligible_events"]:
            errors.append(f"Check {check_id} event không thuộc eligible set")
        if expected.get("reported_sha_binding") == "subject" and actual.get("reported_head_sha") != expected_sha:
            errors.append(f"Check {check_id} reported SHA không khớp subject")
        for field in ("source_evidence_ref", "execution_evidence_ref"):
            if not isinstance(actual.get(field), str) or not actual[field].strip():
                errors.append(f"Check {check_id} thiếu typed {field}")
        evidence_refs = actual.get("evidence_refs")
        if not isinstance(evidence_refs, list) or not evidence_refs or any(not isinstance(ref, str) or not ref for ref in evidence_refs):
            errors.append(f"Check {check_id} thiếu retained evidence refs")
        if actual.get("status") != "completed" or actual.get("conclusion") != "success":
            errors.append(f"Check {check_id} chưa terminal success trên SHA yêu cầu")
        if not isinstance(actual.get("run_id"), str) or not actual["run_id"].isdigit():
            errors.append(f"Check {check_id} thiếu native workflow/check run ID")
        job_id = actual.get("job_id")
        if not isinstance(job_id, (str, int)) or isinstance(job_id, bool) or not str(job_id).isdigit():
            errors.append(f"Check {check_id} thiếu native job ID")
        attempt = actual.get("attempt")
        if not isinstance(attempt, int) or isinstance(attempt, bool) or attempt < 1:
            errors.append(f"Check {check_id} retry attempt phải là số nguyên dương")
        if actual.get("execution_verified") is not True:
            errors.append(f"Check {check_id} thiếu execution-verified proof")
        if actual.get("conclusion") in {"skipped", "neutral"}:
            errors.append(f"Check {check_id} skipped/neutral không phải test PASS")
    unexpected = set(by_id) - expected_ids
    if unexpected:
        errors.append(f"Observed collection có identity ngoài frozen set: {sorted(unexpected)}")
    return errors


def _section_map(text: str) -> tuple[list[str], dict[str, list[str]]]:
    prefix: list[str] = []
    sections: dict[str, list[str]] = {}
    current: str | None = None
    # Markdown lines are separated by LF. Unicode line separators can occur inside
    # canonical JSON/string vectors and must remain content rather than boundaries.
    for line in text.split("\n"):
        match = re.match(r"^##\s+(S\d{2})(?:\s|—|$)", line)
        if match:
            current = match.group(1)
            sections.setdefault(current, []).append(line)
        elif current is None:
            prefix.append(line)
        else:
            sections[current].append(line)
    return prefix, sections


def _change_allowed_s19(old: list[str], new: list[str], task_id: str) -> list[str]:
    errors: list[str] = []
    if len(old) != len(new):
        return ["S19 changed line count; only task row leaves are allowed"]
    changed_target = 0
    old_target: list[str] | None = None
    new_target: list[str] | None = None
    for before, after in zip(old, new):
        if before == after:
            continue
        old_line, new_line = before.rstrip("\r\n"), after.rstrip("\r\n")
        if old_line.startswith(f"| {task_id} |") and new_line.startswith(f"| {task_id} |"):
            old_cells = _split_table_row(old_line)
            new_cells = _split_table_row(new_line)
            if len(old_cells) != 9 or len(new_cells) != 9:
                errors.append("Malformed current S19 task row")
            elif any(old_cells[index] != new_cells[index] for index in range(9) if index not in {6, 7, 8}):
                errors.append("S19 closing changed task scope/track/dependency/requirement/case")
            else:
                changed_target += 1
                old_target, new_target = old_cells, new_cells
            continue
        errors.append(f"S19 changed a non-owned row or derived field: {old_line[:90]!r}")
    if changed_target != 1 or old_target is None or new_target is None:
        errors.append("S19 closing phải sửa đúng một lần hàng task hiện tại")
        return errors
    if old_target[6] != "VERIFY" or new_target[6] != "DONE":
        errors.append("S19 phải chuyển VERIFY → candidate DONE trong closing")
    if old_target[7] != new_target[7]:
        errors.append("S19 closing không được đổi owner đã nhận task")
    before_evidence = _evidence_fields(old_target[8], errors, "S19 payload")
    after_evidence = _evidence_fields(new_target[8], errors, "S19 closing")
    for key in ("task_revision", "delivery", "plan", "priority", "local"):
        if before_evidence.get(key) != after_evidence.get(key):
            errors.append(f"S19 closing làm đổi metadata đã freeze: {key}")
    if before_evidence.get("local") != "PASS":
        errors.append("S19 closing cần local=PASS")
    if not set(before_evidence).issubset(after_evidence):
        errors.append("S19 closing không được xóa metadata evidence đã giữ")
    if set(after_evidence) - set(before_evidence) != {"candidate"}:
        errors.append("S19 closing chỉ được thêm field candidate")
    for key, value in before_evidence.items():
        if key in {"payload_sha", "payload_CI"}:
            if value != "PENDING" and after_evidence.get(key) != value:
                errors.append(f"S19 closing không được thay evidence đã quan sát: {key}")
        elif after_evidence.get(key) != value:
            errors.append(f"S19 closing làm đổi evidence đã freeze: {key}")
    if not isinstance(after_evidence.get("payload_sha"), str) or GIT_SHA.fullmatch(after_evidence["payload_sha"]) is None:
        errors.append("S19 closing cần SHA đầy đủ của payload P")
    if not isinstance(after_evidence.get("payload_CI"), str) or not after_evidence["payload_CI"].startswith("PASS("):
        errors.append("S19 closing cần exact payload CI success reference")
    for key in ("closing_sha", "closing_CI", "archive/live"):
        if after_evidence.get(key) != "PENDING":
            errors.append(f"Closing không được tự ghi kết quả chưa quan sát: {key}=PENDING")
    if after_evidence.get("candidate") != "DONE":
        errors.append("S19 closing phải ghi rõ candidate=DONE; effective state vẫn VERIFY")
    return errors


def _evidence_fields(value: str, errors: list[str], owner: str) -> dict[str, str]:
    fields: dict[str, str] = {}
    pieces = value.split(";")
    if not pieces or not re.fullmatch(r"E\d{3}(?:,E\d{3})*", pieces[0]):
        errors.append(f"{owner} thiếu evidence ID dạng E###")
    for item in pieces[1:]:
        if not item:
            continue
        key, separator, field_value = item.partition("=")
        if not separator or not key or key in fields:
            errors.append(f"{owner} có field rỗng/sai/trùng: {item!r}")
            continue
        fields[key] = field_value
    return fields


def _split_table_row(line: str) -> list[str]:
    body = line.strip()
    if body.startswith("|"):
        body = body[1:]
    if body.endswith("|"):
        body = body[:-1]
    return [cell.strip() for cell in body.split("|")]


def _change_allowed_s20(old: list[str], new: list[str], owning_gate: str) -> list[str]:
    errors: list[str] = []
    if len(old) != len(new):
        return ["S20 changed line count"]
    for before, after in zip(old, new):
        if before == after:
            continue
        if before.startswith(f"| {owning_gate} —") and after.startswith(f"| {owning_gate} —"):
            old_cells, new_cells = _split_table_row(before), _split_table_row(after)
            if len(old_cells) != 4 or len(new_cells) != 4 or old_cells[:3] != new_cells[:3]:
                errors.append("S20 changed gate scope or acceptance contract")
            continue
        errors.append("S20 changed a gate other than the current task's owning gate")
    return errors


def _change_allowed_s21(old: list[str], new: list[str], task_id: str) -> list[str]:
    errors: list[str] = []
    old_lines = [line.rstrip("\r\n") for line in old]
    new_lines = [line.rstrip("\r\n") for line in new]
    # A closing may append new evidence rows, but it cannot edit retained evidence/ADR/schema.
    old_ids = [int(match.group(1)[1:]) for line in old_lines if (match := re.match(r"^\| (E\d{3}) \|", line))]
    new_ids = [int(match.group(1)[1:]) for line in new_lines if (match := re.match(r"^\| (E\d{3}) \|", line))]
    if len(new_ids) != len(set(new_ids)):
        errors.append("S21 chứa evidence ID bị lặp")
    iterator = iter(new_lines)
    inserted: list[str] = []
    for line in old_lines:
        for candidate in iterator:
            if candidate == line:
                break
            inserted.append(candidate)
        else:
            errors.append("S21 removed or edited existing content")
            return errors
    inserted.extend(iterator)
    for line in inserted:
        match = re.match(r"^\| (E\d{3}) \| (T\d{3}) \|", line)
        if match is None or match.group(2) != task_id:
            errors.append(f"S21 closing chỉ được thêm evidence row cho {task_id}: {line[:80]!r}")
    if len(inserted) != 1:
        errors.append("S21 closing phải thêm đúng một evidence row")
    elif old_ids and new_ids and new_ids[-1] != max(old_ids) + 1:
        errors.append("S21 closing evidence ID phải tăng tuần tự")
    return errors


def _change_allowed_s22(old: list[str], new: list[str]) -> list[str]:
    errors: list[str] = []
    if len(old) != len(new):
        return ["S22 changed line count; closing chỉ có thể đổi giá trị derived đã có"]
    parent = ""
    in_yaml = False
    for before, after in zip(old, new):
        if before.strip() == "```yaml":
            in_yaml = True
            parent = ""
            continue
        if in_yaml and before.strip() == "```":
            in_yaml = False
            parent = ""
            continue
        old_match = re.match(r"^(\s*)([A-Za-z_][A-Za-z0-9_-]*):", before)
        if in_yaml and old_match is not None:
            indent, key = old_match.groups()
            if indent == "  ":
                parent = key if key in {"git_delivery", "execution_checkpoint"} else ""
        if before == after:
            continue
        new_match = re.match(r"^(\s*)([A-Za-z_][A-Za-z0-9_-]*):", after)
        if (not in_yaml or old_match is None or new_match is None
                or old_match.groups() != new_match.groups()):
            errors.append("S22 changed YAML structure or content outside its resume mapping")
            continue
        indent, key = old_match.groups()
        full_key = f"{parent}.{key}" if indent == "    " and parent else key
        if full_key not in ALLOWED_S22_KEYS:
            errors.append(f"S22 changed non-derived or authority field {full_key}")
    return errors


def classify_closing_spec_diff(before: str, after: str, *, task_id: str, owning_gate: str) -> list[str]:
    """Classify actual SPEC bytes using row/field leaves, never whole-file exemption."""
    errors: list[str] = []
    before_prefix, before_sections = _section_map(before)
    after_prefix, after_sections = _section_map(after)
    if before_prefix != after_prefix:
        errors.append("SPEC front matter/prefix changed in closing commit")
    if set(before_sections) != set(after_sections):
        errors.append("Closing added or removed SPEC sections")
        return errors
    changed_sections: set[str] = set()
    for section_id in before_sections:
        old, new = before_sections[section_id], after_sections[section_id]
        if old == new:
            continue
        changed_sections.add(section_id)
        if section_id == "S19":
            errors.extend(_change_allowed_s19(old, new, task_id))
        elif section_id == "S20":
            errors.extend(_change_allowed_s20(old, new, owning_gate))
        elif section_id == "S21":
            errors.extend(_change_allowed_s21(old, new, task_id))
        elif section_id == "S22":
            errors.extend(_change_allowed_s22(old, new))
        else:
            errors.append(f"Closing changed non-whitelisted SPEC section S{section_id[1:]}")
    if "S19" not in changed_sections:
        errors.append("Closing phải cập nhật đúng hàng task S19 hiện tại")
    if "S21" not in changed_sections:
        errors.append("Closing phải thêm evidence row S21 cho delivery hiện tại")
    return errors


def validate_git_closing_diff(repo_root: Path, payload_sha: str, closing_sha: str, *, task_id: str, owning_gate: str) -> list[str]:
    errors: list[str] = []
    try:
        parent_line = subprocess.run(
            ["git", "rev-list", "--parents", "-n", "1", closing_sha], cwd=repo_root,
            check=True, capture_output=True, text=True, encoding="utf-8",
        ).stdout.strip().split()
        if len(parent_line) != 2 or parent_line[1] != payload_sha:
            errors.append("Closing commit phải có đúng một direct parent là payload P")
        changed = subprocess.run(
            ["git", "diff-tree", "--no-commit-id", "--name-status", "-r", "--no-renames", payload_sha, closing_sha],
            cwd=repo_root, check=True, capture_output=True, text=True, encoding="utf-8",
        ).stdout.splitlines()
    except (OSError, subprocess.CalledProcessError) as exc:
        return errors + [f"Không thể đọc Git ancestry/diff: {exc}"]
    if not changed:
        return errors + ["Closing diff rỗng; cần closing metadata có reviewable diff"]
    for line in changed:
        parts = line.split("\t")
        if len(parts) != 2 or parts[0] != "M" or parts[1] != SPEC_RELATIVE:
            errors.append(f"CLOSE-DIFF-01 từ chối thay đổi file ngoài SPEC: {line}")
    try:
        old_spec = subprocess.run(["git", "show", f"{payload_sha}:{SPEC_RELATIVE}"], cwd=repo_root,
                                  check=True, capture_output=True).stdout.decode("utf-8")
        new_spec = subprocess.run(["git", "show", f"{closing_sha}:{SPEC_RELATIVE}"], cwd=repo_root,
                                  check=True, capture_output=True).stdout.decode("utf-8")
        message = subprocess.run(["git", "show", "-s", "--format=%B", closing_sha], cwd=repo_root,
                                 check=True, capture_output=True).stdout.decode("utf-8")
    except (OSError, subprocess.CalledProcessError, UnicodeDecodeError) as exc:
        return errors + [f"Không thể đọc SPEC của P/C: {exc}"]
    errors.extend(classify_closing_spec_diff(old_spec, new_spec, task_id=task_id, owning_gate=owning_gate))
    _, sections = _section_map(new_spec)
    task_lines = [line for line in sections.get("S19", []) if line.startswith(f"| {task_id} |")]
    if len(task_lines) != 1:
        errors.append("S19 phải có đúng một hàng cho task hiện tại")
    else:
        cells = _split_table_row(task_lines[0].rstrip("\r\n"))
        if len(cells) == 9:
            evidence = _evidence_fields(cells[8], errors, "S19 closing")
            priority = evidence.get("priority")
            delivery_id = evidence.get("delivery")
            if evidence.get("payload_sha") != payload_sha:
                errors.append("S19 closing payload_sha không khớp actual parent P")
            if not isinstance(priority, str) or priority not in {"P0", "P1", "P2", "P3"}:
                errors.append("S19 current evidence thiếu priority P0–P3")
            else:
                errors.extend(validate_commit_message(message, phase="closing", task_id=task_id, priority=priority))
                if not message.startswith("docs(progress): "):
                    errors.append("Closing subject phải thuộc docs(progress)")
            trailers = dict(_trailers(message.rstrip("\n").splitlines()))
            if trailers.get("MCE4D-Delivery-ID") != delivery_id:
                errors.append("Closing trailer Delivery-ID không khớp S19 delivery")
    return errors


def _command_plan(args: argparse.Namespace) -> int:
    plan_path = Path(args.plan).resolve()
    workflow_path = Path(args.workflow).resolve()
    try:
        plan_bytes = plan_path.read_bytes()
        plan = _strict_json(plan_bytes)
        workflow = workflow_path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError, ValueError) as exc:
        print(f"LỖI: {exc}", file=sys.stderr)
        return 1
    expected = args.sha256 or hashlib.sha256(_canonical_json(plan)).hexdigest()
    errors = validate_check_plan(plan, workflow_text=workflow, expected_sha256=expected)
    raw_hash = hashlib.sha256(plan_bytes).hexdigest()
    canonical_hash = hashlib.sha256(_canonical_json(plan)).hexdigest()
    if raw_hash != canonical_hash:
        errors.append("File plan không chứa đúng bytes canonical MCE-MANIFEST-JSON1")
    if errors:
        for error in errors:
            print(f"LỖI: {error}", file=sys.stderr)
        return 1
    print(f"ĐẠT: GitCheckPlan schema+semantic+workflow; SHA-256 {canonical_hash}; {len(plan['checks'])} check contexts.")
    return 0


def _command_record(args: argparse.Namespace) -> int:
    if __package__:
        from tools.spec_check.schema import validate_governance_file
    else:
        sys.path.insert(0, str(ROOT))
        from tools.spec_check.schema import validate_governance_file
    errors = validate_governance_file(Path(args.spec).resolve(), Path(args.record).resolve(), args.kind)
    if errors:
        for error in errors:
            print(f"LỖI: {error}", file=sys.stderr)
        return 1
    print(f"ĐẠT: record {args.kind} đúng schema và semantic cục bộ; chưa phải GitHub/live observer proof.")
    return 0


def _command_closing(args: argparse.Namespace) -> int:
    errors = validate_git_closing_diff(Path(args.repo).resolve(), args.payload, args.closing,
                                      task_id=args.task, owning_gate=args.gate)
    if errors:
        for error in errors:
            print(f"LỖI: {error}", file=sys.stderr)
        return 1
    print(f"ĐẠT: CLOSE-DIFF-01 cho {args.task}, parent P={args.payload}; chỉ metadata leaf được phép.")
    return 0


def _main(argv: list[str] | None = None) -> int:
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="backslashreplace")
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    plan_parser = subparsers.add_parser("check-plan", help="kiểm plan và workflow đã freeze")
    plan_parser.add_argument("--plan", default=str(ROOT / "tools/ci/check-plan.json"))
    plan_parser.add_argument("--workflow", default=str(ROOT / ".github/workflows/bootstrap.yml"))
    plan_parser.add_argument("--sha256")
    plan_parser.set_defaults(handler=_command_plan)
    record_parser = subparsers.add_parser("check-record", help="kiểm một record governance S21")
    record_parser.add_argument("--spec", default=str(SPEC_PATH))
    record_parser.add_argument("--record", required=True)
    record_parser.add_argument("--kind", required=True)
    record_parser.set_defaults(handler=_command_record)
    diff_parser = subparsers.add_parser("closing-diff", help="kiểm leaf diff P→C theo CLOSE-DIFF-01")
    diff_parser.add_argument("--repo", default=str(ROOT))
    diff_parser.add_argument("--payload", required=True)
    diff_parser.add_argument("--closing", required=True)
    diff_parser.add_argument("--task", required=True)
    diff_parser.add_argument("--gate", required=True)
    diff_parser.set_defaults(handler=_command_closing)
    args = parser.parse_args(argv)
    return args.handler(args)


if __name__ == "__main__":
    raise SystemExit(_main())
