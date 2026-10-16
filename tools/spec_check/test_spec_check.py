"""Regression fixtures cho cấu trúc, parity và schema S21."""

from __future__ import annotations

import copy
import contextlib
import hashlib
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from zipfile import ZipFile

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tools.spec_check.schema import SchemaError, extract_governance_schema, strict_json_loads, validate_schema
from tools.spec_check.manifest import ManifestError, execute_verified_entrypoint, verify_manifest
from tools.spec_check.ci_order import OrderingError, parse_instant, select_latest_actions_attempts, select_latest_actions_generation, select_latest_native_records
from tools.spec_check.semantics import _canonical, validate_delivery_links, validate_semantics
from tools.spec_check.spec import validate_spec_text
from tools.spec_check.archive import ArchiveError, _member_path, _read_bounded, _rename_noreplace, persist_publication_projection, publish_archive, verify_archive
from tools.ci.publish_git_archive import main as publish_git_archive_main
from tools.spec_check.github_observer import GitHubApiClient, GitHubObserverError, _current_tracker_identity, _matrix_matches, _parse_links, _run_bounded_process, _verify_publication_projection


SPEC_PATH = ROOT / "Maps-Context-Engine-4D_SPEC.md"
SPEC_TEXT = SPEC_PATH.read_text(encoding="utf-8")


class SpecContractTests(unittest.TestCase):
    def test_current_spec_passes(self) -> None:
        self.assertEqual([], validate_spec_text(SPEC_TEXT))

    def test_duplicate_task_fails(self) -> None:
        row = next(line for line in SPEC_TEXT.splitlines() if line.startswith("| T108 |"))
        errors = validate_spec_text(SPEC_TEXT.replace(row + "\n", row + "\n" + row + "\n", 1))
        self.assertTrue(any("Task bị lặp: T108" in error for error in errors))

    def test_requirement_reverse_edge_fails(self) -> None:
        lines = SPEC_TEXT.splitlines()
        index = next(i for i, line in enumerate(lines) if line.startswith("| R001 |"))
        lines[index] = lines[index].replace("T001", "", 1)
        errors = validate_spec_text("\n".join(lines) + "\n")
        self.assertTrue(any("Requirement↔task parity sai" in error for error in errors))

    def test_task_cycle_fails(self) -> None:
        changed = SPEC_TEXT.replace("| T001 | G0 | NONE |", "| T001 | G0 | T002 |", 1)
        errors = validate_spec_text(changed)
        self.assertTrue(any("Dependency cycle" in error for error in errors))

    def test_schedule_prerequisite_order_fails(self) -> None:
        changed = SPEC_TEXT.replace("T001 → T002 → T003", "T002 → T001 → T003", 1)
        errors = validate_spec_text(changed)
        self.assertTrue(any("Schedule đặt T002 trước prerequisite T001" in error for error in errors))

    def test_card_case_drift_fails(self) -> None:
        lines = SPEC_TEXT.splitlines()
        index = next(i for i, line in enumerate(lines) if line.startswith("| 003 | T003 |"))
        lines[index] = lines[index].replace("C043", "C042", 1)
        errors = validate_spec_text("\n".join(lines) + "\n")
        self.assertTrue(any("T003 S19↔S47 case/card parity sai" in error for error in errors))

    def test_missing_case_reference_fails(self) -> None:
        lines = SPEC_TEXT.splitlines()
        index = next(i for i, line in enumerate(lines) if line.startswith("| T003 | G0 |"))
        lines[index] = lines[index].replace("C043", "C999", 1)
        errors = validate_spec_text("\n".join(lines) + "\n")
        self.assertTrue(any("T003 tham chiếu case chưa tồn tại" in error for error in errors))

    def test_unknown_status_fails(self) -> None:
        changed = SPEC_TEXT.replace("| T003 | G0 | T001 |", "| T003 | G0 | T001 |", 1)
        lines = changed.splitlines()
        index = next(i for i, line in enumerate(lines) if line.startswith("| T003 | G0 |"))
        cells = lines[index].split("|")
        cells[7] = " MAYBE "
        lines[index] = "|".join(cells)
        errors = validate_spec_text("\n".join(lines) + "\n")
        self.assertTrue(any("status không hợp lệ" in error for error in errors))

    def test_candidate_done_without_delivery_evidence_fails(self) -> None:
        lines = SPEC_TEXT.splitlines()
        index = next(i for i, line in enumerate(lines) if line.startswith("| T003 | G0 |"))
        cells = lines[index].split("|")
        cells[7] = " DONE "
        cells[9] = " NONE "
        lines[index] = "|".join(cells)
        errors = validate_spec_text("\n".join(lines) + "\n")
        self.assertTrue(any("candidate DONE thiếu evidence" in error for error in errors))

    def test_unbalanced_fence_fails(self) -> None:
        errors = validate_spec_text(SPEC_TEXT + "\n```json\n{}\n")
        self.assertTrue(any("fenced code" in error for error in errors))

    def test_final_claim_requires_all_effective_tasks(self) -> None:
        changed = SPEC_TEXT.replace("final_user_acceptance: once_after_engineering_complete", "final_engineering_status: ENGINEERING_COMPLETE_AWAITING_USER_ACCEPTANCE\n  final_user_acceptance: once_after_engineering_complete", 1)
        errors = validate_spec_text(changed)
        self.assertTrue(any("S22 claim hoàn tất" in error for error in errors))


class GovernanceSchemaTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.schema = extract_governance_schema(SPEC_TEXT)

    def test_schema_has_24_closed_definitions(self) -> None:
        self.assertEqual(24, len(self.schema["$defs"]))
        self.assertTrue(all(item.get("additionalProperties") is False for item in self.schema["$defs"].values()))

    def test_valid_repository_identity_passes(self) -> None:
        value = {
            "host": "github.com", "repository_id": 1421031011, "owner": "LonelyTraderBay",
            "name": "maps-context-engine-4d", "visibility": "public",
            "publication_authority_ref": "user-request", "remote_name": "origin",
            "remote_url_sanitized": "https://github.com/LonelyTraderBay/maps-context-engine-4d.git",
            "ref": "refs/heads/main",
        }
        self.assertEqual([], validate_schema(value, self.schema["$defs"]["GitRepositoryIdentity"]))

    def test_boolean_is_not_integer(self) -> None:
        value = {
            "host": "github.com", "repository_id": True, "owner": "owner", "name": "repo",
            "visibility": "public", "publication_authority_ref": None, "remote_name": "origin",
            "remote_url_sanitized": "https://github.com/owner/repo.git", "ref": "refs/heads/main",
        }
        errors = validate_schema(value, self.schema["$defs"]["GitRepositoryIdentity"])
        self.assertTrue(any("repository_id" in error and "type" in error for error in errors))

    def test_closed_definition_rejects_unknown_field(self) -> None:
        value = {"path": "evidence/log.txt", "byte_length": 0, "sha256": "0" * 64, "secret": "x"}
        errors = validate_schema(value, self.schema["$defs"]["GitEvidenceFile"])
        self.assertTrue(any("thuộc tính không được khai báo" in error for error in errors))

    def test_duplicate_json_key_rejects(self) -> None:
        with self.assertRaises(SchemaError):
            strict_json_loads('{"task_id":"T003","task_id":"T004"}')

    def test_non_finite_json_rejects(self) -> None:
        with self.assertRaises(SchemaError):
            strict_json_loads('{"number":NaN}')

    def test_external_schema_reference_rejects(self) -> None:
        damaged = SPEC_TEXT.replace('"$ref": "#/$defs/GitRepositoryIdentity"', '"$ref": "https://example.invalid/schema"', 1)
        with self.assertRaises(SchemaError):
            extract_governance_schema(damaged)

    def test_selection_allocation_is_required_when_active(self) -> None:
        selection = {
            "format_version": 1, "assignment_ref": "assignment:test", "task_id": "T003",
            "task_revision": 1, "active_delivery_id": "eca9684d-8e74-4b06-8168-47f2a3c82e9e",
            "supersedes_delivery_id": None, "invalidation_refs": [], "reason_ref": "assignment:test",
            "updated_at_utc": "2026-10-16T00:00:00Z", "previous_selection_ref": None,
            "allocated_at_utc": None,
        }
        definition = self.schema["$defs"]["GitTaskDeliverySelection"]
        errors = validate_schema(selection, definition)
        self.assertTrue(any("allocated_at_utc" in error for error in errors))

    def test_enum_boolean_is_not_equal_to_integer(self) -> None:
        errors = validate_schema(True, {"type": "integer", "enum": [1]})
        self.assertTrue(any("sai type" in error for error in errors))

    def test_canonical_plan_rejects_fraction_safe_integer_overflow_and_surrogate(self) -> None:
        for value in (1.0, 9007199254740992, chr(0xD800)):
            with self.subTest(value=repr(value)), self.assertRaises(ValueError):
                _canonical(value)


