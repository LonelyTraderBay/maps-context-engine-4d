from __future__ import annotations

import hashlib
import json
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

ROOT = Path.cwd().resolve()
TASK = "T068"
DELIVERY = "36570740-c893-45b5-8f0e-f68653195b6a"
REVISION = 1
BASE_SHA = "12547b660521fecb9592ae40cd786a13c25f63bb"
T067 = "b0e123c8-1a67-47f3-9360-9f8b6ec57bd4"
T067_CLOSING = BASE_SHA
LIVE_ID = "ba0cc8e3-9651-47e8-a836-64dc8076ea04"
ARCHIVE_OP = "a8d725e2-97a0-400d-a1fc-9f97ddfe7c91"
SUPPORT = f"artifacts/git-delivery-support/T068-r1-{DELIVERY}"
T067_SUPPORT = f"artifacts/git-delivery-support/T067-r1-{T067}"
FIXTURE_REF = "artifacts/preflight/T068/polyglot-acceptance-plan-r1.json"
SPEC_PATH = ROOT / "Maps-Context-Engine-4D_SPEC.md"

if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
from artifacts.preflight.T108.foundation_seal import _live_receipt_errors, _verified_archive_identity
from tools.ci.delivery_policy import _canonical_json
from tools.spec_check.archive import _fsync_directory, verify_archive
from tools.spec_check.spec import _effective_status, _evidence_fields, _section, _table_with_headers


def utc() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def sha_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha_file(path: Path) -> str:
    return sha_bytes(path.read_bytes())


def write_json(relative: str, value: dict[str, Any], *, canonical: bool = False) -> str:
    path = ROOT / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        raise FileExistsError(f"preserving existing evidence: {relative}")
    payload = _canonical_json(value) if canonical else (
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    ).encode("utf-8")
    with path.open("xb") as stream:
        stream.write(payload)
        stream.flush()
    return sha_bytes(payload)


def task_rows(text: str) -> dict[str, dict[str, str]]:
    section = _section(text, "## S19 — Bảng triển khai duy nhất") or ""
    table = _table_with_headers(section, "Task", "Gate", "Status", "Evidence")
    if table is None:
        raise SystemExit("Không đọc được bảng S19")
    return {row["Task"].strip(): row for row in table.rows}


def row_cells(line: str) -> list[str]:
    stripped = line.strip()
    return [cell.strip() for cell in stripped[1:-1].split("|")]


def render_row(cells: list[str]) -> str:
    return "| " + " | ".join(cells) + " |"


def mutate_fields(raw: str, updates: dict[str, str], additions: list[tuple[str, str]] = ()) -> str:
    evidence_ids: list[str] = []
    fields: list[tuple[str, str]] = []
    seen_fields = False
    for piece in raw.split(";"):
        if not seen_fields and re.fullmatch(r"E\d{3}", piece.strip()):
            evidence_ids.append(piece.strip())
            continue
        seen_fields = True
        key, sep, value = piece.partition("=")
        if not sep or not key or any(old_key == key for old_key, _ in fields):
            raise SystemExit(f"S19 evidence field malformed or duplicated: {piece!r}")
        fields.append((key, value))
    index = {key: idx for idx, (key, _) in enumerate(fields)}
    for key, value in updates.items():
        if key not in index:
            raise SystemExit(f"Expected existing S19 field {key} is absent")
        fields[index[key]] = (key, value)
    for key, value in additions:
        if key in index:
            if fields[index[key]][1] != value:
                raise SystemExit(f"Conflicting pre-existing S19 field {key}")
            continue
        fields.append((key, value))
        index[key] = len(fields) - 1
    return ";".join([*evidence_ids, *(f"{key}={value}" for key, value in fields)])


def evidence_row(
    evidence_id: str,
    task_id: str,
    code_revision: str,
    environment: str,
    command: str,
    result: str,
    artifacts: str,
    checked: str,
) -> str:
    cells = [evidence_id, task_id, code_revision, environment, command, result, artifacts, checked]
    return render_row(cells)


if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip() != BASE_SHA:
    raise SystemExit("allocation base HEAD differs from verified T067 closing C1")
if subprocess.check_output(["git", "branch", "--show-current"], cwd=ROOT, text=True).strip() != "codex/t068-polyglot-matrix":
    raise SystemExit("allocation must run on the new T068 branch")
if (ROOT / SUPPORT / "selection.json").exists() or (ROOT / "artifacts/preflight/T068").exists():
    raise SystemExit("T068 evidence path exists; preserving it")

