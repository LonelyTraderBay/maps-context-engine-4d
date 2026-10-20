"""Read only bounded, allowlisted Python notebook metadata."""

from __future__ import annotations

import json
import math

MAX_NOTEBOOK_BYTES = 1024 * 1024
MAX_NOTEBOOK_DEPTH = 32
MAX_NOTEBOOK_OBJECT_KEYS = 4096
MAX_NOTEBOOK_JSON_TOKENS = 100_000
MAX_METADATA_VALUE_CHARS = 128


class NotebookMetadataError(ValueError):
    """A bounded metadata parse failure without notebook contents."""


def _check_depth(data: bytes) -> None:
    in_string = False
    escaped = False
    depth = 0
    tokens = 0
    for byte in data:
        if in_string:
            if escaped:
                escaped = False
            elif byte == 0x5C:
                escaped = True
            elif byte == 0x22:
                in_string = False
            continue
        if byte == 0x22:
            in_string = True
        elif byte in (0x7B, 0x5B):
            depth += 1
            tokens += 1
            if depth > MAX_NOTEBOOK_DEPTH:
                raise NotebookMetadataError("notebook JSON exceeds its nesting depth budget")
        elif byte in (0x7D, 0x5D):
            depth -= 1
            tokens += 1
            if depth < 0:
                raise NotebookMetadataError("notebook JSON is malformed")
        elif byte in (0x2C, 0x3A):
            tokens += 1
        if tokens > MAX_NOTEBOOK_JSON_TOKENS:
            raise NotebookMetadataError("notebook JSON exceeds its structural token budget")


def _object_without_duplicates(pairs: list[tuple[str, object]]) -> dict[str, object]:
    if len(pairs) > MAX_NOTEBOOK_OBJECT_KEYS:
        raise NotebookMetadataError("notebook JSON object exceeds its key budget")
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise NotebookMetadataError("notebook JSON contains a duplicate key")
        result[key] = value
    return result


def _parse_integer(value: str) -> int:
    if len(value.lstrip("-")) > 128:
        raise NotebookMetadataError("notebook JSON integer exceeds its digit budget")
    return int(value)


def _parse_float(value: str) -> float:
    parsed = float(value)
    if not math.isfinite(parsed):
        raise NotebookMetadataError("notebook JSON number is not finite")
    return parsed


def _reject_constant(_value: str) -> None:
    raise NotebookMetadataError("notebook JSON number is not finite")


def _safe_value(value: object) -> str | None:
    if not isinstance(value, str) or not value or len(value) > MAX_METADATA_VALUE_CHARS:
        return None
    if any(ord(character) < 0x20 or 0x7F <= ord(character) < 0xA0 or 0xD800 <= ord(character) <= 0xDFFF for character in value):
        return None
    return value


def extract_notebook_metadata(data: bytes) -> tuple[str | None, str | None]:
    """Return a bounded label from a small allowlist, without traversing cells."""
    if len(data) > MAX_NOTEBOOK_BYTES:
        return None, "NOTEBOOK_METADATA_LIMIT"
    try:
        _check_depth(data)
        notebook = json.loads(
            data.decode("utf-8", errors="strict"),
            object_pairs_hook=_object_without_duplicates,
            parse_int=_parse_integer,
            parse_float=_parse_float,
            parse_constant=_reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, NotebookMetadataError, RecursionError, MemoryError):
        return None, "NOTEBOOK_METADATA_INVALID"
    if not isinstance(notebook, dict):
        return None, "NOTEBOOK_METADATA_INVALID"
    metadata = notebook.get("metadata")
    if not isinstance(metadata, dict):
        return None, "NOTEBOOK_METADATA_ABSENT"

    fields: list[tuple[str, str]] = []
    for section, keys in (("kernelspec", ("name", "display_name")), ("language_info", ("name", "version"))):
        values = metadata.get(section)
        if not isinstance(values, dict):
            continue
        for key in keys:
            value = _safe_value(values.get(key))
            if value is not None:
                fields.append((f"{section}.{key}", value))
    if not fields:
        return None, "NOTEBOOK_METADATA_ABSENT"
    label = "; ".join(f"{key}={value}" for key, value in fields)
    if len(label.encode("utf-8")) > 1024:
        return None, "NOTEBOOK_METADATA_INVALID"
    return label, None
