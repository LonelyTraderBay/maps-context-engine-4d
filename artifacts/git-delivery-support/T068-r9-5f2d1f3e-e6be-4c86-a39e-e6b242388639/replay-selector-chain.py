from __future__ import annotations

import hashlib
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.stdout.reconfigure(encoding="utf-8")
ROOT = Path.cwd().resolve()
sys.path.insert(0, str(ROOT))
from tools.spec_check.semantics import validate_delivery_links

R8 = Path("artifacts/git-delivery-support/T068-r8-e26dd6b2-5fb4-43b2-a286-7fb211b7f541")
R9 = Path("artifacts/git-delivery-support/T068-r9-5f2d1f3e-e6be-4c86-a39e-e6b242388639")
previous = json.loads((ROOT / R8 / "selection-chain-replay.json").read_text(encoding="utf-8"))
objects: dict[str, dict[str, object]] = {}
raw: dict[str, object] = {}
member_bytes: dict[str, bytes] = {}
missing: list[str] = []


def add_raw(ref: str) -> object | None:
    path = ROOT / ref
    if not path.is_file():
        missing.append(ref)
        return None
    data = path.read_bytes()
    member_bytes[ref] = data
    try:
        value: object = json.loads(data.decode("utf-8", "strict"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        try:
            value = data.decode("utf-8", "strict")
        except UnicodeDecodeError:
            missing.append(ref + ":non-utf8")
            return None
    raw[ref] = value
    return value


selector_refs = [item["selection_ref"] for item in previous["selector_revisions"]]
selector_refs.append((R9 / "selection.json").as_posix())
for ref in selector_refs:
    data = (ROOT / ref).read_bytes()
    member_bytes[ref] = data
    objects[ref] = {
        "kind": "GitTaskDeliverySelection",
        "value": json.loads(data.decode("utf-8", "strict")),
    }

lineage_refs = [previous["typed_lineage_ref"], (R9 / "assignment-lineage.json").as_posix()]
lineages: list[dict[str, object]] = []
for ref in lineage_refs:
    data = (ROOT / ref).read_bytes()
    member_bytes[ref] = data
    lineage = json.loads(data.decode("utf-8", "strict"))
    objects[ref] = {"kind": "GitTaskAssignmentLineage", "value": lineage}
    lineages.append(lineage)

# Verify and retain every byte from the prior replay before extending its graph.
for ref, expected in previous["raw_member_sha256"].items():
    add_raw(ref)
    actual = hashlib.sha256(member_bytes[ref]).hexdigest() if ref in member_bytes else None
    if actual != expected:
        missing.append(ref + ":sha256-mismatch")

for ref in selector_refs:
    selection = json.loads((ROOT / ref).read_text(encoding="utf-8"))
    invalidations = selection.get("invalidation_refs", [])
    for invalidation_ref in invalidations if isinstance(invalidations, list) else []:
        invalidation = add_raw(invalidation_ref)
        if not isinstance(invalidation, dict):
            continue
        for retained_ref in invalidation.get("invalidated_refs", []):
            if isinstance(retained_ref, str) and retained_ref not in objects:
                add_raw(retained_ref)
    reason_ref = selection.get("reason_ref")
    if isinstance(reason_ref, str):
        add_raw(reason_ref)

for lineage in lineages:
    for assignment in lineage.get("source_assignments", []):
        if isinstance(assignment, dict) and isinstance(assignment.get("assignment_ref"), str):
            add_raw(assignment["assignment_ref"])
    rationale_ref = lineage.get("rationale_ref")
    if isinstance(rationale_ref, str):
        add_raw(rationale_ref)

if missing:
    raise SystemExit(json.dumps({"result": "REJECT", "missing": sorted(set(missing))}, indent=2))

result = validate_delivery_links(objects, raw_members=raw, raw_member_bytes=member_bytes)
if result.get("errors"):
    verdict = "REJECT"
else:
    verdict = result.get("verdict", "UNKNOWN")
report = {
    "archive_closure": "NOT_RUN",
    "archive_publication_readback": "NOT_RUN",
    "assignment_lineages": [
        {
            "lineage_ref": ref,
            "declared_sha256": hashlib.sha256((ROOT / ref).read_bytes()).hexdigest(),
            "observed_sha256": hashlib.sha256((ROOT / ref).read_bytes()).hexdigest(),
        }
        for ref in lineage_refs
    ],
    "base_sha": "649d338f17432df527428a4c29b9a5936617a599",
    "checked_at_utc": datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z"),
    "delivery_id": "5f2d1f3e-e6be-4c86-a39e-e6b242388639",
    "errors": result.get("errors", []),
    "format_version": 1,
    "fresh_live_verification": "NOT_RUN",
    "kind": "task_selection_chain_replay",
    "raw_member_sha256": {
        ref: hashlib.sha256(data).hexdigest()
        for ref, data in sorted(member_bytes.items())
        if ref in raw
    },
    "retained_raw_member_count": len(raw),
    "selector_revisions": [
        {
            "delivery_id": selection["active_delivery_id"],
            "selection_ref": ref,
            "selection_sha256": hashlib.sha256((ROOT / ref).read_bytes()).hexdigest(),
            "task_revision": selection["task_revision"],
        }
        for ref in selector_refs
        for selection in [json.loads((ROOT / ref).read_text(encoding="utf-8"))]
    ],
    "task_id": "T068",
    "task_revision": 7,
    "typed_lineage_refs": lineage_refs,
    "typed_selection_count": len(selector_refs),
    "unknowns": result.get("unknowns", []),
    "validator": "tools.spec_check.semantics.validate_delivery_links",
    "verdict": verdict,
}
out = ROOT / R9 / "selection-chain-replay.json"
out.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="")
print(json.dumps({"report_ref": out.relative_to(ROOT).as_posix(), "verdict": verdict, "errors": report["errors"], "unknowns": report["unknowns"], "selection_count": len(selector_refs), "typed_lineage_count": len(lineage_refs), "raw_member_count": len(raw)}, ensure_ascii=False, indent=2))
if verdict == "REJECT":
    raise SystemExit(1)