spec_text = SPEC_PATH.read_text(encoding="utf-8")
rows = task_rows(spec_text)
if _effective_status(rows["T067"]) != "VERIFY" or _effective_status(rows["T068"]) != "TODO":
    raise SystemExit("unexpected S19 task readiness state")
if rows["T068"]["Status"].strip() != "TODO" or rows["T068"]["Owner"].strip() != "UNASSIGNED":
    raise SystemExit("T068 is not unallocated in S19")

archive = verify_archive(ROOT, T067, spec_path=SPEC_PATH)
if archive.get("kind") != "archive_publication" or archive.get("archive_manifest_sha256") != "848af73a2fc79064aee131e35dc9b68aea7c0b8e39e6388fa4b309aa8a0f794c":
    raise SystemExit("T067 published archive cold-readback failed")
archive_identity = _verified_archive_identity(ROOT, T067, "T067")
if archive_identity.get("closing_sha") != T067_CLOSING:
    raise SystemExit("T067 archive closing SHA is not C1")
publication_ref = f"artifacts/git-delivery/operations/{ARCHIVE_OP}/publication.json"
publication_path = ROOT / publication_ref
publication = json.loads(publication_path.read_text(encoding="utf-8"))
if publication.get("archive_manifest_sha256") != archive["archive_manifest_sha256"] or publication.get("archive_seal_sha256") != archive["archive_seal_sha256"]:
    raise SystemExit("T067 publication projection differs from the cold-read archive")

live_ref = f"artifacts/git-live/{T067}/{LIVE_ID}/live-verification.json"
live_path = ROOT / live_ref
live = json.loads(live_path.read_text(encoding="utf-8"))
live_errors = _live_receipt_errors(live, spec_text=spec_text, expected_verification_id=LIVE_ID, root=ROOT)
if live_errors:
    raise SystemExit("T067 live receipt invalid: " + "; ".join(live_errors))
if live.get("task_id") != "T067" or live.get("subject_sha") != T067_CLOSING or live.get("observed_remote_sha") != T067_CLOSING:
    raise SystemExit("T067 live receipt subject/remote identity mismatch")

support = ROOT / SUPPORT
(support / "authority").mkdir(parents=True)
(support / "rationale").mkdir()
(support / "local-checks/allocation").mkdir(parents=True)
preflight_dir = ROOT / "artifacts/preflight/T068"
preflight_dir.mkdir(parents=True)
allocated_at = utc()
scope_paths = [
    "tests/acceptance/polyglot*",
    "tools/bench/**",
    "artifacts/preflight/T068/**",
    "Maps-Context-Engine-4D_SPEC.md",
]
assignment_ref = f"{SUPPORT}/authority/task-start.txt"
reason_ref = f"{SUPPORT}/rationale/selection-reason.json"
selection_ref = f"{SUPPORT}/selection.json"
mode_ref = f"{SUPPORT}/task-mode.json"
dependency_ref = f"{SUPPORT}/dependency-review.json"
plan_ref = f"{SUPPORT}/check-plan-canonical-r1.json"
allocation_ref = f"{SUPPORT}/allocation-r1.json"
fixture = {
    "format_version": 1,
    "kind": "polyglot_acceptance_plan",
    "task_id": TASK,
    "task_revision": REVISION,
    "delivery_id": DELIVERY,
    "source_mode": "synthetic fixtures only; no user repository was authorized for this benchmark",
    "oracle": "independently authored expected facts and acceptance assertions; engine output is never its own oracle",
    "cohort": {
        "minimum_tasks": 30,
        "minimum_repositories": 3,
        "repositories": ["alpha", "beta", "gamma"],
        "modes": ["baseline", "full", "incremental"],
        "starting_revision_per_task": "identical immutable synthetic fixture revision",
        "repeats": 3,
        "randomization_seed": 68055,
        "real_repository_result": "NOT_RUN",
    },
    "report_contract": {
        "required": [
            "fixture_revision",
            "synthetic_or_real",
            "mode",
            "task_count",
            "repo_count",
            "success_count",
            "failure_count",
            "critical_omissions",
            "false_edits",
            "recall_numerator",
            "recall_denominator",
            "context_proxy_bytes",
            "context_proxy_files",
            "context_proxy_reads",
            "token_usage_status",
            "input_tokens",
            "output_tokens",
            "median_and_distribution",
            "profile_and_tool_versions",
            "host_environment",
        ],
        "token_usage_status_without_host_receipt": "NOT_RUN",
        "token_savings_claim_without_host_receipt": "FORBIDDEN",
        "correctness_failure_may_not_be_offset_by_context_savings": True,
    },
    "cases": [
        {"id": "C055", "domain": "typescript", "assertion": "aliases, reexports, project references preserve supported binding; dynamic JS any remains unknown; malicious plugin is not executed"},
        {"id": "C056", "domain": "frontend-backend", "assertion": "same route path in different services is not linked without explicit source evidence"},
        {"id": "C057", "domain": "python", "assertion": "decorators and dynamic imports remain bounded; target is not imported or executed"},
        {"id": "C058", "domain": "python-notebook", "assertion": "cell, venv and config side effects do not execute; missing metadata stays a gap"},
        {"id": "C059", "domain": "sql", "assertion": "dialect-specific dynamic SQL lineage stays evidence-bound; deployment is not inferred"},
        {"id": "C060", "domain": "http-schema-proto", "assertion": "local refs are bounded; remote refs, cycles and mismatches do not trigger network or false handlers"},
        {"id": "C061", "domain": "events", "assertion": "same topic across broker/environment boundaries is not bridged; retry/ack/replay are preserved"},
        {"id": "C062", "domain": "state-machine", "assertion": "only approved scoped guards/actions are represented; stale sources invalidate and guards do not execute"},
        {"id": "C063", "domain": "federation", "assertion": "stale/denied/missing members remain partial with provenance and no namespace leakage"},
        {"id": "C064", "domain": "temporal-diff", "assertion": "rename/provider changes are not force-matched; missing snapshots remain explicit"},
        {"id": "C065", "domain": "trading", "assertion": "reserve/retry/partial-fill/cancel/ledger fixture yields obligations without money or finality inference"},
        {"id": "C066", "domain": "commerce", "assertion": "duplicate/out-of-order webhook and partial refund preserve distinct order/payment/stock states"},
        {"id": "C067", "domain": "ai-lineage", "assertion": "metadata drift is reported without loading models/datasets or calling an LLM"},
        {"id": "C068", "domain": "architecture", "assertion": "allowed and forbidden witnesses are exact; missing coverage cannot yield whole-system PASS"},
        {"id": "C069", "domain": "benchmark", "assertion": "baseline/full/incremental reports separate synthetic from real and include correct failure/recall denominators"},
    ],
}
fixture_sha = write_json(FIXTURE_REF, fixture)

