"""Publish and verify owned Git delivery evidence archives.

The sealed manifest excludes itself and ``seal.json``.  Publication uses an
exclusive same-volume directory rename and a byte-for-byte readback before it
returns an effective publication projection.
"""

from __future__ import annotations

import ctypes
import errno
import hashlib
import json
import os
import re
import shutil
import stat
import sys
import uuid
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath
from typing import Any

from .schema import SchemaError, extract_governance_schema, validate_schema
from .semantics import validate_delivery_links, validate_semantics


MAX_ARCHIVE_FILES = 4096
MAX_MEMBER_BYTES = 10 * 1024 * 1024
MAX_ARCHIVE_BYTES = 100 * 1024 * 1024
_REPARSE = 0x400
_HEX = re.compile(r"^[0-9a-f]{64}$")


class ArchiveError(ValueError):
    """Raised when an archive cannot be published or proven by readback."""


def _canonical(value: Any) -> bytes:
    if value is None or isinstance(value, (str, bool)):
        return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    if isinstance(value, int) and not isinstance(value, bool):
        if abs(value) > 9007199254740991:
            raise ArchiveError("JSON integer vượt safe integer")
        return str(value).encode("ascii")
    if isinstance(value, float):
        raise ArchiveError("archive canonical JSON không nhận số thập phân")
    if isinstance(value, list):
        return b"[" + b",".join(_canonical(item) for item in value) + b"]"
    if isinstance(value, dict):
        if any(not isinstance(key, str) for key in value):
            raise ArchiveError("JSON key phải là chuỗi")
        return b"{" + b",".join(
            _canonical(key) + b":" + _canonical(value[key]) for key in sorted(value)
        ) + b"}"
    raise ArchiveError(f"giá trị ngoài JSON primitive: {type(value).__name__}")


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _is_reparse(info: os.stat_result) -> bool:
    return stat.S_ISLNK(info.st_mode) or bool(getattr(info, "st_file_attributes", 0) & _REPARSE)


def _safe_relative(value: Any) -> str:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        raise ArchiveError(f"locator archive không an toàn: {value!r}")
    path = PurePosixPath(value)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in value.split("/")):
        raise ArchiveError(f"locator archive không relative-contained: {value!r}")
    return path.as_posix()


def _native_path(path: Path) -> Path:
    """Use Windows extended-length paths for deeply nested retained evidence."""
    if os.name != "nt":
        return path
    value = os.path.abspath(os.fspath(path))
    if value.startswith("\\\\?\\"):
        return Path(value)
    if value.startswith("\\\\"):
        return Path("\\\\?\\UNC\\" + value[2:])
    return Path("\\\\?\\" + value)


def _check_directory(path: Path, *, owner: str) -> os.stat_result:
    try:
        info = path.lstat()
    except OSError as exc:
        raise ArchiveError(f"{owner} không đọc được: {exc}") from exc
    if _is_reparse(info) or not stat.S_ISDIR(info.st_mode):
        raise ArchiveError(f"{owner} phải là directory thật, không phải link/reparse")
    return info


def _member_path(root: Path, relative: str, *, create_parent: bool = False) -> Path:
    parts = _safe_relative(relative).split("/")
    current = _native_path(root)
    for component in parts[:-1]:
        current = current / component
        if create_parent and not current.exists():
            current.mkdir()
        info = current.lstat()
        if _is_reparse(info) or not stat.S_ISDIR(info.st_mode):
            raise ArchiveError(f"archive member parent không phải directory thường: {relative}")
    target = current / parts[-1]
    if target.exists() or target.is_symlink():
        info = target.lstat()
        if _is_reparse(info) or not stat.S_ISREG(info.st_mode):
            raise ArchiveError(f"archive member không phải regular file: {relative}")
    return target


def _read_bounded(path: Path, *, limit: int = MAX_MEMBER_BYTES) -> bytes:
    info = path.lstat()
    if _is_reparse(info) or not stat.S_ISREG(info.st_mode):
        raise ArchiveError(f"evidence không phải regular file: {path}")
    if info.st_size > limit:
        raise ArchiveError(f"evidence vượt giới hạn {limit} byte: {path}")
    with path.open("rb") as stream:
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ArchiveError(f"evidence vượt giới hạn khi đọc: {path}")
    return data


