#!/usr/bin/env python3
"""Compute deterministic, source-scoped checkpoint fingerprints (stdlib only)."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any


RECIPE = "MCE4D-CHECKPOINT-TREE-1"
SPEC_PATH = "Maps-Context-Engine-4D_SPEC.md"
CHECKPOINT_KEY = re.compile(r"^  execution_checkpoint:\s*(?:#.*)?$")
TASK_ROW = re.compile(r"^\|\s*(\d{3})\s*\|\s*(T\d{3})\s*\|")


class DigestError(ValueError):
    """Raised when the observed Git tree cannot be fingerprinted safely."""


def _git(root: Path, *args: str) -> bytes:
    result = subprocess.run(
        ["git", *args], cwd=root, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode:
        detail = result.stderr.decode("utf-8", "backslashreplace").strip()
        raise DigestError(f"git {' '.join(args)} thất bại ({result.returncode}): {detail}")
    return result.stdout


def _decode_paths(raw: bytes, owner: str) -> set[str]:
    paths: set[str] = set()
    for value in raw.split(b"\0"):
        if not value:
            continue
        try:
            path = value.decode("utf-8", "strict")
        except UnicodeDecodeError as exc:
            raise DigestError(f"{owner} có đường dẫn không phải UTF-8: {exc}") from exc
        # Git reports an untracked nested repository as a directory marker.
        if path.endswith("/"):
            path = path[:-1]
        if path.startswith("/") or "\\" in path or any(part in ("", ".", "..") for part in path.split("/")):
            raise DigestError(f"{owner} có đường dẫn không an toàn: {path!r}")
        paths.add(path)
    return paths


def task_source_scope(spec_text: str, task_id: str) -> tuple[str, ...]:
    """Read the task's explicit S47 directory/file roots and include the SPEC projection."""
    rows: list[tuple[int, str]] = []
    for line in spec_text.splitlines():
        match = TASK_ROW.match(line)
        if match and match.group(2) == task_id:
            cells = line.strip().strip("|").split("|")
            if len(cells) >= 6:
                rows.append((int(match.group(1)), cells[3].strip()))
    if len(rows) != 1:
        raise DigestError(f"S47 phải có đúng một phiếu cho {task_id}; thấy {len(rows)}")
    scope_cell = rows[0][1]
    code = re.search(r"`([^`]+)`", scope_cell)
    if code is None:
        raise DigestError(f"Phiếu {task_id} thiếu owned paths dạng code")
    roots: list[str] = []
    for item in code.group(1).split(","):
        root = re.sub(r"\s+\([^)]*\)\s*$", "", item.strip())
        root = root.replace("\\", "/")
        parts = root.rstrip("/").split("/")
        if (not root or root.startswith("/") or re.match(r"^[A-Za-z]:", root)
                or any(part in ("", ".", "..") for part in parts)
                or "*" in root or "?" in root or "[" in root):
            raise DigestError(f"Phiếu {task_id} có owned path không hỗ trợ: {root!r}")
        roots.append(root.rstrip("/") + "/" if root.endswith("/") else root)
    roots.append(SPEC_PATH)
    return tuple(sorted(set(roots), key=lambda item: item.encode("utf-8")))