authority = (
    "Task: T068 revision 1 — Polyglot multi-domain acceptance, regression, benchmark and capability matrix.\n"
    f"Delivery: {DELIVERY}\nBase: {BASE_SHA}\nPriority: P2; mode: implement; owner: Codex.\n"
    "Approved scope: tests/acceptance/polyglot* and tools/bench/**; allocation/evidence under this delivery; S19/S21/S22 task tracking.\n"
    "Requirements: R040,R056; cases: C055-C069; flows: DF14,DF15,DF19; sequence: S47 P11 after T067 and before T048.\n"
    "Dependencies: T063,T064,T065,T066,T067 effective DONE; T067 closure is finalized atomically in this allocation from its exact C1, passing CI, sealed archive and fresh live receipt.\n"
    "Acceptance: >=30 tasks across >=3 isolated synthetic repositories; independently authored oracle; baseline/full/incremental from same revision; three repeats with recorded seed; preserve all known correctness/security boundaries; no engine-output oracle.\n"
    "Reporting: success/failure, critical omissions, false edits, recall numerator/denominator, context-proxy bytes/files/reads, environment/tool versions, and token fields only from host receipts. Missing token telemetry remains NOT_RUN and cannot support token-savings claims. Synthetic results are not production real-repository acceptance.\n"
    "No external user repository, API key, LLM call, target application, production database or arbitrary configuration plugin execution.\n"
)
(ROOT / assignment_ref).write_text(authority, encoding="utf-8", newline="\n")
authority_sha = sha_file(ROOT / assignment_ref)