class GovernanceSemanticsTests(unittest.TestCase):
    def test_payload_pending_changes_may_have_no_commits_until_payload_is_proposed(self) -> None:
        attempt = {
            "delivery_id": "e740c5aa-cf6d-4dd4-9cae-d8dd7ed4ae33",
            "phase": "payload_pending",
            "payload_mode": "changes",
            "ordered_commits": [],
            "noop_reason_ref": None,
        }
        self.assertEqual([], validate_semantics("GitDeliveryAttempt", attempt))
        attempt["phase"] = "closing_proposed"
        self.assertIn(
            "payload_mode=changes cần ordered payload commit",
            validate_semantics("GitDeliveryAttempt", attempt),
        )

    def _push_operation(self) -> dict[str, object]:
        lifecycle = "11111111-1111-4111-8111-111111111111"
        repo = {"host": "github.com", "repository_id": 42, "owner": "owner", "name": "repo"}
        candidate = "a" * 40
        common = {
            "lifecycle_id": lifecycle, "retry_index": 0, "candidate_sha": candidate,
            "repository": repo, "remote_config_digest": "b" * 64,
        }
        return {
            "retry_index": 0, "lifecycle_id": lifecycle, "candidate_sha": candidate,
            "lifecycle_started_at_utc": "2026-10-16T00:00:00Z",
            "deadline_at_utc": "2026-10-16T00:00:05Z",
            "push_started_at_utc": "2026-10-16T00:00:01.2Z",
            "push_completed_at_utc": "2026-10-16T00:00:02.4Z",
            "intervening_events": [],
            "preflight": dict(common, query_id="22222222-2222-4222-8222-222222222222",
                               started_at_utc="2026-10-16T00:00:00.5Z",
                               completed_at_utc="2026-10-16T00:00:01.2Z", observed_remote_sha=None),
            "postflight": dict(common, query_id="33333333-3333-4333-8333-333333333333",
                                started_at_utc="2026-10-16T07:00:02.4+07:00",
                                completed_at_utc="2026-10-16T00:00:02.5Z", observed_remote_sha=candidate),
        }

    def test_push_freshness_valid_equal_fractional_instants(self) -> None:
        self.assertEqual([], validate_semantics("GitPushOperationEvidence", self._push_operation()))

    def test_push_rejects_bool_retry_and_query_identity_coercion(self) -> None:
        record = self._push_operation()
        record["retry_index"] = True
        record["preflight"]["retry_index"] = True
        errors = validate_semantics("GitPushOperationEvidence", record)
        self.assertTrue(any("retry_index" in error for error in errors))
        self.assertTrue(any("preflight" in error for error in errors))

    def test_push_rejects_intervening_event_and_late_preflight(self) -> None:
        record = self._push_operation()
        record["intervening_events"] = ["wait"]
        record["preflight"]["completed_at_utc"] = "2026-10-16T00:00:01.3Z"
        errors = validate_semantics("GitPushOperationEvidence", record)
        self.assertTrue(any("mất hiệu lực" in error for error in errors))
        self.assertTrue(any("trước push" in error for error in errors))

    def test_delivery_pass_requires_exact_remote_and_executed_checks(self) -> None:
        subject = "a" * 40
        record = {
            "phase": "payload", "subject_sha": subject, "observed_remote_sha": subject,
            "binding_verified": True, "required_set_verified": True, "result": "PASS",
            "destination_observation": {"candidate_sha": subject}, "merge_group_binding": None,
            "collection_evidence_ref": "ci/collection.json",
            "checks": [{"status": "completed", "conclusion": "success", "tested_checkout_sha": subject,
                        "execution_verified": True, "source_evidence_ref": "ci/source.json",
                        "execution_evidence_ref": "ci/execution.json"}],
        }
        self.assertEqual([], validate_semantics("GitDeliveryObservation", record))
        record["observed_remote_sha"] = "b" * 40
        self.assertTrue(any("remote SHA" in error for error in validate_semantics("GitDeliveryObservation", record)))

    def test_live_verification_must_follow_published_archive_readback(self) -> None:
        record = {
            "subject_sha": "a" * 40, "observed_remote_sha": "a" * 40,
            "started_at_utc": "2026-10-16T00:00:01Z", "completed_at_utc": "2026-10-16T00:00:02Z",
            "archive_publication": {"published_at_utc": "2026-10-16T00:00:00Z",
                                    "readback_completed_at_utc": "2026-10-16T00:00:00.5Z"},
            "files": [],
        }
        self.assertEqual([], validate_semantics("GitLiveVerification", record))
        record["started_at_utc"] = "2026-10-16T00:00:00.4Z"
        self.assertTrue(any("bắt đầu trước archive" in error for error in validate_semantics("GitLiveVerification", record)))


class ManifestAndSourceBindingTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="mce4d-manifest-fixture-")
        self.root = Path(self.temp.name)
        self.spec_bytes = b"SPEC v1.6.13 fixture\n"
        self.verify_bytes = (
            "from helper import VALUE\n"
            f"from pathlib import Path\nPath({str(self.root / 'root-ran.txt')!r}).write_text(VALUE, encoding='utf-8')\n"
        ).encode("utf-8")
        self.helper_bytes = b"VALUE = 'verified-root'\n"
        (self.root / "final-spec.bin").write_bytes(self.spec_bytes)
        (self.root / "verify_spec.py").write_bytes(self.verify_bytes)
        (self.root / "helper.py").write_bytes(self.helper_bytes)
        child = self.root / "proof"
        child.mkdir()
        self.child_sentinel = self.root / "unlisted-child-ran.txt"
        (child / "verify_spec.py").write_text(
            f"from pathlib import Path\nPath({str(self.child_sentinel)!r}).write_text('untrusted-child', encoding='utf-8')\n",
            encoding="utf-8",
            newline="",
        )
        self.manifest = self._manifest()

    def tearDown(self) -> None:
        self.temp.cleanup()

    def _manifest(self, *, include_helper: bool = True) -> dict[str, object]:
        paths = ["final-spec.bin", "verify_spec.py"] + (["helper.py"] if include_helper else [])
        files = []
        for relative in paths:
            raw = (self.root / relative).read_bytes()
            files.append({"path": relative, "byte_length": len(raw), "sha256": hashlib.sha256(raw).hexdigest()})
        return {"format_version": 1, "spec_sha256": hashlib.sha256(self.spec_bytes).hexdigest(), "members": files}

    def test_intact_manifest_passes(self) -> None:
        result = verify_manifest(self.root, self.manifest, spec_member="final-spec.bin")
        self.assertEqual(hashlib.sha256(self.spec_bytes).hexdigest(), result.spec_sha256)
        self.assertEqual(3, len(result.members))

    def test_missing_member_rejects(self) -> None:
        (self.root / "helper.py").unlink()
        with self.assertRaises(ManifestError):
            verify_manifest(self.root, self.manifest, spec_member="final-spec.bin")

    def test_size_mismatch_rejects(self) -> None:
        damaged = copy.deepcopy(self.manifest)
        damaged["members"][0]["byte_length"] += 1
        with self.assertRaisesRegex(ManifestError, "byte_length"):
            verify_manifest(self.root, damaged, spec_member="final-spec.bin")

    def test_hash_mismatch_rejects(self) -> None:
        damaged = copy.deepcopy(self.manifest)
        damaged["members"][0]["sha256"] = "0" * 64
        with self.assertRaisesRegex(ManifestError, "SHA-256"):
            verify_manifest(self.root, damaged, spec_member="final-spec.bin")

    def test_path_traversal_rejects(self) -> None:
        damaged = copy.deepcopy(self.manifest)
        damaged["members"][0]["path"] = "../final-spec.bin"
        with self.assertRaisesRegex(ManifestError, "path không an toàn"):
            verify_manifest(self.root, damaged, spec_member="final-spec.bin")

    def test_optimized_entrypoint_executes_manifest_bound_root_only(self) -> None:
        execute_verified_entrypoint(
            self.root,
            self.manifest,
            entrypoint="verify_spec.py",
            spec_member="final-spec.bin",
            required_sources=("verify_spec.py", "helper.py"),
        )
        self.assertEqual("verified-root", (self.root / "root-ran.txt").read_text(encoding="utf-8"))
        self.assertFalse(self.child_sentinel.exists())

    def test_entrypoint_runs_the_hashed_snapshot_after_source_path_changes(self) -> None:
        import runpy

        original_run_path = runpy.run_path

        def change_source_then_run(path: str, *, run_name: str) -> dict[str, object]:
            changed = (
                "from pathlib import Path\n"
                f"Path({str(self.root / 'root-ran.txt')!r}).write_text('changed-after-hash', encoding='utf-8')\n"
            ).encode("utf-8")
            (self.root / "verify_spec.py").write_bytes(changed)
            return original_run_path(path, run_name=run_name)

        with patch("tools.spec_check.manifest.runpy.run_path", side_effect=change_source_then_run):
            execute_verified_entrypoint(
                self.root,
                self.manifest,
                entrypoint="verify_spec.py",
                spec_member="final-spec.bin",
                required_sources=("verify_spec.py", "helper.py"),
            )
        self.assertEqual("verified-root", (self.root / "root-ran.txt").read_text(encoding="utf-8"))

    def test_manifest_execution_does_not_rewrite_source_or_manifest(self) -> None:
        source_before = {name: (self.root / name).read_bytes() for name in ("verify_spec.py", "helper.py")}
        manifest_before = copy.deepcopy(self.manifest)
        execute_verified_entrypoint(
            self.root,
            self.manifest,
            entrypoint="verify_spec.py",
            spec_member="final-spec.bin",
            required_sources=("verify_spec.py", "helper.py"),
        )
        self.assertEqual(manifest_before, self.manifest)
        self.assertEqual(source_before, {name: (self.root / name).read_bytes() for name in source_before})

    def test_unlisted_helper_rejects_before_entrypoint(self) -> None:
        manifest = self._manifest(include_helper=False)
        with self.assertRaisesRegex(ManifestError, "source chưa được manifest xác minh"):
            execute_verified_entrypoint(
                self.root,
                manifest,
                entrypoint="verify_spec.py",
                spec_member="final-spec.bin",
                required_sources=("verify_spec.py",),
            )
        self.assertFalse((self.root / "root-ran.txt").exists())

    def test_root_source_hash_mismatch_rejects_before_entrypoint(self) -> None:
        (self.root / "verify_spec.py").write_bytes(b"#" + self.verify_bytes[1:])
        with self.assertRaisesRegex(ManifestError, "SHA-256 member verify_spec.py"):
            execute_verified_entrypoint(
                self.root,
                self.manifest,
                entrypoint="verify_spec.py",
                spec_member="final-spec.bin",
                required_sources=("verify_spec.py", "helper.py"),
            )
        self.assertFalse((self.root / "root-ran.txt").exists())

    def test_current_portable_proof_manifest_members_pass(self) -> None:
        archive_path = ROOT / "artifacts/spec-proof/MCE4D_SPEC_1.6.13_DOCUMENT_PROOF.zip"
        archive_sha_before = hashlib.sha256(archive_path.read_bytes()).hexdigest()
        with ZipFile(archive_path) as archive, tempfile.TemporaryDirectory(prefix="mce4d-proof-extract-") as temp:
            root = Path(temp)
            manifest = json.loads(archive.read("manifest.json"))
            extracted_bytes = 0
            for info in archive.infolist():
                if info.is_dir():
                    continue
                relative = Path(info.filename)
                if relative.is_absolute() or ".." in relative.parts or "\\" in info.filename or (info.external_attr >> 16) & 0o170000 == 0o120000:
                    raise ManifestError(f"ZIP member path/type không an toàn: {info.filename}")
                extracted_bytes += info.file_size
                if extracted_bytes > 10 * 1024 * 1024:
                    raise ManifestError("ZIP fixture vượt giới hạn giải nén 10 MiB")
                destination = root / info.filename
                if not destination.resolve().is_relative_to(root.resolve()):
                    raise ManifestError(f"ZIP member thoát khỏi fixture root: {info.filename}")
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.write_bytes(archive.read(info.filename))
            result = verify_manifest(
                root,
                manifest,
                spec_member="final-spec.bin",
                expected_spec_sha256=manifest["spec_sha256"],
            )
            self.assertEqual(len(manifest["files"]), len(result.members))
        self.assertEqual(archive_sha_before, hashlib.sha256(archive_path.read_bytes()).hexdigest())


