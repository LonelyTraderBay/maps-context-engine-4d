#!/usr/bin/env python3
"""Seal, publish and read back the current delivery evidence archive."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import sys
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tools.spec_check.archive import (
    MAX_ARCHIVE_BYTES, MAX_ARCHIVE_FILES, MAX_MEMBER_BYTES, ArchiveError,
    _canonical, _is_reparse, _read_bounded, _safe_relative, _validate_graph,
    persist_publication_projection, publish_archive, verify_archive,
)
from tools.spec_check.schema import strict_json_loads


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--delivery", required=True)
    parser.add_argument("--operation-id", required=True)
    parser.add_argument("--manifest", type=Path)
    args = parser.parse_args(argv)
    root = args.root.resolve(strict=True)
    try:
        delivery_id = str(uuid.UUID(args.delivery))
        operation_id = str(uuid.UUID(args.operation_id))
    except ValueError:
        print("LỖI: delivery và operation phải là UUID", file=sys.stderr)
        return 2
    if delivery_id != args.delivery or operation_id != args.operation_id:
        print("LỖI: UUID phải canonical lowercase", file=sys.stderr)
        return 2
    staging = root / "artifacts" / "git-delivery" / f".staging-{delivery_id}"
    final = root / "artifacts" / "git-delivery" / delivery_id
    expected = staging / "archive-candidate.json"
    manifest_path = Path(os.path.abspath(args.manifest)) if args.manifest else expected
    if manifest_path != Path(os.path.abspath(expected)):
        print("LỖI: manifest candidate phải nằm trong staging của delivery hiện hành", file=sys.stderr)
        return 2
    if not staging.exists() and final.exists():
        try:
            receipt = root / "artifacts" / "git-delivery" / "operations" / operation_id / "publication.json"
            if not receipt.is_file():
                raise ArchiveError(
                    "archive đã publish nhưng chưa có projection giữ thời điểm publish; không thể suy diễn timestamp"
                )
            from tools.spec_check.github_observer import GitHubObserverError, _verify_publication_projection
            projection, _ = _verify_publication_projection(root, delivery_id, receipt)
        except (ArchiveError, OSError, GitHubObserverError, ValueError) as exc:
            print(f"LỖI: {exc}", file=sys.stderr)
            return 1
        printable = {key: projection[key] for key in (
            "format_version", "kind", "planned_archive_path", "archive_manifest_sha256",
            "archive_seal_sha256", "published_at_utc", "readback_completed_at_utc",
        )}
        printable["publication_projection_ref"] = receipt.relative_to(root).as_posix()
        print(json.dumps(printable, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
        return 0
    try:
        for directory in (root / "artifacts", root / "artifacts" / "git-delivery", staging):
            info = directory.lstat()
            if directory.is_symlink() or getattr(info, "st_file_attributes", 0) & 0x400 or not directory.is_dir():
                raise ArchiveError("archive staging phải là directory thật")
        manifest_member = staging / "manifest.json"
        candidate_exists = manifest_path.exists() or manifest_path.is_symlink()
        staged_manifest_exists = manifest_member.exists() or manifest_member.is_symlink()
        if candidate_exists:
            manifest_info = manifest_path.lstat()
            if _is_reparse(manifest_info) or not stat.S_ISREG(manifest_info.st_mode):
                raise ArchiveError("archive candidate phải là regular file")
            candidate_bytes = _read_bounded(manifest_path)
            archive = strict_json_loads(candidate_bytes)
        elif staged_manifest_exists:
            manifest_info = manifest_member.lstat()
            if _is_reparse(manifest_info) or not stat.S_ISREG(manifest_info.st_mode):
                raise ArchiveError("archive manifest staging phải là regular file")
            candidate_bytes = _read_bounded(manifest_member)
            archive = strict_json_loads(candidate_bytes)
        else:
            raise ArchiveError("thiếu archive candidate và manifest staging để phục hồi")
        if not isinstance(archive, dict) or archive.get("delivery_id") != delivery_id:
            raise ArchiveError("manifest candidate không khớp delivery selector")
        canonical_manifest = _canonical(archive)
        if candidate_exists and candidate_bytes != canonical_manifest:
            raise ArchiveError("archive candidate phải dùng canonical JSON bytes")
        if staged_manifest_exists and candidate_bytes != canonical_manifest:
            raise ArchiveError("archive manifest staging không phải canonical candidate bytes")

        files = archive.get("files")
        if not isinstance(files, list) or not 3 <= len(files) <= MAX_ARCHIVE_FILES:
            raise ArchiveError("archive candidate files phải có 3..4096 member")
        expected_members: set[str] = set()
        total_bytes = 0
        for item in files:
            if not isinstance(item, dict):
                raise ArchiveError("archive candidate file entry phải là object")
            ref = _safe_relative(item.get("path"))
            if ref in expected_members or ref in {"manifest.json", "seal.json", "archive-candidate.json"}:
                raise ArchiveError(f"archive candidate member bị trùng hoặc dành riêng: {ref}")
            expected_members.add(ref)
            member = staging.joinpath(*ref.split("/"))
            data = _read_bounded(member)
            expected_length, expected_sha = item.get("byte_length"), item.get("sha256")
            if (isinstance(expected_length, bool) or not isinstance(expected_length, int)
                    or expected_length != len(data) or not isinstance(expected_sha, str)
                    or not re.fullmatch(r"[0-9a-f]{64}", expected_sha)
                    or hashlib.sha256(data).hexdigest() != expected_sha):
                raise ArchiveError(f"archive candidate member bytes/SHA không khớp: {ref}")
            total_bytes += len(data)
            if total_bytes > MAX_ARCHIVE_BYTES:
                raise ArchiveError("archive candidate tổng vượt 100 MiB")

        allowed_extra = {"manifest.json", "seal.json"}
        if candidate_exists:
            allowed_extra.add("archive-candidate.json")
        actual_members: set[str] = set()
        for directory, dirs, names in os.walk(staging, followlinks=False):
            base = Path(directory)
            for name in dirs:
                if _is_reparse((base / name).lstat()):
                    raise ArchiveError(f"archive staging chứa directory link/reparse: {base / name}")
            for name in names:
                member = base / name
                if _is_reparse(member.lstat()):
                    raise ArchiveError(f"archive staging chứa file link/reparse: {member}")
                actual_members.add(member.relative_to(staging).as_posix())
        if len(actual_members) > MAX_ARCHIVE_FILES + 3:
            raise ArchiveError("archive staging vượt giới hạn files")
        permitted = expected_members | allowed_extra
        if actual_members - permitted or expected_members - actual_members:
            raise ArchiveError("archive staging phải khớp chính xác candidate files và metadata")

        spec_path = root / "Maps-Context-Engine-4D_SPEC.md"
        _validate_graph(staging, archive, spec_path)

        if staged_manifest_exists and _read_bounded(manifest_member) != canonical_manifest:
            raise ArchiveError("manifest đã có khác candidate; giữ nguyên staging để điều tra")
        if not staged_manifest_exists:
            with manifest_member.open("xb") as stream:
                stream.write(canonical_manifest)
                stream.flush()
                os.fsync(stream.fileno())
        seal_member = staging / "seal.json"
        if seal_member.exists() or seal_member.is_symlink():
            seal_bytes = _read_bounded(seal_member)
            seal = strict_json_loads(seal_bytes)
            if (not isinstance(seal, dict)
                    or seal.get("delivery_id") != delivery_id
                    or seal.get("task_id") != archive.get("task_id")
                    or seal.get("task_revision") != archive.get("task_revision")
                    or seal.get("manifest_sha256") != hashlib.sha256(canonical_manifest).hexdigest()
                    or seal_bytes != _canonical(seal)):
                raise ArchiveError("staging seal lệch candidate; giữ nguyên staging để điều tra")

        # Candidate bytes are now durable as manifest.json, so consuming this
        # generated input leaves the same exclusive staging tree that is sealed.
        if candidate_exists:
            manifest_path.unlink()
        projection = publish_archive(root, archive)
        receipt = persist_publication_projection(root, operation_id, projection)
    except (OSError, ValueError, ArchiveError) as exc:
        print(f"LỖI: {exc}", file=sys.stderr)
        return 1
    printable = {key: projection[key] for key in (
        "format_version", "kind", "planned_archive_path", "archive_manifest_sha256",
        "archive_seal_sha256", "published_at_utc", "readback_completed_at_utc", "directory_fsync",
    )}
    printable["publication_projection_ref"] = receipt.relative_to(root).as_posix()
    print(json.dumps(printable, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