reason = {
    "format_version": 1,
    "kind": "task_selection_reason",
    "task_id": TASK,
    "task_revision": REVISION,
    "delivery_id": DELIVERY,
    "selected_at_utc": allocated_at,
    "sequence": "S47 P11 immediately after T067; T048 review follows T068",
    "decision": "Select T068 as the next sequential R2 task after T067 C1 passed exact-SHA main/branch CI, archive cold readback, and fresh live verification; T063-T066 are effective DONE. Only owned synthetic fixtures are available, so the rebenchmark is labelled synthetic and real-repository/token claims remain NOT_RUN.",
    "constraints": [
        "Run C055-C069 regressions against independent expected facts and preserve each provider/domain's explicit evidence gaps.",
        "Compare baseline/full/incremental modes from the same fixture revision; report failure and recall denominators, not only successful tasks.",
        "Keep real-repository and synthetic results separate; no user repository is authorized for this run.",
        "Do not call an LLM, access a token provider, or claim token savings without host-supplied usage receipts.",
        "Correctness and safety failures cannot be traded for context-size savings.",
    ],
    "scope_paths": scope_paths,
}
reason_sha = write_json(reason_ref, reason)
selection = {
    "format_version": 1,
    "task_id": TASK,
    "task_revision": REVISION,
    "active_delivery_id": DELIVERY,
    "allocated_at_utc": allocated_at,
    "updated_at_utc": allocated_at,
    "assignment_ref": assignment_ref,
    "reason_ref": reason_ref,
    "previous_selection_ref": f"{T067_SUPPORT}/selection.json",
    "supersedes_delivery_id": None,
    "invalidation_refs": [],
}
selection_sha = write_json(selection_ref, selection)

task_mode = {
    "format_version": 1,
    "kind": "task_mode",
    "task_id": TASK,
    "task_revision": REVISION,
    "delivery_id": DELIVERY,
    "mode": "implement",
    "owner": "Codex",
    "priority": "P2",
    "spec_scope_ref": "Maps-Context-Engine-4D_SPEC.md#S18,S24,S28-S30,S32,S43-S45,S47",
    "objective": "Tạo acceptance/regression và benchmark matrix polyglot đa domain theo independent oracle, cùng baseline/full/incremental từ fixture revision cố định; báo capability/gap và chỉ số đúng mẫu số.",
    "change_budget": {
        "allowed_paths": scope_paths,
        "behavior": [
            "Cover C055-C069 as a cross-domain cohort while keeping TypeScript/Python/contracts/events/state/federation/diff/trading/commerce/AI/architecture capabilities distinguishable.",
            "Use independently authored expectations; never treat the engine output as its own ground truth.",
            "Run synthetic baseline/full/incremental modes over at least 30 tasks and 3 isolated fixtures, with repeat and seed provenance.",
            "Report task success/failures, critical omissions, false edits, recall numerator/denominator, and context proxy metrics with per-mode denominators.",
            "Label token usage absent when no host receipt is supplied; keep production real-repository acceptance NOT_RUN.",
        ],
        "exclusions": [
            "No user/production repository access, LLM/API token calls, production DB, runtime target, or plugin execution.",
            "No safety-boundary relaxation to improve benchmark ratios.",
            "No claim that synthetic fixtures prove all polyglot repositories or production token savings.",
        ],
    },
    "verification_plan": [
        "Add independently specified acceptance expectations for C055-C069 and test cross-domain capability/gap reporting.",
        "Run baseline/full/incremental cohort on the same frozen fixtures; >=30 tasks, >=3 fixtures, recorded seed/repeats.",
        "Verify correctness counters and denominators, synthetic/real labels, and token NOT_RUN behavior without host usage receipts.",
        "Run gofmt, relevant acceptance and benchmark package tests, go test ./..., go vet ./..., go build ./..., SPEC and delivery-policy checks.",
    ],
}
mode_sha = write_json(mode_ref, task_mode)

old_rows = rows
dependencies: list[dict[str, Any]] = []
for dependency in ("T063", "T064", "T065", "T066", "T067"):
    row = old_rows[dependency]
    evidence = _evidence_fields(row["Evidence"].strip())
    if dependency == "T067":
        closing_sha = T067_CLOSING
        pub_ref = publication_ref
        live_dep_ref = live_ref
        live_dep_id = LIVE_ID
        effective = "DONE"  # exact closure proof is finalized in this allocation commit
    else:
        if _effective_status(row) != "DONE":
            raise SystemExit(f"T068 prerequisite {dependency} is not effective DONE")
        closing_sha = evidence.get("closing_sha")
        pub_ref = evidence.get("archive_publication_ref")
        live_dep_ref = evidence.get("live_verification_ref")
        live_dep_id = evidence.get("live_verification_id")
        effective = _effective_status(row)
    if not closing_sha or not pub_ref or not live_dep_ref or not live_dep_id:
        raise SystemExit(f"T068 prerequisite {dependency} lacks closing/archive/live evidence")
    dependencies.append({
        "task_id": dependency,
        "effective_status": effective,
        "closing_sha": closing_sha,
        "archive_publication_ref": pub_ref,
        "live_verification_ref": live_dep_ref,
        "live_verification_id": live_dep_id,
    })

