"""Fixtures positive/negative cho commit, check observations và CLOSE-DIFF-01."""

from __future__ import annotations

import hashlib
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tools.ci.delivery_policy import (
    classify_closing_spec_diff,
    validate_check_plan,
    validate_commit_message,
    validate_git_closing_diff,
    validate_observed_checks,
)
from tools.spec_check.schema import strict_json_loads


SPEC_PATH = ROOT / "Maps-Context-Engine-4D_SPEC.md"
SPEC_TEXT = SPEC_PATH.read_text(encoding="utf-8")
DELIVERY_ID = "eca9684d-8e74-4b06-8168-47f2a3c82e9e"
PAYLOAD_SHA = "1" * 40
PLAN_SHA = "2" * 64
PLAN_FILE_SHA = "0adcedf6dcd2e57f159d2be096cbc7edfe2563d794bd2ca2369925a53cce0009"
COMMIT_BODY = (
    "Mục đích: Kiểm chứng luồng giao task.\n"
    "Thay đổi: Ghi bằng chứng đã khóa.\n"
    "Kiểm tra: Chạy fixture đúng phạm vi."
)


def _delivery_evidence(payload_sha: str = "PENDING", *, closing: bool = False) -> str:
    payload_ci = "PENDING" if payload_sha == "PENDING" else "PASS(run:1)"
    value = (f"E007;task_revision=1;delivery={DELIVERY_ID};plan={PLAN_FILE_SHA};priority=P0;"
             f"local=PASS;payload_sha={payload_sha};payload_CI={payload_ci};closing_sha=PENDING;"
             "closing_CI=PENDING;archive/live=PENDING")
    return value + (";candidate=DONE" if closing else "")


def _row_with_cells(line: str, *, status: str | None = None, owner: str | None = None, evidence: str | None = None) -> str:
    cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
    if status is not None:
        cells[6] = status
    if owner is not None:
        cells[7] = owner
    if evidence is not None:
        cells[8] = evidence
    return "| " + " | ".join(cells) + " |"


def _with_task_row(text: str, status: str, evidence: str) -> str:
    lines = text.splitlines()
    index = next(i for i, line in enumerate(lines) if line.startswith("| T003 | G0 |"))
    lines[index] = _row_with_cells(lines[index], status=status, evidence=evidence)
    return "\n".join(lines) + "\n"


def _append_evidence(text: str, task_id: str = "T003") -> str:
    lines = text.splitlines()
    insertion = next(i for i, line in enumerate(lines) if line.startswith("Format result:"))
    ids = [int(match.group(1)) for line in lines if (match := re.match(r"^\| E(\d{3}) \|", line))]
    evidence_id = max(ids, default=0) + 1
    lines.insert(insertion, f"| E{evidence_id:03d} | {task_id} | payload={PAYLOAD_SHA} | Windows + Ubuntu fixture runtime | classifier positive fixture | PASS local/static only | fixture.json=sha256:{PLAN_SHA} | 2026-10-16T00:00:00Z |")
    return "\n".join(lines) + "\n"


