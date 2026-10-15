"""Kiểm tra cấu trúc và traceability của SPEC v1.6.13."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

if __package__:
    from .spec import validate_spec
else:
    sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
    from tools.spec_check.spec import validate_spec


def main(argv: list[str] | None = None) -> int:
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="backslashreplace")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--spec",
        type=Path,
        default=Path(__file__).resolve().parents[2] / "Maps-Context-Engine-4D_SPEC.md",
        help="đường dẫn SPEC cần kiểm tra",
    )
    args = parser.parse_args(argv)
    errors = validate_spec(args.spec.resolve())
    if errors:
        for error in errors:
            print(f"LỖI: {error}", file=sys.stderr)
        return 1
    print("ĐẠT: SPEC v1.6.13; 108 task, DAG/schedule, traceability hai chiều, work cards và 24 governance definitions.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
