"""Đọc các bảng authority của SPEC v1.6.13 và kiểm tra liên kết hai chiều."""

from __future__ import annotations

import re
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

from .ci_order import OrderingError, parse_instant
from .schema import SchemaError, extract_governance_schema


SPEC_VERSION = "1.6.13"
TASK_COUNT = 108
REQUIREMENT_COUNT = 130
CASE_COUNT = 195
SECTION_COUNT = 54
REQUIREMENT_TASK_EDGE_COUNT = 387
TASK_PATTERN = re.compile(r"^T\d{3}$")
REQ_PATTERN = re.compile(r"^R\d{3}$")
CASE_PATTERN = re.compile(r"^C\d{3}$")
CASE_TOKEN = re.compile(r"C(\d{3})(?:\s*[-–—]\s*C?(\d{3}))?")


@dataclass(frozen=True)
class Table:
    headers: tuple[str, ...]
    rows: tuple[dict[str, str], ...]


def _section(text: str, heading: str) -> str | None:
    lines = text.splitlines()
    section_id = re.fullmatch(r"##\s+(S\d{2})", heading)
    start = next((
        i for i, line in enumerate(lines)
        if line.strip() == heading or (
            section_id is not None
            and re.match(rf"^##\s+{section_id.group(1)}(?:\s|—|$)", line.strip()) is not None
        )
    ), None)
    if start is None:
        return None
    for end in range(start + 1, len(lines)):
        if re.match(r"^##\s+", lines[end]):
            return "\n".join(lines[start:end])
    return "\n".join(lines[start:])


def _split_row(line: str) -> list[str]:
    body = line.strip()
    if body.startswith("|"):
        body = body[1:]
    if body.endswith("|"):
        body = body[:-1]
    cells: list[str] = []
    current: list[str] = []
    escaped = False
    for char in body:
        if escaped:
            current.append(char)
            escaped = False
        elif char == "\\":
            escaped = True
        elif char == "|":
            cells.append("".join(current).strip())
            current = []
        else:
            current.append(char)
    if escaped:
        current.append("\\")
    cells.append("".join(current).strip())
    return cells


def _is_separator(line: str) -> bool:
    cells = _split_row(line)
    return bool(cells) and all(re.fullmatch(r":?-{3,}:?", cell.replace(" ", "")) for cell in cells)


def _tables(text: str) -> list[Table]:
    lines = text.splitlines()
    found: list[Table] = []
    index = 0
    while index + 1 < len(lines):
        if lines[index].lstrip().startswith("|") and _is_separator(lines[index + 1]):
            headers = tuple(_split_row(lines[index]))
            index += 2
            rows: list[dict[str, str]] = []
            while index < len(lines):
                if not lines[index].strip():
                    next_row = index
                    while next_row < len(lines) and not lines[next_row].strip():
                        next_row += 1
                    if (next_row < len(lines) and lines[next_row].lstrip().startswith("|")
                            and not (next_row + 1 < len(lines) and _is_separator(lines[next_row + 1]))
                            and len(_split_row(lines[next_row])) == len(headers)):
                        index = next_row
                    else:
                        break
                if not lines[index].lstrip().startswith("|") or _is_separator(lines[index]):
                    break
                cells = _split_row(lines[index])
                if len(cells) == len(headers):
                    rows.append(dict(zip(headers, cells)))
                index += 1
            found.append(Table(headers, tuple(rows)))
        else:
            index += 1
    return found


def _table_with_headers(text: str, *required: str) -> Table | None:
    wanted = set(required)
    return next((table for table in _tables(text) if wanted.issubset(table.headers)), None)


def _ids(value: str, pattern: re.Pattern[str]) -> set[str]:
    token_pattern = pattern.pattern.removeprefix("^").removesuffix("$")
    return set(re.findall(token_pattern, value))