dependency_review = {
    "format_version": 1,
    "kind": "task_dependency_review",
    "task_id": TASK,
    "delivery_id": DELIVERY,
    "result": "READY",
    "contracts_reviewed": ["S18 benchmark fairness/correctness", "S24 R2 scope", "S32 R2 release matrix", "S47 card 091 / T068", "S53 AW-08 task delivery"],
    "dependencies": dependencies[:4] + [
        {
            **dependencies[4],
            "evidence_refs": [
                publication_ref,
                live_ref,
                f"{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json",
                f"{T067_SUPPORT}/local-checks/closing-C1-exact/summary.json",
            ],
        }
    ],
    "sequence": {
        **dependencies[4],
        "task_revision": "1",
        "archive_publication_sha256": sha_file(publication_path),
        "live_verification_sha256": sha_file(live_path),
    },
    "source_review": {
        "requirements": ["R040", "R056"],
        "acceptance_cases": [f"C{i:03}" for i in range(55, 70)],
        "flows": ["DF14", "DF15", "DF19"],
        "task_card": "S47 card 091 / T068",
        "scope_paths": scope_paths,
    },
    "limitations": [
        "Current suite uses synthetic isolated fixtures; this is not real-repository production acceptance.",
        "No host token-usage receipt exists; actual token totals/savings remain NOT_RUN.",
        "No benchmark score can override a correctness, omission, safety or failure-rate regression.",
    ],
}
dependency_sha = write_json(dependency_ref, dependency_review)

plan_template = json.loads((ROOT / f"{T067_SUPPORT}/check-plan-canonical-r1.json").read_text(encoding="utf-8"))
plan_template["canary_ref"] = f"delivery:{DELIVERY}:payload"
plan_template["repository"]["publication_authority_ref"] = assignment_ref
plan_template["local_check_refs"] = [
    "local-checks/payload/records/bootstrap.json",
    "local-checks/payload/records/bootstrap-fixtures.json",
    "local-checks/payload/records/delivery-plan.json",
    "local-checks/payload/records/spec-check.json",
    "local-checks/payload/records/spec-check-fixtures.json",
    "local-checks/payload/records/delivery-policy-fixtures.json",
    "local-checks/payload/records/checkpoint-digest-fixtures.json",
    "local-checks/payload/records/go-format.json",
    "local-checks/payload/records/go-test.json",
    "local-checks/payload/records/go-vet.json",
    "local-checks/payload/records/go-build.json",
    "local-checks/payload/records/explorer-install.json",
    "local-checks/payload/records/explorer-contract.json",
]
plan_sha = write_json(plan_ref, plan_template, canonical=True)

allocation = {
    "format_version": 1,
    "kind": "git_task_allocation",
    "task_id": TASK,
    "task_revision": REVISION,
    "delivery_id": DELIVERY,
    "mode": "implement",
    "owner": "Codex",
    "priority": "P2",
    "allocated_at_utc": allocated_at,
    "base_sha": BASE_SHA,
    "selection_ref": selection_ref,
    "task_mode_ref": mode_ref,
    "dependency_review_ref": dependency_ref,
    "dependency_review_sha256": dependency_sha,
    "assignment_ref": assignment_ref,
    "check_plan_ref": plan_ref,
    "check_plan_sha256": plan_sha,
    "fixture_plan_ref": FIXTURE_REF,
    "fixture_plan_sha256": fixture_sha,
    "scope_paths": [*scope_paths, f"{SUPPORT}/**"],
    "sequence_predecessor": {
        "task_id": "T067",
        "effective_status": "DONE",
        "closing_sha": T067_CLOSING,
        "archive_publication_ref": publication_ref,
        "archive_publication_sha256": sha_file(publication_path),
        "live_verification_ref": live_ref,
        "live_verification_id": LIVE_ID,
    },
}
allocation_sha = write_json(allocation_ref, allocation)

commit_message = (
    "docs(progress): [P2][T068] Phân bổ benchmark polyglot sau T067\n\n"
    "Mục đích: Mở T068 sau khi T067 đạt exact-SHA CI, archive readback và live PASS.\n"
    "Thay đổi: Khóa scope C055-C069, cohort synthetic độc lập và báo cáo baseline/full/incremental.\n"
    "Kiểm tra: Allocation preflight 6/6; SPEC checker/fixtures và delivery policy PASS.\n\n"
    f"MCE4D-Delivery-ID: {DELIVERY}\n"
    "MCE4D-Delivery-Phase: allocation\n"
)
(support / "allocation-commit-message.txt").write_text(commit_message, encoding="utf-8", newline="\n")

