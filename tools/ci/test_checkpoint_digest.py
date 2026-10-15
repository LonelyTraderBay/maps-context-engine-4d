#!/usr/bin/env python3
"""Positive and failure fixtures for the pinned checkpoint digest recipe."""

from __future__ import annotations

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tools.ci.checkpoint_digest import (
    DigestError,
    compute_checkpoint_digests,
    project_spec_checkpoint,
    task_source_scope,
)


SPEC = """---
spec_version: 1.6.13
resume:
  implementation_revision: previous
  active_task: T003
---

## S47

| Seq | Task | Đọc thêm | Owned production paths / allowed test artifacts | Flow contract | Oracle / gate evidence |
|---|---|---|---|---|---|
| 003 | T003 | S43 | `tools/spec_check/,tools/ci/,.github/workflows/,.githooks/ (repo-local only when required)` | DF00 | C137 |

## S22

```yaml
resume:
  execution_checkpoint:
    checkpoint_id: {checkpoint}
    command_state: SUCCEEDED
  active_task: T003
```

"""


def _git(root: Path, *args: str) -> None:
    subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


class CheckpointDigestTests(unittest.TestCase):
    def test_scope_comes_from_single_s47_card_and_pins_spec_projection(self) -> None:
        scope = task_source_scope(SPEC.format(checkpoint="first"), "T003")
        self.assertEqual(
            (".githooks/", ".github/workflows/", "Maps-Context-Engine-4D_SPEC.md",
             "tools/ci/", "tools/spec_check/"),
            scope,
        )

    def test_checkpoint_projection_ignores_only_the_nested_s22_mapping(self) -> None:
        before = project_spec_checkpoint(SPEC.format(checkpoint="first").encode("utf-8"))
        after = project_spec_checkpoint(SPEC.format(checkpoint="second").encode("utf-8"))
        self.assertEqual(before, after)
        changed = SPEC.format(checkpoint="first").replace("active_task: T003", "active_task: T004")
        self.assertNotEqual(before, project_spec_checkpoint(changed.encode("utf-8")))

    def test_projection_rejects_missing_or_duplicate_checkpoint_key(self) -> None:
        missing = SPEC.format(checkpoint="first").replace("  execution_checkpoint:\n", "  checkpoint_removed:\n")
        with self.assertRaises(DigestError):
            project_spec_checkpoint(missing.encode("utf-8"))
        prefix, suffix = SPEC.format(checkpoint="first").split("## S22", 1)
        duplicate = prefix + "## S22" + suffix.replace(
            "  active_task: T003", "  execution_checkpoint:\n    duplicate: true\n  active_task: T003", 1
        )
        with self.assertRaises(DigestError):
            project_spec_checkpoint(duplicate.encode("utf-8"))

    def test_worktree_and_index_digests_track_only_scoped_inputs(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            _git(root, "init", "-b", "main")
            _git(root, "config", "user.name", "Checkpoint Test")
            _git(root, "config", "user.email", "checkpoint@example.invalid")
            paths = {
                "Maps-Context-Engine-4D_SPEC.md": SPEC.format(checkpoint="first"),
                ".gitattributes": "*.py text eol=lf\n*.md text eol=lf\n",
                "tools/ci/check.py": "print('one')\n",
                "tools/spec_check/check.py": "print('spec')\n",
                ".github/workflows/check.yml": "name: check\n",
                "outside/ignored.py": "print('outside')\n",
            }
            for relative, content in paths.items():
                target = root / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(content, encoding="utf-8", newline="\n")
            _git(root, "add", "--all")
            _git(root, "commit", "-m", "fixture")

            initial = compute_checkpoint_digests(root, "T003")
            self.assertEqual(initial, compute_checkpoint_digests(root, "T003"))

            spec_path = root / "Maps-Context-Engine-4D_SPEC.md"
            spec_path.write_text(SPEC.format(checkpoint="second"), encoding="utf-8", newline="\n")
            checkpoint_only = compute_checkpoint_digests(root, "T003")
            self.assertEqual(initial["working_tree_digest"], checkpoint_only["working_tree_digest"])
            self.assertEqual(initial["index_digest"], checkpoint_only["index_digest"])

            spec_path.write_text(
                SPEC.format(checkpoint="second").replace("active_task: T003", "active_task: T004"),
                encoding="utf-8", newline="\n",
            )
            spec_changed = compute_checkpoint_digests(root, "T003")
            self.assertNotEqual(initial["working_tree_digest"], spec_changed["working_tree_digest"])
            self.assertEqual(initial["index_digest"], spec_changed["index_digest"])
            _git(root, "add", "Maps-Context-Engine-4D_SPEC.md")
            staged_spec = compute_checkpoint_digests(root, "T003")
            self.assertNotEqual(initial["index_digest"], staged_spec["index_digest"])

            outside = root / "outside/ignored.py"
            outside.write_text("print('outside changed')\n", encoding="utf-8")
            outside_changed = compute_checkpoint_digests(root, "T003")
            self.assertEqual(staged_spec["working_tree_digest"], outside_changed["working_tree_digest"])
            self.assertEqual(staged_spec["index_digest"], outside_changed["index_digest"])

            source = root / "tools/ci/check.py"
            source.write_text("print('two')\n", encoding="utf-8")
            source_changed = compute_checkpoint_digests(root, "T003")
            self.assertNotEqual(outside_changed["working_tree_digest"], source_changed["working_tree_digest"])
            self.assertEqual(outside_changed["index_digest"], source_changed["index_digest"])
            _git(root, "add", "tools/ci/check.py")
            staged_source = compute_checkpoint_digests(root, "T003")
            self.assertNotEqual(source_changed["index_digest"], staged_source["index_digest"])
            _git(root, "update-index", "--chmod=+x", "tools/ci/check.py")
            staged_mode = compute_checkpoint_digests(root, "T003")
            self.assertNotEqual(staged_source["index_digest"], staged_mode["index_digest"])

            _git(root, "rm", "tools/spec_check/check.py")
            staged_deletion = compute_checkpoint_digests(root, "T003")
            self.assertNotEqual(staged_mode["working_tree_digest"], staged_deletion["working_tree_digest"])
            self.assertNotEqual(staged_mode["index_digest"], staged_deletion["index_digest"])

    def test_symlink_parent_is_rejected_without_traversal(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            outside = root / "outside"
            outside.mkdir()
            (outside / "secret.py").write_text("secret", encoding="utf-8")
            link = root / "tools/ci"
            link.parent.mkdir()
            try:
                link.symlink_to(outside, target_is_directory=True)
            except (OSError, NotImplementedError):
                self.skipTest("nền tảng không cấp quyền tạo symlink fixture")
            from tools.ci.checkpoint_digest import _reject_symlink_parent

            with self.assertRaises(DigestError):
                _reject_symlink_parent(root, "tools/ci/secret.py")


if __name__ == "__main__":
    unittest.main(verbosity=2)
