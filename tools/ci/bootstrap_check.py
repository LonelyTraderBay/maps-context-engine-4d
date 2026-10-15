#!/usr/bin/env python3
"""Validate the versioned MCE4D bootstrap sources without third-party packages."""

from __future__ import annotations

import argparse
import hashlib
import re
import sys
from pathlib import Path


SPEC_FILE = "Maps-Context-Engine-4D_SPEC.md"
SPEC_VERSION = "1.6.13"
WORKFLOW_FILE = ".github/workflows/bootstrap.yml"
CHECKOUT_ACTION_SHA = "3d3c42e5aac5ba805825da76410c181273ba90b1"
MARKDOWN_FILES = (
    SPEC_FILE,
    "docs/AI_RULES.md",
    "docs/AI_RULES_STORAGE_SUPPLEMENT.md",
)
WORKFLOW_REQUIREMENTS = (
    "on:\n",
    "  push:\n",
    "  pull_request:\n",
    "  workflow_dispatch:\n",
    "permissions:\n",
    "  contents: read\n",
    f"actions/checkout@{CHECKOUT_ACTION_SHA}",
    "persist-credentials: false",
    "ubuntu-latest",
    "windows-latest",
    "name: bootstrap (${{ matrix.os }})",
    "name: spec-contract (${{ matrix.os }})",
    "python -B tools/ci/bootstrap_check.py",
    "python -B tools/ci/test_bootstrap_check.py",
    "python -B tools/ci/delivery_policy.py check-plan",
    "python -B tools/spec_check/check.py",
    "python -B tools/spec_check/test_spec_check.py",
    "python -B tools/ci/test_delivery_policy.py",
    "python -B tools/ci/test_checkpoint_digest.py",
)
POLICY_SOURCES = {
    "docs/AI_RULES.md": "1662406d9ac9e63646130516ad6811934b2bd7f0a6b7d240403749200777f9e3",
    "docs/AI_RULES_STORAGE_SUPPLEMENT.md": "eea81c9ac63344efde5f1eccb30e43d06146875aabaf8396fec3b46e5e878dac",
    "docs/PROJECT_BOOTSTRAP_PROMPT.txt": "b1d692184f9b4bac87f6b55b25e1c49bab90af5d5eb1706a428a09d85212b686",
}
REQUIRED_FILES = (
    SPEC_FILE, WORKFLOW_FILE, "tools/spec_check/check.py",
    "tools/spec_check/spec.py", "tools/spec_check/schema.py", "tools/spec_check/manifest.py",
    "tools/spec_check/ci_order.py",
    "tools/spec_check/semantics.py", "tools/spec_check/test_spec_check.py",
    "tools/ci/delivery_policy.py", "tools/ci/test_delivery_policy.py",
    "tools/ci/checkpoint_digest.py", "tools/ci/test_checkpoint_digest.py",
    "tools/ci/check-plan.json",
)


def validate(root: Path) -> list[str]:
    """Return bootstrap contract violations found under ``root``."""
    errors: list[str] = []
    decoded: dict[str, str] = {}

    for relative_path in (*REQUIRED_FILES, *POLICY_SOURCES):
        path = root / relative_path
        try:
            contents = path.read_bytes()
        except FileNotFoundError:
            errors.append(f"Thiếu tệp bắt buộc: {relative_path}")
            continue
        except OSError as exc:
            errors.append(f"Không đọc được {relative_path}: {exc}")
            continue

        try:
            decoded[relative_path] = contents.decode("utf-8")
        except UnicodeDecodeError:
            errors.append(f"Tệp không phải UTF-8 hợp lệ: {relative_path}")

        expected_hash = POLICY_SOURCES.get(relative_path)
        if expected_hash:
            actual_hash = hashlib.sha256(contents).hexdigest()
            if actual_hash != expected_hash:
                errors.append(
                    f"SHA-256 nguồn chính sách sai: {relative_path} "
                    f"(mong đợi {expected_hash}, thực tế {actual_hash})"
                )

    spec = decoded.get(SPEC_FILE)
    if spec is not None:
        normalized_spec = spec.replace("\r\n", "\n")
        if not normalized_spec.startswith("---\n"):
            errors.append(f"Thiếu YAML front matter trong {SPEC_FILE}")
        if not re.search(r"(?m)^#\s+\S", normalized_spec):
            errors.append(f"Thiếu tiêu đề Markdown trong {SPEC_FILE}")
        version_line = re.compile(r"(?m)^spec_version:\s*([^\s]+)\s*$")
        match = version_line.search(normalized_spec)
        if match is None:
            errors.append(f"Không tìm thấy spec_version trong {SPEC_FILE}")
        elif match.group(1) != SPEC_VERSION:
            errors.append(
                f"Sai phiên bản đặc tả: cần {SPEC_VERSION}, "
                f"đang có {match.group(1)}"
            )

    for relative_path in MARKDOWN_FILES[1:]:
        contents = decoded.get(relative_path)
        if contents is not None and not re.search(r"(?m)^#\s+\S", contents):
            errors.append(f"Thiếu tiêu đề Markdown trong {relative_path}")

    workflow = decoded.get(WORKFLOW_FILE)
    if workflow is not None:
        normalized_workflow = workflow.replace("\r\n", "\n")
        for required in WORKFLOW_REQUIREMENTS:
            if required not in normalized_workflow:
                errors.append(f"Workflow bootstrap thiếu cấu hình bắt buộc: {required!r}")

    return errors


def main(argv: list[str] | None = None) -> int:
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="backslashreplace")

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--root",
        type=Path,
        default=Path(__file__).resolve().parents[2],
        help="thư mục gốc dự án (mặc định: thư mục chứa SPEC)",
    )
    args = parser.parse_args(argv)

    errors = validate(args.root.resolve())
    if errors:
        for error in errors:
            print(f"LỖI: {error}", file=sys.stderr)
        return 1

    print(f"ĐẠT: SPEC v{SPEC_VERSION}; 3 nguồn chính sách đúng UTF-8 và SHA-256.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
