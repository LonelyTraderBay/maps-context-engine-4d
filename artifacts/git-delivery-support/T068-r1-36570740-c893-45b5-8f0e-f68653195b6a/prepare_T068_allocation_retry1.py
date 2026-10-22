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
TASK, REVISION = "T068", 1
DELIVERY = "36570740-c893-45b5-8f0e-f68653195b6a"
BASE_SHA = "12547b660521fecb9592ae40cd786a13c25f63bb"
T067 = "b0e123c8-1a67-47f3-9360-9f8b6ec57bd4"
T067_CLOSING = BASE_SHA
LIVE_ID = "ba0cc8e3-9651-47e8-a836-64dc8076ea04"
ARCHIVE_OP = "a8d725e2-97a0-400d-a1fc-9f97ddfe7c91"
T065 = "79b3e5bf-99d3-4e62-a0e6-86098b786d92"
T065_LIVE = "e8a07fe2-0414-4143-bdb6-1c4f138f5615"
SUPPORT = f"artifacts/git-delivery-support/T068-r1-{DELIVERY}"
RETRY = f"{SUPPORT}/allocation-retry1"
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
    data = _canonical_json(value) if canonical else (
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    ).encode("utf-8")
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()
    return sha_bytes(data)


def row_cells(line: str) -> list[str]:
    return [cell.strip() for cell in line.strip()[1:-1].split("|")]


def render_row(cells: list[str]) -> str:
    return "| " + " | ".join(cells) + " |"


def mutate_fields(raw: str, updates: dict[str, str], additions: list[tuple[str, str]] = ()) -> str:
    ids: list[str] = []
    fields: list[tuple[str, str]] = []
    started = False
    for part in raw.split(";"):
        if not started and re.fullmatch(r"E\d{3}", part.strip()):
            ids.append(part.strip())
            continue
        started = True
        key, sep, value = part.partition("=")
        if not sep or not key or any(old == key for old, _ in fields):
            raise SystemExit(f"malformed/duplicate S19 field {part!r}")
        fields.append((key, value))
    index = {key: i for i, (key, _) in enumerate(fields)}
    for key, value in updates.items():
        if key not in index:
            raise SystemExit(f"missing S19 field {key}")
        fields[index[key]] = (key, value)
    for key, value in additions:
        if key in index:
            if fields[index[key]][1] != value:
                raise SystemExit(f"conflicting S19 field {key}")
        else:
            fields.append((key, value))
            index[key] = len(fields) - 1
    return ";".join([*ids, *(f"{key}={value}" for key, value in fields)])


def add_id(raw: str, evidence_id: str) -> str:
    parts = raw.split(";")
    ids: list[str] = []
    split_at = 0
    for i, part in enumerate(parts):
        if not re.fullmatch(r"E\d{3}", part.strip()):
            split_at = i
            break
        ids.append(part.strip())
    else:
        split_at = len(parts)
    if evidence_id not in ids:
        ids.append(evidence_id)
    return ";".join([*ids, *parts[split_at:]])


def s19_rows(text: str) -> dict[str, dict[str, str]]:
    section = _section(text, "## S19 — Bảng triển khai duy nhất") or ""
    table = _table_with_headers(section, "Task", "Gate", "Status", "Evidence")
    if table is None:
        raise SystemExit("S19 table is unreadable")
    return {r["Task"].strip(): r for r in table.rows}


def evidence_row(eid: str, task: str, revision: str, environment: str,
                 command: str, result: str, artifacts: str, checked: str) -> str:
    return render_row([eid, task, revision, environment, command, result, artifacts, checked])


if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip() != BASE_SHA:
    raise SystemExit("unexpected T067 C1 base")
if subprocess.check_output(["git", "branch", "--show-current"], cwd=ROOT, text=True).strip() != "codex/t068-polyglot-matrix":
    raise SystemExit("unexpected T068 branch")
retry_root = ROOT / RETRY
if retry_root.exists():
    raise SystemExit("allocation retry1 already exists; preserving it")
