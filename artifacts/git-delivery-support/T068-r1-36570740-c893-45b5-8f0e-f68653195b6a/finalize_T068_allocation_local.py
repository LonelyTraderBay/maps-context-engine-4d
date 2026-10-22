from __future__ import annotations

import hashlib
import json
import re
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd().resolve()
TASK, DELIVERY = "T068", "36570740-c893-45b5-8f0e-f68653195b6a"
SUPPORT = f"artifacts/git-delivery-support/T068-r1-{DELIVERY}"
CHECK_REF = f"{SUPPORT}/allocation-check-r1.json"
PREFLIGHT_REF = f"{SUPPORT}/allocation-preflight-r1.json"
SUMMARY_REF = f"{SUPPORT}/local-checks/allocation/attempt1/summary.json"
SPEC = ROOT / "Maps-Context-Engine-4D_SPEC.md"


def sha(ref: str) -> str:
    return hashlib.sha256((ROOT / ref).read_bytes()).hexdigest()


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


check = json.loads((ROOT / CHECK_REF).read_text(encoding="utf-8"))
preflight = json.loads((ROOT / PREFLIGHT_REF).read_text(encoding="utf-8"))
summary = json.loads((ROOT / SUMMARY_REF).read_text(encoding="utf-8"))
if any(doc.get("result") != "PASS" for doc in (check, preflight, summary)):
    raise SystemExit("allocation preflight records are not all PASS")
if any(doc.get("passed_count") != 7 or doc.get("failed_count") != 0 for doc in (check, preflight, summary)):
    raise SystemExit("allocation preflight did not pass exactly 7/7")
if any(doc.get("task_id") != TASK or doc.get("delivery_id") != DELIVERY for doc in (check, preflight, summary)):
    raise SystemExit("allocation preflight identity mismatch")

lines = SPEC.read_text(encoding="utf-8").splitlines()
found = False
for i, line in enumerate(lines):
    if line.startswith("| T068 |"):
        cells = [item.strip() for item in line.strip()[1:-1].split("|")]
        parts = cells[8].split(";")
        fields: dict[str, tuple[int, str]] = {}
        for j, part in enumerate(parts):
            if "=" in part:
                key, value = part.split("=", 1)
                if key in fields:
                    raise SystemExit(f"duplicate T068 field {key}")
                fields[key] = (j, value)
        updates = {
            "allocation_check": "PASS(7/7 local)",
            "allocation_local": "PASS(7/7)",
            "allocation_check_ref": CHECK_REF,
            "allocation_check_sha256": sha(CHECK_REF),
            "allocation_preflight_ref": PREFLIGHT_REF,
            "allocation_preflight_sha256": sha(PREFLIGHT_REF),
            "local_check_summary_ref": SUMMARY_REF,
            "local_check_summary_sha256": sha(SUMMARY_REF),
            "result": "PASS(7/7 allocation local)",
        }
        for key, value in updates.items():
            if key not in fields:
                raise SystemExit(f"missing T068 field {key}")
            parts[fields[key][0]] = f"{key}={value}"
        cells[8] = ";".join(parts)
        lines[i] = "| " + " | ".join(cells) + " |"
        found = True
        break
if not found:
    raise SystemExit("T068 S19 row missing")

for i, line in enumerate(lines):
    if line.startswith("| E743 | T068 |"):
        cells = [item.strip() for item in line.strip()[1:-1].split("|")]
        if len(cells) != 8:
            raise SystemExit("E743 S21 row shape changed")
        cells[5] = "PASS — allocation preflight đạt 7/7 local; SPEC, 142 fixture, 33 delivery-policy và dependency proof đều đúng."
        additional = [
            f"{SUMMARY_REF} sha256:{sha(SUMMARY_REF)}",
            f"{CHECK_REF} sha256:{sha(CHECK_REF)}",
            f"{PREFLIGHT_REF} sha256:{sha(PREFLIGHT_REF)}",
        ]
        existing = cells[6]
        cells[6] = existing + "; " + "; ".join(additional)
        cells[7] = now()
        lines[i] = "| " + " | ".join(cells) + " |"
        break
else:
    raise SystemExit("E743 S21 row missing")

SPEC.write_text("\n".join(lines) + "\n", encoding="utf-8", newline="\n")
print(json.dumps({
    "result": "PASS",
    "allocation_check_ref": CHECK_REF,
    "allocation_check_sha256": sha(CHECK_REF),
    "allocation_preflight_ref": PREFLIGHT_REF,
    "allocation_preflight_sha256": sha(PREFLIGHT_REF),
    "local_summary_ref": SUMMARY_REF,
    "local_summary_sha256": sha(SUMMARY_REF),
}, ensure_ascii=True, sort_keys=True))