def _cases(value: str) -> set[str]:
    result: set[str] = set()
    for match in CASE_TOKEN.finditer(value):
        first = int(match.group(1))
        last = int(match.group(2) or match.group(1))
        if last < first or last - first > 500:
            continue
        result.update(f"C{number:03d}" for number in range(first, last + 1))
    return result


def _ranges(value: str, prefix: str) -> set[str]:
    result: set[str] = set()
    token = re.compile(rf"{re.escape(prefix)}(\d{{3}})(?:\s*[-–—]\s*{re.escape(prefix)}?(\d{{3}}))?")
    for match in token.finditer(value):
        first = int(match.group(1))
        last = int(match.group(2) or match.group(1))
        if last >= first and last - first <= 500:
            result.update(f"{prefix}{number:03d}" for number in range(first, last + 1))
    return result


def _record_error(errors: list[str], message: str) -> None:
    errors.append(message)


def _effective_status(row: dict[str, str]) -> str:
    status = row.get("Status", "").strip()
    if status != "DONE":
        return status
    fields = _evidence_fields(row.get("Evidence", ""))
    sha_pattern = re.compile(r"^(?:[0-9a-f]{40}|[0-9a-f]{64})$")
    if not all(sha_pattern.fullmatch(fields.get(key, "")) for key in ("payload_sha", "closing_sha")):
        return "VERIFY"
    if not fields.get("payload_CI", "").startswith("PASS") or not fields.get("closing_CI", "").startswith("PASS"):
        return "VERIFY"
    if not fields.get("archive", "").startswith(("PASS", "sealed-readback-PASS")):
        return "VERIFY"
    if not fields.get("live", "").endswith("PASS"):
        return "VERIFY"
    return "DONE"


def _evidence_fields(value: str) -> dict[str, str]:
    fields: dict[str, str] = {}
    for item in value.split(";")[1:]:
        key, separator, field_value = item.partition("=")
        if separator and key and key not in fields:
            fields[key] = field_value
    return fields


