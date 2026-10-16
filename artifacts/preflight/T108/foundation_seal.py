"""Create a read-only evidence report for the current G0 foundation boundary."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[3]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tools.ci import bootstrap_check
from tools.ci.delivery_policy import _canonical_json, validate_check_plan
from tools.spec_check.archive import ArchiveError, verify_archive
from tools.spec_check.schema import extract_governance_schema, validate_schema
from tools.spec_check.semantics import validate_semantics
from tools.spec_check.spec import (
    _effective_status,
    _evidence_fields,
    _ranges,
    _section,
    _table_with_headers,
    validate_spec,
)


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _unquote(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value


def _source_hashes(root: Path) -> list[dict[str, str]]:
    refs = [
        "Maps-Context-Engine-4D_SPEC.md",
        "contracts/mce4d.v1.schema.json",
        "contracts/operations.v1.json",
        "go.mod",
        "go.sum",
        ".github/workflows/bootstrap.yml",
        "tools/ci/bootstrap_check.py",
        "tools/spec_check/spec.py",
        "tools/spec_check/contract_artifacts.py",
        "artifacts/preflight/T108/foundation_seal.py",
        "tools/spec_check/test_spec_check.py",
        "internal/model/contracts.go",
        "internal/store/sqlite/migrations/001_init.sql",
        "tests/contract/storage_contract_test.go",
        "tools/spec_check/t107_contract_fixtures.py",
    ]
    for ref in ("docs/AI_RULES.md", "docs/AI_RULES_STORAGE_SUPPLEMENT.md", "docs/PROJECT_BOOTSTRAP_PROMPT.txt"):
        refs.append(ref)
    results = []
    for ref in refs:
        path = root / ref
        if path.is_file():
            results.append({"ref": ref, "sha256": _sha256(path), "byte_length": str(path.stat().st_size)})
    return results


def _verified_archive_identity(root: Path, delivery_id: str, expected_task: str) -> dict[str, Any]:
    """Read task identity from the sealed manifest; the readback projection has no task_id."""
    projection = verify_archive(root, delivery_id)
    manifest_path = root / "artifacts" / "git-delivery" / delivery_id / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if not isinstance(manifest, dict):
        raise ArchiveError("archive manifest root không phải object")
    if manifest.get("delivery_id") != delivery_id or manifest.get("task_id") != expected_task:
        raise ArchiveError("archive manifest identity không khớp S19")
    return {
        "archive_result": "PASS",
        "manifest_sha256": projection["archive_manifest_sha256"],
        "seal_sha256": projection["archive_seal_sha256"],
        "closing_sha": manifest.get("closing_sha"),
        "task_revision": manifest.get("task_revision"),
    }


def _live_receipt_errors(
    record: dict[str, Any],
    *,
    spec_text: str,
    expected_verification_id: str,
    root: Path,
) -> list[str]:
    """Validate the retained live receipt against its schema, S19 identity and sealed archive."""
    errors: list[str] = []
    schema = extract_governance_schema(spec_text)
    errors.extend(validate_schema(record, schema["$defs"]["GitLiveVerification"], root_schema=schema))
    errors.extend(validate_semantics("GitLiveVerification", record))
    if record.get("verification_id") != expected_verification_id:
        errors.append("verification_id không khớp S22 last_live_verification_id")
    delivery_id = record.get("delivery_id")
    task_id = record.get("task_id")
    if not isinstance(delivery_id, str) or not isinstance(task_id, str):
        errors.append("live receipt thiếu task/delivery identity")
        return errors
    try:
        archive_identity = _verified_archive_identity(root, delivery_id, task_id)
    except (ArchiveError, OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
        errors.append(f"live receipt archive: {type(exc).__name__}: {exc}")
        return errors
    publication = record.get("archive_publication")
    if isinstance(publication, dict):
        if publication.get("archive_manifest_sha256") != archive_identity["manifest_sha256"]:
            errors.append("live receipt manifest SHA không khớp archive đã seal/readback")
        if publication.get("archive_seal_sha256") != archive_identity["seal_sha256"]:
            errors.append("live receipt seal SHA không khớp archive đã seal/readback")
    if record.get("task_revision") != archive_identity["task_revision"]:
        errors.append("live task_revision không khớp archive đã seal/readback")
    if (record.get("subject_sha") != archive_identity["closing_sha"]
            or record.get("observed_remote_sha") != archive_identity["closing_sha"]):
        errors.append("live subject/remote SHA không khớp closing SHA trong archive")
    return errors


def build_report(root: Path) -> dict[str, Any]:
    root = root.resolve(strict=True)
    spec_path = root / "Maps-Context-Engine-4D_SPEC.md"
    spec_text = spec_path.read_text(encoding="utf-8")
    errors = validate_spec(spec_path)
    errors.extend(bootstrap_check.validate(root))

    task_section = _section(spec_text, "## S19 — Bảng triển khai duy nhất") or ""
    task_table = _table_with_headers(task_section, "Task", "Gate", "Status", "Evidence")
    gate_section = _section(spec_text, "## S20 — Gates và Definition of Done") or ""
    gate_table = _table_with_headers(gate_section, "Gate", "Phạm vi task")
    if task_table is None or gate_table is None:
        return {
            "format_version": 1,
            "kind": "foundation_seal_preflight",
            "result": "FAIL",
            "errors": ["Không đọc được bảng S19/S20"],
        }
    task_rows = {row["Task"].strip(): row for row in task_table.rows}
    g0 = next((row for row in gate_table.rows if row["Gate"].split(" — ", 1)[0].strip() == "G0"), None)
    members = _ranges(g0.get("Phạm vi task", ""), "T") if g0 else set()
    task_mode_match = re.search(r"(?m)^  active_task:\s*([^\s#]+)", _section(spec_text, "## S22 — Trạng thái tiếp tục cho phiên AI sau") or "")
    active_task = _unquote(task_mode_match.group(1)) if task_mode_match else None
    completed: list[dict[str, Any]] = []
    archive_errors: list[str] = []

    # Current-format delivery archives begin with the sealed model/storage foundation.
    # Earlier tasks keep their immutable historical receipts and are not rewritten into
    # the current archive schema during this task.
    current_archive_tasks = {"T105", "T106", "T107", "T108"}
    for task_id in sorted(members):
        row = task_rows.get(task_id)
        if row is None:
            errors.append(f"G0 task thiếu trong S19: {task_id}")
            continue
        effective = _effective_status(row)
        evidence = _evidence_fields(row.get("Evidence", ""))
        delivery_id = evidence.get("delivery")
        if effective == "DONE" and delivery_id:
            entry: dict[str, Any] = {"task_id": task_id, "delivery_id": delivery_id, "effective_status": effective}
            if task_id in current_archive_tasks:
                try:
                    entry.update(_verified_archive_identity(root, delivery_id, task_id))
                except (ArchiveError, OSError, ValueError) as exc:
                    archive_errors.append(f"{task_id}/{delivery_id}: {type(exc).__name__}: {exc}")
                    entry["archive_result"] = "FAIL"
            else:
                archive_dir = root / "artifacts" / "git-delivery" / delivery_id
                manifest, seal = archive_dir / "manifest.json", archive_dir / "seal.json"
                if manifest.is_file() and seal.is_file():
                    entry.update({
                        "archive_result": "RETAINED_HISTORICAL_BYTES_NOT_REPLAYED",
                        "manifest_sha256": _sha256(manifest),
                        "seal_sha256": _sha256(seal),
                        "limitation": "Current typed archive evaluator is not retroactively applied to this earlier receipt.",
                    })
                else:
                    archive_errors.append(f"{task_id}/{delivery_id}: historical manifest or seal is missing")
                    entry["archive_result"] = "FAIL"
            completed.append(entry)
        elif task_id == "T108" and active_task == task_id and effective in {"IN_PROGRESS", "VERIFY"}:
            completed.append({
                "task_id": task_id,
                "delivery_id": delivery_id,
                "effective_status": effective,
                "archive_result": "PENDING_CURRENT_TASK_CLOSURE",
            })
        else:
            archive_errors.append(f"{task_id}: G0 prerequisite is not effective DONE (status={effective})")

    plan_ref_match = re.search(
        r"(?m)^    expected_check_plan_ref:\s*(.+?)\s*$",
        _section(spec_text, "## S22 — Trạng thái tiếp tục cho phiên AI sau") or "",
    )
    plan_ref = _unquote(plan_ref_match.group(1)) if plan_ref_match else ""
    if not plan_ref:
        errors.append("S22 chưa trỏ tới frozen check plan")
    else:
        try:
            plan_path = root / Path(*plan_ref.split("/"))
            plan = json.loads(plan_path.read_text(encoding="utf-8"))
            workflow = (root / ".github/workflows/bootstrap.yml").read_text(encoding="utf-8")
            active_row = task_rows.get(active_task or "", {})
            active_evidence = _evidence_fields(active_row.get("Evidence", ""))
            expected_plan_sha = active_evidence.get("plan")
            actual_plan_sha = hashlib.sha256(_canonical_json(plan)).hexdigest()
            if not isinstance(expected_plan_sha, str) or expected_plan_sha != actual_plan_sha:
                errors.append("frozen plan SHA không khớp allocation S19")
            if plan.get("canary_ref") != f"delivery:{active_evidence.get('delivery')}:payload":
                errors.append("frozen plan canary_ref không khớp delivery active S19")
            plan_errors = validate_check_plan(
                plan, workflow_text=workflow, expected_sha256=expected_plan_sha if isinstance(expected_plan_sha, str) else None
            )
            errors.extend(f"frozen plan: {message}" for message in plan_errors)
        except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
            errors.append(f"Không kiểm tra được frozen plan {plan_ref}: {type(exc).__name__}: {exc}")

    live_ref_match = re.search(
        r"(?m)^    last_live_verification_ref:\s*(.+?)\s*$",
        _section(spec_text, "## S22 — Trạng thái tiếp tục cho phiên AI sau") or "",
    )
    live_id_match = re.search(
        r"(?m)^    last_live_verification_id:\s*(.+?)\s*$",
        _section(spec_text, "## S22 — Trạng thái tiếp tục cho phiên AI sau") or "",
    )
    live_record: dict[str, Any] | None = None
    if live_ref_match and live_id_match:
        live_ref = _unquote(live_ref_match.group(1))
        live_id = _unquote(live_id_match.group(1))
        try:
            live_record = json.loads((root / Path(*live_ref.split("/"))).read_text(encoding="utf-8"))
            live_errors = _live_receipt_errors(
                live_record, spec_text=spec_text, expected_verification_id=live_id, root=root
            )
            errors.extend(f"last live receipt: {message}" for message in live_errors)
        except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
            errors.append(f"Không kiểm tra được S22 last live receipt: {type(exc).__name__}: {exc}")
    else:
        errors.append("S22 thiếu last_live_verification_ref hoặc verification_id")

    errors.extend(archive_errors)
    g0_done = all(task_id in task_rows and _effective_status(task_rows[task_id]) == "DONE" for task_id in members)
    gate_status = "PASS" if g0_done and not errors else "IN_PROGRESS"
    preflight_ready = (
        active_task == "T108"
        and all(
            task_id in task_rows and _effective_status(task_rows[task_id]) == "DONE"
            for task_id in members - {"T108"}
        )
        and not errors
    )
    return {
        "format_version": 1,
        "kind": "foundation_seal_preflight",
        "captured_at_utc": datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z"),
        "task_id": "T108",
        "active_task": active_task,
        "base_revision": "128b586989340ef8204d1bb04de87b88fc02223d",
        "result": "PASS" if preflight_ready or (g0_done and not errors) else "FAIL",
        "preflight_ready": preflight_ready,
        "gate": "G0",
        "gate_status": gate_status,
        "g0_effective_done": sum(
            task_id in task_rows and _effective_status(task_rows[task_id]) == "DONE"
            for task_id in members
        ),
        "g0_required": len(members),
        "deliveries": completed,
        "live_receipt": {
            "validation_result": "PASS" if live_record and not any(item.startswith("last live receipt:") for item in errors) else "FAIL",
            "result_field_present": "result" in live_record if live_record else False,
            "task_id": live_record.get("task_id"),
            "delivery_id": live_record.get("delivery_id"),
            "subject_sha": live_record.get("subject_sha"),
            "archive_manifest_sha256": (
                live_record.get("archive_publication", {}).get("archive_manifest_sha256")
                if isinstance(live_record.get("archive_publication"), dict) else None
            ),
        } if live_record else None,
        "source_hashes": _source_hashes(root),
        "errors": errors,
        "claims": {
            "product_runtime": "NOT_CLAIMED",
            "os_enforcement": "NOT_CLAIMED",
            "browser_runtime": "NOT_CLAIMED",
            "scope": "G0 design/readiness and retained archive evidence only",
        },
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args(argv)
    root = args.root.resolve(strict=True)
    try:
        report = build_report(root)
        output = args.output if args.output.is_absolute() else root / args.output
        output.parent.mkdir(parents=True, exist_ok=True)
        if output.exists():
            raise FileExistsError(f"giữ nguyên foundation seal đã có: {output}")
        payload = json.dumps(report, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n"
        output.write_text(payload, encoding="utf-8", newline="\n")
    except (OSError, ValueError) as exc:
        print(f"LỖI: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1
    print(json.dumps({
        "result": report["result"],
        "preflight_ready": report["preflight_ready"],
        "gate_status": report["gate_status"],
        "g0_effective_done": report["g0_effective_done"],
        "g0_required": report["g0_required"],
        "output": output.relative_to(root).as_posix(),
        "error_count": len(report["errors"]),
    }, ensure_ascii=False, sort_keys=True))
    return 0 if report["result"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
