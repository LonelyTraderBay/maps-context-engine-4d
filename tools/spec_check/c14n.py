"""Python reference for the byte recipe in SPEC S43 CT-04."""

from __future__ import annotations

import hashlib
import json
from typing import Any

from .schema import SchemaError, strict_json_loads


MAX_SAFE_INTEGER = 9007199254740991
MAX_DEPTH = 32
MAX_OBJECT_KEYS = 4096
MAX_INPUT_BYTES = 16 * 1024 * 1024
PREFIX = b"MCE4D\x00C14N1\x00"


class CanonicalValueError(ValueError):
    """Value is outside the MCE-C14N1 input domain."""


def encode_value(value: Any, *, depth: int = 0) -> bytes:
    if depth > MAX_DEPTH:
        raise CanonicalValueError("maximum depth exceeded")
    if value is None:
        return b"n"
    if isinstance(value, bool):
        return b"t" if value else b"f"
    if isinstance(value, int):
        if abs(value) > MAX_SAFE_INTEGER:
            raise CanonicalValueError("integer outside safe range")
        return b"i" + str(value).encode("ascii") + b";"
    if isinstance(value, float):
        raise CanonicalValueError("fraction/exponent is not an identity integer")
    if isinstance(value, str):
        try:
            encoded = value.encode("utf-8", errors="strict")
        except UnicodeEncodeError as exc:
            raise CanonicalValueError("string is not valid UTF-8") from exc
        return b"s" + str(len(encoded)).encode("ascii") + b":" + encoded
    if isinstance(value, list):
        if depth == MAX_DEPTH:
            raise CanonicalValueError("maximum depth exceeded")
        return (b"a" + str(len(value)).encode("ascii") + b":"
                + b"".join(encode_value(item, depth=depth + 1) for item in value))
    if isinstance(value, dict):
        if len(value) > MAX_OBJECT_KEYS:
            raise CanonicalValueError("too many object keys")
        if depth == MAX_DEPTH:
            raise CanonicalValueError("maximum depth exceeded")
        if any(not isinstance(key, str) for key in value):
            raise CanonicalValueError("object keys must be strings")
        try:
            keys = sorted(value, key=lambda key: key.encode("utf-8", errors="strict"))
        except UnicodeEncodeError as exc:
            raise CanonicalValueError("object key is not valid UTF-8") from exc
        body = b"".join(encode_value(key, depth=depth + 1) + encode_value(value[key], depth=depth + 1)
                        for key in keys)
        return b"o" + str(len(keys)).encode("ascii") + b":" + body
    raise CanonicalValueError(f"unsupported value type: {type(value).__name__}")


def hash_value(domain: str, value: Any) -> str:
    try:
        domain_bytes = encode_value(domain)
        value_bytes = encode_value(value)
    except CanonicalValueError:
        raise
    return hashlib.sha256(PREFIX + domain_bytes + value_bytes).hexdigest()


def hash_json(domain: str, raw: bytes) -> str:
    if len(raw) > MAX_INPUT_BYTES:
        raise CanonicalValueError(f"input exceeds {MAX_INPUT_BYTES} bytes")
    try:
        value = strict_json_loads(raw)
    except SchemaError as exc:
        raise CanonicalValueError(str(exc)) from exc
    return hash_value(domain, value)