def _write_exclusive(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        with path.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    except FileExistsError:
        if _read_bounded(path, limit=max(len(data), MAX_MEMBER_BYTES)) != data:
            raise ArchiveError(f"resume gặp bytes khác tại {path}")


def _fsync_directory(path: Path) -> bool:
    if os.name == "nt":
        return False
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return False
    try:
        os.fsync(fd)
        return True
    except OSError as exc:
        if exc.errno in {errno.EINVAL, errno.ENOTSUP, errno.EBADF, errno.EACCES}:
            return False
        raise
    finally:
        os.close(fd)


def _rename_noreplace(source: Path, destination: Path) -> None:
    if os.name == "nt":
        # os.rename maps to the non-replacing MoveFileEx operation on Windows.
        os.rename(source, destination)
        return
    if sys.platform.startswith("linux"):
        libc = ctypes.CDLL(None, use_errno=True)
        renameat2 = getattr(libc, "renameat2", None)
        if renameat2 is None:
            raise ArchiveError("filesystem không cung cấp renameat2(RENAME_NOREPLACE)")
        renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        renameat2.restype = ctypes.c_int
        result = renameat2(-100, os.fsencode(source), -100, os.fsencode(destination), 1)
        if result != 0:
            code = ctypes.get_errno()
            if code == errno.EEXIST:
                raise FileExistsError(code, os.strerror(code), str(destination))
            raise OSError(code, os.strerror(code), str(destination))
        return
    raise ArchiveError(f"atomic no-replace rename chưa được chứng minh trên {sys.platform}")


def _load_index(root: Path, archive: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any], dict[str, bytes]]:
    index_path = _member_path(root, "bundle-index.json")
    index_bytes = _read_bounded(index_path)
    try:
        index = json.loads(index_bytes.decode("utf-8", "strict"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ArchiveError(f"bundle-index.json không phải UTF-8 JSON: {exc}") from exc
    if not isinstance(index, dict) or set(index) != {"format_version", "objects", "raw_members"}:
        raise ArchiveError("bundle-index.json thiếu fields hoặc có field lạ")
    if index.get("format_version") != 1 or not isinstance(index.get("objects"), dict) or not isinstance(index.get("raw_members"), list):
        raise ArchiveError("bundle-index.json sai version/shape")
    listed = {item.get("path") for item in archive.get("files", []) if isinstance(item, dict)}
    if "bundle-index.json" not in listed:
        raise ArchiveError("bundle-index.json phải được giữ trong GitEvidenceArchive.files")
    typed: dict[str, Any] = {"manifest.json": {"kind": "GitEvidenceArchive", "value": archive}}
    raw: dict[str, Any] = {}
    member_bytes: dict[str, bytes] = {"bundle-index.json": index_bytes}
    refs: set[str] = set()
    for ref, kind in index["objects"].items():
        rel = _safe_relative(ref)
        if rel not in listed or rel in refs or not isinstance(kind, str) or not kind:
            raise ArchiveError(f"bundle object ref/kind sai hoặc không được manifest giữ: {ref!r}")
        refs.add(rel)
        data = _read_bounded(_member_path(root, rel))
        member_bytes[rel] = data
        try:
            value = json.loads(data.decode("utf-8", "strict"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ArchiveError(f"typed JSON member lỗi tại {rel}: {exc}") from exc
        if not isinstance(value, dict):
            raise ArchiveError(f"typed JSON member phải là object: {rel}")
        typed[rel] = {"kind": kind, "value": value}
    raw_refs: set[str] = set()
    for ref in index["raw_members"]:
        rel = _safe_relative(ref)
        if rel not in listed or rel in raw_refs or rel in refs:
            raise ArchiveError(f"raw member ref sai/trùng/không được manifest giữ: {ref!r}")
        raw_refs.add(rel)
        data = _read_bounded(_member_path(root, rel))
        member_bytes[rel] = data
        try:
            text = data.decode("utf-8", "strict")
        except UnicodeDecodeError as exc:
            raise ArchiveError(f"raw member không phải UTF-8 tại {rel}: {exc}") from exc
        try:
            raw[rel] = json.loads(text)
        except json.JSONDecodeError as exc:
            if Path(rel).suffix.casefold() not in {".md", ".txt", ".log"}:
                raise ArchiveError(f"raw JSON member lỗi tại {rel}: {exc}") from exc
            # Selection invalidation may retain exact human-readable evidence.
            # Keep its decoded text for graph checks while member_bytes preserves
            # the original bytes and manifest hash without normalization.
            raw[rel] = text
    return typed, raw, member_bytes


def _validate_graph(root: Path, archive: dict[str, Any], spec_path: Path) -> None:
    try:
        schema = extract_governance_schema(spec_path.read_text(encoding="utf-8", errors="strict"))
    except (OSError, UnicodeDecodeError, SchemaError) as exc:
        raise ArchiveError(f"không nạp được governance schema: {exc}") from exc
    errors = validate_schema(archive, schema["$defs"]["GitEvidenceArchive"], root_schema=schema)
    errors.extend(validate_semantics("GitEvidenceArchive", archive))
    if errors:
        raise ArchiveError("manifest không hợp lệ: " + "; ".join(errors[:8]))
    objects, raw, member_bytes = _load_index(root, archive)
    result = validate_delivery_links(objects, raw_members=raw, raw_member_bytes=member_bytes)
    if result["verdict"] == "REJECT":
        raise ArchiveError("evidence graph bị reject: " + "; ".join(result["errors"][:8]))


def _verify_tree(root: Path, archive: dict[str, Any], seal: dict[str, Any], spec_path: Path) -> dict[str, Any]:
    _check_directory(root, owner="archive readback")
    manifest_bytes = _read_bounded(root / "manifest.json")
    seal_bytes = _read_bounded(root / "seal.json")
    expected_manifest = _canonical(archive)
    if manifest_bytes != expected_manifest:
        raise ArchiveError("manifest readback bytes không khớp canonical GitEvidenceArchive")
    try:
        actual_seal = json.loads(seal_bytes.decode("utf-8", "strict"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ArchiveError(f"seal.json không phải UTF-8 JSON: {exc}") from exc
    if actual_seal != seal or seal_bytes != _canonical(seal):
        raise ArchiveError("seal readback bytes/identity không khớp")
    if seal.get("manifest_sha256") != _sha256(manifest_bytes):
        raise ArchiveError("seal manifest_sha256 không khớp bytes manifest")

    files = archive.get("files")
    if not isinstance(files, list) or not 3 <= len(files) <= MAX_ARCHIVE_FILES:
        raise ArchiveError("archive files phải có 3..4096 member")
    expected_paths: set[str] = set()
    total = 0
    for item in files:
        if not isinstance(item, dict):
            raise ArchiveError("archive files chứa item không phải object")
        rel = _safe_relative(item.get("path"))
        if rel in {"manifest.json", "seal.json"} or rel in expected_paths:
            raise ArchiveError(f"archive member path trùng hoặc dành riêng: {rel}")
        expected_paths.add(rel)
        data = _read_bounded(_member_path(root, rel))
        total += len(data)
        if len(data) != item.get("byte_length") or _sha256(data) != item.get("sha256"):
            raise ArchiveError(f"archive member byte_length/SHA readback lệch: {rel}")
        if total > MAX_ARCHIVE_BYTES:
            raise ArchiveError("archive tổng vượt 100 MiB")
    actual_paths: set[str] = set()
    native_root = _native_path(root)
    for directory, dirs, names in os.walk(native_root, followlinks=False):
        base = Path(directory)
        for name in dirs:
            if _is_reparse((base / name).lstat()):
                raise ArchiveError(f"archive chứa directory link/reparse: {base / name}")
        for name in names:
            path = base / name
            if _is_reparse(path.lstat()):
                raise ArchiveError(f"archive chứa file link/reparse: {path}")
            actual_paths.add(path.relative_to(native_root).as_posix())
    if actual_paths != expected_paths | {"manifest.json", "seal.json"}:
        raise ArchiveError("archive tree có member thiếu hoặc ngoài manifest")
    _validate_graph(root, archive, spec_path)
    return {
        "format_version": 1,
        "kind": "archive_publication",
        "planned_archive_path": f"artifacts/git-delivery/{archive['delivery_id']}",
        "archive_manifest_sha256": _sha256(manifest_bytes),
        "archive_seal_sha256": _sha256(seal_bytes),
    }


def publish_archive(
    repo_root: Path,
    archive: dict[str, Any],
    *,
    spec_path: Path | None = None,
) -> dict[str, Any]:
    """Seal staging bytes, atomically publish without replacement and read them back.

    An already-published matching archive is returned idempotently.  A partial
    staging archive is resumed only when pre-existing manifest/seal bytes match
    the exact immutable input record.
    """
    root = repo_root.resolve(strict=True)
    _check_directory(root / "artifacts", owner="workspace artifacts root")
    archive_root = root / "artifacts" / "git-delivery"
    parent_info = _check_directory(archive_root, owner="approved archive root")
    try:
        delivery_id = str(uuid.UUID(archive.get("delivery_id", "")))
    except (ValueError, TypeError, AttributeError) as exc:
        raise ArchiveError("delivery_id phải là UUID lowercase") from exc
    if delivery_id != archive.get("delivery_id"):
        raise ArchiveError("delivery_id UUID phải canonical lowercase")
    staging = archive_root / f".staging-{delivery_id}"
    final = archive_root / delivery_id
    if staging.parent != final.parent or staging.name != f".staging-{delivery_id}":
        raise ArchiveError("staging/final path không deterministic")
    if spec_path is None:
        spec_path = root / "Maps-Context-Engine-4D_SPEC.md"
    manifest = _canonical(archive)
    seal_path = staging / "seal.json"
    if seal_path.exists():
        try:
            existing_seal_bytes = _read_bounded(seal_path)
            seal = json.loads(existing_seal_bytes.decode("utf-8", "strict"))
        except (ArchiveError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ArchiveError(f"staging seal không thể resume an toàn: {exc}") from exc
        if (not isinstance(seal, dict)
                or seal.get("delivery_id") != delivery_id
                or seal.get("task_id") != archive.get("task_id")
                or seal.get("task_revision") != archive.get("task_revision")
                or seal.get("manifest_sha256") != _sha256(manifest)
                or existing_seal_bytes != _canonical(seal)):
            raise ArchiveError("staging seal không bind đúng immutable manifest")
    else:
        seal = {
            "format_version": 1,
            "delivery_id": delivery_id,
            "task_id": archive.get("task_id"),
            "task_revision": archive.get("task_revision"),
            "manifest_sha256": _sha256(manifest),
            "sealed_at_utc": _utc_now(),
        }
    schema = extract_governance_schema(spec_path.read_text(encoding="utf-8", errors="strict"))
    seal_errors = validate_schema(seal, schema["$defs"]["GitArchiveSeal"], root_schema=schema)
    seal_errors.extend(validate_semantics("GitArchiveSeal", seal))
    if seal_errors:
        raise ArchiveError("seal không hợp lệ: " + "; ".join(seal_errors[:8]))

    if final.exists():
        _check_directory(final, owner="published archive")
        if staging.exists():
            raise ArchiveError("cùng lúc tồn tại staging và final; cần điều tra trạng thái thay vì tự xóa")
        # A retry may arrive after publication.  Read and verify the immutable
        # manifest and its original seal; never rewrite a published archive.
        manifest_bytes = _read_bounded(final / "manifest.json")
        try:
            published_archive = json.loads(manifest_bytes.decode("utf-8", "strict"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ArchiveError(f"published manifest lỗi: {exc}") from exc
        if published_archive != archive:
            raise ArchiveError("final archive đã tồn tại với identity/bytes khác")
        published_seal = json.loads(_read_bounded(final / "seal.json").decode("utf-8", "strict"))
        projection = _verify_tree(final, archive, published_seal, spec_path)
        projection.update({"published_at_utc": None, "readback_completed_at_utc": _utc_now(),
                           "directory_fsync": "not_claimed_on_resume"})
        return projection

    stage_info = _check_directory(staging, owner="archive staging")
    if stage_info.st_dev != parent_info.st_dev:
        raise ArchiveError("staging và final không cùng volume/filesystem")
    _write_exclusive(staging / "manifest.json", manifest)
    seal_bytes = _canonical(seal)
    _write_exclusive(staging / "seal.json", seal_bytes)
    projection = _verify_tree(staging, archive, seal, spec_path)
    _fsync_directory(staging)
    _fsync_directory(archive_root)
    _rename_noreplace(staging, final)
    published_at = _utc_now()
    projection = _verify_tree(final, archive, seal, spec_path)
    readback_at = _utc_now()
    projection.update({
        "published_at_utc": published_at,
        "readback_completed_at_utc": readback_at,
        "directory_fsync": "supported" if _fsync_directory(archive_root) else "unsupported",
    })
    return projection


def verify_archive(repo_root: Path, delivery_id: str, *, spec_path: Path | None = None) -> dict[str, Any]:
    """Verify a published archive's seal, exact member set, hashes and links."""
    root = repo_root.resolve(strict=True)
    _check_directory(root / "artifacts", owner="workspace artifacts root")
    try:
        canonical_id = str(uuid.UUID(delivery_id))
    except (ValueError, TypeError, AttributeError) as exc:
        raise ArchiveError("delivery_id không phải UUID") from exc
    if canonical_id != delivery_id:
        raise ArchiveError("delivery_id phải canonical lowercase")
    final = root / "artifacts" / "git-delivery" / delivery_id
    _check_directory(final.parent, owner="approved archive root")
    _check_directory(final, owner="published archive")
    manifest_bytes = _read_bounded(final / "manifest.json")
    seal_bytes = _read_bounded(final / "seal.json")
    try:
        archive = json.loads(manifest_bytes.decode("utf-8", "strict"))
        seal = json.loads(seal_bytes.decode("utf-8", "strict"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ArchiveError(f"archive metadata không phải UTF-8 JSON: {exc}") from exc
    if spec_path is None:
        spec_path = root / "Maps-Context-Engine-4D_SPEC.md"
    projection = _verify_tree(final, archive, seal, spec_path)
    projection.update({"published_at_utc": None, "readback_completed_at_utc": _utc_now(),
                       "directory_fsync": "not_observed_by_readback"})
    return projection


def persist_publication_projection(repo_root: Path, operation_id: str, projection: dict[str, Any]) -> Path:
    """Retain a post-readback publication receipt outside the sealed archive."""
    root = repo_root.resolve(strict=True)
    _check_directory(root / "artifacts", owner="workspace artifacts root")
    archive_root = root / "artifacts" / "git-delivery"
    _check_directory(archive_root, owner="approved archive root")
    try:
        canonical_id = str(uuid.UUID(operation_id))
    except (ValueError, TypeError, AttributeError) as exc:
        raise ArchiveError("publication operation ID phải là UUID") from exc
    if canonical_id != operation_id:
        raise ArchiveError("publication operation ID phải canonical lowercase")
    required = {"format_version", "kind", "planned_archive_path", "archive_manifest_sha256",
                "archive_seal_sha256", "published_at_utc", "readback_completed_at_utc"}
    record = {key: projection.get(key) for key in required}
    if set(record) != required or any(value is None for value in record.values()):
        raise ArchiveError("publication projection thiếu trường actual publish/readback")
    if record["format_version"] != 1 or record["kind"] != "archive_publication":
        raise ArchiveError("publication projection sai version/kind")
    if not _HEX.fullmatch(str(record["archive_manifest_sha256"])) or not _HEX.fullmatch(str(record["archive_seal_sha256"])):
        raise ArchiveError("publication projection SHA-256 sai shape")
    published = datetime.fromisoformat(str(record["published_at_utc"]).replace("Z", "+00:00"))
    readback = datetime.fromisoformat(str(record["readback_completed_at_utc"]).replace("Z", "+00:00"))
    if published.tzinfo is None or readback.tzinfo is None or readback < published:
        raise ArchiveError("publication projection chronology không hợp lệ")
    operations = archive_root / "operations"
    if not operations.exists():
        operations.mkdir()
    _check_directory(operations, owner="publication operations root")
    operation_root = operations / operation_id
    operation_root.mkdir(exist_ok=False)
    _check_directory(operation_root, owner="publication operation root")
    path = operation_root / "publication.json"
    data = _canonical(record)
    _write_exclusive(path, data)
    _fsync_directory(operation_root)
    _fsync_directory(operations)
    if _read_bounded(path) != data:
        raise ArchiveError("publication projection flush/readback không khớp")
    return path
