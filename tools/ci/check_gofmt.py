"""Fail CI khi Go source thuộc implementation chưa qua gofmt."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SOURCE_ROOTS = (ROOT / "internal", ROOT / "contracts", ROOT / "tests" / "contract")


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="backslashreplace")
    files = sorted(path for root in SOURCE_ROOTS if root.exists() for path in root.rglob("*.go"))
    if not files:
        print("Không tìm thấy Go source trong các thư mục implementation đã cấp quyền.", file=sys.stderr)
        return 1
    try:
        result = subprocess.run(
            ["gofmt", "-l", *(str(path) for path in files)],
            cwd=ROOT,
            check=False,
            capture_output=True,
            text=True,
            timeout=60,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        print(f"Không chạy được gofmt: {exc}", file=sys.stderr)
        return 1
    if result.returncode != 0:
        print(result.stderr.strip() or f"gofmt exit={result.returncode}", file=sys.stderr)
        return 1
    unformatted = [line for line in result.stdout.splitlines() if line.strip()]
    if unformatted:
        for path in unformatted:
            print(f"Chưa gofmt: {path}", file=sys.stderr)
        return 1
    print(f"ĐẠT: gofmt kiểm {len(files)} Go source trong implementation.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