def project_spec_checkpoint(raw: bytes) -> bytes:
    """Remove only the derived S22 checkpoint mapping; retain all other SPEC bytes."""
    try:
        text = raw.decode("utf-8", "strict")
    except UnicodeDecodeError as exc:
        raise DigestError(f"SPEC không phải UTF-8 hợp lệ: {exc}") from exc
    text = text.replace("\r\n", "\n")
    if "\r" in text:
        raise DigestError("SPEC có line ending CR đơn lẻ")
    lines = text.splitlines(keepends=True)
    headings = [index for index, line in enumerate(lines)
                if re.match(r"^## S22(?:\s|$)", line.rstrip("\n"))]
    if len(headings) != 1:
        raise DigestError(f"SPEC phải có đúng một heading S22; thấy {len(headings)}")
    section_end = next((index for index in range(headings[0] + 1, len(lines))
                        if re.match(r"^##\s+", lines[index].rstrip("\n"))), len(lines))
    fence_start = next((index for index in range(headings[0] + 1, section_end)
                        if lines[index].strip() == "```yaml"), None)
    if fence_start is None:
        raise DigestError("S22 thiếu fenced YAML block")
    fence_end = next((index for index in range(fence_start + 1, section_end)
                      if lines[index].strip() == "```"), None)
    if fence_end is None:
        raise DigestError("S22 fenced YAML block chưa đóng")
    matches = [index for index in range(fence_start + 1, fence_end)
               if CHECKPOINT_KEY.fullmatch(lines[index].rstrip("\n"))]
    if len(matches) != 1:
        raise DigestError(f"S22 YAML phải có đúng một execution_checkpoint; thấy {len(matches)}")
    start = matches[0]
    end = start + 1
    while end < fence_end:
        content = lines[end].rstrip("\n")
        if not content.strip():
            end += 1
            continue
        indent = len(content) - len(content.lstrip(" "))
        if indent <= 2:
            break
        end += 1
    return "".join(lines[:start] + lines[end:]).encode("utf-8")


def _in_scope(path: str, roots: tuple[str, ...]) -> bool:
    return any(path == root or (root.endswith("/") and path.startswith(root)) for root in roots)


def _index_entries(root: Path, roots: tuple[str, ...]) -> dict[str, tuple[str, str, int]]:
    result: dict[str, tuple[str, str, int]] = {}
    for record in _git(root, "ls-files", "--stage", "-z").split(b"\0"):
        if not record:
            continue
        metadata, separator, encoded_path = record.partition(b"\t")
        if not separator:
            raise DigestError("git ls-files --stage trả record sai dạng")
        try:
            mode, object_id, stage = metadata.decode("ascii").split()
            path = encoded_path.decode("utf-8", "strict")
        except (UnicodeDecodeError, ValueError) as exc:
            raise DigestError(f"Index record không hợp lệ: {record!r}") from exc
        if not _in_scope(path, roots):
            continue
        if stage != "0":
            raise DigestError(f"Index có merge stage chưa giải quyết tại {path}")
        if path in result:
            raise DigestError(f"Index có path trùng: {path}")
        result[path] = (mode, object_id, int(stage))
    return result


def _scoped_paths(root: Path, roots: tuple[str, ...], index: dict[str, tuple[str, str, int]]) -> tuple[str, ...]:
    head = _decode_paths(_git(root, "ls-tree", "-r", "--name-only", "-z", "HEAD"), "HEAD")
    untracked = _decode_paths(_git(root, "ls-files", "--others", "--exclude-standard", "-z"), "working tree")
    paths = {path for path in head | set(index) | untracked if _in_scope(path, roots)}
    return tuple(sorted(paths, key=lambda item: item.encode("utf-8")))


