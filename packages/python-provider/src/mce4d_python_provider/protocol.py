"""Single-frame Content-Length transport with strict JSON decoding."""

from __future__ import annotations

import json
import math
import re

MAX_HEADER_BYTES = 64 * 1024
MAX_BODY_BYTES = 16 * 1024 * 1024
MAX_JSON_DEPTH = 32
MAX_OBJECT_KEYS = 4096
MAX_JSON_INTEGER_DIGITS = 128
SEPARATOR = b"\r\n\r\n"
HEADER_RE = re.compile(rb"Content-Length: (0|[1-9][0-9]*)\Z")


class ProtocolError(ValueError):
    """A bounded framing or JSON error."""


def _object_without_duplicates(pairs: list[tuple[str, object]]) -> dict[str, object]:
    if len(pairs) > MAX_OBJECT_KEYS:
        raise ProtocolError("JSON object exceeds its key budget")
    value: dict[str, object] = {}
    for key, item in pairs:
        if key in value:
            raise ProtocolError("JSON object contains a duplicate key")
        value[key] = item
    return value


def _check_json_structure(body: bytes) -> None:
    stack: list[dict[str, int | bool]] = []
    in_string = False
    escaped = False
    for byte in body:
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
            if stack and stack[-1]["kind"] == 0x7B:
                stack[-1]["has_content"] = True
            continue
        if byte in (0x7B, 0x5B):
            if stack and stack[-1]["kind"] == 0x7B:
                stack[-1]["has_content"] = True
            stack.append({"kind": byte, "commas": 0, "has_content": False})
            if len(stack) > MAX_JSON_DEPTH:
                raise ProtocolError("JSON exceeds its nesting depth budget")
            continue
        if byte in (0x7D, 0x5D):
            if stack:
                current = stack.pop()
                if current["kind"] == 0x7B and current["has_content"] and current["commas"] >= MAX_OBJECT_KEYS:
                    raise ProtocolError("JSON object exceeds its key budget")
                if stack and stack[-1]["kind"] == 0x7B:
                    stack[-1]["has_content"] = True
            continue
        if byte == 0x2C and stack and stack[-1]["kind"] == 0x7B:
            stack[-1]["commas"] += 1
            if stack[-1]["commas"] >= MAX_OBJECT_KEYS:
                raise ProtocolError("JSON object exceeds its key budget")
            continue
        if byte not in b" \t\r\n" and stack and stack[-1]["kind"] == 0x7B:
            stack[-1]["has_content"] = True


def _reject_constant(_value: str) -> None:
    raise ProtocolError("JSON number is not finite")


def _parse_integer(value: str) -> int:
    if len(value.lstrip("-")) > MAX_JSON_INTEGER_DIGITS:
        raise ProtocolError("JSON integer exceeds its digit budget")
    return int(value)


def _parse_float(value: str) -> float:
    parsed = float(value)
    if not math.isfinite(parsed):
        raise ProtocolError("JSON number is not finite")
    return parsed


def decode_frame(frame: bytes) -> object:
    if not isinstance(frame, bytes) or len(frame) > MAX_HEADER_BYTES + len(SEPARATOR) + MAX_BODY_BYTES:
        raise ProtocolError("provider frame exceeds its byte budget")
    separator = frame.find(SEPARATOR)
    if separator < 0 or separator > MAX_HEADER_BYTES:
        raise ProtocolError("provider frame header is incomplete or too large")
    match = HEADER_RE.fullmatch(frame[:separator])
    if not match:
        raise ProtocolError("provider frame must contain one canonical Content-Length header")
    length_text = match.group(1)
    maximum_length = str(MAX_BODY_BYTES).encode("ascii")
    if len(length_text) > len(maximum_length) or (len(length_text) == len(maximum_length) and length_text > maximum_length):
        raise ProtocolError("provider frame length exceeds its byte budget")
    length = int(length_text)
    body_start = separator + len(SEPARATOR)
    if length < 1 or length > MAX_BODY_BYTES or body_start + length != len(frame):
        raise ProtocolError("provider frame length is invalid or has trailing bytes")
    body = frame[body_start:]
    _check_json_structure(body)
    try:
        text = body.decode("utf-8", errors="strict")
        return json.loads(
            text,
            object_pairs_hook=_object_without_duplicates,
            parse_int=_parse_integer,
            parse_float=_parse_float,
            parse_constant=_reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProtocolError("provider JSON is malformed") from exc


def encode_frame(value: object) -> bytes:
    encoder = json.JSONEncoder(ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    body = bytearray()
    for chunk in encoder.iterencode(value):
        encoded = chunk.encode("utf-8")
        if len(body) + len(encoded) > MAX_BODY_BYTES:
            raise ProtocolError("provider response exceeds its byte budget")
        body.extend(encoded)
    if not body or len(body) > MAX_BODY_BYTES:
        raise ProtocolError("provider response exceeds its byte budget")
    return b"Content-Length: " + str(len(body)).encode("ascii") + SEPARATOR + bytes(body)
