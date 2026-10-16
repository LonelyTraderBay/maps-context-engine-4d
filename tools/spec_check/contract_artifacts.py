"""Trích schema/registry contract từ SPEC và chặn drift của artifact."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[2]
if __package__:
    from .schema import SchemaError, strict_json_loads
else:
    sys.path.insert(0, str(ROOT))
    from tools.spec_check.schema import SchemaError, strict_json_loads


ARTIFACTS = {
    "MCE4D_CONTRACT_SCHEMA_V1": "mce4d.v1.schema.json",
    "MCE4D_OPERATION_REGISTRY_V1": "operations.v1.json",
}
PRIVATE_STORAGE_CONTRACTS = {
    "VolumeAdmissionJournal",
    "VolumeAdmissionEntry",
    "AdmissionOwnedPath",
    "StoredCaptureManifestRef",
}


class ContractArtifactError(ValueError):
    """SPEC block hoặc artifact contract sai cấu trúc/parity."""


def normalized_json(value: Any) -> bytes:
    """Encode JSON ổn định theo thứ tự key UTF-8 và không chấp nhận số không an toàn."""
    if value is None or isinstance(value, (str, bool)):
        try:
            return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        except UnicodeEncodeError as exc:
            raise ContractArtifactError("JSON chứa surrogate không hợp lệ") from exc
    if isinstance(value, int) and not isinstance(value, bool):
        if abs(value) > 9007199254740991:
            raise ContractArtifactError("JSON integer vượt miền safe integer")
        return str(value).encode("ascii")
    if isinstance(value, float):
        raise ContractArtifactError("MCE-MANIFEST-JSON1 không nhận fraction/exponent")
    if isinstance(value, list):
        return b"[" + b",".join(normalized_json(child) for child in value) + b"]"
    if isinstance(value, dict):
        if not all(isinstance(key, str) for key in value):
            raise ContractArtifactError("JSON object key phải là chuỗi")
        try:
            keys = sorted(value, key=lambda key: key.encode("utf-8", errors="strict"))
        except UnicodeEncodeError as exc:
            raise ContractArtifactError("JSON object key chứa surrogate không hợp lệ") from exc
        return b"{" + b",".join(normalized_json(key) + b":" + normalized_json(value[key]) for key in keys) + b"}"
    raise ContractArtifactError(f"Giá trị ngoài JSON: {type(value).__name__}")


def normalized_sha256(value: Any) -> str:
    return hashlib.sha256(normalized_json(value)).hexdigest()


def _extract_block(spec_text: str, marker: str) -> Any:
    begin = f"<!-- BEGIN {marker} -->"
    end = f"<!-- END {marker} -->"
    if spec_text.count(begin) != 1 or spec_text.count(end) != 1:
        raise ContractArtifactError(f"Marker {marker} phải xuất hiện đúng một lần")
    start = spec_text.index(begin) + len(begin)
    finish = spec_text.index(end, start)
    if finish < start:
        raise ContractArtifactError(f"Marker {marker} bị đảo thứ tự")
    fenced = re.search(r"```json\s*(.*?)\s*```", spec_text[start:finish], flags=re.DOTALL)
    if fenced is None:
        raise ContractArtifactError(f"Block {marker} thiếu fenced JSON")
    try:
        return strict_json_loads(fenced.group(1))
    except SchemaError as exc:
        raise ContractArtifactError(f"Block {marker}: {exc}") from exc


def extract_contract_artifacts(spec_text: str) -> dict[str, Any]:
    schema = _extract_block(spec_text, "MCE4D_CONTRACT_SCHEMA_V1")
    registry = _extract_block(spec_text, "MCE4D_OPERATION_REGISTRY_V1")
    if not isinstance(schema, dict) or schema.get("$schema") != "https://json-schema.org/draft/2020-12/schema":
        raise ContractArtifactError("Schema contract phải là Draft 2020-12 object")
    definitions = schema.get("$defs")
    if not isinstance(definitions, dict) or not definitions:
        raise ContractArtifactError("Schema contract thiếu $defs")
    def check_closed_objects(value: Any, location: str) -> None:
        if isinstance(value, dict):
            if value.get("type") == "object" and value.get("additionalProperties") is not False:
                raise ContractArtifactError(f"Object schema {location} phải đóng additionalProperties")
            for key, child in value.items():
                check_closed_objects(child, f"{location}.{key}")
        elif isinstance(value, list):
            for index, child in enumerate(value):
                check_closed_objects(child, f"{location}[{index}]")

    check_closed_objects(schema, "$schema")
    if not isinstance(registry, dict) or not registry:
        raise ContractArtifactError("Operation registry phải là object không rỗng")
    for operation, pair in registry.items():
        if not isinstance(operation, str) or not operation or not isinstance(pair, list) or len(pair) != 2:
            raise ContractArtifactError(f"Operation registry sai cặp request/data: {operation!r}")
        if any(not isinstance(name, str) or name not in definitions for name in pair):
            raise ContractArtifactError(f"Operation {operation!r} tham chiếu definition không tồn tại")
    registered = {name for pair in registry.values() for name in pair}
    leaked = PRIVATE_STORAGE_CONTRACTS & registered
    if leaked:
        raise ContractArtifactError("Private storage schema không được đưa vào registry: " + ", ".join(sorted(leaked)))
    return {
        "mce4d.v1.schema.json": schema,
        "operations.v1.json": registry,
    }


def render_artifact(value: Any) -> bytes:
    try:
        normalized_json(value)
        return (json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2, allow_nan=False) + "\n").encode("utf-8")
    except (TypeError, ValueError, UnicodeEncodeError) as exc:
        raise ContractArtifactError(f"Không thể render contract artifact: {exc}") from exc


def validate_contract_artifacts(spec_path: Path, contract_dir: Path | None = None) -> list[str]:
    target = contract_dir or (spec_path.resolve().parent / "contracts")
    try:
        spec_text = spec_path.read_text(encoding="utf-8")
        expected = extract_contract_artifacts(spec_text)
    except (OSError, UnicodeDecodeError, ContractArtifactError) as exc:
        return [f"Không trích được contract từ SPEC: {exc}"]
    errors: list[str] = []
    for name, expected_value in expected.items():
        path = target / name
        try:
            actual_value = strict_json_loads(path.read_bytes())
        except (OSError, SchemaError) as exc:
            errors.append(f"Thiếu/JSON không hợp lệ {path}: {exc}")
            continue
        expected_sha = normalized_sha256(expected_value)
        actual_sha = normalized_sha256(actual_value)
        if actual_sha != expected_sha:
            errors.append(f"Contract artifact drift {path.name}: SPEC={expected_sha}, artifact={actual_sha}")
    return errors


def write_contract_artifacts(spec_path: Path, contract_dir: Path | None = None) -> None:
    target = contract_dir or (spec_path.resolve().parent / "contracts")
    values = extract_contract_artifacts(spec_path.read_text(encoding="utf-8"))
    target.mkdir(parents=True, exist_ok=True)
    for name, value in values.items():
        (target / name).write_bytes(render_artifact(value))


def main(argv: list[str] | None = None) -> int:
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="backslashreplace")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--spec", type=Path, default=ROOT / "Maps-Context-Engine-4D_SPEC.md")
    parser.add_argument("--contracts", type=Path, default=ROOT / "contracts")
    parser.add_argument("--write", action="store_true", help="sinh artifacts từ block authority trong SPEC")
    args = parser.parse_args(argv)
    try:
        if args.write:
            write_contract_artifacts(args.spec.resolve(), args.contracts.resolve())
        errors = validate_contract_artifacts(args.spec.resolve(), args.contracts.resolve())
    except ContractArtifactError as exc:
        errors = [str(exc)]
    if errors:
        for error in errors:
            print(f"LỖI: {error}", file=sys.stderr)
        return 1
    print("ĐẠT: schema và operation registry khớp normalized SHA với block authority trong SPEC.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