class CommitPolicyTests(unittest.TestCase):
    def test_payload_commit_with_vietnamese_note_passes(self) -> None:
        message = "test(spec-check): [P0][T003] Bắt lỗi thiếu cạnh truy vết\n\n" + COMMIT_BODY
        self.assertEqual([], validate_commit_message(message, phase="payload", task_id="T003", priority="P0"))

    def test_p3_revert_commit_passes(self) -> None:
        message = "revert(spec-check): [P3][T003] Khôi phục kiểm tra đã đổi\n\n" + COMMIT_BODY
        self.assertEqual([], validate_commit_message(message, phase="payload", task_id="T003", priority="P3"))

    def test_duplicate_delivery_trailer_outside_final_block_fails(self) -> None:
        message = (
            "docs(progress): [P0][T003] Đề xuất đóng task\n\n" + COMMIT_BODY
            + "\nMCE4D-Delivery-ID: " + DELIVERY_ID
            + "\nGhi chú chen giữa trailer.\n\nMCE4D-Delivery-ID: " + DELIVERY_ID
            + "\nMCE4D-Delivery-Phase: closing"
        )
        errors = validate_commit_message(message, phase="closing", task_id="T003", priority="P0")
        self.assertTrue(any("duy nhất và nằm trong trailer block cuối" in error for error in errors))

    def test_breaking_marker_requires_explanation(self) -> None:
        message = "feat(spec-check)!: [P0][T003] Đổi contract đã duyệt\n\n" + COMMIT_BODY
        errors = validate_commit_message(message, phase="payload", task_id="T003", priority="P0")
        self.assertTrue(any("BREAKING CHANGE" in error for error in errors))

    def test_wrong_task_fails(self) -> None:
        errors = validate_commit_message("test(spec-check): [P0][T004] Bắt lỗi truy vết\n\n" + COMMIT_BODY, phase="payload", task_id="T003", priority="P0")
        self.assertTrue(any("đúng task T003" in error for error in errors))

    def test_english_note_fails(self) -> None:
        errors = validate_commit_message("test(spec-check): [P0][T003] Add traceability guard\n\n" + COMMIT_BODY, phase="payload", task_id="T003", priority="P0")
        self.assertTrue(any("tiếng Việt" in error for error in errors))

    def test_skip_ci_fails(self) -> None:
        errors = validate_commit_message("docs(progress): [P0][T003] Cập nhật ledger\n\n" + COMMIT_BODY + "\n[skip ci]", phase="payload", task_id="T003", priority="P0")
        self.assertTrue(any("bỏ qua CI" in error for error in errors))

    def test_payload_cannot_claim_closing_trailers(self) -> None:
        message = "docs(progress): [P0][T003] Ghi nhận delivery\n\n" + COMMIT_BODY + "\n\nMCE4D-Delivery-ID: " + DELIVERY_ID + "\nMCE4D-Delivery-Phase: closing"
        errors = validate_commit_message(message, phase="payload", task_id="T003", priority="P0")
        self.assertTrue(any("Payload không được" in error for error in errors))

    def test_closing_trailers_pass(self) -> None:
        message = "docs(progress): [P0][T003] Đề xuất đóng task\n\n" + COMMIT_BODY + "\n\nMCE4D-Delivery-ID: " + DELIVERY_ID + "\nMCE4D-Delivery-Phase: closing"
        self.assertEqual([], validate_commit_message(message, phase="closing", task_id="T003", priority="P0"))

    def test_duplicate_closing_trailer_fails(self) -> None:
        message = "docs(progress): [P0][T003] Đề xuất đóng task\n\n" + COMMIT_BODY + "\n\nMCE4D-Delivery-ID: " + DELIVERY_ID + "\nMCE4D-Delivery-ID: " + DELIVERY_ID + "\nMCE4D-Delivery-Phase: closing"
        errors = validate_commit_message(message, phase="closing", task_id="T003", priority="P0")
        self.assertTrue(any("đúng một Delivery-ID" in error for error in errors))


class CheckObservationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.expected = [{
            "id": "required/linux", "kind": "check_run", "context": "checks (ubuntu)",
            "source_app": "github.com:app:15368", "workflow_path": "workflow.yml", "job_name": "checks",
            "matrix": {"os": "ubuntu"}, "eligible_events": ["push"], "reported_sha_binding": "subject",
        }]
        self.observed = [{
            "id": "required/linux", "kind": "check_run", "context": "checks (ubuntu)", "source_app": "github.com:app:15368",
            "workflow_path": "workflow.yml", "job_name": "checks", "matrix": {"os": "ubuntu"}, "event": "push",
            "run_id": "123", "job_id": "456", "attempt": 1, "reported_head_sha": PAYLOAD_SHA,
            "status": "completed", "conclusion": "success", "tested_checkout_sha": PAYLOAD_SHA,
            "execution_verified": True, "source_evidence_ref": "ci/source.json",
            "execution_evidence_ref": "ci/execution.json", "evidence_refs": ["ci/log.txt"],
        }]

    def test_exact_success_passes(self) -> None:
        self.assertEqual([], validate_observed_checks(self.expected, self.observed, PAYLOAD_SHA))

    def test_stale_sha_fails(self) -> None:
        changed = [dict(self.observed[0], tested_checkout_sha="0" * 40)]
        self.assertTrue(any("không khớp subject SHA" in e for e in validate_observed_checks(self.expected, changed, PAYLOAD_SHA)))

    def test_missing_check_fails(self) -> None:
        self.assertTrue(any("hiện có 0" in e for e in validate_observed_checks(self.expected, [], PAYLOAD_SHA)))

    def test_skipped_check_fails(self) -> None:
        changed = [dict(self.observed[0], conclusion="skipped")]
        self.assertTrue(any("terminal success" in e or "skipped/neutral" in e for e in validate_observed_checks(self.expected, changed, PAYLOAD_SHA)))

    def test_duplicate_native_identity_fails(self) -> None:
        changed = [self.observed[0], self.observed[0]]
        self.assertTrue(any("đúng một observed" in e for e in validate_observed_checks(self.expected, changed, PAYLOAD_SHA)))

    def test_current_frozen_plan_maps_to_real_workflow(self) -> None:
        plan_path = ROOT / "tools/ci/check-plan.json"
        workflow = (ROOT / ".github/workflows/bootstrap.yml").read_text(encoding="utf-8")
        raw = plan_path.read_bytes()
        plan = strict_json_loads(raw)
        task_line = next(line for line in SPEC_TEXT.splitlines() if line.startswith("| T003 | G0 |"))
        match = re.search(r"(?:^|;)plan=([0-9a-f]{64})(?:;|\s*\|)", task_line)
        self.assertIsNotNone(match, "S19 T003 row must retain the immutable expected plan SHA")
        self.assertEqual(match.group(1), hashlib.sha256(raw).hexdigest())
        self.assertEqual([], validate_check_plan(plan, workflow_text=workflow, expected_sha256=match.group(1)))

    def test_command_moved_to_wrong_job_fails(self) -> None:
        plan = strict_json_loads((ROOT / "tools/ci/check-plan.json").read_bytes())
        workflow = (ROOT / ".github/workflows/bootstrap.yml").read_text(encoding="utf-8")
        moved = workflow.replace("run: python -B tools/spec_check/check.py", "run: python -B tools/ci/bootstrap_check.py", 1)
        errors = validate_check_plan(plan, workflow_text=moved, expected_sha256=PLAN_FILE_SHA)
        self.assertTrue(any("không chạy command đã freeze" in error for error in errors))

    def test_matrix_context_not_declared_by_job_fails(self) -> None:
        plan = strict_json_loads((ROOT / "tools/ci/check-plan.json").read_bytes())
        workflow = (ROOT / ".github/workflows/bootstrap.yml").read_text(encoding="utf-8")
        changed = workflow.replace("os: [ubuntu-latest, windows-latest]", "os: [ubuntu-latest]", 1)
        errors = validate_check_plan(plan, workflow_text=changed, expected_sha256=PLAN_FILE_SHA)
        self.assertTrue(any("matrix OS 'windows-latest'" in error for error in errors))