class CIOrderingTests(unittest.TestCase):
    def test_offset_and_long_fraction_compare_as_exact_instants(self) -> None:
        utc = parse_instant("2026-10-16T00:00:00.123456789Z")
        offset = parse_instant("2026-10-16T01:00:00.123456789+01:00")
        self.assertEqual(utc, offset)
        self.assertLess(parse_instant("2026-10-16T00:00:00.123456788Z"), utc)

    def test_bad_timestamp_rejects_instead_of_dropping_row(self) -> None:
        with self.assertRaises(OrderingError):
            parse_instant("2026-10-16T00:00:00")
        result = select_latest_native_records([
            {"id": "11", "created_at_utc": "invalid", "source_app": "app", "context": "ci", "workflow_path": "wf", "job_name": "job", "event": "push", "subject_sha": "a" * 40},
        ], source_kind="check_run")
        self.assertEqual("UNKNOWN", result["verdict"])

    def test_latest_actions_generation_uses_instant_then_native_id(self) -> None:
        runs = [
            {"workflow_identity": "wf", "event": "push", "subject_sha": "a" * 40, "run_id": "19", "created_at_utc": "2026-10-16T00:00:00Z", "collection_complete": True},
            {"workflow_identity": "wf", "event": "push", "subject_sha": "a" * 40, "run_id": "20", "created_at_utc": "2026-10-15T20:00:00-04:00", "collection_complete": True},
        ]
        selected = select_latest_actions_generation(runs, workflow_identity="wf", event="push", subject_sha="a" * 40)
        self.assertEqual("SELECTED", selected["verdict"])
        self.assertEqual("20", selected["run"]["run_id"])

    def test_newest_generation_with_no_required_job_is_unknown(self) -> None:
        runs = [
            {"workflow_identity": "wf", "event": "pull_request", "subject_sha": "b" * 40, "run_id": "31", "created_at_utc": "2026-10-15T00:00:00Z", "collection_complete": True},
            {"workflow_identity": "wf", "event": "pull_request", "subject_sha": "b" * 40, "run_id": "32", "created_at_utc": "2026-10-16T00:00:00Z", "collection_complete": True},
        ]
        generation = select_latest_actions_generation(runs, workflow_identity="wf", event="pull_request", subject_sha="b" * 40)
        checks = select_latest_actions_attempts([], selected_run_id=generation["run"]["run_id"], required_job_identities={"linux"})
        self.assertEqual("UNKNOWN", checks["verdict"])
        self.assertIn("thiếu required job", checks["reason"])

    def test_partial_rerun_selects_latest_attempt_per_job(self) -> None:
        jobs = [
            {"run_id": 42, "attempt": 1, "check_identity": "ubuntu", "conclusion": "failure"},
            {"run_id": 42, "attempt": 2, "check_identity": "ubuntu", "conclusion": "success"},
            {"run_id": 42, "attempt": 1, "check_identity": "windows", "conclusion": "success"},
        ]
        selected = select_latest_actions_attempts(jobs, selected_run_id="42", required_job_identities={"ubuntu", "windows"})
        self.assertEqual("SELECTED", selected["verdict"])
        self.assertEqual(2, selected["attempts"]["ubuntu"]["attempt"])
        self.assertEqual(1, selected["attempts"]["windows"]["attempt"])

    def test_boolean_retry_attempt_is_unknown(self) -> None:
        result = select_latest_actions_attempts(
            [{"run_id": 42, "attempt": True, "check_identity": "ubuntu"}],
            selected_run_id=42,
            required_job_identities={"ubuntu"},
        )
        self.assertEqual("UNKNOWN", result["verdict"])

    def test_newest_native_failure_is_not_replaced_by_old_success(self) -> None:
        records = [
            {"id": "9", "created_at_utc": "2026-10-15T00:00:00Z", "source_app": "app", "context": "ci", "workflow_path": "wf", "job_name": "job", "event": "push", "subject_sha": "c" * 40, "conclusion": "success"},
            {"id": "10", "created_at_utc": "2026-10-16T00:00:00Z", "source_app": "app", "context": "ci", "workflow_path": "wf", "job_name": "job", "event": "push", "subject_sha": "c" * 40, "conclusion": "failure"},
        ]
        selected = select_latest_native_records(records, source_kind="check_run")
        identity = next(iter(selected["records"]))
        self.assertEqual("failure", selected["records"][identity]["conclusion"])

    def test_malformed_extra_precision_is_bounded(self) -> None:
        with self.assertRaisesRegex(OrderingError, "64 chữ số"):
            parse_instant("2026-10-16T00:00:00." + "1" * 65 + "Z")


