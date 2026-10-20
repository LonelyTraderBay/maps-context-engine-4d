"""Process entry point: one bounded frame in, one bounded frame out."""

from __future__ import annotations

import sys

from .analyze import analyze_provider_request
from .protocol import ProtocolError, decode_frame, encode_frame

MAX_INPUT_BYTES = 64 * 1024 + 4 + 16 * 1024 * 1024


def _read_stdin() -> bytes:
    chunks: list[bytes] = []
    total = 0
    while True:
        chunk = sys.stdin.buffer.read(min(64 * 1024, MAX_INPUT_BYTES + 1 - total))
        if not chunk:
            break
        total += len(chunk)
        if total > MAX_INPUT_BYTES:
            raise ProtocolError("provider request exceeds its byte budget")
        chunks.append(chunk)
    return b"".join(chunks)


def main() -> int:
    try:
        request = decode_frame(_read_stdin())
        result = analyze_provider_request(request)
        sys.stdout.buffer.write(encode_frame(result))
        sys.stdout.buffer.flush()
        return 0
    except Exception:
        sys.stderr.write("python provider rejected the request or exceeded a bounded resource limit\n")
        sys.stderr.flush()
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