for rel in ("selection.json", "task-mode.json", "rationale/selection-reason.json", "authority/task-start.txt"):
    if not (ROOT / SUPPORT / rel).is_file():
        raise SystemExit(f"allocation attempt1 partial evidence missing: {rel}")
if not (ROOT / FIXTURE_REF).is_file():
    raise SystemExit("preserving fixture plan from allocation preparation attempt1 is required")

spec_text = SPEC_PATH.read_text(encoding="utf-8")
rows = s19_rows(spec_text)
if _effective_status(rows["T067"]) != "VERIFY" or _effective_status(rows["T068"]) != "TODO":
    raise SystemExit("unexpected current T067/T068 readiness")

# Validate T065's existing sealed evidence before repairing only its derived state fields.
t065_evidence = _evidence_fields(rows["T065"]["Evidence"].strip())
t065_archive = verify_archive(ROOT, T065, spec_path=SPEC_PATH)
t065_identity = _verified_archive_identity(ROOT, T065, "T065")
t065_live_ref = t065_evidence.get("live_verification_ref", "")
t065_live_id = t065_evidence.get("live_verification_id", "")
t065_live_path = ROOT / Path(*t065_live_ref.split("/"))
t065_live = json.loads(t065_live_path.read_text(encoding="utf-8"))
t065_live_errors = _live_receipt_errors(
    t065_live, spec_text=spec_text, expected_verification_id=T065_LIVE, root=ROOT
)
if (t065_archive.get("kind") != "archive_publication"
        or t065_archive.get("archive_manifest_sha256") != t065_evidence.get("archive_manifest_sha256")
        or t065_archive.get("archive_seal_sha256") != t065_evidence.get("archive_seal_sha256")
        or t065_identity.get("closing_sha") != t065_evidence.get("closing_sha")
        or t065_live_errors or t065_live_id != T065_LIVE
        or t065_live.get("subject_sha") != t065_evidence.get("closing_sha")
        or t065_live.get("observed_remote_sha") != t065_evidence.get("closing_sha")):
    raise SystemExit("T065 archive/live receipts do not support the claimed DONE state")

allocated_at = utc()
retry_root.mkdir(parents=True)
(retry_root / "authority").mkdir()
(retry_root / "rationale").mkdir()
scope_paths = [
    "tests/acceptance/polyglot*",
    "tools/bench/**",
    "artifacts/preflight/T068/**",
    "Maps-Context-Engine-4D_SPEC.md",
]
assignment_ref = f"{RETRY}/authority/task-start.txt"
reason_ref = f"{RETRY}/rationale/selection-reason.json"
selection_ref = f"{RETRY}/selection.json"
mode_ref = f"{RETRY}/task-mode.json"
dependency_ref = f"{RETRY}/dependency-review.json"
plan_ref = f"{RETRY}/check-plan-canonical-r1.json"
allocation_ref = f"{RETRY}/allocation.json"

archive = verify_archive(ROOT, T067, spec_path=SPEC_PATH)
archive_identity = _verified_archive_identity(ROOT, T067, "T067")
publication_ref = f"artifacts/git-delivery/operations/{ARCHIVE_OP}/publication.json"
publication_path = ROOT / publication_ref
publication = json.loads(publication_path.read_text(encoding="utf-8"))
live_ref = f"artifacts/git-live/{T067}/{LIVE_ID}/live-verification.json"
live_path = ROOT / live_ref
live = json.loads(live_path.read_text(encoding="utf-8"))
live_errors = _live_receipt_errors(live, spec_text=spec_text, expected_verification_id=LIVE_ID, root=ROOT)
if (archive_identity.get("closing_sha") != T067_CLOSING or live_errors
        or live.get("observed_remote_sha") != T067_CLOSING
        or publication.get("archive_manifest_sha256") != archive.get("archive_manifest_sha256")
        or publication.get("archive_seal_sha256") != archive.get("archive_seal_sha256")):
    raise SystemExit("T067 closure evidence failed verification")