def _delivery_link_bundle() -> tuple[dict[str, dict[str, object]], dict[str, object]]:
    delivery_id = "eca9684d-8e74-4b06-8168-47f2a3c82e9e"
    payload_sha, closing_sha = "1" * 40, "2" * 40
    plan = strict_json_loads((ROOT / "tools/ci/check-plan.json").read_bytes())
    plan["canary_ref"] = f"delivery:{delivery_id}:payload"
    plan_sha = hashlib.sha256(_canonical(plan)).hexdigest()
    repository = plan["repository"]
    objects: dict[str, dict[str, object]] = {"plan": {"kind": "GitCheckPlan", "value": plan}}
    raw_members: dict[str, object] = {}

    def add(ref: str, kind: str, value: dict[str, object]) -> str:
        objects[ref] = {"kind": kind, "value": value}
        return ref

    def operation(phase: str, subject: str) -> dict[str, object]:
        suffix = "1" if phase == "payload" else "2"
        lifecycle_id = "22222222-2222-4222-8222-22222222222" + suffix
        operation_id = "11111111-1111-4111-8111-11111111111" + suffix
        command_ref = f"commands/{phase}.json"
        deadline = "2026-10-16T00:01:00Z"
        add(command_ref, "GitCommandLifecycle", {
            "format_version": 1, "kind": "command_lifecycle",
            "operation_id": operation_id, "lifecycle_id": lifecycle_id, "retry_index": 0,
            "assignment_ref": "assignment:T003", "task_id": "T003", "task_revision": 1,
            "delivery_id": delivery_id, "command_state": "SUCCEEDED",
            "contract": {
                "command_id": f"push-T003-{phase}", "task_mode": "implement", "owner": "Codex",
                "baseline_ref": "git:base", "executable": "git",
                "argv": ["push", "--porcelain", "origin", f"{subject}:refs/heads/main"],
                "cwd": ".", "tool_versions": {"git": "2.51.0"},
                "command_source_ref": "toolchain/git.json", "input_refs": [],
                "contracts": ["PUSH-FRESH-01"], "fingerprints": {}, "expected_writes": [],
                "temporary_roots": [], "cache_roots": [], "log_roots": [],
                "download_roots": [], "network_scope": ["github.com"], "child_processes": [],
                "side_effect_class": "external_service", "permission_ref": "authority/task-start.txt",
                "storage_mode": "bounded_small", "volumes": [], "budget_refs": [],
                "retention_ref": "retention/T003", "deadline_at_utc": deadline,
                "monitor_interval_ms": 1000, "run_id": f"77777777-7777-4777-8777-77777777777{suffix}",
                "output_root": "artifacts/T003", "cleanup_plan_ref": "cleanup/T003",
                "cleanup_permission_ref": None,
            },
            "result": {
                "started_at_utc": "2026-10-16T00:00:01Z",
                "completed_at_utc": "2026-10-16T00:00:05.500Z",
                "command_exit_status": 0, "termination_reason": "completed",
                "cleanup_status": "NOT_REQUIRED", "final_status": "SUCCEEDED",
                "outputs_created": [], "artifact_refs": [],
            },
        })
        raw_preflight = {
            "query_id": f"33333333-3333-4333-8333-33333333333{suffix}",
            "lifecycle_id": lifecycle_id, "retry_index": 0, "candidate_sha": subject,
            "repository": repository, "remote_config_digest": "5" * 64,
            "started_at_utc": "2026-10-16T00:00:00.100Z",
            "completed_at_utc": "2026-10-16T00:00:00.500Z", "observed_remote_sha": None,
        }
        raw_postflight = {
            "query_id": f"44444444-4444-4444-8444-44444444444{suffix}",
            "lifecycle_id": lifecycle_id, "retry_index": 0, "candidate_sha": subject,
            "repository": repository, "remote_config_digest": "5" * 64,
            "started_at_utc": "2026-10-16T00:00:02.100Z",
            "completed_at_utc": "2026-10-16T00:00:02.500Z", "observed_remote_sha": subject,
        }
        raw_members[f"push/{phase}/preflight.json"] = raw_preflight
        raw_members[f"push/{phase}/postflight.json"] = raw_postflight
        return {
            "format_version": 1, "kind": "push_operation",
            "push_operation_id": operation_id,
            "lifecycle_id": lifecycle_id, "retry_index": 0, "candidate_sha": subject,
            "credential_class": "local_user", "command_record_ref": command_ref,
            "lifecycle_started_at_utc": "2026-10-16T00:00:00Z",
            "deadline_at_utc": deadline,
            "push_started_at_utc": "2026-10-16T00:00:01Z",
            "push_completed_at_utc": "2026-10-16T00:00:02Z", "intervening_events": [],
            "preflight": {
                "query_id": raw_preflight["query_id"],
                "lifecycle_id": lifecycle_id, "retry_index": 0, "candidate_sha": subject,
                "repository": repository, "remote_config_digest": "5" * 64,
                "started_at_utc": raw_preflight["started_at_utc"],
                "completed_at_utc": raw_preflight["completed_at_utc"],
                "observed_remote_sha": None, "raw_api_ref": f"push/{phase}/preflight.json",
            },
            "postflight": {
                "query_id": raw_postflight["query_id"],
                "lifecycle_id": lifecycle_id, "retry_index": 0, "candidate_sha": subject,
                "repository": repository, "remote_config_digest": "5" * 64,
                "started_at_utc": raw_postflight["started_at_utc"],
                "completed_at_utc": raw_postflight["completed_at_utc"],
                "observed_remote_sha": subject, "raw_api_ref": f"push/{phase}/postflight.json",
            },
        }

    def make_observation(phase: str, subject: str, parent: str) -> dict[str, object]:
        observed_checks: list[dict[str, object]] = []
        raw_check_runs: list[dict[str, object]] = []
        raw_jobs: list[dict[str, object]] = []
        for index, expected in enumerate(plan["checks"], start=1):
            source_ref = f"ci/{phase}/source-{index}.json"
            execution_ref = f"ci/{phase}/execution-{index}.json"
            raw_api_ref = f"ci/{phase}/pages/check-runs-1.json"
            job_id = str(200 + index)
            job_url = f"https://github.com/{repository['owner']}/{repository['name']}/actions/runs/101/job/{job_id}"
            check_run_url = f"https://api.github.com/repos/{repository['owner']}/{repository['name']}/check-runs/{job_id}"
            source = {
                "format_version": 1, "kind": "check_source", "source_kind": "actions_check_run",
                "host": repository["host"], "repository_id": repository["repository_id"],
                "record_id": job_id, "run_id": "101", "job_id": job_id, "attempt": 1,
                "created_at_utc": "2026-10-16T00:00:04Z", "source_app": expected["source_app"],
                "context": expected["context"], "workflow_path": expected["workflow_path"],
                "job_name": expected["job_name"], "matrix": expected["matrix"], "event": "push",
                "reported_head_sha": subject, "native_status": "completed",
                "native_conclusion": "success", "raw_api_ref": raw_api_ref,
                "check_suite_id": "301", "workflow_head_sha": subject, "job_head_sha": subject,
                "job_check_run_url": job_url,
            }
            add(source_ref, "GitCheckSourceEvidence", source)
            raw_check_runs.append({
                "id": int(job_id), "name": expected["context"], "head_sha": subject,
                "status": "completed", "conclusion": "success", "app": {"id": 15368},
                "url": check_run_url,
                "check_suite": {"id": 301},
                "details_url": job_url,
            })
            raw_jobs.append({
                "id": 200 + index, "name": expected["context"], "head_sha": subject,
                "conclusion": "success", "created_at": "2026-10-16T00:00:04Z",
                "run_id": 101, "run_attempt": 1,
                "check_run_url": check_run_url,
                "html_url": job_url,
            })
            command_results = [
                {"command_ref": command, "revision": subject, "result": "PASS",
                 "log_ref": f"ci/{phase}/logs/{index}-{command_index}.txt"}
                for command_index, command in enumerate(expected["command_refs"], start=1)
            ]
            add(execution_ref, "GitCheckExecutionEvidence", {
                "format_version": 1, "kind": "check_execution", "check_id": expected["id"],
                "subject_sha": subject, "tested_checkout_sha": subject,
                "source_evidence_ref": source_ref, "command_results": command_results,
            })
            observed_checks.append({
                "check_id": expected["id"], "kind": expected["kind"], "context": expected["context"],
                "source_app": expected["source_app"], "workflow_path": expected["workflow_path"],
                "job_name": expected["job_name"], "matrix": expected["matrix"], "event": "push",
                "run_id": "101", "job_id": job_id, "attempt": 1,
                "reported_head_sha": subject, "tested_checkout_sha": subject,
                "pr_head_sha": None, "pr_base_sha": None, "tested_merge_sha": None,
                "status": "completed", "conclusion": "success", "execution_verified": True,
                "url": f"https://github.com/owner/repo/actions/runs/101/job/{job_id}",
                "evidence_refs": [
                    source_ref, execution_ref, raw_api_ref,
                    *(row["log_ref"] for row in command_results),
                ],
                "run_created_at_utc": "2026-10-16T00:00:04Z",
                "source_evidence_ref": source_ref, "execution_evidence_ref": execution_ref,
            })
        pages = [
            {"query_id": f"55555555-5555-4555-8555-55555555555{index}",
             "endpoint": endpoint, "requested_sha": subject, "page_number": 1,
             "next_page_number": None,
             "raw_page_ref": f"ci/{phase}/pages/{page_name}.json",
             "workflow_run_id": "101" if endpoint == "workflow_run_attempt" else None,
             "workflow_run_attempt": 1 if endpoint == "workflow_run_attempt" else None}
            for index, (endpoint, page_name) in enumerate(
                (("check_runs", "check-runs-1"), ("workflow_runs", "workflow-runs-1"),
                 ("workflow_run_attempt", "workflow-attempt-1")), start=1)
        ]
        raw_members[f"ci/{phase}/pages/check-runs-1.json"] = {
            "total_count": len(raw_check_runs), "check_runs": raw_check_runs,
        }
        raw_members[f"ci/{phase}/pages/workflow-runs-1.json"] = {
            "total_count": 1,
            "workflow_runs": [{
                "id": 101, "head_sha": subject, "event": "push", "status": "completed",
                "conclusion": "success", "run_attempt": 1, "path": ".github/workflows/bootstrap.yml",
                "created_at": "2026-10-16T00:00:04Z", "check_suite_id": 301,
            }],
        }
        raw_members[f"ci/{phase}/pages/workflow-attempt-1.json"] = {
            "total_count": len(raw_jobs), "jobs": raw_jobs,
        }
        collection_ref = f"ci/{phase}/collection.json"
        add(collection_ref, "GitCICollectionEvidence", {
            "format_version": 1, "kind": "ci_collection",
            "collection_id": "66666666-6666-4666-8666-66666666666" + ("1" if phase == "payload" else "2"),
            "host": repository["host"], "repository_id": repository["repository_id"],
            "subject_sha": subject, "check_plan_sha256": plan_sha,
            "started_at_utc": "2026-10-16T00:00:03Z",
            "completed_at_utc": "2026-10-16T00:00:05Z", "pages": pages,
        })
        operation_ref = f"push/{phase}/operation.json"
        push = operation(phase, subject)
        add(operation_ref, "GitPushOperationEvidence", push)
        return {
            "format_version": 1, "delivery_id": delivery_id, "phase": phase,
            "subject_sha": subject, "parent_sha": parent, "check_plan_sha256": plan_sha,
            "observed_remote_sha": subject, "observed_at_utc": "2026-10-16T00:00:06Z",
            "binding_verified": True, "required_set_verified": True, "checks": observed_checks,
            "result": "PASS", "blocker": None, "merge_group_binding": None,
            "destination_observation": {
                "format_version": 1, "push_operation_id": push["push_operation_id"],
                "candidate_sha": subject, "repository": repository,
                "local_remote_config_digest": "5" * 64, "observed_at_utc": "2026-10-16T00:00:00.500Z",
                "push_started_at_utc": "2026-10-16T00:00:01Z", "identity_verified": True,
                "visibility_verified": True, "operation_evidence_ref": operation_ref,
            },
            "pull_request_binding_ref": None, "collection_evidence_ref": collection_ref,
        }

    payload_observation = make_observation("payload", payload_sha, "0" * 40)
    closing_observation = make_observation("closing", closing_sha, payload_sha)
    add("payload", "GitDeliveryObservation", payload_observation)
    add("closing", "GitDeliveryObservation", closing_observation)
    payload_subject = "feat(spec-check): [P0][T003] Tạo payload kiểm tra"
    payload_body = (
        "Mục đích: Kiểm chứng hợp đồng delivery của T003.\n"
        "Thay đổi: Ghi nhận scope fixture đã khóa.\n"
        "Kiểm tra: Validator fixture synthetic."
    )
    closing_subject = "docs(progress): [P0][T003] Đề xuất đóng kiểm tra"
    closing_body = (
        "Mục đích: Đề xuất đóng T003 sau khi kiểm tra.\n"
        "Thay đổi: Ghi receipt P/C trong ledger.\n"
        "Kiểm tra: Kiểm tra parent, scope và CI chính xác.\n\n"
        f"MCE4D-Delivery-ID: {delivery_id}\nMCE4D-Delivery-Phase: closing"
    )
    raw_members["rationale/priority.json"] = {
        "format_version": 1, "kind": "priority_rationale", "assignment_ref": "assignment:T003",
        "task_id": "T003", "task_revision": 1, "delivery_id": delivery_id,
        "priority": "P0", "reason": "T003 là prerequisite G0 cho evaluator và commit delivery.",
    }
    raw_members["rationale/shared.json"] = {
        "format_version": 1, "kind": "shared_invariant", "assignment_ref": "assignment:T003",
        "task_ids": ["T003"], "invariant": "Một evidence identity phải nối đúng revision, SHA và nguồn retained.",
    }
    raw_members["rationale/breaking.json"] = {
        "format_version": 1, "kind": "breaking_decision", "assignment_ref": "assignment:T003",
        "task_id": "T003", "task_revision": 1, "delivery_id": delivery_id,
        "approved_decision_ref": "decision:T003:compatibility",
        "decision": "Giữ contract hiện hữu, không tạo thay đổi breaking.",
    }
    add("scope/payload.json", "GitCommitScopeEvidence", {
        "format_version": 1, "kind": "commit_scope", "phase": "payload", "task_ids": ["T003"],
        "priority": "P0", "priority_reason_ref": "rationale/priority.json",
        "shared_invariant_ref": "rationale/shared.json", "breaking_decision_ref": "rationale/breaking.json",
        "changed_paths": ["tools/spec_check/semantics.py"],
    })
    add("scope/closing.json", "GitCommitScopeEvidence", {
        "format_version": 1, "kind": "commit_scope", "phase": "closing", "task_ids": ["T003"],
        "priority": "P0", "priority_reason_ref": "rationale/priority.json",
        "shared_invariant_ref": "rationale/shared.json", "breaking_decision_ref": "rationale/breaking.json",
        "changed_paths": ["Maps-Context-Engine-4D_SPEC.md"],
    })
    raw_members["git/payload.json"] = {
        "kind": "git_commit_snapshot", "sha": payload_sha, "parents": ["0" * 40],
        "subject": payload_subject, "body": payload_body,
        "changed_paths": ["tools/spec_check/semantics.py"],
    }
    raw_members["git/closing.json"] = {
        "kind": "git_commit_snapshot", "sha": closing_sha, "parents": [payload_sha],
        "subject": closing_subject, "body": closing_body,
        "changed_paths": ["Maps-Context-Engine-4D_SPEC.md"],
    }
    add("commit", "GitCommit", {
        "sha": closing_sha, "subject": closing_subject,
        "scope_ref": "scope/closing.json",
        "body": closing_body,
        "git_evidence_ref": "git/closing.json",
    })
    add("selection", "GitTaskDeliverySelection", {
        "format_version": 1, "active_delivery_id": delivery_id, "task_id": "T003",
        "task_revision": 1, "assignment_ref": "assignment:T003", "supersedes_delivery_id": None,
        "invalidation_refs": [], "reason_ref": "assignment:T003",
        "updated_at_utc": "2026-10-16T00:00:00Z", "previous_selection_ref": None,
        "allocated_at_utc": "2026-10-15T23:59:00Z",
    })
    local_commands = list(dict.fromkeys(
        command for check in plan["checks"] for command in check["command_refs"]
    ))
    local_refs = plan["local_check_refs"]
    for index, local_ref in enumerate(local_refs):
        add(local_ref, "GitLocalCheckEvidence", {
            "format_version": 1, "kind": "local_check", "command_ref": local_commands[index],
            "task_id": "T003", "task_revision": 1, "delivery_id": delivery_id,
            "phase": "payload", "revision": payload_sha, "result": "PASS",
            "execution_verified": True, "log_refs": [f"local-logs/check-{index + 1}.txt"],
            "reused_from_ref": None,
        })
    add("attempt", "GitDeliveryAttempt", {
        "format_version": 1, "delivery_id": delivery_id, "task_id": "T003", "task_revision": 1,
        "assignment_ref": "assignment:T003", "priority": "P0", "phase": "closing_proposed",
        "base_sha": "0" * 40,
        "ordered_commits": [{
            "sha": payload_sha, "subject": payload_subject,
            "scope_ref": "scope/payload.json", "body": payload_body, "git_evidence_ref": "git/payload.json",
        }],
        "verified_payload_sha": payload_sha, "check_plan_ref": "plan", "check_plan_sha256": plan_sha,
        "local_evidence_refs": local_refs, "payload_observation_ref": "payload",
        "closing_marker": f"MCE4D-Delivery-ID: {delivery_id}", "closing_parent_sha": payload_sha,
        "planned_archive_path": f"artifacts/git-delivery/{delivery_id}",
        "archive_staging_path": f"artifacts/git-delivery/.staging-{delivery_id}",
        "payload_mode": "changes", "noop_reason_ref": None,
    })
    member_paths = {
        "attempt", "selection", "payload", "closing", "commit", "plan", "local/checks.json",
        "local-checks/bootstrap.json", "local-checks/spec-check.json",
        "local-checks/spec-check-fixtures.json", "local-checks/delivery-policy-fixtures.json",
        "local-checks/diff-check.json", "git/archive.json", "scope/closing.json",
        "git/closing.json", "scope/payload.json", "git/payload.json", "rationale/priority.json",
        "rationale/shared.json", "rationale/breaking.json",
    }
    member_paths.update(local_refs)
    for entry in objects.values():
        value = entry["value"]
        if not isinstance(value, dict):
            continue
        kind = entry["kind"]
        if kind == "GitDeliveryObservation":
            member_paths.update((value["collection_evidence_ref"],
                                 value["destination_observation"]["operation_evidence_ref"]))
            for check in value["checks"]:
                member_paths.update((check["source_evidence_ref"], check["execution_evidence_ref"]))
                member_paths.update(check["evidence_refs"])
        elif kind == "GitCICollectionEvidence":
            member_paths.update(page["raw_page_ref"] for page in value["pages"])
        elif kind == "GitCheckSourceEvidence":
            member_paths.add(value["raw_api_ref"])
        elif kind == "GitCheckExecutionEvidence":
            member_paths.update(row["log_ref"] for row in value["command_results"])
        elif kind == "GitLocalCheckEvidence":
            member_paths.update(value["log_refs"])
        elif kind == "GitPushOperationEvidence":
            member_paths.update((value["command_record_ref"], value["preflight"]["raw_api_ref"],
                                 value["postflight"]["raw_api_ref"]))
    add("archive", "GitEvidenceArchive", {
        "format_version": 1, "delivery_id": delivery_id, "task_id": "T003", "task_revision": 1,
        "assignment_ref": "assignment:T003", "attempt_ref": "attempt", "selection_ref": "selection",
        "payload_observation_ref": "payload", "closing_observation_ref": "closing",
        "closing_commit_ref": "commit", "closing_sha": closing_sha, "git_evidence_ref": "git/archive.json",
        "verified_payload_sha": payload_sha, "check_plan_sha256": plan_sha,
        "files": [{"path": path, "byte_length": 1, "sha256": "6" * 64} for path in sorted(member_paths)],
        "retention_authority_ref": "authority/task-start.txt", "created_at_utc": "2026-10-16T00:00:07Z",
    })
    return objects, raw_members


