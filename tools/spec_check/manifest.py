"""Verifier manifest portable, tối ưu-safe và source-bound cho proof tài liệu."""

from __future__ import annotations

import ast
import hashlib
import json
import os
import runpy
import stat
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path, PurePosixPath, PureWindowsPath
from typing import Any

from .schema import SchemaError, strict_json_loads


DEFAULT_MAX_MEMBERS = 4096
DEFAULT_MAX_TOTAL_BYTES = 10 * 1024 * 1024
DEFAULT_MAX_SPEC_BYTES = 4 * 1024 * 1024


@dataclass(frozen=True)
class VerifiedMember:
    path: str
    byte_length: int
    sha256: str


@dataclass(frozen=True)
class ManifestResult:
    members: dict[str, VerifiedMember]
    total_bytes: int
    spec_sha256: str


class ManifestError(ValueError):
    """Manifest thiếu member, có hash sai hoặc vượt giới hạn đã đặt."""


def _safe_relative(path: Any) -> bool:
    if not isinstance(path, str) or not path or "\\" in path or "\x00" in path:
        return False
    if PurePosixPath(path).is_absolute() or PureWindowsPath(path).is_absolute() or PureWindowsPath(path).drive:
        return False
    return all(part not in {"", ".", ".."} for part in path.split("/"))


def _member_records(manifest: dict[str, Any]) -> list[dict[str, Any]]:
    members = manifest.get("members")
    if isinstance(members, list):
        result = []
        for item in members:
            if not isinstance(item, dict):
                raise ManifestError("Mỗi members[] phải là object")
            result.append(item)
        return result
    files = manifest.get("files")
    if isinstance(files, dict):
        result = []
        for path, item in files.items():
            if not isinstance(item, dict):
                raise ManifestError(f"files[{path!r}] phải là object")
            size = item.get("byte_length", item.get("bytes", item.get("size")))
            result.append({"path": path, "byte_length": size, "sha256": item.get("sha256")})
        return result
    raise ManifestError("Manifest cần members[] hoặc files{}")


def _read_member(root: Path, relative: str) -> bytes:
    candidate = root
    for part in relative.split("/"):
        candidate = candidate / part
        try:
            mode = candidate.lstat().st_mode
        except OSError as exc:
            raise ManifestError(f"Member thiếu/không đọc được: {relative}: {exc}") from exc
        if stat.S_ISLNK(mode):
            raise ManifestError(f"Member đi qua symlink/reparse point: {relative}")
    if not stat.S_ISREG(candidate.lstat().st_mode):
        raise ManifestError(f"Member không phải regular file: {relative}")
    try:
        return candidate.read_bytes()
    except OSError as exc:
        raise ManifestError(f"Không đọc được member {relative}: {exc}") from exc


def verify_manifest(
    root: Path,
    manifest: dict[str, Any] | bytes | str,
    *,
    spec_member: str,
    expected_spec_sha256: str | None = None,
    max_members: int = DEFAULT_MAX_MEMBERS,
    max_total_bytes: int = DEFAULT_MAX_TOTAL_BYTES,
    max_spec_bytes: int = DEFAULT_MAX_SPEC_BYTES,
) -> ManifestResult:
    """Kiểm đủ existence/type/size/hash trước khi trả thành viên tin cậy."""
    if isinstance(manifest, (bytes, str)):
        try:
            manifest = strict_json_loads(manifest)
        except SchemaError as exc:
            raise ManifestError(str(exc)) from exc
    if not isinstance(manifest, dict):
        raise ManifestError("Manifest root phải là object")
    if not _safe_relative(spec_member):
        raise ManifestError("spec_member phải là relative path an toàn")
    resolved_root = root.resolve(strict=True)
    if not resolved_root.is_dir():
        raise ManifestError("Manifest root không phải thư mục")
    records = _member_records(manifest)
    if not records or len(records) > max_members:
        raise ManifestError(f"Số member phải nằm trong 1..{max_members}")
    seen: set[str] = set()
    seen_casefold: set[str] = set()
    verified: dict[str, VerifiedMember] = {}
    total_bytes = 0
    spec_bytes: bytes | None = None
    for index, record in enumerate(records):
        relative = record.get("path")
        if not _safe_relative(relative):
            raise ManifestError(f"members[{index}].path không an toàn")
        normalized = PurePosixPath(relative).as_posix()
        if normalized in seen or normalized.casefold() in seen_casefold:
            raise ManifestError(f"Member duplicate/case collision: {normalized}")
        seen.add(normalized)
        seen_casefold.add(normalized.casefold())
        expected_size = record.get("byte_length")
        expected_hash = record.get("sha256")
        if not isinstance(expected_size, int) or isinstance(expected_size, bool) or expected_size < 0:
            raise ManifestError(f"members[{index}].byte_length không phải số nguyên không âm")
        if not isinstance(expected_hash, str) or len(expected_hash) != 64 or any(c not in "0123456789abcdef" for c in expected_hash):
            raise ManifestError(f"members[{index}].sha256 không hợp lệ")
        raw = _read_member(resolved_root, normalized)
        if len(raw) != expected_size:
            raise ManifestError(f"Sai byte_length member {normalized}: manifest={expected_size}, thực tế={len(raw)}")
        actual_hash = hashlib.sha256(raw).hexdigest()
        if actual_hash != expected_hash:
            raise ManifestError(f"Sai SHA-256 member {normalized}")
        total_bytes += len(raw)
        if total_bytes > max_total_bytes:
            raise ManifestError(f"Tổng kích thước member vượt {max_total_bytes} byte")
        verified[normalized] = VerifiedMember(normalized, len(raw), actual_hash)
        if normalized == spec_member:
            spec_bytes = raw
            if len(raw) > max_spec_bytes:
                raise ManifestError(f"Final SPEC vượt giới hạn {max_spec_bytes} byte")
    if spec_member not in verified or spec_bytes is None:
        raise ManifestError(f"Final SPEC chưa được manifest liệt kê: {spec_member}")
    declared_spec_hash = expected_spec_sha256 or manifest.get("spec_sha256")
    if not isinstance(declared_spec_hash, str) or len(declared_spec_hash) != 64:
        raise ManifestError("Thiếu expected final SPEC SHA-256")
    actual_spec_hash = hashlib.sha256(spec_bytes).hexdigest()
    if actual_spec_hash != declared_spec_hash:
        raise ManifestError("Final SPEC SHA-256 không khớp expected digest")
    return ManifestResult(verified, total_bytes, actual_spec_hash)