fixture = json.loads((ROOT / FIXTURE_REF).read_text(encoding="utf-8"))
fixture_sha = sha_file(ROOT / FIXTURE_REF)
if fixture.get("kind") != "polyglot_acceptance_plan" or [item.get("id") for item in fixture.get("cases", [])] != [f"C{i:03}" for i in range(55, 70)]:
    raise SystemExit("T068 case plan is not the exact C055-C069 set")

authority = (
    "Task: T068 revision 1 — Polyglot multi-domain acceptance, regression, benchmark and capability matrix.\n"
    f"Delivery: {DELIVERY}\nBase: {BASE_SHA}\nPriority: P2; mode: implement; owner: Codex.\n"
    "Approved scope: tests/acceptance/polyglot* and tools/bench/**; allocation/evidence under this delivery; S19/S21/S22 task tracking.\n"
    "Requirements: R040,R056; cases: C055-C069; flows: DF14,DF15,DF19; sequence: S47 P11 after T067 and before T048.\n"
    "Dependencies: T063,T064,T065,T066,T067 effective DONE. T065 tracker metadata is normalized from its independently verified sealed archive and matching live receipt; no archived payload bytes are changed.\n"
    "Acceptance: >=30 tasks across >=3 isolated synthetic repositories; independently authored oracle; baseline/full/incremental from same revision; three repeats with recorded seed; preserve all known correctness/security boundaries.\n"
    "Reporting: success/failure, critical omissions, false edits, recall numerator/denominator, context-proxy bytes/files/reads, environment/tool versions, and token fields only from host receipts. Missing token telemetry remains NOT_RUN. Synthetic results are not production acceptance.\n"
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
    "decision": "Select T068 as the next sequential R2 task after T067 C1 passed exact-SHA CI, archive readback and fresh live verification; T063-T066 and T067 closure are verified. T065 carried effective=DONE but omitted split derived archive/live fields, so its already-sealed receipts are verified and normalized without modifying the archive. Only owned synthetic fixtures are available; real-repository and token claims remain NOT_RUN.",
    "constraints": [
        "Run C055-C069 regressions against independent expected facts and preserve domain-specific evidence gaps.",
        "Compare baseline/full/incremental from identical fixture revisions; report failure and recall denominators.",
        "Keep real and synthetic repository results separate; no real user repository is authorized.",
        "No LLM/token-provider call or token-savings claim without a host-supplied usage receipt.",
        "Correctness and safety failures cannot be traded for context-size savings.",
        "T065 normalized state fields are derived only from its existing sealed manifest, seal and verified live receipt; historical bytes remain intact.",
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
mode = {
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
            "Cover C055-C069 while keeping each provider/domain capability distinguishable.",
            "Use independently authored expectations; engine output is never its own ground truth.",
            "Run synthetic baseline/full/incremental modes over >=30 tasks and >=3 isolated fixtures with recorded repeats and seed.",
            "Report successes, failures, critical omissions, false edits, recall numerator/denominator and context-proxy metrics.",
            "Token usage absent without a host receipt and production real-repository acceptance remains NOT_RUN.",
        ],
        "exclusions": [
            "No user/production repository, LLM/API, production DB, target runtime, or plugin execution.",
            "No safety-boundary relaxation to improve benchmark ratios.",
            "No claim that synthetic fixtures prove every polyglot repository or real token savings.",
        ],
    },
    "verification_plan": [
        "Add independent acceptance expectations for C055-C069 and capability/gap assertions.",
        "Run baseline/full/incremental cohort on the same frozen fixtures; >=30 tasks, >=3 fixtures, recorded seed/repeats.",
        "Verify correct metric denominators, synthetic/real labels and token NOT_RUN without host usage receipts.",
        "Run gofmt, focused acceptance/benchmark tests, go test ./..., go vet ./..., go build ./..., SPEC and delivery-policy checks.",
    ],
}
mode_sha = write_json(mode_ref, mode)

