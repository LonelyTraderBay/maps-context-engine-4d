"""Closed Python profile validation against the sealed local runtime."""

from __future__ import annotations

import hashlib
import re
import sys
from pathlib import Path
from typing import Any

from .descriptor import language_provider_descriptor

MAX_FILES = 4096
MAX_SOURCE_BYTES = 10 * 1024 * 1024
MAX_PATH_BYTES = 1024 * 1024
MAX_FRAME_BYTES = 16 * 1024 * 1024
HASH_RE = re.compile(r"^[0-9a-f]{64}$")
VERSION_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")
PROFILE_KEYS = {"schema_id", "schema_version", "profile_id", "config"}
CONFIG_KEYS = {
    "schema_version",
    "language",
    "python_implementation",
    "python_version",
    "python_executable_digest",
    "provider_digest",
    "module_roots",
    "analysis_mode",
    "allow_project_plugins",
}
REQUEST_KEYS = {
    "schema_version", "message_type", "request_id", "language", "profile", "files", "execution_policy"
}
POLICY = {
    "target_code": "never_execute",
    "project_scripts": "never_execute",
    "project_plugins": "never_execute",
    "notebook_cells": "never_execute",
    "network": False,
}
METADATA_NAMES = {
    ".python-version", "pyproject.toml", "requirements.txt", "requirements-dev.txt",
    "pipfile", "pipfile.lock", "poetry.lock", "uv.lock", "pyvenv.cfg", "environment.yml",
}


class ProviderInputError(ValueError):
    """A bounded validation error that never includes target source text."""


def _exact_keys(value: Any, keys: set[str]) -> bool:
    return isinstance(value, dict) and set(value) == keys


def _repo_path(value: Any) -> str:
    if not isinstance(value, str) or not value or len(value.encode("utf-8")) > 4096:
        raise ProviderInputError("source path is invalid")
    if "\\" in value or ":" in value or "\0" in value or value.startswith("/"):
        raise ProviderInputError("source path is not repository-relative")
    parts = value.split("/")
    if any(part in ("", ".", "..") for part in parts):
        raise ProviderInputError("source path is not normalized")
    return value


def _python_executable_digest() -> str:
    if sys.version_info < (3, 12):
        raise ProviderInputError("Python provider requires CPython 3.12 or newer")
    try:
        executable = Path(sys.executable).resolve(strict=True)
        return hashlib.sha256(executable.read_bytes()).hexdigest()
    except (OSError, RuntimeError) as exc:
        raise ProviderInputError("sealed Python executable is unavailable") from exc


def create_local_profile(module_roots: list[str], profile_id: str, analysis_mode: str = "syntax") -> dict[str, Any]:
    """Build the local effective profile; the host supplies its canonical profile ID."""
    if not isinstance(profile_id, str) or not HASH_RE.fullmatch(profile_id):
        raise ProviderInputError("profile ID must be a lowercase SHA-256")
    if sys.implementation.name != "cpython" or sys.version_info < (3, 12):
        raise ProviderInputError("Python provider requires CPython 3.12 or newer")
    if not isinstance(module_roots, list):
        raise ProviderInputError("module roots must be a list")
    roots = [_repo_path(root) for root in module_roots]
    if not roots or roots != sorted(set(roots), key=lambda item: item.encode("utf-8")):
        raise ProviderInputError("module roots must be nonempty, unique and sorted")
    if analysis_mode not in ("syntax", "typed_when_available"):
        raise ProviderInputError("analysis mode is unsupported")
    config = {
        "schema_version": 1,
        "language": "python",
        "python_implementation": "CPython",
        "python_version": ".".join(map(str, sys.version_info[:3])),
        "python_executable_digest": _python_executable_digest(),
        "provider_digest": language_provider_descriptor["digest"],
        "module_roots": roots,
        "analysis_mode": analysis_mode,
        "allow_project_plugins": False,
    }
    return {"schema_id": "python-profile", "schema_version": 1, "profile_id": profile_id, "config": config}


