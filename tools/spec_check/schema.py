"""Trích và kiểm tra schema governance S21 bằng thư viện chuẩn Python."""

from __future__ import annotations

import json
import math
import re
from datetime import datetime
from pathlib import Path
from typing import Any
from uuid import UUID


BEGIN_MARKER = "<!-- BEGIN MCE4D_GIT_GOVERNANCE_SCHEMA_V1 -->"
END_MARKER = "<!-- END MCE4D_GIT_GOVERNANCE_SCHEMA_V1 -->"
SUPPORTED_KEYWORDS = {
    "$defs", "$id", "$ref", "$schema", "additionalProperties", "allOf",
    "anyOf", "const", "else", "enum", "format", "if", "items",
    "maxItems", "maxLength", "maxProperties", "maximum", "minItems",
    "minLength", "minProperties", "minimum", "not", "oneOf", "pattern",
    "properties", "required", "then", "type", "uniqueItems", "description",
    "title", "default", "examples", "deprecated", "readOnly", "writeOnly",
}
TYPE_MAP = {
    "null": type(None), "boolean": bool, "object": dict, "array": list,
    "string": str,
}


class SchemaError(ValueError):
    """Lỗi schema hoặc JSON không thể diễn giải an toàn."""


def _reject_constant(value: str) -> None:
    raise SchemaError(f"Giá trị JSON không hữu hạn bị từ chối: {value}")


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise SchemaError(f"Khóa JSON trùng lặp: {key}")
        result[key] = value
    return result


def strict_json_loads(raw: str | bytes) -> Any:
    """Đọc JSON nghiêm ngặt: cấm khóa trùng, NaN và Infinity."""
    try:
        return json.loads(
            raw,
            object_pairs_hook=_unique_object,
            parse_constant=_reject_constant,
        )
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise SchemaError(f"JSON không hợp lệ: {exc}") from exc


def extract_governance_schema(spec_text: str) -> dict[str, Any]:
    if spec_text.count(BEGIN_MARKER) != 1 or spec_text.count(END_MARKER) != 1:
        raise SchemaError("Marker schema governance phải xuất hiện đúng một lần")
    start = spec_text.index(BEGIN_MARKER) + len(BEGIN_MARKER)
    end = spec_text.index(END_MARKER, start)
    block = spec_text[start:end]
    fenced = re.search(r"```json\s*(.*?)\s*```", block, flags=re.DOTALL)
    if fenced is None:
        raise SchemaError("Không tìm thấy khối JSON schema trong marker")
    schema = strict_json_loads(fenced.group(1))
    if not isinstance(schema, dict):
        raise SchemaError("Schema governance phải là object")
    unknown = _schema_unknown_keywords(schema)
    if unknown:
        raise SchemaError("Schema dùng keyword chưa hỗ trợ: " + ", ".join(unknown))
    if schema.get("$schema") != "https://json-schema.org/draft/2020-12/schema":
        raise SchemaError("Schema governance không khai báo Draft 2020-12")
    definitions = schema.get("$defs")
    if not isinstance(definitions, dict):
        raise SchemaError("Schema governance thiếu $defs")

    def check_refs(node: Any) -> None:
        if isinstance(node, dict):
            if "$ref" in node:
                ref = node["$ref"]
                match = re.fullmatch(r"#/\$defs/([A-Za-z][A-Za-z0-9]*)", ref) if isinstance(ref, str) else None
                if match is None or match.group(1) not in definitions:
                    raise SchemaError(f"$ref phải trỏ tới definition local đã khai báo: {ref!r}")
            for child in node.values():
                check_refs(child)
        elif isinstance(node, list):
            for child in node:
                check_refs(child)

    check_refs(schema)
    return schema