# Normalize only the derived fields from T065's existing verified evidence, then prove it computes DONE.
expected_t065_archive = f"PASS(sealed-readback:{t065_archive['archive_manifest_sha256']}+{t065_archive['archive_seal_sha256']})"
expected_t065_live = f"PASS({T065_LIVE})"
rows = s19_rows(spec_text)
for idx, line in enumerate(spec_text.splitlines()):
    if line.startswith("| T065 |"):
        cells = row_cells(line)
        cells[8] = add_id(cells[8], "E741")
        cells[8] = mutate_fields(cells[8], {}, [("archive", expected_t065_archive), ("live", expected_t065_live)])
        t065_line = render_row(cells)
        break
else:
    raise SystemExit("T065 row not found")
lines = spec_text.splitlines()
for i, line in enumerate(lines):
    if line.startswith("| T065 |"):
        lines[i] = t065_line
        break
spec_text = "\n".join(lines) + "\n"
rows = s19_rows(spec_text)
if _effective_status(rows["T065"]) != "DONE":
    raise SystemExit("T065 remains not effective DONE after verified metadata normalization")

dependencies: list[dict[str, Any]] = []
for dep_id in ("T063", "T064", "T065", "T066", "T067"):
    row = rows[dep_id]
    evidence = _evidence_fields(row["Evidence"].strip())
    effective = _effective_status(row)
    if dep_id == "T067" and effective == "VERIFY":
        # Close T067 atomically below, after its archived/live receipts were just revalidated.
        effective = "DONE"
    if effective != "DONE":
        raise SystemExit(f"T068 prerequisite {dep_id} is not effective DONE")
    if dep_id == "T067":
        dep_closing_sha = T067_CLOSING
        dep_delivery = T067
        dep_revision = "1"
        dep_archive_ref = publication_ref
        dep_live_ref = live_ref
    else:
        dep_closing_sha = evidence.get("closing_sha")
        dep_delivery = evidence.get("delivery")
        dep_revision = evidence.get("task_revision", "1")
        dep_archive_ref = evidence.get("archive_publication_ref")
        dep_live_ref = evidence.get("live_verification_ref")
    dependencies.append({
        "task_id": dep_id,
        "task_revision": dep_revision,
        "delivery_id": dep_delivery,
        "effective_status": effective,
        "closing_sha": dep_closing_sha,
        "evidence_refs": [
            dep_archive_ref,
            dep_live_ref,
        ],
    })

archive_identity = _verified_archive_identity(ROOT, T067, "T067")
dep_review = {
    "format_version": 1,
    "kind": "task_dependency_review",
    "task_id": TASK,
    "delivery_id": DELIVERY,
    "result": "READY",
    "contracts_reviewed": ["S18 benchmark fairness/correctness", "S24 R2 scope", "S32 R2 release matrix", "S47 card 091 / T068", "S53 AW-08 task delivery"],
    "dependencies": dependencies,
    "sequence": {
        "task_id": "T067",
        "task_revision": "1",
        "delivery_id": T067,
        "effective_status": "DONE",
        "closing_sha": T067_CLOSING,
        "archive_publication_ref": publication_ref,
        "archive_publication_sha256": sha_file(publication_path),
        "live_verification_ref": live_ref,
        "live_verification_id": LIVE_ID,
        "live_verification_sha256": sha_file(live_path),
    },
    "source_review": {
        "requirements": ["R040", "R056"],
        "acceptance_cases": [f"C{i:03}" for i in range(55, 70)],
        "flows": ["DF14", "DF15", "DF19"],
        "task_card": "S47 card 091 / T068",
        "scope_paths": scope_paths,
    },
    "normalization_note": {
        "task_id": "T065",
        "archive_manifest_sha256": t065_identity["manifest_sha256"],
        "archive_seal_sha256": t065_identity["seal_sha256"],
        "live_verification_id": T065_LIVE,
        "added_derived_fields": {"archive": expected_t065_archive, "live": expected_t065_live},
        "archived_bytes_modified": False,
        "reason": "S19 recorded effective=DONE and archive/live PASS but omitted the separate archive/live fields consumed by the current effective-status evaluator.",
    },
    "limitations": [
        "Current cohort is synthetic fixture evidence, not production real-repository acceptance.",
        "No host token usage receipt is available; token totals and savings remain NOT_RUN.",
        "Correctness or safety failures cannot be offset by context-size improvements.",
    ],
}
dep_sha = write_json(dependency_ref, dep_review)