closing_local_ref = f"{T067_SUPPORT}/local-checks/closing-C1-exact/summary.json"
closing_ci_ref = f"{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json"
ci_main_run = "39742410812"
ci_branch_run = "39742427676"
ci_pair = json.loads((ROOT / closing_ci_ref).read_text(encoding="utf-8"))
if ci_pair.get("subject_sha") != T067_CLOSING or ci_pair.get("result") != "PASS":
    raise SystemExit("T067 closing exact-SHA CI pair is not PASS")
closing_local = json.loads((ROOT / closing_local_ref).read_text(encoding="utf-8"))
if closing_local.get("result") != "PASS" or closing_local.get("passed_count") != 13:
    raise SystemExit("T067 exact C1 local checks are not 13/13 PASS")

source_graph_ref = f"{T067_SUPPORT}/archive-view/ci/closing-C1-sourcegraph-retry1/source-graph.json"
source_collector_ref = f"{T067_SUPPORT}/archive-view/ci/closing-C1-sourcegraph-retry1/collector-summary.json"
branch_push_ref = f"{T067_SUPPORT}/archive-view/operations/closing-C1-branch-push/receipt.json"
main_push_ref = f"{T067_SUPPORT}/archive-view/operations/closing-C1-main-push/push-operation.json"
branch_push = json.loads((ROOT / branch_push_ref).read_text(encoding="utf-8"))
main_push = json.loads((ROOT / main_push_ref).read_text(encoding="utf-8"))
for name, push in (("branch", branch_push), ("main", main_push)):
    if push.get("push_exit_code") != 0 or push.get("postflight_sha") != T067_CLOSING:
        raise SystemExit(f"T067 {name} push receipt does not prove C1")

# The publisher reports unsupported directory fsync on this Windows filesystem.
# Re-measure the same repository helper now and retain the exact observed result.
directory_fsync_supported = _fsync_directory(ROOT / "artifacts/git-delivery")
directory_probe = {
    "format_version": 1,
    "kind": "directory_fsync_capability_probe",
    "observed_at_utc": utc(),
    "path": "artifacts/git-delivery",
    "method": "tools.spec_check.archive._fsync_directory",
    "supported": bool(directory_fsync_supported),
    "result": "supported" if directory_fsync_supported else "unsupported",
    "scope": "filesystem capability observation; does not claim power-loss durability",
}
probe_ref = f"{SUPPORT}/archive-view/directory-fsync-probe.json"
probe_sha = write_json(probe_ref, directory_probe)

row_updates = {
    "closing_sha": T067_CLOSING,
    "closing_CI": f"PASS(6/6:{ci_main_run},branch=6/6:{ci_branch_run})",
    "archive/live": f"PASS(sealed-readback,fresh-live:{LIVE_ID})",
    "closing_local": "PASS(13/13)",
    "candidate": "DONE",
    "effective": "DONE",
}
row_additions = [
    ("closing_local_summary_ref", closing_local_ref),
    ("closing_local_summary_sha256", sha_file(ROOT / closing_local_ref)),
    ("closing_CI_run_pair_ref", closing_ci_ref),
    ("closing_CI_run_pair_sha256", sha_file(ROOT / closing_ci_ref)),
    ("closing_CI_collection_ref", source_collector_ref),
    ("closing_CI_collection_sha256", sha_file(ROOT / source_collector_ref)),
    ("closing_CI_source_graph_ref", source_graph_ref),
    ("closing_CI_source_graph_sha256", sha_file(ROOT / source_graph_ref)),
    ("closing_main_push_ref", main_push_ref),
    ("closing_main_push_sha256", sha_file(ROOT / main_push_ref)),
    ("closing_branch_push_ref", branch_push_ref),
    ("closing_branch_push_sha256", sha_file(ROOT / branch_push_ref)),
    ("archive_ref", f"artifacts/git-delivery/{T067}"),
    ("archive_manifest_sha256", archive["archive_manifest_sha256"]),
    ("archive_seal_sha256", archive["archive_seal_sha256"]),
    ("archive_publication_ref", publication_ref),
    ("archive_publication_sha256", sha_file(publication_path)),
    ("archive_readback_at_utc", archive.get("readback_completed_at_utc") or publication.get("readback_completed_at_utc", "")),
    ("live_verification_ref", live_ref),
    ("live_verification_id", LIVE_ID),
    ("live_verification_sha256", sha_file(live_path)),
    ("static_verdict", "UNKNOWN"),
    ("directory_fsync", "supported" if directory_fsync_supported else "unsupported"),
    ("directory_fsync_probe_ref", probe_ref),
    ("directory_fsync_probe_sha256", probe_sha),
    ("observed_remote_sha", T067_CLOSING),
    ("last_closed_at_utc", live["completed_at_utc"]),
    ("archive", f"PASS(sealed-readback:{archive['archive_manifest_sha256']}+{archive['archive_seal_sha256']})"),
    ("live", f"PASS({LIVE_ID})"),
    ("closure_result", "PASS"),
]