def _schema_unknown_keywords(node: Any, path: str = "$") -> list[str]:
    unknown: list[str] = []
    if isinstance(node, dict):
        for key, value in node.items():
            if key in SUPPORTED_KEYWORDS:
                unknown.extend(_schema_unknown_keywords(value, f"{path}.{key}"))
            elif path.endswith(".$defs"):
                # Tên mỗi definition là data key, không phải schema keyword.
                unknown.extend(_schema_unknown_keywords(value, f"{path}.{key}"))
            elif path.endswith(".properties"):
                unknown.extend(_schema_unknown_keywords(value, f"{path}.{key}"))
            elif key not in {"GitRepositoryIdentity", "GitCheckSpec", "GitCheckPlan",
                             "GitCommit", "GitDeliveryAttempt", "GitObservedCheck",
                             "GitDeliveryObservation", "GitEvidenceFile", "GitEvidenceArchive",
                             "GitArchiveSeal", "GitTaskAssignmentLineage",
                             "GitTaskDeliverySelection", "GitExecutionCheckpoint",
                             "GitMergeGroupBinding", "GitDestinationObservation", "GitLocalCheckEvidence",
                             "GitCheckSourceEvidence", "GitCheckExecutionEvidence", "GitPullRequestBinding",
                             "GitCanaryEvidence", "GitPushOperationEvidence", "GitCICollectionEvidence",
                             "GitLiveVerification", "GitCommandLifecycle", "GitCommitScopeEvidence"}:
                unknown.append(f"{path}.{key}")
            else:
                unknown.extend(_schema_unknown_keywords(value, f"{path}.{key}"))
    elif isinstance(node, list):
        for index, value in enumerate(node):
            unknown.extend(_schema_unknown_keywords(value, f"{path}[{index}]"))
    return unknown


def _is_type(value: Any, expected: str) -> bool:
    if expected == "integer":
        return isinstance(value, int) and not isinstance(value, bool)
    if expected == "number":
        return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)
    cls = TYPE_MAP.get(expected)
    return cls is not None and isinstance(value, cls)


def _json_equal(left: Any, right: Any) -> bool:
    """Compare JSON values without Python's bool/int equality coercion."""
    if type(left) is not type(right):
        return False
    if isinstance(left, dict):
        return left.keys() == right.keys() and all(_json_equal(left[key], right[key]) for key in left)
    if isinstance(left, list):
        return len(left) == len(right) and all(_json_equal(a, b) for a, b in zip(left, right))
    return left == right


def _format_error(value: str, format_name: str) -> str | None:
    if format_name == "uuid":
        try:
            parsed = UUID(value)
        except (ValueError, AttributeError):
            return "không phải UUID"
        if str(parsed) != value.lower():
            return "UUID không ở dạng chuẩn"
        return None
    if format_name == "date-time":
        if not re.search(r"(?:Z|[+-]\d{2}:\d{2})$", value):
            return "date-time thiếu múi giờ"
        try:
            datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            return "date-time không hợp lệ"
        return None
    return f"format chưa hỗ trợ: {format_name}"