plan = json.loads((ROOT / f"{T067_SUPPORT}/check-plan-canonical-r1.json").read_text(encoding="utf-8"))
plan["canary_ref"] = f"delivery:{DELIVERY}:payload"
plan["repository"]["publication_authority_ref"] = assignment_ref
plan_sha = write_json(plan_ref, plan, canonical=True)

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
    "dependency_review_sha256": dep_sha,
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

e741_ref = f"{SUPPORT}/archive-view/history/allocation-prepare-attempt1.json"
e741_sha = sha_file(ROOT / e741_ref)
t065_identity = _verified_archive_identity(ROOT, T065, "T065")
t065_archive_ref = t065_evidence["archive_publication_ref"]
t065_live_hash = sha_file(t065_live_path)
e741 = evidence_row(
    "E741", "T065", t065_identity["closing_sha"], "Windows 11 x64; repository ID 1421031011",
    "Cold verify existing T065 archive and live receipt; reconcile effective-status derived leaves only",
    "PASS — manifest/seal/live subject khớp S19; thêm archive=PASS và live=PASS để evaluator hiện hành suy effective DONE; không sửa archive.",
    f"{t065_archive_ref} sha256:{sha_file(ROOT / t065_archive_ref)}; {t065_live_ref} sha256:{t065_live_hash}", utc(),
)
e742 = evidence_row(
    "E742", "T067", T067_CLOSING, "Windows 11 x64; GitHub Actions Ubuntu/Windows; repository ID 1421031011",
    "C1 exact local 13/13; main/branch CI 6/6; archive cold readback; fresh live verification ba0cc8e3",
    "PASS — T067 đạt effective DONE sau archive seal/readback và live receipt khớp remote C1; directory fsync capability unsupported.",
    f"{publication_ref} sha256:{sha_file(publication_path)}; {live_ref} sha256:{sha_file(live_path)}; {T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json sha256:{sha_file(ROOT / f'{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json')}",
    live["completed_at_utc"],
)
e743 = evidence_row(
    "E743", "T068", BASE_SHA, "Windows 11 x64; GitHub Actions Ubuntu/Windows; repository ID 1421031011",
    "Allocation preflight C055-C069; readiness dependencies; SPEC and delivery-policy fixtures",
    "PENDING — allocation local verification not run yet.",
    f"{allocation_ref} sha256:{allocation_sha}; {FIXTURE_REF} sha256:{fixture_sha}; {dependency_ref} sha256:{dep_sha}; {plan_ref} sha256:{plan_sha}",
    allocated_at,
)

# Update T065-derived evidence, close T067, and assign T068 in one atomic allocation checkpoint.
for i, line in enumerate(spec_text.splitlines()):
    if line.startswith("| T067 |"):
        cells = row_cells(line)
        fields = _evidence_fields(cells[8])
        updates = {
            "closing_sha": T067_CLOSING,
            "closing_CI": "PASS(6/6:39742410812,branch=6/6:39742427676)",
            "archive/live": f"PASS(sealed-readback,fresh-live:{LIVE_ID})",
            "closing_local": "PASS(13/13)",
            "candidate": "DONE",
            "effective": "DONE",
        }
        additions = [
            ("closing_local_summary_ref", f"{T067_SUPPORT}/local-checks/closing-C1-exact/summary.json"),
            ("closing_local_summary_sha256", sha_file(ROOT / f"{T067_SUPPORT}/local-checks/closing-C1-exact/summary.json")),
            ("closing_CI_run_pair_ref", f"{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json"),
            ("closing_CI_run_pair_sha256", sha_file(ROOT / f"{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json")),
            ("closing_CI_collection_ref", f"{T067_SUPPORT}/archive-view/ci/closing-C1-sourcegraph-retry1/collector-summary.json"),
            ("closing_CI_collection_sha256", sha_file(ROOT / f"{T067_SUPPORT}/archive-view/ci/closing-C1-sourcegraph-retry1/collector-summary.json")),
            ("closing_CI_source_graph_ref", f"{T067_SUPPORT}/archive-view/ci/closing-C1-sourcegraph-retry1/source-graph.json"),
            ("closing_CI_source_graph_sha256", sha_file(ROOT / f"{T067_SUPPORT}/archive-view/ci/closing-C1-sourcegraph-retry1/source-graph.json")),
            ("closing_CI_ref", f"{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json"),
            ("closing_CI_sha256", sha_file(ROOT / f"{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json")),
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
            ("directory_fsync", "unsupported"),
            ("observed_remote_sha", T067_CLOSING),
            ("last_closed_at_utc", live["completed_at_utc"]),
            ("archive", f"PASS(sealed-readback:{archive['archive_manifest_sha256']}+{archive['archive_seal_sha256']})"),
            ("live", f"PASS({LIVE_ID})"),
            ("closure_result", "PASS"),
        ]
        cells[8] = mutate_fields(cells[8], updates, additions)
        lines = spec_text.splitlines()
        lines[i] = render_row(cells)
        spec_text = "\n".join(lines) + "\n"
        break