lines = spec_text.splitlines()
changed = False
for idx, line in enumerate(lines):
    if line.startswith("| T067 |"):
        cells = row_cells(line)
        cells[8] = mutate_fields(cells[8], row_updates, row_additions)
        lines[idx] = render_row(cells)
        changed = True
        break
if not changed:
    raise SystemExit("T067 S19 row not found")

allocation_fields = [
    ("task_revision", str(REVISION)),
    ("delivery", DELIVERY),
    ("allocation_phase", "allocation-preflight"),
    ("base_sha", BASE_SHA),
    ("plan_sha256", plan_sha),
    ("priority", "P2"),
    ("task_mode", "implement"),
    ("dependencies", "DONE(T063,T064,T065,T066,T067)"),
    ("sequence_predecessor", "DONE(T067)"),
    ("assignment_ref", assignment_ref),
    ("assignment_sha256", authority_sha),
    ("selection_reason_ref", reason_ref),
    ("selection_reason_sha256", reason_sha),
    ("selector_ref", selection_ref),
    ("selector_sha256", selection_sha),
    ("task_mode_ref", mode_ref),
    ("task_mode_sha256", mode_sha),
    ("dependency_review_ref", dependency_ref),
    ("dependency_review_sha256", dependency_sha),
    ("allocation_ref", allocation_ref),
    ("allocation_sha256", allocation_sha),
    ("fixture_plan_ref", FIXTURE_REF),
    ("fixture_plan_sha256", fixture_sha),
    ("candidate", "VERIFY"),
    ("effective", "VERIFY"),
    ("allocation_check", "PENDING"),
    ("allocation_local", "PENDING"),
    ("allocation_check_ref", "PENDING"),
    ("allocation_check_sha256", "PENDING"),
    ("allocation_preflight_ref", "PENDING"),
    ("allocation_preflight_sha256", "PENDING"),
    ("local_check_summary_ref", "PENDING"),
    ("local_check_summary_sha256", "PENDING"),
    ("allocation_commit_sha", "PENDING"),
    ("allocation_CI", "PENDING"),
    ("allocation_branch_CI", "PENDING"),
    ("result", "PENDING"),
    ("closing_sha", "PENDING"),
    ("closing_CI", "PENDING"),
    ("archive/live", "PENDING"),
    ("closing_local", "PENDING"),
]
for idx, line in enumerate(lines):
    if line.startswith("| T068 |"):
        cells = row_cells(line)
        cells[6] = "IN_PROGRESS"
        cells[7] = "Codex"
        cells[8] = ";".join(["E742", *(f"{key}={value}" for key, value in allocation_fields)])
        lines[idx] = render_row(cells)
        break
else:
    raise SystemExit("T068 S19 row not found")

spec_after = "\n".join(lines) + "\n"
if spec_after == spec_text:
    raise SystemExit("SPEC content did not change")
if "| E741 | T067 |" in spec_after or "| E742 | T068 |" in spec_after:
    raise SystemExit("S21 allocation evidence already exists")
e741 = evidence_row(
    "E741", "T067", T067_CLOSING,
    "Windows 11 x64; GitHub Actions Ubuntu/Windows; repository ID 1421031011",
    "C1 exact local 13/13; main/branch CI 6/6; archive cold readback; fresh live verification ba0cc8e3",
    "PASS — T067 đạt effective DONE sau archive seal/readback và live receipt khớp remote C1; directory fsync probe unsupported.",
    f"{publication_ref} sha256:{sha_file(publication_path)}; {live_ref} sha256:{sha_file(live_path)}; {probe_ref} sha256:{probe_sha}; {closing_ci_ref} sha256:{sha_file(ROOT / closing_ci_ref)}",
    live["completed_at_utc"],
)
e742 = evidence_row(
    "E742", "T068", BASE_SHA,
    "Windows 11 x64; Python 3.12; Go 1.24; GitHub Actions Ubuntu/Windows; repository ID 1421031011",
    "Allocation preflight; R040/R056 readiness; C055-C069 fixture plan; SPEC and delivery-policy checks",
    "PASS — T068 được phân bổ theo P11 sau khi T063-T067 effective DONE; benchmark synthetic-only, token usage NOT_RUN nếu thiếu host receipt.",
    f"{allocation_ref} sha256:{allocation_sha}; {FIXTURE_REF} sha256:{fixture_sha}; {dependency_ref} sha256:{dependency_sha}; {plan_ref} sha256:{plan_sha}",
    allocated_at,
)
for index, line in enumerate(lines):
    if line.startswith("## S22 —"):
        lines.insert(index, e741)
        lines.insert(index + 1, e742)
        break