def canonical_json1(value: Any, *, depth: int = 0) -> bytes:
    """Encode the physical safe-integer JSON form with the MCE4D escape rules."""
    if depth > MAX_DEPTH:
        raise CanonicalValueError("maximum JSON depth exceeded")
    if value is None:
        return b"null"
    if isinstance(value, bool):
        return b"true" if value else b"false"
    if isinstance(value, int):
        if abs(value) > MAX_SAFE_INTEGER:
            raise CanonicalValueError("integer outside safe range")
        return str(value).encode("ascii")
    if isinstance(value, float):
        raise CanonicalValueError("fraction/exponent is not allowed")
    if isinstance(value, str):
        return _json1_string(value)
    if isinstance(value, list):
        if depth == MAX_DEPTH:
            raise CanonicalValueError("maximum JSON depth exceeded")
        return b"[" + b",".join(canonical_json1(item, depth=depth + 1) for item in value) + b"]"
    if isinstance(value, dict):
        if depth == MAX_DEPTH:
            raise CanonicalValueError("maximum JSON depth exceeded")
        if len(value) > MAX_OBJECT_KEYS or any(not isinstance(key, str) for key in value):
            raise CanonicalValueError("invalid JSON object keys")
        try:
            keys = sorted(value, key=lambda key: key.encode("utf-8", errors="strict"))
        except UnicodeEncodeError as exc:
            raise CanonicalValueError("object key is not valid UTF-8") from exc
        return b"{" + b",".join(
            _json1_string(key) + b":" + canonical_json1(value[key], depth=depth + 1)
            for key in keys
        ) + b"}"
    raise CanonicalValueError(f"unsupported JSON type: {type(value).__name__}")


def _json1_string(value: str) -> bytes:
    try:
        value.encode("utf-8", errors="strict")
    except UnicodeEncodeError as exc:
        raise CanonicalValueError("string is not valid UTF-8") from exc
    escaped: list[str] = ['"']
    short_escapes = {"\b": "\\b", "\f": "\\f", "\n": "\\n", "\r": "\\r", "\t": "\\t"}
    for char in value:
        if char in {'"', "\\"}:
            escaped.append("\\" + char)
        elif char in short_escapes:
            escaped.append(short_escapes[char])
        elif ord(char) < 0x20 or char in {"\u2028", "\u2029"}:
            escaped.append(f"\\u{ord(char):04x}")
        else:
            escaped.append(char)
    escaped.append('"')
    return "".join(escaped).encode("utf-8")


def capture_manifest_json1(manifest: Any) -> bytes:
    """Normalize manifest file/dependency arrays, then serialize all keys canonically."""
    if not isinstance(manifest, dict):
        raise CanonicalValueError("capture manifest must be an object")
    files = manifest.get("files")
    dependencies = manifest.get("dependency_input_digests")
    if not isinstance(files, list) or not isinstance(dependencies, list):
        raise CanonicalValueError("capture manifest arrays are required")
    if any(not isinstance(item, dict) or not isinstance(item.get("path"), str) for item in files):
        raise CanonicalValueError("capture manifest files need string paths")
    try:
        sorted_files = sorted(files, key=lambda item: item["path"].encode("utf-8", errors="strict"))
        sorted_dependencies = sorted(dependencies, key=lambda item: item.encode("utf-8", errors="strict")
                                     if isinstance(item, str) else b"")
    except UnicodeEncodeError as exc:
        raise CanonicalValueError("capture manifest path is not valid UTF-8") from exc
    if any(sorted_files[index - 1]["path"] == sorted_files[index]["path"]
           for index in range(1, len(sorted_files))):
        raise CanonicalValueError("duplicate capture manifest path")
    normalized = dict(manifest)
    normalized["files"] = sorted_files
    normalized["dependency_input_digests"] = sorted_dependencies
    return canonical_json1(normalized)


def capture_snapshot_digest(manifest: dict[str, Any]) -> str:
    """Compute the logical capture digest, excluding its own digest field."""
    if not isinstance(manifest, dict):
        raise CanonicalValueError("capture manifest must be an object")
    normalized = {key: value for key, value in manifest.items() if key != "snapshot_digest"}
    files = normalized.get("files")
    dependencies = normalized.get("dependency_input_digests")
    if not isinstance(files, list) or not isinstance(dependencies, list):
        raise CanonicalValueError("capture manifest arrays are required")
    normalized["files"] = sorted(files, key=lambda item: item["path"].encode("utf-8", errors="strict"))
    normalized["dependency_input_digests"] = sorted(dependencies)
    return hash_value("capture-v1", normalized)