else:
    raise SystemExit("T067 row not found for closure")

allocation_fields = [
    ("task_revision", str(REVISION)), ("delivery", DELIVERY), ("allocation_phase", "allocation-preflight"),
    ("base_sha", BASE_SHA), ("plan_sha256", plan_sha), ("priority", "P2"), ("task_mode", "implement"),
    ("dependencies", "DONE(T063,T064,T065,T066,T067)"), ("sequence_predecessor", "DONE(T067)"),
    ("assignment_ref", assignment_ref), ("assignment_sha256", authority_sha),
    ("selection_reason_ref", reason_ref), ("selection_reason_sha256", reason_sha),
    ("selector_ref", selection_ref), ("selector_sha256", selection_sha),
    ("task_mode_ref", mode_ref), ("task_mode_sha256", mode_sha),
    ("dependency_review_ref", dependency_ref), ("dependency_review_sha256", dep_sha),
    ("allocation_ref", allocation_ref), ("allocation_sha256", allocation_sha),
    ("fixture_plan_ref", FIXTURE_REF), ("fixture_plan_sha256", fixture_sha),
    ("candidate", "VERIFY"), ("effective", "VERIFY"), ("allocation_check", "PENDING"),
    ("allocation_local", "PENDING"), ("allocation_check_ref", "PENDING"), ("allocation_check_sha256", "PENDING"),
    ("allocation_preflight_ref", "PENDING"), ("allocation_preflight_sha256", "PENDING"),
    ("local_check_summary_ref", "PENDING"), ("local_check_summary_sha256", "PENDING"),
    ("allocation_commit_sha", "PENDING"), ("allocation_CI", "PENDING"), ("allocation_branch_CI", "PENDING"),
    ("result", "PENDING"), ("closing_sha", "PENDING"), ("closing_CI", "PENDING"),
    ("archive/live", "PENDING"), ("closing_local", "PENDING"),
]
lines = spec_text.splitlines()
for i, line in enumerate(lines):
    if line.startswith("| T068 |"):
        cells = row_cells(line)
        cells[6], cells[7] = "IN_PROGRESS", "Codex"
        cells[8] = ";".join(["E743", *(f"{k}={v}" for k, v in allocation_fields)])
        lines[i] = render_row(cells)
        break
else:
    raise SystemExit("T068 row not found")
spec_text = "\n".join(lines) + "\n"

# Recompute dependency review against the just-normalized tracker state.
rows = s19_rows(spec_text)
if any(_effective_status(rows[dep]) != "DONE" for dep in ("T063", "T064", "T065", "T066", "T067")):
    raise SystemExit("a required dependency is not effective DONE after closure/normalization")

for row in (e741, e742, e743):
    if f"| {row.split('|')[1].strip()} | {row.split('|')[2].strip()} |" in spec_text:
        raise SystemExit("S21 evidence ID collision")
insert_at = spec_text.index("\n## S22 —")
spec_text = spec_text[:insert_at] + "\n" + "\n".join((e741, e742, e743)) + spec_text[insert_at:]