class ClosingDiffTests(unittest.TestCase):
    def _payload_and_closing(self) -> tuple[str, str]:
        payload = _with_task_row(SPEC_TEXT, "VERIFY", _delivery_evidence())
        closing = _with_task_row(payload, "DONE", _delivery_evidence(PAYLOAD_SHA, closing=True))
        closing = _append_evidence(closing)
        return payload, closing

    def test_candidate_closing_leaf_passes(self) -> None:
        payload, closing = self._payload_and_closing()
        self.assertEqual([], classify_closing_spec_diff(payload, closing, task_id="T003", owning_gate="G0"))

    def test_another_task_status_change_fails(self) -> None:
        payload, closing = self._payload_and_closing()
        lines = closing.splitlines()
        index = next(i for i, line in enumerate(lines) if line.startswith("| T004 |"))
        lines[index] = _row_with_cells(lines[index], status="IN_PROGRESS")
        errors = classify_closing_spec_diff(payload, "\n".join(lines) + "\n", task_id="T003", owning_gate="G0")
        self.assertTrue(any("non-owned row" in error for error in errors))

    def test_design_section_change_fails(self) -> None:
        payload, closing = self._payload_and_closing()
        changed = closing.replace("T003 xây `go run ./tools/spec_check`", "T003 đổi hợp đồng `go run ./tools/spec_check`", 1)
        errors = classify_closing_spec_diff(payload, changed, task_id="T003", owning_gate="G0")
        self.assertTrue(any("non-whitelisted SPEC section" in error for error in errors))

    def test_unrelated_evidence_insert_fails(self) -> None:
        payload, closing = self._payload_and_closing()
        payload_lines = set(payload.splitlines())
        inserted = next(line for line in closing.splitlines() if line.startswith("| E") and line not in payload_lines)
        changed = closing.replace(inserted, inserted.replace("| T003 |", "| T004 |", 1), 1)
        errors = classify_closing_spec_diff(payload, changed, task_id="T003", owning_gate="G0")
        self.assertTrue(any("chỉ được thêm evidence row" in error for error in errors))

    def test_gate_scope_change_fails(self) -> None:
        payload, closing = self._payload_and_closing()
        changed = closing.replace("Site survey, pinned materials", "Changed survey, pinned materials", 1)
        errors = classify_closing_spec_diff(payload, changed, task_id="T003", owning_gate="G0")
        self.assertTrue(any("S20 changed gate scope" in error for error in errors))

    def test_actual_git_parent_and_non_spec_path_guard(self) -> None:
        payload, closing = self._payload_and_closing()
        with tempfile.TemporaryDirectory(prefix="mce4d-closing-fixture-") as temp:
            repo = Path(temp)
            spec_path = repo / "Maps-Context-Engine-4D_SPEC.md"
            spec_path.write_text(payload, encoding="utf-8", newline="")
            subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
            subprocess.run(["git", "config", "core.autocrlf", "false"], cwd=repo, check=True)
            subprocess.run(["git", "config", "user.name", "Fixture"], cwd=repo, check=True)
            subprocess.run(["git", "config", "user.email", "fixture@example.invalid"], cwd=repo, check=True)
            subprocess.run(["git", "add", "Maps-Context-Engine-4D_SPEC.md"], cwd=repo, check=True)
            payload_message = "feat(spec-check): [P0][T003] Tạo payload kiểm tra\n\n" + COMMIT_BODY
            subprocess.run(["git", "commit", "-m", payload_message], cwd=repo, check=True)
            payload_sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, check=True, capture_output=True, text=True).stdout.strip()
            closing = _with_task_row(payload, "DONE", _delivery_evidence(payload_sha, closing=True))
            closing = _append_evidence(closing)
            spec_path.write_text(closing, encoding="utf-8", newline="")
            subprocess.run(["git", "add", "Maps-Context-Engine-4D_SPEC.md"], cwd=repo, check=True)
            closing_message = ("docs(progress): [P0][T003] Đề xuất đóng kiểm tra\n\n" + COMMIT_BODY + "\n\n"
                               f"MCE4D-Delivery-ID: {DELIVERY_ID}\nMCE4D-Delivery-Phase: closing")
            subprocess.run(["git", "commit", "-m", closing_message], cwd=repo, check=True)
            closing_sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, check=True, capture_output=True, text=True).stdout.strip()
            self.assertEqual([], validate_git_closing_diff(repo, payload_sha, closing_sha, task_id="T003", owning_gate="G0"))
            (repo / "extra.txt").write_text("payload drift", encoding="utf-8")
            subprocess.run(["git", "add", "extra.txt"], cwd=repo, check=True)
            subprocess.run(["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "D"], cwd=repo, check=True)
            drift_sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, check=True, capture_output=True, text=True).stdout.strip()
            errors = validate_git_closing_diff(repo, closing_sha, drift_sha, task_id="T003", owning_gate="G0")
            self.assertTrue(any("file ngoài SPEC" in error for error in errors))


if __name__ == "__main__":
    unittest.main(verbosity=2)