else:
    raise SystemExit("S22 boundary missing while appending S21 evidence")
spec_after = "\n".join(lines) + "\n"

s22_start = spec_after.index("## S22 — Trạng thái tiếp tục cho phiên AI sau")
s23_start = spec_after.index("\n## S23 —", s22_start)
s22 = spec_after[s22_start:s23_start]
top_updates = {
    "next_task": "T068",
    "active_task": "T068",
    "completed_scope_tasks": "72",
    "last_evidence_id": '"E742"',
    "last_checked_at_utc": f'"{allocated_at}"',
    "todo_r2_tasks": "4",
    "assignment_start_status": "IMPLEMENTATION_STARTED",
    "final_engineering_status": "IN_PROGRESS",
}
for key, value in top_updates.items():
    pattern = re.compile(rf"(?m)^(  {re.escape(key)}:)\s*.*$")
    s22, count = pattern.subn(rf"\1 {value}", s22, count=1)
    if count != 1:
        raise SystemExit(f"S22 top-level field missing/ambiguous: {key}")

delivery_updates = {
    "local_branch": '"codex/t068-polyglot-matrix"',
    "local_head_sha": f'"{BASE_SHA}"',
    "allocation_commit_sha": '"PENDING"',
    "last_verified_payload_sha": f'"{rows["T067"]["Evidence"].split("payload_sha=", 1)[1].split(";", 1)[0]}"',
    "closing_commit_ref": '"PENDING"',
    "pending_delivery_id": f'"{DELIVERY}"',
    "active_delivery_id": f'"{DELIVERY}"',
    "task_revision": f'"{REVISION}"',
    "planned_archive_path": f'"artifacts/git-delivery/{DELIVERY}"',
    "closing_parent_sha": f'"{BASE_SHA}"',
    "durable_archive_ref": f'"artifacts/git-delivery/{T067}"',
    "observed_remote_sha": f'"{T067_CLOSING}"',
    "expected_check_plan_ref": f'"{plan_ref}"',
    "last_ci_result": '"T067 closing C1 exact-SHA CI PASS 6/6 on main (39742410812) and task branch (39742427676); sealed archive/readback and fresh live verification ba0cc8e3 PASS."',
    "previous_closed_task_live_ref": f'"{live_ref}"',
    "last_live_verification_ref": f'"{live_ref}"',
    "last_live_verification_id": f'"{LIVE_ID}"',
    "previous_revision_ci_collection_ref": f'"{source_collector_ref}"',
    "last_ci_collection_ref": f'"{closing_ci_ref}"',
    "pending_delivery_task": '"T068"',
    "source_gap_review_ref": f'"{reason_ref}"',
    "last_delivery_evidence_ref": f'"{live_ref}"',
}
for key, value in delivery_updates.items():
    pattern = re.compile(rf"(?m)^(    {re.escape(key)}:)\s*.*$")
    s22, count = pattern.subn(rf"\1 " + value, s22, count=1)
    if count != 1:
        raise SystemExit(f"S22 git_delivery field missing/ambiguous: {key}")

spec_after = spec_after[:s22_start] + s22 + spec_after[s23_start:]
SPEC_PATH.write_text(spec_after, encoding="utf-8", newline="\n")

print(json.dumps({
    "result": "PREPARED",
    "task_id": TASK,
    "delivery_id": DELIVERY,
    "base_sha": BASE_SHA,
    "fixture_plan_sha256": fixture_sha,
    "check_plan_sha256": plan_sha,
    "allocation_sha256": allocation_sha,
    "dependency_review_sha256": dependency_sha,
    "live_verification_sha256": sha_file(live_path),
    "directory_fsync": directory_probe["result"],
    "spec_bytes": len(spec_after.encode("utf-8")),
}, ensure_ascii=True, sort_keys=True))