s22_start = spec_text.index("## S22 — Trạng thái tiếp tục cho phiên AI sau")
s23_start = spec_text.index("\n## S23 —", s22_start)
s22 = spec_text[s22_start:s23_start]
top = {
    "next_task": "T068", "active_task": "T068", "completed_scope_tasks": "72",
    "last_evidence_id": '"E743"', "last_checked_at_utc": f'"{allocated_at}"',
    "todo_r2_tasks": "4", "assignment_start_status": "IMPLEMENTATION_STARTED",
    "final_engineering_status": "IN_PROGRESS",
}
for key, value in top.items():
    s22, n = re.subn(rf"(?m)^(  {re.escape(key)}:)\s*.*$", rf"\1 {value}", s22, count=1)
    if n != 1:
        raise SystemExit(f"missing S22 field {key}")

payload_sha = _evidence_fields(rows["T067"]["Evidence"].strip()).get("payload_sha", "")
updates = {
    "local_branch": '"codex/t068-polyglot-matrix"', "local_head_sha": f'"{BASE_SHA}"',
    "allocation_commit_sha": '"PENDING"', "last_verified_payload_sha": f'"{payload_sha}"',
    "closing_commit_ref": '"PENDING"', "pending_delivery_id": f'"{DELIVERY}"',
    "active_delivery_id": f'"{DELIVERY}"', "task_revision": f'"{REVISION}"',
    "planned_archive_path": f'"artifacts/git-delivery/{DELIVERY}"', "closing_parent_sha": f'"{BASE_SHA}"',
    "durable_archive_ref": f'"artifacts/git-delivery/{T067}"', "observed_remote_sha": f'"{T067_CLOSING}"',
    "expected_check_plan_ref": f'"{plan_ref}"',
    "last_ci_result": '"T067 closing C1 exact-SHA CI PASS 6/6 on main (39742410812) and task branch (39742427676); sealed archive/readback and fresh live verification ba0cc8e3 PASS."',
    "previous_closed_task_live_ref": f'"{live_ref}"', "last_live_verification_ref": f'"{live_ref}"',
    "last_live_verification_id": f'"{LIVE_ID}"',
    "previous_revision_ci_collection_ref": f'"{T067_SUPPORT}/archive-view/ci/closing-C1-sourcegraph-retry1/collector-summary.json"',
    "last_ci_collection_ref": f'"{T067_SUPPORT}/archive-view/ci/closing-C1-run-pair/summary.json"',
    "pending_delivery_task": '"T068"', "source_gap_review_ref": f'"{reason_ref}"',
    "last_delivery_evidence_ref": f'"{live_ref}"',
}
for key, value in updates.items():
    s22, n = re.subn(rf"(?m)^(    {re.escape(key)}:)\s*.*$", rf"\1 {value}", s22, count=1)
    if n != 1:
        raise SystemExit(f"missing S22 git_delivery field {key}")
spec_text = spec_text[:s22_start] + s22 + spec_text[s23_start:]
SPEC_PATH.write_text(spec_text, encoding="utf-8", newline="\n")

print(json.dumps({
    "result": "PREPARED",
    "task_id": TASK,
    "delivery_id": DELIVERY,
    "base_sha": BASE_SHA,
    "normalized_dependency": "T065 effective DONE (sealed archive/live receipt independently verified)",
    "fixture_plan_sha256": fixture_sha,
    "check_plan_sha256": plan_sha,
    "allocation_sha256": allocation_sha,
    "dependency_review_sha256": dep_sha,
    "T067_live_verification_sha256": sha_file(live_path),
    "T065_archive_manifest_sha256": t065_identity["manifest_sha256"],
    "T065_live_receipt_sha256": t065_live_hash,
    "directory_fsync": "unsupported" if not _fsync_directory(ROOT / "artifacts/git-delivery") else "supported",
    "attempt1_failure_ref": e741_ref,
    "attempt1_failure_sha256": e741_sha,
}, ensure_ascii=True, sort_keys=True))