class DeliveryLinkTests(unittest.TestCase):
    def _bundle(self, *, payload_phase: str = "payload", payload_ref: str = "payload") -> dict[str, object]:
        bundle, raw_members = _delivery_link_bundle()
        self.raw_members = raw_members
        if payload_phase != "payload":
            bundle["payload"]["value"]["phase"] = payload_phase
        if payload_ref != "payload":
            bundle["attempt"]["value"]["payload_observation_ref"] = payload_ref
        return bundle

    def _validate(self, bundle: object) -> dict[str, object]:
        return validate_delivery_links(bundle, raw_members=self.raw_members)

    def test_complete_static_link_graph_remains_external_unknown(self) -> None:
        result = self._validate(self._bundle())
        self.assertEqual("UNKNOWN", result["verdict"])
        self.assertEqual([], result["errors"])
        self.assertTrue(result["unknowns"])

    def test_swapped_payload_phase_rejects(self) -> None:
        result = self._validate(self._bundle(payload_phase="closing"))
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("payload_observation_ref trỏ sai phase" in error for error in result["errors"]))

    def test_swapped_pointer_rejects(self) -> None:
        result = self._validate(self._bundle(payload_ref="commit"))
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("trỏ sai kind" in error for error in result["errors"]))

    def test_stale_selector_revision_rejects(self) -> None:
        bundle = self._bundle()
        bundle["selection"]["value"]["task_revision"] = 2
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("selection lệch task_revision" in error for error in result["errors"]))

    def test_missing_required_check_rejects(self) -> None:
        bundle = self._bundle()
        bundle["payload"]["value"]["checks"].pop()
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("số required checks không khớp plan" in error for error in result["errors"]))

    def test_local_evidence_covers_each_unique_frozen_command_once(self) -> None:
        bundle = self._bundle()
        first, second = bundle["attempt"]["value"]["local_evidence_refs"][:2]
        bundle[second]["value"]["command_ref"] = bundle[first]["value"]["command_ref"]
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("local evidence command set phải khớp" in error for error in result["errors"]))

    def test_native_source_is_bound_to_observed_check(self) -> None:
        bundle = self._bundle()
        source_ref = bundle["payload"]["value"]["checks"][0]["source_evidence_ref"]
        bundle[source_ref]["value"]["reported_head_sha"] = "3" * 40
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("native source fields lệch" in error for error in result["errors"]))

    def test_actions_source_timestamp_uses_native_workflow_creation(self) -> None:
        bundle = self._bundle()
        source_ref = bundle["payload"]["value"]["checks"][0]["source_evidence_ref"]
        bundle[source_ref]["value"]["created_at_utc"] = "2026-10-16T00:00:05Z"
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("native workflow_run.created_at" in error for error in result["errors"]))

    def test_actions_job_check_run_url_binds_native_check_identity(self) -> None:
        bundle = self._bundle()
        collection_ref = bundle["payload"]["value"]["collection_evidence_ref"]
        collection = bundle[collection_ref]["value"]
        attempt_page = next(page for page in collection["pages"]
                            if page["endpoint"] == "workflow_run_attempt")
        self.raw_members[attempt_page["raw_page_ref"]]["jobs"][0]["check_run_url"] = (
            "https://api.github.com/repos/owner/repo/check-runs/999"
        )
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("job.check_run_url không bind native check ID" in error for error in result["errors"]))

    def test_actions_check_suite_identity_matches_workflow(self) -> None:
        bundle = self._bundle()
        source_ref = bundle["payload"]["value"]["checks"][0]["source_evidence_ref"]
        raw_ref = bundle[source_ref]["value"]["raw_api_ref"]
        self.raw_members[raw_ref]["check_runs"][0]["check_suite"]["id"] = 999
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("check_suite_id không khớp" in error for error in result["errors"]))

    def test_command_execution_is_bound_to_subject_revision(self) -> None:
        bundle = self._bundle()
        execution_ref = bundle["payload"]["value"]["checks"][0]["execution_evidence_ref"]
        bundle[execution_ref]["value"]["command_results"][0]["revision"] = "3" * 40
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("command chưa PASS trên subject" in error for error in result["errors"]))

    def test_collection_must_cover_actions_attempt(self) -> None:
        bundle = self._bundle()
        collection_ref = bundle["payload"]["value"]["collection_evidence_ref"]
        bundle[collection_ref]["value"]["pages"] = bundle[collection_ref]["value"]["pages"][:2]
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("workflow_run_attempt" in error for error in result["errors"]))

    def test_partial_rerun_selects_latest_attempt_per_job(self) -> None:
        bundle = self._bundle()
        collection_ref = bundle["payload"]["value"]["collection_evidence_ref"]
        collection = bundle[collection_ref]["value"]
        page_ref = "ci/payload/pages/workflow-attempt-2.json"
        old_page = next(page for page in collection["pages"] if page["endpoint"] == "workflow_run_attempt")
        page = dict(old_page, query_id="55555555-5555-4555-8555-555555555559",
                    workflow_run_attempt=2, raw_page_ref=page_ref)
        collection["pages"].append(page)
        raw_jobs = self.raw_members[old_page["raw_page_ref"]]["jobs"]
        rerun_job = dict(raw_jobs[0], id=900, conclusion="success", created_at="2026-10-16T00:00:04.500Z")
        self.raw_members[page_ref] = {"total_count": 1, "jobs": [rerun_job]}
        bundle["archive"]["value"]["files"].append({"path": page_ref, "byte_length": 1, "sha256": "6" * 64})
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("latest job attempt" in error for error in result["errors"]))

    def test_newer_failed_workflow_generation_supersedes_old_green_run(self) -> None:
        bundle = self._bundle()
        raw_page_ref = "ci/payload/pages/workflow-runs-1.json"
        raw_page = self.raw_members[raw_page_ref]
        raw_page["total_count"] = 2
        raw_page["workflow_runs"].append({
            "id": 102, "head_sha": "1" * 40, "event": "push", "status": "completed",
            "conclusion": "failure", "run_attempt": 1, "path": ".github/workflows/bootstrap.yml",
            "created_at": "2026-10-16T00:00:04.500Z",
        })
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("latest workflow" in error for error in result["errors"]))

    def test_newer_failed_native_check_run_supersedes_old_green_check(self) -> None:
        bundle = self._bundle()
        raw_page = self.raw_members["ci/payload/pages/check-runs-1.json"]
        raw_page["total_count"] += 1
        subject = bundle["payload"]["value"]["subject_sha"]
        raw_page["check_runs"].append({
            "id": 999, "name": "bootstrap (ubuntu-latest)", "head_sha": subject,
            "status": "completed", "conclusion": "failure", "app": {"id": 15368},
            "url": f"https://api.github.com/repos/{bundle['plan']['value']['repository']['owner']}/{bundle['plan']['value']['repository']['name']}/check-runs/999",
            "check_suite": {"id": 301},
            "details_url": f"https://github.com/{bundle['plan']['value']['repository']['owner']}/{bundle['plan']['value']['repository']['name']}/actions/runs/101/job/999",
        })
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("latest native check-run" in error for error in result["errors"]))

    def test_push_operation_id_is_bound_to_destination(self) -> None:
        bundle = self._bundle()
        operation_ref = bundle["payload"]["value"]["destination_observation"]["operation_evidence_ref"]
        bundle[operation_ref]["value"]["push_operation_id"] = "77777777-7777-4777-8777-777777777777"
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("push operation id/candidate không khớp" in error for error in result["errors"]))

    def test_common_command_lifecycle_must_match_phase_identity(self) -> None:
        bundle = self._bundle()
        command_ref = bundle["payload"]["value"]["destination_observation"]["operation_evidence_ref"]
        command_ref = bundle[command_ref]["value"]["command_record_ref"]
        bundle[command_ref]["value"]["delivery_id"] = "00000000-0000-4000-8000-000000000000"
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("common command delivery_id" in error for error in result["errors"]))

    def test_push_command_rejects_force_and_wrong_refspec(self) -> None:
        for argv in (
            ["push", "--force", "origin", "1" * 40 + ":refs/heads/main"],
            ["push", "--porcelain", "origin", "1" * 40 + ":refs/heads/other"],
        ):
            with self.subTest(argv=argv):
                bundle = self._bundle()
                operation_ref = bundle["payload"]["value"]["destination_observation"]["operation_evidence_ref"]
                command_ref = bundle[operation_ref]["value"]["command_record_ref"]
                bundle[command_ref]["value"]["contract"]["argv"] = argv
                result = self._validate(bundle)
                self.assertEqual("REJECT", result["verdict"])
                self.assertTrue(any("Git argv không phải push thường" in error for error in result["errors"]))

    def test_raw_preflight_snapshot_must_match_projection(self) -> None:
        bundle = self._bundle()
        self.raw_members["push/payload/preflight.json"]["remote_config_digest"] = "9" * 64
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("preflight raw query snapshot lệch" in error for error in result["errors"]))

    def test_commit_snapshot_parent_and_scope_are_actual_git_bound(self) -> None:
        bundle = self._bundle()
        self.raw_members["git/closing.json"]["parents"] = ["3" * 40]
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("actual Git parents không khớp" in error for error in result["errors"]))

        bundle = self._bundle()
        self.raw_members["git/payload.json"]["changed_paths"] = ["README.md"]
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("scope changed_paths lệch actual Git snapshot" in error for error in result["errors"]))

    def test_delivery_graph_does_not_mutate_inputs(self) -> None:
        bundle = self._bundle()
        raw_members = copy.deepcopy(self.raw_members)
        original_bundle = copy.deepcopy(bundle)
        original_raw = copy.deepcopy(raw_members)
        result = validate_delivery_links(bundle, raw_members=raw_members)
        self.assertEqual("UNKNOWN", result["verdict"])
        self.assertEqual(original_bundle, bundle)
        self.assertEqual(original_raw, raw_members)

    def test_six_retained_member_roles_reject_wrong_root_types(self) -> None:
        wrong_roots = ([], True, 7, None, "invalid")
        for role in ("raw_ci_page", "priority_rationale", "shared_invariant",
                     "breaking_decision", "verified_noop_reason", "git_commit_snapshot"):
            for wrong_root in wrong_roots:
                with self.subTest(role=role, root=type(wrong_root).__name__):
                    bundle = self._bundle()
                    if role == "raw_ci_page":
                        ref = "ci/payload/pages/check-runs-1.json"
                    elif role == "priority_rationale":
                        ref = "rationale/priority.json"
                    elif role == "shared_invariant":
                        ref = "rationale/shared.json"
                    elif role == "breaking_decision":
                        ref = "rationale/breaking.json"
                    elif role == "git_commit_snapshot":
                        ref = "git/payload.json"
                    else:
                        ref = "rationale/noop.json"
                        bundle["attempt"]["value"]["payload_mode"] = "verified_noop"
                        bundle["attempt"]["value"]["base_sha"] = bundle["attempt"]["value"]["verified_payload_sha"]
                        bundle["attempt"]["value"]["noop_reason_ref"] = ref
                        bundle["payload"]["value"]["parent_sha"] = bundle["attempt"]["value"]["base_sha"]
                        self.raw_members[ref] = {
                            "format_version": 1, "kind": "verified_noop_reason",
                            "assignment_ref": "assignment:T003", "task_id": "T003",
                            "task_revision": 1, "delivery_id": bundle["attempt"]["value"]["delivery_id"],
                            "verified_payload_sha": bundle["attempt"]["value"]["verified_payload_sha"],
                            "reason": "Payload đã được kiểm chứng đầy đủ và được giữ nguyên.",
                        }
                        bundle["archive"]["value"]["files"].append({
                            "path": ref, "byte_length": 1, "sha256": "6" * 64,
                        })
                    self.raw_members[ref] = wrong_root
                    result = self._validate(bundle)
                    self.assertEqual("REJECT", result["verdict"])
                    self.assertTrue(result["errors"])

    def test_verified_noop_has_typed_identity_bound_reason(self) -> None:
        bundle = self._bundle()
        ref = "rationale/noop.json"
        bundle["attempt"]["value"]["payload_mode"] = "verified_noop"
        bundle["attempt"]["value"]["base_sha"] = bundle["attempt"]["value"]["verified_payload_sha"]
        bundle["attempt"]["value"]["noop_reason_ref"] = ref
        bundle["payload"]["value"]["parent_sha"] = bundle["attempt"]["value"]["base_sha"]
        self.raw_members[ref] = {
            "format_version": 1, "kind": "verified_noop_reason",
            "assignment_ref": "assignment:T003", "task_id": "T003",
            "task_revision": 1, "delivery_id": bundle["attempt"]["value"]["delivery_id"],
            "verified_payload_sha": bundle["attempt"]["value"]["verified_payload_sha"],
            "reason": "Payload đã được kiểm chứng đầy đủ và được giữ nguyên.",
        }
        bundle["archive"]["value"]["files"].append({"path": ref, "byte_length": 1, "sha256": "6" * 64})
        self.assertEqual("UNKNOWN", self._validate(bundle)["verdict"])
        self.raw_members[ref]["delivery_id"] = "00000000-0000-4000-8000-000000000000"
        self.assertEqual("REJECT", self._validate(bundle)["verdict"])

    def test_verified_noop_preserves_original_closing_payload(self) -> None:
        bundle = self._bundle()
        attempt = bundle["attempt"]["value"]
        attempt["payload_mode"] = "verified_noop"
        attempt["base_sha"] = attempt["verified_payload_sha"]
        attempt["noop_reason_ref"] = "rationale/noop.json"
        bundle["payload"]["value"]["parent_sha"] = attempt["base_sha"]
        self.raw_members[attempt["noop_reason_ref"]] = {
            "format_version": 1, "kind": "verified_noop_reason",
            "assignment_ref": "assignment:T003", "task_id": "T003",
            "task_revision": 1, "delivery_id": attempt["delivery_id"],
            "verified_payload_sha": attempt["verified_payload_sha"],
            "reason": "Giữ nguyên payload lịch sử đã được xác minh.",
        }
        old_delivery = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
        old_subject = "docs(progress): [P0][T003] Đã ghi nhận closing lịch sử"
        old_body = (
            "Mục đích: Giữ nguyên commit closing đã có.\n"
            "Thay đổi: Không sửa nội dung payload.\n"
            "Kiểm tra: Giữ lại parent, scope và CI nguồn.\n\n"
            f"MCE4D-Delivery-ID: {old_delivery}\nMCE4D-Delivery-Phase: closing"
        )
        payload_commit = attempt["ordered_commits"][0]
        payload_commit["subject"] = old_subject
        payload_commit["body"] = old_body
        bundle["scope/payload.json"]["value"]["phase"] = "closing"
        bundle["scope/payload.json"]["value"]["changed_paths"] = ["Maps-Context-Engine-4D_SPEC.md"]
        self.raw_members["git/payload.json"]["subject"] = old_subject
        self.raw_members["git/payload.json"]["body"] = old_body
        self.raw_members["git/payload.json"]["changed_paths"] = ["Maps-Context-Engine-4D_SPEC.md"]
        bundle["archive"]["value"]["files"].append({
            "path": attempt["noop_reason_ref"], "byte_length": 1, "sha256": "6" * 64,
        })
        self.assertEqual("UNKNOWN", self._validate(bundle)["verdict"])
        payload_commit["body"] = old_body.replace(old_delivery, attempt["delivery_id"])
        self.raw_members["git/payload.json"]["body"] = payload_commit["body"]
        self.assertEqual("REJECT", self._validate(bundle)["verdict"])

    def test_closing_commit_trailers_bind_delivery(self) -> None:
        bundle = self._bundle()
        bundle["commit"]["value"]["body"] = (
            "MCE4D-Delivery-ID: 00000000-0000-4000-8000-000000000000\n"
            "MCE4D-Delivery-Phase: closing"
        )
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("closing commit phải có đúng" in error for error in result["errors"]))

    def test_archive_must_retain_transitive_source_members(self) -> None:
        bundle = self._bundle()
        archive = bundle["archive"]["value"]
        source_ref = bundle["payload"]["value"]["checks"][0]["source_evidence_ref"]
        archive["files"] = [item for item in archive["files"] if item["path"] != source_ref]
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("archive thiếu evidence member" in error for error in result["errors"]))

    def test_plan_digest_must_bind_attempt(self) -> None:
        bundle = self._bundle()
        bundle["attempt"]["value"]["check_plan_sha256"] = "0" * 64
        result = self._validate(bundle)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("check_plan_sha256 không khớp plan" in error for error in result["errors"]))

    def test_non_object_bundle_rejects_without_exception(self) -> None:
        result = validate_delivery_links([])
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(result["errors"])

    def test_selection_history_accepts_retained_supersede_chain(self) -> None:
        old_id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
        new_id = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
        previous = {
            "format_version": 1, "assignment_ref": "assignment:T003", "task_id": "T003",
            "task_revision": 1, "active_delivery_id": old_id, "supersedes_delivery_id": None,
            "invalidation_refs": [], "reason_ref": "assignment:T003",
            "updated_at_utc": "2026-10-15T19:24:54Z", "previous_selection_ref": None,
            "allocated_at_utc": "2026-10-15T19:24:54Z",
        }
        reason_ref = "history/plan-invalidation.json"
        reason = {
            "format_version": 1, "kind": "delivery_selection_invalidation", "task_id": "T003",
            "previous_task_revision": 1, "task_revision": 2, "previous_delivery_id": old_id,
            "active_delivery_id": new_id, "reason": "Kế hoạch kiểm tra đổi sau khi khóa revision 1.",
            "invalidated_refs": ["history/check-plan-revision-1.json"],
            "recorded_at_utc": "2026-10-15T20:01:00Z",
        }
        current = {
            "format_version": 1, "assignment_ref": "assignment:T003", "task_id": "T003",
            "task_revision": 2, "active_delivery_id": new_id, "supersedes_delivery_id": old_id,
            "invalidation_refs": [reason_ref], "reason_ref": reason_ref,
            "updated_at_utc": "2026-10-15T20:01:00Z", "previous_selection_ref": "history/selection-r1.json",
            "allocated_at_utc": "2026-10-15T20:01:00Z",
        }
        result = validate_delivery_links(
            {
                "history/selection-r1.json": {"kind": "GitTaskDeliverySelection", "value": previous},
                "selection-r2.json": {"kind": "GitTaskDeliverySelection", "value": current},
            },
            raw_members={
                "history/selection-r1.json": previous,
                reason_ref: reason,
                "history/check-plan-revision-1.json": {"check_plan_sha256": "1" * 64},
            },
        )
        self.assertEqual("UNKNOWN", result["verdict"])
        self.assertEqual([], result["errors"])

    def test_selection_history_rejects_missing_reason_revision_jump_and_reused_uuid(self) -> None:
        old_id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
        current_id = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
        previous = {
            "format_version": 1, "assignment_ref": "assignment:T003", "task_id": "T003",
            "task_revision": 1, "active_delivery_id": old_id, "supersedes_delivery_id": None,
            "invalidation_refs": [], "reason_ref": "assignment:T003",
            "updated_at_utc": "2026-10-15T19:24:54Z", "previous_selection_ref": None,
            "allocated_at_utc": "2026-10-15T19:24:54Z",
        }
        current = {
            "format_version": 1, "assignment_ref": "assignment:T003", "task_id": "T003",
            "task_revision": 3, "active_delivery_id": current_id, "supersedes_delivery_id": old_id,
            "invalidation_refs": [], "reason_ref": "missing-reason.json",
            "updated_at_utc": "2026-10-15T20:01:00Z", "previous_selection_ref": "history/selection-r1.json",
            "allocated_at_utc": "2026-10-15T20:01:00Z",
        }
        reused = dict(current, task_revision=2, active_delivery_id=old_id)
        for candidate in (current, reused):
            result = validate_delivery_links(
                {
                    "history/selection-r1.json": {"kind": "GitTaskDeliverySelection", "value": previous},
                    "selection-current.json": {"kind": "GitTaskDeliverySelection", "value": candidate},
                },
                raw_members={"history/selection-r1.json": previous},
            )
            self.assertEqual("REJECT", result["verdict"])
            self.assertTrue(result["errors"])

    def test_malformed_nested_shapes_reject_without_crashing(self) -> None:
        mutations = (
            lambda bundle: bundle["plan"]["value"]["checks"][0].__setitem__("command_refs", 7),
            lambda bundle: bundle["attempt"]["value"].__setitem__("ordered_commits", [None]),
            lambda bundle: bundle["archive"]["value"].__setitem__("files", [{"path": []}]),
            lambda bundle: bundle["payload"]["value"].__setitem__("checks", [None]),
            lambda bundle: bundle["payload"]["value"]["destination_observation"].__setitem__(
                "operation_evidence_ref", []
            ),
        )
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                bundle = self._bundle()
                mutate(bundle)
                result = self._validate(bundle)
                self.assertEqual("REJECT", result["verdict"])
                self.assertTrue(result["errors"])


