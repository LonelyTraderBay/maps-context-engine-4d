"""Failure fixtures for the minimal bootstrap validator."""

from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

from bootstrap_check import POLICY_SOURCES, REQUIRED_FILES, SPEC_FILE, validate


PROJECT_ROOT = Path(__file__).resolve().parents[2]


class BootstrapValidatorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory(prefix="mce4d-bootstrap-")
        self.root = Path(self.temp_dir.name)
        for relative_path in (*REQUIRED_FILES, *POLICY_SOURCES):
            destination = self.root / relative_path
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(PROJECT_ROOT / relative_path, destination)

    def tearDown(self) -> None:
        self.temp_dir.cleanup()

    def test_current_sources_pass(self) -> None:
        self.assertEqual([], validate(self.root))

    def test_missing_policy_source_fails(self) -> None:
        (self.root / "docs/AI_RULES.md").unlink()
        errors = validate(self.root)
        self.assertTrue(any("Thiếu tệp bắt buộc: docs/AI_RULES.md" in e for e in errors))

    def test_changed_policy_source_fails(self) -> None:
        path = self.root / "docs/AI_RULES.md"
        path.write_bytes(path.read_bytes() + b"\n")
        errors = validate(self.root)
        self.assertTrue(any("SHA-256 nguồn chính sách sai" in e for e in errors))

    def test_wrong_spec_version_fails(self) -> None:
        path = self.root / SPEC_FILE
        path.write_text(path.read_text(encoding="utf-8").replace(
            "spec_version: 1.6.13", "spec_version: 1.6.12", 1
        ), encoding="utf-8", newline="")
        errors = validate(self.root)
        self.assertTrue(any("Sai phiên bản đặc tả" in e for e in errors))

    def test_invalid_utf8_fails(self) -> None:
        path = self.root / "docs/PROJECT_BOOTSTRAP_PROMPT.txt"
        path.write_bytes(b"\xff")
        errors = validate(self.root)
        self.assertTrue(any("không phải UTF-8 hợp lệ" in e for e in errors))

    def test_missing_push_trigger_fails(self) -> None:
        path = self.root / ".github/workflows/bootstrap.yml"
        workflow = path.read_text(encoding="utf-8")
        path.write_text(workflow.replace("  push:\n", "", 1), encoding="utf-8", newline="")
        errors = validate(self.root)
        self.assertTrue(any("Workflow bootstrap thiếu cấu hình bắt buộc" in e for e in errors))

    def test_missing_markdown_heading_fails(self) -> None:
        path = self.root / "docs/AI_RULES_STORAGE_SUPPLEMENT.md"
        path.write_text("Nội dung không có tiêu đề.\n", encoding="utf-8", newline="")
        errors = validate(self.root)
        self.assertTrue(any("Thiếu tiêu đề Markdown" in e for e in errors))


if __name__ == "__main__":
    unittest.main(verbosity=2)
