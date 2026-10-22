from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path.cwd().resolve()
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from tools.spec_check.spec import _effective_status, _section, _table_with_headers

SPEC = ROOT / "Maps-Context-Engine-4D_SPEC.md"
text = SPEC.read_text(encoding="utf-8")
lines = text.splitlines()

# Preserve the original rows byte-for-byte, but put the recent evidence into the contiguous S21 evidence table.
recent: dict[str, str] = {}
for eid in ("E738", "E739", "E740", "E741", "E742", "E743"):
    matches = [line for line in lines if line.startswith(f"| {eid} |")]
    if len(matches) != 1:
        raise SystemExit(f"expected exactly one retained {eid} row; got {len(matches)}")
    recent[eid] = matches[0]
lines = [line for line in lines if not any(line.startswith(f"| {eid} |") for eid in recent)]
insert_after = next((i for i, line in enumerate(lines) if line.startswith("| E737 | T067 |")), None)
if insert_after is None:
    raise SystemExit("E737 evidence table row is missing")
for offset, eid in enumerate(recent):
    lines.insert(insert_after + 1 + offset, recent[eid])
text = "\n".join(lines) + "\n"

def cells(line: str) -> list[str]:
    return [part.strip() for part in line.strip()[1:-1].split("|")]

def render(parts: list[str]) -> str:
    return "| " + " | ".join(parts) + " |"

def update_evidence(raw: str, *, add_id: str | None = None, updates: dict[str, str] | None = None) -> str:
    parts = raw.split(";")
    id_parts: list[str] = []
    index = 0
    ids: set[str] = set()
    for index, part in enumerate(parts):
        tokens = [token.strip() for token in part.split(",")]
        if not tokens or not all(re.fullmatch(r"E\d{3}", token) for token in tokens):
            break
        id_parts.append(part)
        ids.update(tokens)
    else:
        index = len(parts)
    if index == len(parts):
        fields = []
    else:
        fields = parts[index:]
    if add_id and add_id not in ids:
        id_parts.append(add_id)
    field_index: dict[str, int] = {}
    for i, part in enumerate(fields):
        key, sep, _ = part.partition("=")
        if not sep or not key or key in field_index:
            raise SystemExit(f"bad/duplicate evidence field while repairing: {part!r}")
        field_index[key] = i
    for key, value in (updates or {}).items():
        if key in field_index:
            fields[field_index[key]] = f"{key}={value}"
        else:
            fields.append(f"{key}={value}")
    return ";".join([*id_parts, *fields])

lines = text.splitlines()
for i, line in enumerate(lines):
    if line.startswith("| T067 |"):
        row = cells(line)
        row[8] = update_evidence(row[8], add_id="E742")
        lines[i] = render(row)
    elif line.startswith("| T068 |"):
        row = cells(line)
        evidence = row[8]
        match = re.search(r"(?:^|;)plan_sha256=([0-9a-f]{64})(?:;|$)", evidence)
        if not match:
            raise SystemExit("T068 plan_sha256 is missing")
        row[8] = update_evidence(evidence, updates={"plan": match.group(1)})
        lines[i] = render(row)

text = "\n".join(lines) + "\n"
section = _section(text, "## S19 — Bảng triển khai duy nhất") or ""
table = _table_with_headers(section, "Task", "Gate", "Status", "Evidence")
if table is None:
    raise SystemExit("S19 unreadable after repair")
done = sum(_effective_status(row) == "DONE" for row in table.rows)
s22_start = text.index("## S22 — Trạng thái tiếp tục cho phiên AI sau")
s23_start = text.index("\n## S23 —", s22_start)
s22 = text[s22_start:s23_start]
s22, count = re.subn(r"(?m)^(  completed_scope_tasks:)\s*.*$", rf"\1 {done}", s22, count=1)
if count != 1:
    raise SystemExit("S22 completed_scope_tasks field missing")
text = text[:s22_start] + s22 + text[s23_start:]
SPEC.write_text(text, encoding="utf-8", newline="\n")
print(f"Repaired S21 E738-E743 ordering, T067 closure reference, T068 plan alias; derived DONE count={done}.")
