"""Pure T107 contract oracles. These fixtures do not enforce runtime behavior."""

from __future__ import annotations

import json
import re
from collections import defaultdict
from collections.abc import Callable, Mapping, Sequence
from typing import Any

SAFE_INTEGER = 9_007_199_254_740_991


class FixtureContractError(ValueError):
    """A synthetic contract input is malformed or cannot be safely admitted."""


def _nonnegative_integer(value: Any, field: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0 or value > SAFE_INTEGER:
        raise FixtureContractError(f"{field} must be a safe nonnegative integer")
    return value


def _safe_sum(values: Sequence[int], field: str) -> int:
    total = 0
    for value in values:
        total += _nonnegative_integer(value, field)
        if total > SAFE_INTEGER:
            raise FixtureContractError(f"{field} sum exceeds safe integer")
    return total


def _same_typed_value(left: Any, right: Any) -> bool:
    if type(left) is not type(right):
        return False
    if isinstance(left, dict):
        return left.keys() == right.keys() and all(_same_typed_value(left[key], right[key]) for key in left)
    if isinstance(left, list):
        return len(left) == len(right) and all(_same_typed_value(a, b) for a, b in zip(left, right))
    return left == right


def measure_serialized_payload(
    build: Callable[[int], bytes], *, max_bytes: int, max_iterations: int = 8
) -> tuple[bytes, int]:
    """Measure the whole serialized payload to a byte-count fixed point."""
    cap = _nonnegative_integer(max_bytes, "max_bytes")
    if not isinstance(max_iterations, int) or isinstance(max_iterations, bool) or max_iterations < 1:
        raise FixtureContractError("max_iterations must be a positive integer")
    count = 0
    for _ in range(max_iterations):
        raw = build(count)
        if not isinstance(raw, bytes):
            raise FixtureContractError("serializer must return bytes")
        try:
            json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise FixtureContractError("serialized payload must be valid UTF-8 JSON") from exc
        measured = len(raw)
        if measured == count:
            if measured > cap:
                raise FixtureContractError("serialized payload exceeds byte cap")
            return raw, measured
        count = measured
    raise FixtureContractError("serialized byte count did not converge within the bounded iterations")


def select_delivery_mode(mode: str | None, *, host_version_verified: bool) -> str:
    selected = "mcp_dual" if mode is None else mode
    if selected not in {"mcp_dual", "mcp_structured_verified", "mcp_text"}:
        raise FixtureContractError("unknown MCP delivery mode")
    if selected == "mcp_structured_verified" and host_version_verified is not True:
        raise FixtureContractError("structured-only mode requires host/version evidence")
    return selected


def group_destinations(destinations: Mapping[str, str]) -> dict[str, tuple[str, ...]]:
    if not isinstance(destinations, Mapping) or not destinations:
        raise FixtureContractError("at least one destination is required")
    grouped: dict[str, list[str]] = defaultdict(list)
    for path_id, volume_id in destinations.items():
        if not isinstance(path_id, str) or not path_id.strip() or not isinstance(volume_id, str) or not volume_id.strip():
            raise FixtureContractError("destination and volume identities must be nonempty strings")
        grouped[volume_id].append(path_id)
    return {volume: tuple(sorted(paths)) for volume, paths in sorted(grouped.items())}


def remaining_reservation(peak_bytes: int, already_written_bytes: int) -> int:
    peak = _nonnegative_integer(peak_bytes, "peak_bytes")
    written = _nonnegative_integer(already_written_bytes, "already_written_bytes")
    # Actual writes are already reflected in available bytes; only future growth remains reserved.
    return max(0, peak - written)


def preflight_volumes(
    destinations: Mapping[str, str], observations: Mapping[str, Mapping[str, Any]]
) -> dict[str, dict[str, Any]]:
    if not isinstance(observations, Mapping):
        raise FixtureContractError("volume observations must be a mapping")
    grouped = group_destinations(destinations)
    result: dict[str, dict[str, Any]] = {}
    for volume_id, paths in grouped.items():
        observed = observations.get(volume_id)
        if not isinstance(observed, Mapping) or observed.get("measured") is not True:
            raise FixtureContractError(f"volume {volume_id} has no successful measurement")
        available = _nonnegative_integer(observed.get("available_bytes"), "available_bytes")
        block = _nonnegative_integer(observed.get("block_threshold_bytes"), "block_threshold_bytes")
        warning = _nonnegative_integer(observed.get("warning_threshold_bytes"), "warning_threshold_bytes")
        reserve = _nonnegative_integer(observed.get("safety_reserve_bytes"), "safety_reserve_bytes")
        if warning < block:
            raise FixtureContractError("warning threshold must be at least the block threshold")
        new_job = _nonnegative_integer(observed.get("new_job_peak_remaining_bytes"), "new_job_peak_remaining_bytes")
        other_jobs = observed.get("other_jobs", [])
        if not isinstance(other_jobs, list):
            raise FixtureContractError("other_jobs must be a list of observed reservations")
        remaining: list[int] = []
        for job in other_jobs:
            if not isinstance(job, Mapping):
                raise FixtureContractError("other job reservation must be an object")
            remaining.append(remaining_reservation(job.get("peak_bytes"), job.get("already_written_bytes")))
        demand = _safe_sum([new_job, *remaining, reserve], "required_bytes")
        required = max(block, demand)
        result[volume_id] = {
            "destination_ids": list(paths),
            "available_bytes": available,
            "required_bytes": required,
            "admitted": available >= required,
            "warning": available < warning,
            "remaining_other_job_reservations": remaining,
        }
    return result


def concurrency_decision(output_resource_ids: Sequence[str], active_writer_resource_ids: Sequence[str] | None) -> str:
    if active_writer_resource_ids is None:
        return "UNKNOWN_WRITERS"
    if not isinstance(output_resource_ids, Sequence) or isinstance(output_resource_ids, (str, bytes)):
        raise FixtureContractError("output resources must be a sequence")
    if not isinstance(active_writer_resource_ids, Sequence) or isinstance(active_writer_resource_ids, (str, bytes)):
        raise FixtureContractError("active writer resources must be a sequence or unknown")
    if any(not isinstance(item, str) or not item for item in (*output_resource_ids, *active_writer_resource_ids)):
        raise FixtureContractError("resource identities must be nonempty strings")
    return "SERIALIZE_SHARED_RESOURCE" if set(output_resource_ids) & set(active_writer_resource_ids) else "PARALLEL_ISOLATED_OUTPUTS"


def validate_command_contract(record: Mapping[str, Any]) -> list[str]:
    errors: list[str] = []
    if not isinstance(record, Mapping):
        return ["command contract must be an object"]
    for field in ("command_id", "executable", "cwd", "command_source_ref", "permission_ref", "run_id"):
        if not isinstance(record.get(field), str) or not record[field].strip():
            errors.append(f"{field} must be present and verified")
    argv = record.get("argv")
    if not isinstance(argv, list) or not argv or any(not isinstance(arg, str) or not arg for arg in argv):
        errors.append("argv must contain the exact executable arguments")
    else:
        if argv[0] != record.get("executable"):
            errors.append("argv[0] must match the verified executable")
        if any(re.search(r"(?:\{\{.*?\}\}|<[^>]+>|\bTODO\b|\.\.\.)", arg, re.IGNORECASE) for arg in argv):
            errors.append("placeholder argv cannot be executed")
    side_effect = record.get("side_effect_class")
    if side_effect not in {"read_only", "temporary_write", "durable_write", "external_service"}:
        errors.append("side_effect_class must be explicit")
    if record.get("dry_run") is True and side_effect != "read_only":
        errors.append("dry-run label conflicts with declared side effects")
    versions = record.get("tool_versions")
    if not isinstance(versions, Mapping) or not versions or any(not isinstance(v, str) or not v for v in versions.values()):
        errors.append("tool_versions must record actual tool versions")
    for field in ("expected_writes", "temporary_roots", "cache_roots", "log_roots", "download_roots", "child_processes", "network_scope"):
        if field not in record:
            errors.append(f"{field} must be recorded, including an empty list when none")
    return errors


def validate_dry_run(record: Mapping[str, Any]) -> list[str]:
    errors: list[str] = []
    for field in ("workload_executed", "run_record_created", "filesystem_mutated", "network_called", "installed_or_repaired"):
        if record.get(field) is not False:
            errors.append(f"dry-run must prove {field}=false")
    if record.get("report_channel") not in {"stdout", "stderr", "approved_external_output"}:
        errors.append("dry-run needs an approved report output channel")
    if record.get("candidate_inventory_read_only") is not True:
        errors.append("dry-run candidate inventory must be read-only")
    return errors


def finalize_command(
    *,
    command_exit_status: int | None,
    termination_reason: str | None,
    writers_stopped: bool,
    cleanup_required: bool,
    cleanup_status: str,
) -> dict[str, Any]:
    if command_exit_status is not None and (
        not isinstance(command_exit_status, int) or isinstance(command_exit_status, bool) or not 0 <= command_exit_status <= 255
    ):
        raise FixtureContractError("command_exit_status must be an observed exit code or null")
    if cleanup_status not in {"NOT_STARTED", "SUCCEEDED", "FAILED", "UNKNOWN", "NOT_REQUIRED"}:
        raise FixtureContractError("cleanup_status is not recognized")
    command_result = "CANCELLED" if termination_reason == "cancelled" else (
        "UNKNOWN" if command_exit_status is None else "SUCCEEDED" if command_exit_status == 0 else "FAILED"
    )
    if not writers_stopped:
        return {
            "command_exit_status": command_exit_status,
            "termination_reason": termination_reason,
            "command_result": command_result,
            "cleanup_status": "NOT_STARTED",
            "final_status": "FINALIZATION_PENDING",
        }
    if not cleanup_required and cleanup_status == "NOT_REQUIRED":
        final = command_result
    elif cleanup_status == "FAILED":
        final = "FAILED" if command_result == "SUCCEEDED" else command_result
    elif cleanup_status in {"UNKNOWN", "NOT_STARTED"}:
        final = "UNKNOWN" if command_result == "SUCCEEDED" else command_result
    elif cleanup_required and cleanup_status != "SUCCEEDED":
        final = "UNKNOWN" if command_result == "SUCCEEDED" else command_result
    else:
        final = command_result
    return {
        "command_exit_status": command_exit_status,
        "termination_reason": termination_reason,
        "command_result": command_result,
        "cleanup_status": cleanup_status,
        "final_status": final,
    }


def cleanup_disposition(candidate: Mapping[str, Any]) -> str:
    """Return a decision only; this fixture never removes filesystem entries."""
    if not isinstance(candidate, Mapping):
        return "REFUSE_MALFORMED"
    if candidate.get("path_kind") not in {"fresh_synthetic_repo_copy", "fresh_temporary_sqlite_copy", "fresh_mutable_source_copy"}:
        return "REFUSE_PROTECTED_OR_UNSUPPORTED_KIND"
    gates = (
        "same_run", "baseline_absent", "reconstructible", "exact_candidate_listed", "owner_verified",
        "contained", "no_links_or_reparse_points", "writers_stopped", "evidence_retained", "not_deliverable",
    )
    if candidate.get("authority_ref") not in {"TEARDOWN-01", "TEARDOWN-FIXTURE-01"}:
        return "REFUSE_NO_SCOPED_AUTHORITY"
    if any(candidate.get(gate) is not True for gate in gates):
        return "REFUSE_INCOMPLETE_OWNERSHIP_PROOF"
    if candidate.get("crash_leftover") is True or candidate.get("other_run") is True:
        return "REFUSE_STALE_OR_OTHER_RUN"
    return "ALLOW_FRESH_SYNTHETIC_FIXTURE_ONLY"


def evidence_applicable(evidence: Mapping[str, Any], expected: Mapping[str, Any]) -> bool:
    if not isinstance(evidence, Mapping) or not isinstance(expected, Mapping):
        return False
    if evidence.get("result") != "PASS" or evidence.get("execution_verified") is not True:
        return False
    logs = evidence.get("log_refs")
    if not isinstance(logs, list) or not logs or any(not isinstance(item, str) or not item for item in logs):
        return False
    for field in ("command_ref", "subject_sha", "task_id", "task_revision", "delivery_id", "fingerprints"):
        if field not in expected or field not in evidence or not _same_typed_value(evidence[field], expected[field]):
            return False
    return True


def preview_status(state: Mapping[str, Any], *, permission_valid: bool, sdk_live: bool) -> dict[str, Any]:
    """Read-only status projection; it never renews the session or permission."""
    if not isinstance(state, Mapping):
        raise FixtureContractError("preview state must be an object")
    state_copy = json.loads(json.dumps(state, ensure_ascii=False))
    ready = permission_valid is True and sdk_live is True and state.get("server_state") == "bound"
    return {"ready": ready, "shielded": not ready, "state": state_copy}


def mode_ack_decision(expected: Mapping[str, Any], ack: Mapping[str, Any], *, permission_valid: bool, sdk_live: bool) -> str:
    keys = ("session_id", "build_id", "doc_epoch", "hmr_epoch", "command_id", "auth_epoch")
    if permission_valid is not True or sdk_live is not True:
        return "KEEP_SHIELDED_DISARM"
    if any(key not in expected or key not in ack or not _same_typed_value(expected[key], ack[key]) for key in keys):
        return "KEEP_SHIELDED_DISARM"
    if expected.get("deadline_passed") is True or expected.get("origin_valid") is not True:
        return "KEEP_SHIELDED_DISARM"
    return "ALLOW_BOUND_ACK_ONLY"


def continuation_decision(
    expected_scope: Mapping[str, Any], actual_scope: Mapping[str, Any], *, expired: bool, spool_available: bool
) -> str:
    if expired:
        return "SNAPSHOT_EXPIRED"
    if not spool_available:
        return "RESULT_EXPIRED"
    if not _same_typed_value(expected_scope, actual_scope):
        return "SCOPE_OR_REQUEST_CHANGED"
    return "CONTINUE_PINNED_RESULT"


def config_cas_decision(*, expected_revision: int, current_revision: int, candidate_valid: bool, validate_only: bool) -> str:
    for field, value in (("expected_revision", expected_revision), ("current_revision", current_revision)):
        if not isinstance(value, int) or isinstance(value, bool) or value < 1 or value > SAFE_INTEGER:
            raise FixtureContractError(f"{field} must be a positive safe integer")
    if not isinstance(candidate_valid, bool) or not isinstance(validate_only, bool):
        raise FixtureContractError("candidate_valid and validate_only must be booleans")
    if not candidate_valid:
        return "REJECT_INVALID_NO_WRITE"
    if validate_only:
        return "VALIDATED_NO_WRITE"
    if expected_revision != current_revision:
        return "CONFIG_CONFLICT_NO_WRITE"
    return "CAS_REPLACE_WHOLE_CONFIG"