def _import_targets(source: bytes, module_path: str) -> set[str]:
    try:
        tree = ast.parse(source, filename=module_path)
    except (SyntaxError, UnicodeDecodeError) as exc:
        raise ManifestError(f"Source không parse được trước khi chạy {module_path}: {exc}") from exc
    targets: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            targets.update(alias.name for alias in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module:
            for alias in node.names:
                targets.add(f"{node.module}.{alias.name}")
            targets.add(node.module)
    return targets


def _possible_local_modules(root: Path, module_name: str) -> tuple[str, ...]:
    dotted = module_name.removeprefix(".")
    if not dotted or dotted.startswith("."):
        return ()
    stem = dotted.replace(".", "/")
    return (stem + ".py", stem + "/__init__.py")


def execute_verified_entrypoint(
    root: Path,
    manifest: dict[str, Any] | bytes | str,
    *,
    entrypoint: str,
    spec_member: str,
    required_sources: tuple[str, ...],
    expected_spec_sha256: str | None = None,
    run_name: str = "__main__",
) -> dict[str, Any]:
    """Verify the manifest-bound source closure before runpy executes its entrypoint.

    This binds the selected tree and ordinary import closure. It is not an OS
    sandbox against arbitrary native code or deliberate dynamic loading.
    """
    result = verify_manifest(root, manifest, spec_member=spec_member, expected_spec_sha256=expected_spec_sha256)
    if not _safe_relative(entrypoint):
        raise ManifestError("Entrypoint phải là relative path an toàn")
    normalized_required = set(required_sources) | {entrypoint}
    for source in normalized_required:
        if not _safe_relative(source) or source not in result.members:
            raise ManifestError(f"Executable source chưa được manifest xác minh: {source}")
        raw = _read_member(root.resolve(strict=True), source)
        if hashlib.sha256(raw).hexdigest() != result.members[source].sha256:
            raise ManifestError(f"Executable source đổi sau manifest verification: {source}")
    if "verify_spec.py" not in result.members or "verify_spec.py" not in normalized_required:
        raise ManifestError("Proof entrypoint closure phải include verify_spec.py đã manifest-bind")
    source_bytes = {name: _read_member(root.resolve(strict=True), name) for name in normalized_required}
    for source_name, raw in source_bytes.items():
        if hashlib.sha256(raw).hexdigest() != result.members[source_name].sha256:
            raise ManifestError(f"Executable source đổi sau manifest verification: {source_name}")
    declared_sources = set(result.members)
    root = root.resolve(strict=True)
    for source_name, raw in source_bytes.items():
        module_name = PurePosixPath(source_name).stem
        for imported in _import_targets(raw, source_name):
            for candidate in _possible_local_modules(root, imported):
                if (root / candidate).is_file() and candidate not in declared_sources:
                    raise ManifestError(f"Import trỏ tới source chưa được manifest xác minh: {candidate}")
    # Execute an owned snapshot of the bytes that passed the manifest hashes.
    # This avoids reopening mutable caller paths between validation and runpy.
    with tempfile.TemporaryDirectory(prefix="mce4d-proof-") as temporary_root:
        staged_root = Path(temporary_root)
        for source_name, raw in source_bytes.items():
            destination = staged_root.joinpath(*PurePosixPath(source_name).parts)
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(raw)
        original_path = list(sys.path)
        current_directory = os.path.normcase(str(Path.cwd().resolve()))
        try:
            sys.path[:] = [str(staged_root)] + [
                item for item in original_path
                if item and os.path.normcase(str(Path(item).resolve())) != current_directory
            ]
            return runpy.run_path(str(staged_root / entrypoint), run_name=run_name)
        finally:
            sys.path[:] = original_path
