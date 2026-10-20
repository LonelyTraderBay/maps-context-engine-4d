"""Immutable provider metadata and a cross-platform source digest."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path


def _canonical_text_bytes(data: bytes) -> bytes:
    text = data.decode("utf-8", errors="strict")
    return text.replace("\r\n", "\n").replace("\r", "\n").encode("utf-8")


def compute_provider_manifest_digest(files: list[tuple[str, bytes]]) -> str:
    manifest: list[dict[str, object]] = []
    seen: set[str] = set()
    for relative_path, source in files:
        if not isinstance(relative_path, str) or not isinstance(source, bytes):
            raise TypeError("provider manifest entries must contain a path and bytes")
        if relative_path in seen:
            raise ValueError("provider manifest contains a duplicate path")
        seen.add(relative_path)
        normalized = _canonical_text_bytes(source)
        manifest.append({
            "byte_length": len(normalized),
            "path": relative_path,
            "sha256": hashlib.sha256(normalized).hexdigest(),
        })
    manifest.sort(key=lambda item: item["path"].encode("utf-8"))
    payload = json.dumps(manifest, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def _provider_digest() -> str:
    package_root = Path(__file__).resolve().parents[2]
    source_root = Path(__file__).resolve().parent
    paths = [package_root / "package.json"]
    paths.extend(sorted(source_root.glob("*.py"), key=lambda item: item.name.encode("utf-8")))
    paths.append(source_root / "python-profile.v1.schema.json")
    return compute_provider_manifest_digest([
        (path.relative_to(package_root).as_posix(), path.read_bytes())
        for path in paths
    ])


language_provider_descriptor = {
    "id": "python-v1",
    "version": "1.0.0",
    "license": "Apache-2.0",
    "digest": _provider_digest(),
    "languages": ["python"],
    "capabilities": [
        {"id": "diagnostics.gaps", "version": "1.0.0"},
        {"id": "relations.imports", "version": "1.0.0"},
        {"id": "relations.scopes", "version": "1.0.0"},
        {"id": "symbols.syntax", "version": "1.0.0"},
    ],
}