def validate_schema(
    instance: Any,
    schema: dict[str, Any],
    *,
    root_schema: dict[str, Any] | None = None,
    max_errors: int = 100,
) -> list[str]:
    """Validate phần Draft 2020-12 được dùng trong schema S21; fail closed."""
    errors: list[str] = []
    definitions = (root_schema or schema).get("$defs", {})

    def add(path: str, message: str) -> None:
        if len(errors) < max_errors:
            errors.append(f"{path}: {message}")

    def check(value: Any, rule: Any, path: str) -> None:
        if len(errors) >= max_errors:
            return
        if not isinstance(rule, dict):
            add(path, "schema con không phải object")
            return
        unknown = _schema_unknown_keywords(rule)
        if unknown:
            add(path, "schema dùng keyword chưa hỗ trợ: " + ", ".join(unknown))
            return
        ref = rule.get("$ref")
        if ref is not None:
            match = re.fullmatch(r"#/\$defs/([A-Za-z][A-Za-z0-9]*)", ref) if isinstance(ref, str) else None
            if match is None or match.group(1) not in definitions:
                add(path, f"$ref không local hoặc không tồn tại: {ref!r}")
                return
            check(value, definitions[match.group(1)], path)
        declared_type = rule.get("type")
        if declared_type is not None:
            choices = declared_type if isinstance(declared_type, list) else [declared_type]
            if not any(isinstance(name, str) and _is_type(value, name) for name in choices):
                add(path, f"sai type; cần {declared_type!r}")
                return
        if "const" in rule and not _json_equal(value, rule["const"]):
            add(path, f"không khớp const {rule['const']!r}")
        if "enum" in rule and not any(_json_equal(value, item) for item in rule["enum"]):
            add(path, "giá trị không thuộc enum")
        if isinstance(value, str):
            if len(value) < rule.get("minLength", 0):
                add(path, "chuỗi ngắn hơn minLength")
            if len(value) > rule.get("maxLength", math.inf):
                add(path, "chuỗi dài hơn maxLength")
            pattern = rule.get("pattern")
            if pattern is not None:
                try:
                    if re.search(pattern, value) is None:
                        add(path, f"không khớp pattern {pattern!r}")
                except re.error as exc:
                    add(path, f"pattern schema không hợp lệ: {exc}")
            if "format" in rule:
                problem = _format_error(value, rule["format"])
                if problem:
                    add(path, f"sai format {rule['format']}: {problem}")
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            if "minimum" in rule and value < rule["minimum"]:
                add(path, "nhỏ hơn minimum")
            if "maximum" in rule and value > rule["maximum"]:
                add(path, "lớn hơn maximum")
        if isinstance(value, list):
            if len(value) < rule.get("minItems", 0):
                add(path, "ít phần tử hơn minItems")
            if len(value) > rule.get("maxItems", math.inf):
                add(path, "nhiều phần tử hơn maxItems")
            if rule.get("uniqueItems"):
                encoded = [json.dumps(item, sort_keys=True, ensure_ascii=False, separators=(",", ":")) for item in value]
                if len(encoded) != len(set(encoded)):
                    add(path, "mảng vi phạm uniqueItems")
            if "items" in rule:
                for index, item in enumerate(value):
                    check(item, rule["items"], f"{path}[{index}]")
        if isinstance(value, dict):
            if len(value) < rule.get("minProperties", 0):
                add(path, "ít thuộc tính hơn minProperties")
            if len(value) > rule.get("maxProperties", math.inf):
                add(path, "nhiều thuộc tính hơn maxProperties")
            for required in rule.get("required", []):
                if required not in value:
                    add(path, f"thiếu thuộc tính bắt buộc {required!r}")
            properties = rule.get("properties", {})
            for key, child in value.items():
                if key in properties:
                    check(child, properties[key], f"{path}.{key}")
                elif rule.get("additionalProperties") is False:
                    add(path, f"thuộc tính không được khai báo: {key!r}")
                elif isinstance(rule.get("additionalProperties"), dict):
                    check(child, rule["additionalProperties"], f"{path}.{key}")
        for child in rule.get("allOf", []):
            check(value, child, path)
        if "anyOf" in rule:
            matched = False
            for child in rule["anyOf"]:
                before = len(errors)
                check(value, child, path)
                if len(errors) == before:
                    matched = True
                    break
                del errors[before:]
            if not matched:
                add(path, "không khớp nhánh anyOf")
        if "oneOf" in rule:
            matches = 0
            for child in rule["oneOf"]:
                before = len(errors)
                check(value, child, path)
                if len(errors) == before:
                    matches += 1
                del errors[before:]
            if matches != 1:
                add(path, f"oneOf cần đúng một nhánh, có {matches}")
        if "not" in rule:
            before = len(errors)
            check(value, rule["not"], path)
            matched = len(errors) == before
            del errors[before:]
            if matched:
                add(path, "khớp nhánh bị cấm bởi not")
        if "if" in rule:
            before = len(errors)
            check(value, rule["if"], path)
            condition_matches = len(errors) == before
            del errors[before:]
            branch = rule.get("then") if condition_matches else rule.get("else")
            if branch is not None:
                check(value, branch, path)

    check(instance, schema, "$" )
    return errors


def validate_governance_file(spec_path: Path, record_path: Path, kind: str) -> list[str]:
    try:
        spec_text = spec_path.read_text(encoding="utf-8")
        schema = extract_governance_schema(spec_text)
        record = strict_json_loads(record_path.read_bytes())
    except (OSError, UnicodeDecodeError, SchemaError) as exc:
        return [str(exc)]
    definition = schema.get("$defs", {}).get(kind)
    if definition is None:
        return [f"Không có governance definition: {kind}"]
    errors = validate_schema(record, definition, root_schema=schema)
    from .semantics import validate_semantics
    errors.extend(validate_semantics(kind, record))
    return errors