def validate_provider_request(request: Any) -> tuple[dict[str, Any], dict[str, bytes]]:
    if not _exact_keys(request, REQUEST_KEYS):
        raise ProviderInputError("provider request has missing or unknown fields")
    if request["schema_version"] != 1 or request["message_type"] != "analyze_request":
        raise ProviderInputError("provider request protocol identity is invalid")
    if not isinstance(request["request_id"], str) or not re.fullmatch(
        r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", request["request_id"]
    ):
        raise ProviderInputError("provider request ID is invalid")
    if request["language"] != "python":
        raise ProviderInputError("provider language is unsupported")

    profile = request["profile"]
    if not _exact_keys(profile, PROFILE_KEYS) or profile["schema_id"] != "python-profile" or profile["schema_version"] != 1:
        raise ProviderInputError("Python profile identity is invalid")
    if not isinstance(profile["profile_id"], str) or not HASH_RE.fullmatch(profile["profile_id"]):
        raise ProviderInputError("Python profile ID is invalid")
    config = profile["config"]
    if not _exact_keys(config, CONFIG_KEYS):
        raise ProviderInputError("Python profile config has missing or unknown fields")
    if (
        type(config["schema_version"]) is not int
        or config["schema_version"] != 1
        or config["language"] != "python"
        or config["python_implementation"] != "CPython"
        or not isinstance(config["python_version"], str)
        or len(config["python_version"]) > 32
        or not VERSION_RE.fullmatch(config["python_version"])
        or not isinstance(config["python_executable_digest"], str)
        or not HASH_RE.fullmatch(config["python_executable_digest"])
        or config["provider_digest"] != language_provider_descriptor["digest"]
        or config["analysis_mode"] not in ("syntax", "typed_when_available")
        or config["allow_project_plugins"] is not False
    ):
        raise ProviderInputError("Python profile does not match the sealed provider")
    if config["python_version"] != ".".join(map(str, sys.version_info[:3])):
        raise ProviderInputError("Python runtime version does not match the profile")
    if sys.implementation.name != "cpython" or sys.version_info < (3, 12):
        raise ProviderInputError("Python provider requires CPython 3.12 or newer")
    if config["python_executable_digest"] != _python_executable_digest():
        raise ProviderInputError("Python executable digest does not match the profile")

    roots_value = config["module_roots"]
    if not isinstance(roots_value, list) or not roots_value or len(roots_value) > 64:
        raise ProviderInputError("Python profile must contain module roots")
    roots = [_repo_path(root) for root in roots_value]
    if roots != sorted(set(roots), key=lambda item: item.encode("utf-8")):
        raise ProviderInputError("Python module roots must be unique and sorted")

    policy = request["execution_policy"]
    if (
        not _exact_keys(policy, set(POLICY))
        or any(policy[key] != value or (key == "network" and type(policy[key]) is not bool) for key, value in POLICY.items())
    ):
        raise ProviderInputError("provider execution policy is not closed")
    files = request["files"]
    if not isinstance(files, list) or not files or len(files) > MAX_FILES:
        raise ProviderInputError("provider file count is outside its limit")
    source_by_path: dict[str, bytes] = {}
    previous_path: bytes | None = None
    source_bytes = 0
    path_bytes = 0
    for entry in files:
        if not _exact_keys(entry, {"path", "content_base64", "content_sha256"}):
            raise ProviderInputError("provider file entry has missing or unknown fields")
        path = _repo_path(entry["path"])
        encoded_path = path.encode("utf-8")
        if previous_path is not None and encoded_path <= previous_path:
            raise ProviderInputError("provider file paths must be unique and sorted")
        previous_path = encoded_path
        path_bytes += len(encoded_path)
        if not isinstance(entry["content_base64"], str) or len(entry["content_base64"]) > MAX_FRAME_BYTES:
            raise ProviderInputError("provider file encoding exceeds its limit")
        import base64

        try:
            content = base64.b64decode(entry["content_base64"], validate=True)
        except (ValueError, TypeError) as exc:
            raise ProviderInputError("provider file encoding is invalid") from exc
        if base64.b64encode(content).decode("ascii") != entry["content_base64"]:
            raise ProviderInputError("provider file encoding is not canonical")
        if not isinstance(entry["content_sha256"], str) or not HASH_RE.fullmatch(entry["content_sha256"]):
            raise ProviderInputError("provider file digest is invalid")
        if hashlib.sha256(content).hexdigest() != entry["content_sha256"]:
            raise ProviderInputError("provider file digest does not match its bytes")
        source_bytes += len(content)
        if source_bytes > MAX_SOURCE_BYTES or path_bytes > MAX_PATH_BYTES:
            raise ProviderInputError("provider source or path budget exceeded")
        source_by_path[path] = content

    python_sources = [
        path for path in source_by_path
        if path.endswith((".py", ".pyi"))
    ]
    if not python_sources:
        raise ProviderInputError("provider request contains no Python source files")
    for root in roots:
        if not any(path.startswith(root + "/") and path.endswith((".py", ".pyi")) for path in python_sources):
            raise ProviderInputError("Python module root has no captured source")
    if any(
        path.endswith((".py", ".pyi"))
        and not any(path.startswith(root + "/") for root in roots)
        for path in python_sources
    ):
        raise ProviderInputError("Python source is outside the effective module roots")
    return config, source_by_path