class ArchivePublicationTests(unittest.TestCase):
    @staticmethod
    def _seed_valid_archive(root: Path) -> dict[str, object]:
        objects, raw = _delivery_link_bundle()
        archive = copy.deepcopy(objects["archive"]["value"])
        typed = {ref: entry for ref, entry in objects.items() if ref != "archive"}
        raw = dict(raw)
        raw["authority/task-start.txt"] = "Ủy quyền fixture cho task T003.".encode("utf-8")
        raw["history/invalidation-note.md"] = "Bằng chứng invalidation UTF-8 được giữ nguyên.\n"
        index = {
            "format_version": 1,
            "objects": {ref: entry["kind"] for ref, entry in sorted(typed.items())},
            "raw_members": sorted(
                ref for ref, value in raw.items()
                if isinstance(value, dict) or (ref.endswith(".md") and isinstance(value, str))
            ),
        }
        paths = {item["path"] for item in archive["files"]}
        paths.update(typed)
        paths.update(raw)
        paths.add("bundle-index.json")
        member_bytes: dict[str, bytes] = {}
        for ref in paths:
            if ref == "bundle-index.json":
                data = _canonical(index)
            elif ref in typed:
                data = _canonical(typed[ref]["value"])
            elif ref in raw and isinstance(raw[ref], dict):
                data = _canonical(raw[ref])
            elif ref in raw and isinstance(raw[ref], bytes):
                data = raw[ref]
            elif ref in raw and isinstance(raw[ref], str):
                data = raw[ref].encode("utf-8")
            else:
                data = b"fixture retained output\n"
            member_bytes[ref] = data
        archive["files"] = [
            {"path": ref, "byte_length": len(data), "sha256": hashlib.sha256(data).hexdigest()}
            for ref, data in sorted(member_bytes.items())
        ]
        archive_root = root / "artifacts" / "git-delivery"
        archive_root.mkdir(parents=True)
        staging = archive_root / f".staging-{archive['delivery_id']}"
        staging.mkdir()
        for ref, data in member_bytes.items():
            path = staging.joinpath(*ref.split("/"))
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        return archive

    def test_publish_seal_readback_and_cold_verification(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-publish-") as temp:
            root = Path(temp)
            archive = self._seed_valid_archive(root)
            published = publish_archive(root, archive, spec_path=SPEC_PATH)
            cold = verify_archive(root, archive["delivery_id"], spec_path=SPEC_PATH)
            self.assertEqual(f"artifacts/git-delivery/{archive['delivery_id']}", published["planned_archive_path"])
            self.assertIsNotNone(published["published_at_utc"])
            self.assertIsNone(cold["published_at_utc"])
            self.assertEqual(published["archive_manifest_sha256"], cold["archive_manifest_sha256"])
            self.assertEqual(published["archive_seal_sha256"], cold["archive_seal_sha256"])

    def test_archive_preserves_utf8_text_raw_members_through_cold_readback(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-text-raw-") as temp:
            root = Path(temp)
            archive = self._seed_valid_archive(root)
            expected = "Bằng chứng invalidation UTF-8 được giữ nguyên.\n".encode("utf-8")
            published = publish_archive(root, archive, spec_path=SPEC_PATH)
            cold = verify_archive(root, archive["delivery_id"], spec_path=SPEC_PATH)
            final = root / published["planned_archive_path"]
            index = json.loads((final / "bundle-index.json").read_text(encoding="utf-8"))
            self.assertIn("history/invalidation-note.md", index["raw_members"])
            self.assertEqual((final / "history/invalidation-note.md").read_bytes(), expected)
            self.assertEqual(published["archive_manifest_sha256"], cold["archive_manifest_sha256"])

    def test_member_read_supports_deep_windows_paths(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-long-path-") as temp:
            root = Path(temp)
            relative = "/".join(["history"] * 21 + ["invalidation-note.txt"])
            member = _member_path(root, relative, create_parent=True)
            expected = b"retained evidence\n"
            try:
                member.write_bytes(expected)
                self.assertEqual(_read_bounded(_member_path(root, relative)), expected)
            finally:
                member.unlink(missing_ok=True)

    def test_publisher_recovers_fully_staged_members_before_manifest_seal(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-resume-") as temp:
            root = Path(temp)
            archive = self._seed_valid_archive(root)
            resumed = publish_archive(root, archive, spec_path=SPEC_PATH)
            self.assertIsNotNone(resumed["published_at_utc"])
            self.assertTrue((root / resumed["planned_archive_path"] / "seal.json").is_file())
            self.assertFalse((root / "artifacts" / "git-delivery" / f".staging-{archive['delivery_id']}").exists())

    def test_publisher_recovers_after_manifest_and_seal_were_flushed(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-sealed-resume-") as temp:
            root = Path(temp)
            archive = self._seed_valid_archive(root)
            stage = root / "artifacts" / "git-delivery" / f".staging-{archive['delivery_id']}"
            manifest_bytes = _canonical(archive)
            seal = {
                "format_version": 1, "delivery_id": archive["delivery_id"],
                "task_id": archive["task_id"], "task_revision": archive["task_revision"],
                "manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
                "sealed_at_utc": "2026-10-16T00:00:00Z",
            }
            (stage / "manifest.json").write_bytes(manifest_bytes)
            (stage / "seal.json").write_bytes(_canonical(seal))
            resumed = publish_archive(root, archive, spec_path=SPEC_PATH)
            self.assertEqual(hashlib.sha256(_canonical(seal)).hexdigest(), resumed["archive_seal_sha256"])

    def test_archive_cli_consumes_candidate_only_after_durable_manifest_and_publishes(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-cli-") as temp:
            root = Path(temp)
            (root / SPEC_PATH.name).write_bytes(SPEC_PATH.read_bytes())
            archive = self._seed_valid_archive(root)
            stage = root / "artifacts" / "git-delivery" / f".staging-{archive['delivery_id']}"
            (stage / "archive-candidate.json").write_bytes(_canonical(archive))
            operation_id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
            with contextlib.redirect_stdout(io.StringIO()):
                result = publish_git_archive_main([
                    "--root", str(root), "--delivery", archive["delivery_id"],
                    "--operation-id", operation_id,
                ])
            self.assertEqual(0, result)
            final = root / "artifacts" / "git-delivery" / archive["delivery_id"]
            receipt = root / "artifacts" / "git-delivery" / "operations" / operation_id / "publication.json"
            self.assertTrue(final.is_dir())
            self.assertFalse(stage.exists())
            self.assertTrue(receipt.is_file())
            self.assertEqual(
                hashlib.sha256(_canonical(strict_json_loads((final / "manifest.json").read_bytes()))).hexdigest(),
                json.loads(receipt.read_text(encoding="utf-8"))["archive_manifest_sha256"],
            )
            with contextlib.redirect_stdout(io.StringIO()):
                retry = publish_git_archive_main([
                    "--root", str(root), "--delivery", archive["delivery_id"],
                    "--operation-id", operation_id,
                ])
            self.assertEqual(0, retry)

    def test_archive_cli_resumes_from_durable_manifest_after_candidate_consumption(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-cli-resume-") as temp:
            root = Path(temp)
            (root / SPEC_PATH.name).write_bytes(SPEC_PATH.read_bytes())
            archive = self._seed_valid_archive(root)
            stage = root / "artifacts" / "git-delivery" / f".staging-{archive['delivery_id']}"
            (stage / "manifest.json").write_bytes(_canonical(archive))
            operation_id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
            with contextlib.redirect_stdout(io.StringIO()):
                result = publish_git_archive_main([
                    "--root", str(root), "--delivery", archive["delivery_id"],
                    "--operation-id", operation_id,
                ])
            self.assertEqual(0, result)
            self.assertTrue((root / "artifacts" / "git-delivery" / archive["delivery_id"]).is_dir())

    def test_publisher_rejects_changed_manifest_after_staging_seal(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-seal-mismatch-") as temp:
            root = Path(temp)
            archive = self._seed_valid_archive(root)
            stage = root / "artifacts" / "git-delivery" / f".staging-{archive['delivery_id']}"
            manifest_bytes = _canonical(archive)
            seal = {
                "format_version": 1, "delivery_id": archive["delivery_id"],
                "task_id": archive["task_id"], "task_revision": archive["task_revision"],
                "manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
                "sealed_at_utc": "2026-10-16T00:00:00Z",
            }
            (stage / "manifest.json").write_bytes(manifest_bytes)
            (stage / "seal.json").write_bytes(_canonical(seal))
            changed = copy.deepcopy(archive)
            changed["created_at_utc"] = "2026-10-16T00:00:01Z"
            with self.assertRaisesRegex(ArchiveError, "không bind đúng immutable manifest"):
                publish_archive(root, changed, spec_path=SPEC_PATH)
            self.assertTrue(stage.is_dir())

    def test_existing_archive_reuse_never_invents_original_publish_time(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-replay-") as temp:
            root = Path(temp)
            archive = self._seed_valid_archive(root)
            first = publish_archive(root, archive, spec_path=SPEC_PATH)
            second = publish_archive(root, archive, spec_path=SPEC_PATH)
            self.assertIsNotNone(first["published_at_utc"])
            self.assertIsNone(second["published_at_utc"])
            self.assertEqual(first["archive_seal_sha256"], second["archive_seal_sha256"])

    def test_cold_readback_rejects_tampered_member_bytes(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-tamper-") as temp:
            root = Path(temp)
            archive = self._seed_valid_archive(root)
            published = publish_archive(root, archive, spec_path=SPEC_PATH)
            member = root / published["planned_archive_path"] / "attempt"
            member.write_bytes(member.read_bytes() + b"tamper")
            with self.assertRaisesRegex(ArchiveError, "byte_length/SHA"):
                verify_archive(root, archive["delivery_id"], spec_path=SPEC_PATH)

    def test_publication_projection_is_written_once_after_actual_readback(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-projection-") as temp:
            root = Path(temp)
            archive_root = root / "artifacts" / "git-delivery"
            archive_root.mkdir(parents=True)
            operation_id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
            projection = {
                "format_version": 1, "kind": "archive_publication",
                "planned_archive_path": "artifacts/git-delivery/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
                "archive_manifest_sha256": "a" * 64, "archive_seal_sha256": "b" * 64,
                "published_at_utc": "2026-10-16T00:00:00Z",
                "readback_completed_at_utc": "2026-10-16T00:00:01Z",
            }
            path = persist_publication_projection(root, operation_id, projection)
            self.assertEqual(projection, json.loads(path.read_text(encoding="utf-8")))
            with self.assertRaises(OSError):
                persist_publication_projection(root, operation_id, projection)

    def test_atomic_archive_rename_never_replaces_final_directory(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-archive-rename-") as temp:
            root = Path(temp)
            source = root / "staging"
            destination = root / "published"
            source.mkdir()
            (source / "marker").write_text("first", encoding="utf-8")
            _rename_noreplace(source, destination)
            retry = root / "retry"
            retry.mkdir()
            (retry / "marker").write_text("second", encoding="utf-8")
            with self.assertRaises(OSError):
                _rename_noreplace(retry, destination)
            self.assertEqual("first", (destination / "marker").read_text(encoding="utf-8"))
            self.assertTrue(retry.is_dir())

    def test_live_observer_rejects_projection_outside_owned_operations_root(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-live-projection-path-") as temp:
            root = Path(temp)
            outside = root / "publication.json"
            outside.write_text("{}", encoding="utf-8")
            with patch("tools.spec_check.github_observer.verify_archive") as verify_archive:
                with self.assertRaises(GitHubObserverError):
                    _verify_publication_projection(root, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", outside)
                verify_archive.assert_not_called()

    def test_live_observer_rejects_symlinked_projection_ancestor(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-live-projection-link-") as temp:
            root = Path(temp)
            git_delivery = root / "artifacts" / "git-delivery"
            operations = git_delivery / "operations"
            operations.mkdir(parents=True)
            operation_id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
            actual = operations / operation_id
            actual.mkdir()
            projection = actual / "publication.json"
            projection.write_text("{}", encoding="utf-8")
            alias = operations / "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
            try:
                alias.symlink_to(actual, target_is_directory=True)
            except (OSError, NotImplementedError):
                self.skipTest("filesystem hiện tại không cho tạo symlink không nâng quyền")
            with patch("tools.spec_check.github_observer.verify_archive") as verify_archive:
                with self.assertRaises(GitHubObserverError):
                    _verify_publication_projection(root, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", alias / "publication.json")
                verify_archive.assert_not_called()


class GitHubObserverTests(unittest.TestCase):
    @staticmethod
    def _response(payload: object) -> tuple[dict[str, str], bytes, object, str, str]:
        body = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
        return {}, body, payload, "2026-10-16T00:00:00Z", "2026-10-16T00:00:01Z"

    def test_current_tracker_reader_rejects_malformed_row_without_index_error(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-live-tracker-") as temp:
            malformed = Path(temp) / "spec.md"
            malformed.write_text("## S19 — Tracker\n| T003 | truncated |\n## S20 — Gates\n", encoding="utf-8")
            with self.assertRaisesRegex(GitHubObserverError, "sai cấu trúc cột"):
                _current_tracker_identity(malformed, "T003")
        tracker = next(line for line in SPEC_TEXT.splitlines() if line.startswith("| T003 | G0 | T001 |"))
        metadata = dict(item.split("=", 1) for item in tracker.strip().split("|")[-2].strip().split(";")[1:])
        self.assertEqual(
            ("T003", int(metadata["task_revision"]), metadata["delivery"]),
            _current_tracker_identity(SPEC_PATH, "T003"),
        )

    def test_api_process_output_is_capped_while_pipes_are_drained(self) -> None:
        class FakeProcess:
            returncode = 0

            def __init__(self, stdout: bytes, stderr: bytes = b"") -> None:
                self.stdout = io.BytesIO(stdout)
                self.stderr = io.BytesIO(stderr)
                self.killed = False

            def wait(self, timeout: int) -> int:
                return self.returncode

            def kill(self) -> None:
                self.killed = True
                self.returncode = -9

        accepted = FakeProcess(b"header/body", b"warning")
        with patch("tools.spec_check.github_observer.subprocess.Popen", return_value=accepted):
            result = _run_bounded_process(
                ["gh", "api"], env={}, timeout=1, stdout_limit=20, stderr_limit=10,
            )
        self.assertEqual((0, b"header/body", b"warning"), result)
        self.assertFalse(accepted.killed)

        oversized = FakeProcess(b"x" * 21)
        with patch("tools.spec_check.github_observer.subprocess.Popen", return_value=oversized):
            with self.assertRaisesRegex(GitHubObserverError, "vượt giới hạn đọc"):
                _run_bounded_process(
                    ["gh", "api"], env={}, timeout=1, stdout_limit=20, stderr_limit=10,
                )
        self.assertTrue(oversized.killed)

    def test_pagination_follows_actual_link_and_retains_raw_body(self) -> None:
        sha = "a" * 40
        first = {"total_count": 2, "check_runs": [{"id": 11}]}
        second = {"total_count": 2, "check_runs": [{"id": 12}]}
        with tempfile.TemporaryDirectory(prefix="mce4d-github-pages-") as temp:
            client = GitHubApiClient("github.com", Path(temp))

            def response(url: str) -> tuple[dict[str, str], bytes, object, str, str]:
                if url.endswith("page=1"):
                    return ({"link": f'<https://api.github.com/repos/o/r/commits/{sha}/check-runs?head_sha={sha}&per_page=100&page=2>; rel="next"'},
                            *self._response(first)[1:])
                self.assertIn("page=2", url)
                return self._response(second)

            with patch.object(client, "_request", side_effect=response):
                rows = client.paginate(
                    endpoint="check_runs", requested_sha=sha,
                    path=f"repos/o/r/commits/{sha}/check-runs", params={"head_sha": sha},
                    group_id="checks",
                )
            self.assertEqual([11, 12], [row["id"] for row in rows])
            self.assertEqual([1, 2], [page["page_number"] for page in client.collection_pages])
            for page in client.collection_pages:
                envelope = client.raw_members[page["raw_page_ref"]]
                self.assertEqual(envelope["body"], client.raw_members[envelope["body_ref"]])

    def test_pagination_rejects_changed_filter(self) -> None:
        sha = "a" * 40
        body = {"total_count": 1, "check_runs": []}
        with tempfile.TemporaryDirectory(prefix="mce4d-github-pages-") as temp:
            client = GitHubApiClient("github.com", Path(temp))
            client._request = lambda url: (
                {"link": f'<https://api.github.com/repos/o/r/commits/{sha}/check-runs?head_sha={"b" * 40}&per_page=100&page=2>; rel="next"'},
                *self._response(body)[1:],
            )
            with self.assertRaisesRegex(GitHubObserverError, "đổi host/path/filter"):
                client.paginate(
                    endpoint="check_runs", requested_sha=sha,
                    path=f"repos/o/r/commits/{sha}/check-runs", params={"head_sha": sha},
                    group_id="checks",
                )

    def test_duplicate_link_relations_are_rejected(self) -> None:
        with self.assertRaisesRegex(GitHubObserverError, "lặp relation next"):
            _parse_links('<https://api.github.com/a?page=2>; rel="next", <https://api.github.com/a?page=3>; rel="next"')

    def test_runner_matrix_fails_closed_when_label_is_missing(self) -> None:
        self.assertTrue(_matrix_matches({"os": "ubuntu-latest"}, {"labels": ["ubuntu-latest", "x64"]}))
        self.assertFalse(_matrix_matches({"os": "windows-latest"}, {"labels": ["ubuntu-latest", "x64"]}))
        self.assertFalse(_matrix_matches({"runner": "ubuntu-latest"}, {"labels": ["ubuntu-latest"]}))

    @staticmethod
    def _byte_bound_bundle() -> tuple[dict[str, object], dict[str, object], dict[str, bytes], str]:
        bundle, raw = _delivery_link_bundle()
        payload = bundle["payload"]["value"]
        collection = bundle[payload["collection_evidence_ref"]]["value"]
        page = next(item for item in collection["pages"] if item["endpoint"] == "check_runs")
        page_ref = page["raw_page_ref"]
        body = raw[page_ref]
        body_ref = page_ref.removesuffix(".json") + ".body.json"
        body_bytes = _canonical(body)
        envelope = {
            "format_version": 1, "kind": "github_api_page", "query_id": page["query_id"],
            "endpoint": page["endpoint"], "requested_sha": page["requested_sha"],
            "page_number": page["page_number"], "next_page_number": page["next_page_number"],
            "requested_url": f"https://api.github.com/repos/owner/repo/commits/{payload['subject_sha']}/check-runs?per_page=100&page=1",
            "next_url": None, "request_started_at_utc": "2026-10-16T00:00:03.500Z",
            "response_received_at_utc": "2026-10-16T00:00:04Z", "http_status": 200,
            "response_headers": {"content-type": "application/json"}, "body_ref": body_ref,
            "body_sha256": hashlib.sha256(body_bytes).hexdigest(),
            "body_byte_length": len(body_bytes), "body": body,
        }
        raw[page_ref] = envelope
        raw[body_ref] = body
        bundle["archive"]["value"]["files"].append({"path": body_ref, "byte_length": len(body_bytes),
                                                          "sha256": hashlib.sha256(body_bytes).hexdigest()})
        return bundle, raw, {page_ref: _canonical(envelope), body_ref: body_bytes}, page_ref

    def test_static_graph_checks_api_page_against_exact_raw_body_bytes(self) -> None:
        bundle, raw, member_bytes, _ = self._byte_bound_bundle()
        result = validate_delivery_links(bundle, raw_members=raw, raw_member_bytes=member_bytes)
        self.assertEqual("UNKNOWN", result["verdict"])
        self.assertEqual([], result["errors"])

    def test_static_graph_rejects_declared_api_body_hash_mismatch(self) -> None:
        bundle, raw, member_bytes, page_ref = self._byte_bound_bundle()
        raw[page_ref]["body_sha256"] = "0" * 64
        member_bytes[page_ref] = _canonical(raw[page_ref])
        result = validate_delivery_links(bundle, raw_members=raw, raw_member_bytes=member_bytes)
        self.assertEqual("REJECT", result["verdict"])
        self.assertTrue(any("body bytes lệch declared length/SHA" in error for error in result["errors"]))


if __name__ == "__main__":
    unittest.main(verbosity=2)