def _record_bytes(record: dict[str, Any]) -> bytes:
    return json.dumps(record, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")


def _tree_digest(kind: str, roots: tuple[str, ...], entries: list[dict[str, Any]]) -> str:
    envelope = {"format": RECIPE, "kind": kind, "scope": list(roots), "entries": entries}
    return hashlib.sha256(_record_bytes(envelope)).hexdigest()


def _git_object_bytes(root: Path, object_id: str) -> bytes:
    return _git(root, "cat-file", "blob", object_id)


def _spec_bytes(path: str, content: bytes) -> bytes:
    return project_spec_checkpoint(content) if path == SPEC_PATH else content


def _index_digest(root: Path, roots: tuple[str, ...], index: dict[str, tuple[str, str, int]], paths: tuple[str, ...]) -> str:
    entries: list[dict[str, Any]] = []
    for path in paths:
        indexed = index.get(path)
        if indexed is None:
            entries.append({"path": path, "state": "absent", "mode": None, "sha256": None})
            continue
        mode, object_id, _ = indexed
        if mode not in {"100644", "100755", "120000", "160000"}:
            raise DigestError(f"Index mode không hỗ trợ tại {path}: {mode}")
        if mode == "160000":
            content_hash = hashlib.sha256(object_id.encode("ascii")).hexdigest()
            state = "gitlink"
        else:
            content = _spec_bytes(path, _git_object_bytes(root, object_id))
            content_hash = hashlib.sha256(content).hexdigest()
            state = "blob"
        entries.append({"path": path, "state": state, "mode": mode, "sha256": content_hash})
    return _tree_digest("index", roots, entries)


def _reject_symlink_parent(root: Path, path: str) -> None:
    current = root
    for part in path.split("/")[:-1]:
        current = current / part
        try:
            info = current.lstat()
        except FileNotFoundError:
            return
        if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400):
            raise DigestError(f"Không đi qua symlink/reparse parent: {path}")
        if not stat.S_ISDIR(info.st_mode):
            raise DigestError(f"Parent path không phải directory: {path}")


def _read_regular_no_follow(path: Path) -> bytes:
    flags = os.O_RDONLY | getattr(os, "O_BINARY", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    try:
        info = os.fstat(descriptor)
        if (not stat.S_ISREG(info.st_mode)
                or getattr(info, "st_file_attributes", 0) & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400)):
            raise DigestError(f"Source path không phải regular file: {path}")
        chunks: list[bytes] = []
        while True:
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                return b"".join(chunks)
            chunks.append(chunk)
    finally:
        os.close(descriptor)


def _worktree_digest(root: Path, roots: tuple[str, ...], index: dict[str, tuple[str, str, int]], paths: tuple[str, ...]) -> str:
    entries: list[dict[str, Any]] = []
    for relative in paths:
        _reject_symlink_parent(root, relative)
        path = root.joinpath(*relative.split("/"))
        try:
            info = path.lstat()
        except FileNotFoundError:
            entries.append({"path": relative, "state": "absent", "mode": None, "sha256": None})
            continue
        if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400):
            content = os.readlink(path).encode("utf-8", "strict")
            mode = "120000"
            state = "symlink"
        elif stat.S_ISREG(info.st_mode):
            content = _read_regular_no_follow(path)
            indexed = index.get(relative)
            mode = indexed[0] if indexed is not None else "100644"
            if mode not in {"100644", "100755"}:
                raise DigestError(f"Working file/index mode không khớp tại {relative}: {mode}")
            state = "file"
        else:
            raise DigestError(f"Task-owned source không phải file: {relative}")
        content = _spec_bytes(relative, content)
        entries.append({"path": relative, "state": state, "mode": mode, "sha256": hashlib.sha256(content).hexdigest()})
    return _tree_digest("working-tree", roots, entries)


def compute_checkpoint_digests(root: Path, task_id: str) -> dict[str, Any]:
    root = root.resolve(strict=True)
    spec_path = root / SPEC_PATH
    spec_text = spec_path.read_text(encoding="utf-8", errors="strict")
    roots = task_source_scope(spec_text, task_id)
    index = _index_entries(root, roots)
    paths = _scoped_paths(root, roots, index)
    if SPEC_PATH not in paths:
        raise DigestError("SPEC không nằm trong observed source scope")
    return {
        "format": RECIPE,
        "task_id": task_id,
        "scope": list(roots),
        "entry_count": len(paths),
        "working_tree_digest": _worktree_digest(root, roots, index, paths),
        "index_digest": _index_digest(root, roots, index, paths),
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--task", required=True, help="task ID đã có phiếu trong S47")
    args = parser.parse_args(argv)
    try:
        result = compute_checkpoint_digests(args.root, args.task)
    except (DigestError, OSError, subprocess.SubprocessError) as exc:
        print(f"LỖI: {exc}", file=sys.stderr)
        return 1
    print(json.dumps(result, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