def validate_spec_text(text: str) -> list[str]:
    errors: list[str] = []
    normalized = text.replace("\r\n", "\n")
    if not normalized.startswith("---\n"):
        errors.append("Thiếu YAML front matter")
    version = re.search(r"(?m)^spec_version:\s*([^\s]+)\s*$", normalized)
    if version is None or version.group(1) != SPEC_VERSION:
        errors.append(f"spec_version phải là {SPEC_VERSION}")
    if not re.search(r"(?m)^#\s+\S", normalized):
        errors.append("Thiếu tiêu đề cấp một")
    section_ids = re.findall(r"(?m)^##\s+S\d{2}(?:\s|—|$)", normalized)
    if len(section_ids) != SECTION_COUNT:
        errors.append(f"SPEC v{SPEC_VERSION} cần {SECTION_COUNT} section authority, thấy {len(section_ids)}")

    fence_count = len(re.findall(r"(?m)^\s*(```+|~~~+)\s*[^\n]*$", normalized))
    # Blocks can use longer markers. Match by marker token and require each run paired.
    active: tuple[str, int] | None = None
    for line_number, line in enumerate(normalized.splitlines(), 1):
        match = re.match(r"^\s*(`{3,}|~{3,})(.*)$", line)
        if not match:
            continue
        marker, suffix = match.group(1), match.group(2)
        if active is None:
            active = (marker[0], len(marker))
        elif marker[0] == active[0] and len(marker) >= active[1] and not suffix.strip():
            active = None
    if active is not None or fence_count % 2:
        errors.append("Khối fenced code chưa được đóng cân bằng")

    required_sections = ("## S00", "## S01", "## S17", "## S19 — Bảng triển khai duy nhất",
                         "## S20 — Gates và Definition of Done", "## S21 — ADR, evidence, gaps và lịch sử thực thi",
                         "## S50",
                         "## S22 — Trạng thái tiếp tục cho phiên AI sau", "## S47 — Thứ tự thi công chính xác và phiếu việc",
                         "## S48 — Kiểm chứng bản vẽ, oracle và bàn giao", "## S53 — Thi công tự chủ108 task và nghiệm thu người dùng một lần")
    sections: dict[str, str] = {}
    for heading in required_sections:
        section = _section(normalized, heading)
        if section is None:
            errors.append(f"Thiếu section authority: {heading}")
        else:
            sections[heading] = section

    req_table = _table_with_headers(sections.get("## S01", ""), "ID", "Task")
    task_table = _table_with_headers(sections.get("## S19 — Bảng triển khai duy nhất", ""), "Task", "Gate", "Dependencies", "Req", "Cases", "Status", "Owner", "Evidence")
    case_heading = "### Acceptance case catalog"
    case_start = normalized.find(case_heading)
    case_section = normalized[case_start:] if case_start >= 0 else ""
    next_sub = re.search(r"(?m)^###\s+", case_section[len(case_heading):])
    if next_sub:
        case_section = case_section[:len(case_heading) + next_sub.start()]
    case_table = _table_with_headers(case_section, "Case", "Oracle bắt buộc")
    card_section = sections.get("## S47 — Thứ tự thi công chính xác và phiếu việc", "")
    card_table = _table_with_headers(card_section, "Seq", "Task", "Owned production paths / allowed test artifacts", "Oracle / gate evidence")
    gate_table = _table_with_headers(sections.get("## S20 — Gates và Definition of Done", ""), "Gate", "Phạm vi task", "Status ban đầu")

    if req_table is None:
        errors.append("Không tìm thấy bảng requirement R trong S01")
    if task_table is None:
        errors.append("Không tìm thấy bảng task S19")
    if case_table is None:
        errors.append("Không tìm thấy Acceptance case catalog")
    if card_table is None:
        errors.append("Không tìm thấy bảng work card S47")
    if gate_table is None:
        errors.append("Không tìm thấy bảng gate S20")
    if any(table is None for table in (req_table, task_table, case_table, card_table, gate_table)):
        return errors

    if req_table is None or task_table is None or case_table is None or card_table is None or gate_table is None:
        return errors
    requirements: dict[str, dict[str, str]] = {}
    for row in req_table.rows:
        key = row.get("ID", "").strip()
        if not REQ_PATTERN.fullmatch(key):
            continue
        if key in requirements:
            errors.append(f"Requirement bị lặp: {key}")
        requirements[key] = row
    tasks: dict[str, dict[str, str]] = {}
    for row in task_table.rows:
        key = row.get("Task", "").strip()
        if not TASK_PATTERN.fullmatch(key):
            continue
        if key in tasks:
            errors.append(f"Task bị lặp: {key}")
        tasks[key] = row
    cases: set[str] = set()
    for row in case_table.rows:
        key = row.get("Case", "").strip()
        if not CASE_PATTERN.fullmatch(key):
            continue
        if key in cases:
            errors.append(f"Case bị lặp: {key}")
        cases.add(key)
    cards: dict[str, dict[str, str]] = {}
    for row in card_table.rows:
        key = row.get("Task", "").strip()
        if not TASK_PATTERN.fullmatch(key):
            continue
        if key in cards:
            errors.append(f"Work card bị lặp: {key}")
        cards[key] = row

    expected_tasks = {f"T{number:03d}" for number in range(1, TASK_COUNT + 1)}
    if set(tasks) != expected_tasks:
        errors.append(f"S19 phải chứa đúng {TASK_COUNT} task T001–T108; thiếu={sorted(expected_tasks-set(tasks))[:6]}, thừa={sorted(set(tasks)-expected_tasks)[:6]}")
    if set(cards) != set(tasks):
        errors.append(f"S47/S19 task parity sai; thiếu card={sorted(set(tasks)-set(cards))[:6]}, card thừa={sorted(set(cards)-set(tasks))[:6]}")
    if len(requirements) != len(req_table.rows):
        errors.append("Bảng S01 có dòng requirement không mang ID Rnnn")
    if len(requirements) != REQUIREMENT_COUNT:
        errors.append(f"SPEC v{SPEC_VERSION} cần {REQUIREMENT_COUNT} requirements, thấy {len(requirements)}")
    if len(cases) != len(case_table.rows):
        errors.append("Acceptance catalog có dòng case không mang ID Cnnn")
    if len(cases) != CASE_COUNT:
        errors.append(f"SPEC v{SPEC_VERSION} cần {CASE_COUNT} acceptance cases, thấy {len(cases)}")

    evidence_section = sections.get("## S21 — ADR, evidence, gaps và lịch sử thực thi", "")
    evidence_headers = {"Evidence ID", "Task", "Result", "Checked at UTC"}
    evidence_tables = [table for table in _tables(evidence_section) if evidence_headers.issubset(table.headers)]
    evidence_ids: set[str] = set()
    evidence_by_task: dict[str, list[dict[str, str]]] = defaultdict(list)
    numeric_evidence: list[int] = []
    if not evidence_tables:
        errors.append("S21 thiếu evidence log")
    else:
        for row in (row for table in evidence_tables for row in table.rows):
            evidence_id = row.get("Evidence ID", "").strip()
            task_id = row.get("Task", "").strip()
            if evidence_id in evidence_ids:
                errors.append(f"Evidence ID bị lặp: {evidence_id}")
            evidence_ids.add(evidence_id)
            if re.fullmatch(r"E\d{3}", evidence_id):
                numeric_evidence.append(int(evidence_id[1:]))
            elif not re.fullmatch(r"E-T\d{3}-LOCAL-\d{2}", evidence_id):
                errors.append(f"Evidence ID sai định dạng: {evidence_id!r}")
            if task_id not in tasks:
                errors.append(f"{evidence_id} trỏ tới task không tồn tại: {task_id!r}")
            else:
                evidence_by_task[task_id].append(row)
            result = row.get("Result", "").strip()
            if not result.startswith(("PASS", "FAIL", "NOT_RUN")):
                errors.append(f"{evidence_id} có Result không hợp lệ")
            elif result.startswith("NOT_RUN") and len(result) == len("NOT_RUN"):
                errors.append(f"{evidence_id} NOT_RUN cần lý do")
            timestamp = row.get("Checked at UTC", "").strip()
            try:
                parse_instant(timestamp)
            except (OrderingError, TypeError, ValueError):
                errors.append(f"{evidence_id} Checked at UTC không phải RFC3339 instant")
        if numeric_evidence != sorted(numeric_evidence) or len(numeric_evidence) != len(set(numeric_evidence)):
            errors.append("S21 numeric Evidence ID phải tăng tuần tự, không lặp")

    gates = {row.get("Gate", "").split(" — ", 1)[0].strip() for row in gate_table.rows}
    gate_aliases = {"R2": "G6", "R3": "G7", "U0": "GU0", "U1": "GU1", "U2": "GU2", "U3": "GU3"}
    req_edges_from_requirements: set[tuple[str, str]] = set()
    for req_id, row in requirements.items():
        listed = _ids(row.get("Task", ""), TASK_PATTERN)
        unknown = listed - set(tasks)
        if unknown:
            errors.append(f"{req_id} tham chiếu task chưa tồn tại: {sorted(unknown)}")
        req_edges_from_requirements.update((req_id, task_id) for task_id in listed)
    req_edges_from_tasks: set[tuple[str, str]] = set()
    dependency_map: dict[str, set[str]] = {}
    task_cases: dict[str, set[str]] = {}
    statuses = {"TODO", "IN_PROGRESS", "BLOCKED", "VERIFY", "DONE", "DEFERRED", "CANCELLED"}
    for task_id, row in tasks.items():
        listed_reqs = _ids(row.get("Req", ""), REQ_PATTERN)
        unknown_reqs = listed_reqs - set(requirements)
        if unknown_reqs:
            errors.append(f"{task_id} tham chiếu requirement chưa tồn tại: {sorted(unknown_reqs)}")
        req_edges_from_tasks.update((req_id, task_id) for req_id in listed_reqs)
        deps = _ids(row.get("Dependencies", ""), TASK_PATTERN)
        if "NONE" in row.get("Dependencies", "").upper():
            deps = set()
        missing_deps = deps - set(tasks)
        if missing_deps:
            errors.append(f"{task_id} tham chiếu dependency chưa tồn tại: {sorted(missing_deps)}")
        dependency_map[task_id] = deps
        referenced_cases = _cases(row.get("Cases", ""))
        task_cases[task_id] = referenced_cases
        missing_cases = referenced_cases - cases
        if missing_cases:
            errors.append(f"{task_id} tham chiếu case chưa tồn tại: {sorted(missing_cases)[:8]}")
        gate = row.get("Gate", "").split(" — ", 1)[0].strip()
        if gate not in gates and gate_aliases.get(gate) not in gates:
            errors.append(f"{task_id} dùng gate chưa khai báo: {gate}")
        status = row.get("Status", "").strip()
        if status not in statuses:
            errors.append(f"{task_id} có status không hợp lệ: {status!r}")
        if status == "DONE":
            evidence = row.get("Evidence", "").strip()
            required = ("delivery=", "payload_sha=", "closing_sha=", "payload_CI=PASS")
            if evidence.upper() in {"", "NONE", "NOT_RUN"} or any(token not in evidence for token in required):
                errors.append(f"{task_id} candidate DONE thiếu evidence delivery/P/C/CI")
            references = set(re.findall(r"(?<![A-Za-z0-9-])E\d{3}(?!\d)", evidence))
            missing_evidence = references - evidence_ids
            if missing_evidence:
                errors.append(f"{task_id} tham chiếu evidence chưa tồn tại: {sorted(missing_evidence)}")
            if _effective_status(row) == "DONE" and not any(
                    item.get("Result", "").startswith("PASS") for item in evidence_by_task.get(task_id, [])):
                errors.append(f"{task_id} effective DONE thiếu S21 evidence record PASS")
        else:
            references = set(re.findall(r"(?<![A-Za-z0-9-])E\d{3}(?!\d)", row.get("Evidence", "")))
            missing_evidence = references - evidence_ids
            if missing_evidence:
                errors.append(f"{task_id} tham chiếu evidence chưa tồn tại: {sorted(missing_evidence)}")
        card = cards.get(task_id)
        if card is not None:
            card_cases = _cases(card.get("Oracle / gate evidence", ""))
            if card_cases != referenced_cases:
                errors.append(f"{task_id} S19↔S47 case/card parity sai; tracker-only={sorted(referenced_cases-card_cases)[:6]}, card-only={sorted(card_cases-referenced_cases)[:6]}")
            if not card.get("Owned production paths / allowed test artifacts", "").strip():
                errors.append(f"{task_id} work card thiếu owned paths")
    if req_edges_from_requirements != req_edges_from_tasks:
        errors.append(f"Requirement↔task parity sai; S01-only={sorted(req_edges_from_requirements-req_edges_from_tasks)[:8]}, S19-only={sorted(req_edges_from_tasks-req_edges_from_requirements)[:8]}")
    if len(req_edges_from_tasks) != REQUIREMENT_TASK_EDGE_COUNT:
        errors.append(f"SPEC v{SPEC_VERSION} cần {REQUIREMENT_TASK_EDGE_COUNT} requirement↔task edges, thấy {len(req_edges_from_tasks)}")

    track_counts: dict[str, int] = defaultdict(int)
    for row in tasks.values():
        track = row.get("Gate", "").split(" — ", 1)[0].strip()
        track_counts[track] += 1
    expected_track_counts = {"G0": 7, "G1": 9, "G2": 11, "G3": 12, "G4": 11, "G5": 2,
                             "R2": 20, "R3": 12, "U0": 4, "U1": 10, "U2": 6, "U3": 4}
    if track_counts != expected_track_counts:
        errors.append(f"Phân bổ task theo gate/track sai: {dict(sorted(track_counts.items()))}")

    # S20 is the explicit membership authority; resolve R2/R3/U labels to gates.
    expected_gate_members: dict[str, set[str]] = defaultdict(set)
    for task_id, row in tasks.items():
        track = row.get("Gate", "").split(" — ", 1)[0].strip()
        expected_gate_members[gate_aliases.get(track, track)].add(task_id)
    actual_gate_members: dict[str, set[str]] = defaultdict(set)
    for row in gate_table.rows:
        gate = row.get("Gate", "").split(" — ", 1)[0].strip()
        actual_gate_members[gate] |= _ranges(row.get("Phạm vi task", ""), "T")
    for gate in set(expected_gate_members) | set(actual_gate_members):
        if expected_gate_members.get(gate, set()) != actual_gate_members.get(gate, set()):
            errors.append(f"S19/S20 gate ownership mismatch {gate}: tracker-only={sorted(expected_gate_members.get(gate,set())-actual_gate_members.get(gate,set()))[:5]}, S20-only={sorted(actual_gate_members.get(gate,set())-expected_gate_members.get(gate,set()))[:5]}")

    # DAG validation catches cycles even when the schedule itself is a permutation.
    visiting: set[str] = set()
    visited: set[str] = set()
    def visit(task_id: str) -> None:
        if task_id in visiting:
            errors.append(f"Dependency cycle chứa {task_id}")
            return
        if task_id in visited:
            return
        visiting.add(task_id)
        for dependency in dependency_map.get(task_id, set()):
            visit(dependency)
        visiting.remove(task_id)
        visited.add(task_id)
    for task_id in tasks:
        visit(task_id)

    phase_start = card_section.find("### Lịch theo phase")
    phase_end = card_section.find("### Phiếu thi công từng task")
    schedule_section = card_section[phase_start:phase_end] if phase_start >= 0 and phase_end > phase_start else ""
    schedule_table = _table_with_headers(schedule_section, "Phase", "Thứ tự bắt buộc", "Điểm đóng")
    scheduled: list[str] = []
    if schedule_table is None:
        errors.append("Không tìm thấy bảng thứ tự theo phase trong S47")
    else:
        for row in schedule_table.rows:
            scheduled.extend(re.findall(r"T\d{3}", row.get("Thứ tự bắt buộc", "")))
        if len(scheduled) != len(set(scheduled)):
            errors.append("Schedule lặp task")
        if set(scheduled) != set(tasks):
            errors.append(f"Schedule phải là hoán vị của S19; thiếu={sorted(set(tasks)-set(scheduled))[:6]}, thừa={sorted(set(scheduled)-set(tasks))[:6]}")
        card_order = [row.get("Task", "").strip() for row in sorted(cards.values(), key=lambda row: int(row.get("Seq", "0")))]
        expected_sequence = [f"{number:03d}" for number in range(1, TASK_COUNT + 1)]
        if sorted(row.get("Seq", "") for row in cards.values()) != expected_sequence:
            errors.append("S47 work-card Seq phải là 001–108 không lặp")
        if card_order != scheduled:
            errors.append("S47 work-card sequence phải khớp topological phase schedule")
        position = {task_id: index for index, task_id in enumerate(scheduled)}
        for task_id, dependencies in dependency_map.items():
            for dependency in dependencies:
                if dependency in position and task_id in position and position[dependency] >= position[task_id]:
                    errors.append(f"Schedule đặt {task_id} trước prerequisite {dependency}")

    try:
        schema = extract_governance_schema(normalized)
        defs = schema.get("$defs", {})
        if len(defs) != 24:
            errors.append(f"S21 cần 24 governance definitions; thấy {len(defs)}")
        for name, definition in defs.items():
            if definition.get("additionalProperties") is not False:
                errors.append(f"Governance definition {name} không đóng additionalProperties")
            for ref in re.findall(r"\"\$ref\"\s*:\s*\"([^\"]+)\"", str(definition)):
                if not ref.startswith("#/$defs/"):
                    errors.append(f"Schema ref phải local: {name} -> {ref}")
    except SchemaError as exc:
        errors.append(f"S21 governance schema không hợp lệ: {exc}")

    # S22 remains a resume cache; it cannot claim final completion before all tasks close.
    final_match = re.search(r"(?m)^\s*final_engineering_status:\s*([^\s#]+)", normalized)
    if final_match:
        effective_done = sum(_effective_status(row) == "DONE" for row in tasks.values())
        if final_match.group(1) in {"ENGINEERING_COMPLETE_AWAITING_USER_ACCEPTANCE", "COMPLETE"} and effective_done != TASK_COUNT:
            errors.append(f"S22 claim hoàn tất khi effective DONE={effective_done}/{TASK_COUNT}")

    policy_section = _section(normalized, "## S50") or ""
    policy_table = _table_with_headers(policy_section, "Policy ID", "Đường dẫn dự kiến", "Version nội dung", "SHA-256 bytes gốc được cung cấp", "Vai trò/kích hoạt")
    expected_policies = {
        "POL-SRC-01": ("docs/AI_RULES.md", "3.1", "1662406d9ac9e63646130516ad6811934b2bd7f0a6b7d240403749200777f9e3"),
        "POL-SRC-02": ("docs/AI_RULES_STORAGE_SUPPLEMENT.md", "1.0.0", "eea81c9ac63344efde5f1eccb30e43d06146875aabaf8396fec3b46e5e878dac"),
        "POL-SRC-03": ("docs/PROJECT_BOOTSTRAP_PROMPT.txt", "3.1", "b1d692184f9b4bac87f6b55b25e1c49bab90af5d5eb1706a428a09d85212b686"),
    }
    if policy_table is None:
        errors.append("S50 thiếu POL-02 nguồn chính sách")
    else:
        policies = {row.get("Policy ID", "").strip(): row for row in policy_table.rows if row.get("Policy ID", "").startswith("POL-SRC-")}
        if set(policies) != set(expected_policies):
            errors.append("S50 phải chứa đúng ba POL-SRC nguồn đã tiếp nhận")
        for policy_id, (path, policy_version, digest) in expected_policies.items():
            row = policies.get(policy_id)
            if row is None:
                continue
            actual = (row.get("Đường dẫn dự kiến", "").strip().strip("`"), row.get("Version nội dung", "").strip(), row.get("SHA-256 bytes gốc được cung cấp", "").strip().strip("`"))
            if actual != (path, policy_version, digest):
                errors.append(f"S50 source ledger drift: {policy_id}")
    mode_table = _table_with_headers(policy_section, "task_mode", "Cho phép theo quyền thực tế", "Không tự cho phép")
    if mode_table is None:
        errors.append("S50 thiếu bảng task_mode")
    else:
        modes = {row.get("task_mode", "").strip() for row in mode_table.rows}
        if modes != {"docs_only", "implement", "review", "integrate"}:
            errors.append("S50 task_mode enum drift")
    required_resume = {
        "spec_version": "1.6.13",
        "last_request_mode": "implement_full_108",
        "next_task_mode": "implement",
        "autonomous_contract": "S53",
        "git_delivery_policy": "S53_AW_08",
        "intermediate_user_approval": "false",
        "final_user_acceptance": "once_after_engineering_complete",
    }
    for key, expected in required_resume.items():
        match = re.search(rf"(?m)^\s+{re.escape(key)}:\s*([^\s#]+)", normalized)
        if match is None or match.group(1) != expected:
            errors.append(f"S22 resume contract sai hoặc thiếu: {key}={expected}")

    return errors


def validate_spec(path: Path) -> list[str]:
    try:
        return validate_spec_text(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError) as exc:
        return [f"Không đọc được SPEC UTF-8: {exc}"]
