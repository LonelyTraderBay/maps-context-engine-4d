from __future__ import annotations

import hashlib
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd()
TASK = "T034"
DELIVERY = "bd1c82a0-cbbf-4487-880e-0e85a0672c8d"
SUPPORT_REF = f"artifacts/git-delivery-support/{TASK}-r4-{DELIVERY}"
SUPPORT = ROOT / SUPPORT_REF
SPEC = ROOT / "Maps-Context-Engine-4D_SPEC.md"
BASE = "16136bf18b8d0e77ede1500ccf57399391addcd5"
PAYLOAD = "6827429e6360741ceb8a42db94a5b4b0778b4991"
PLAN = "bc157681517301a6cdff3601707d09f2f5f5d8e8365fc05474d047100aa9b495"
CI_RUN = "39208327203"
MATRIX_DIR = "artifacts/acceptance/T034/runs/windows-20261019-payload-6827429"


def git(*args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=ROOT, text=True, encoding="utf-8").strip()


def read(ref: str) -> dict:
    value = json.loads((ROOT / ref).read_text(encoding="utf-8", errors="strict"))
    if not isinstance(value, dict):
        raise SystemExit(f"JSON object required: {ref}")
    return value


def sha(ref: str) -> str:
    return hashlib.sha256((ROOT / ref).read_bytes()).hexdigest()


def update_s22(lines: list[str], key: str, old: object, new: object) -> None:
    field = f"{key}: "
    positions = [index for index, line in enumerate(lines) if line.lstrip().startswith(field)]
    if len(positions) != 1:
        raise SystemExit(f"S22 field is missing or duplicated: {key}")
    index = positions[0]
    indent = lines[index][:len(lines[index]) - len(lines[index].lstrip())]
    if json.loads(lines[index].lstrip()[len(field):]) != old:
        raise SystemExit(f"S22 precondition changed: {key}")
    lines[index] = indent + field + json.dumps(new, ensure_ascii=False)


if git("branch", "--show-current") != "main" or git("rev-parse", "HEAD") != PAYLOAD:
    raise SystemExit("payload checkpoint requires the verified T034 r4 code payload on main")
if git("ls-remote", "origin", "refs/heads/main").split() != [PAYLOAD, "refs/heads/main"]:
    raise SystemExit("remote main does not match the verified payload SHA")
if git("status", "--porcelain", "--untracked-files=no") or git("write-tree") != git("rev-parse", "HEAD^{tree}"):
    raise SystemExit("tracked worktree or index is not clean")
if SPEC.read_bytes() != subprocess.check_output(["git", "show", f"{PAYLOAD}:Maps-Context-Engine-4D_SPEC.md"], cwd=ROOT):
    raise SystemExit("SPEC differs from the verified payload commit")
if hashlib.sha256((SUPPORT / "check-plan.json").read_bytes()).hexdigest() != PLAN:
    raise SystemExit("frozen T034 r4 check plan SHA mismatch")

local_ref = f"{SUPPORT_REF}/local-checks/payload/attempt-2/receipt.json"
local = read(local_ref)
local_summary_ref = f"{SUPPORT_REF}/local-checks/payload/attempt-2/summary.json"
local_summary = read(local_summary_ref)
if (local.get("candidate_sha"), local.get("candidate_tree_sha"), local.get("base_sha"), local.get("phase"),
        local.get("check_plan_sha256"), local.get("result"), local.get("check_count"),
        local.get("passed_checks"), local.get("failed_checks")) != (
        PAYLOAD, git("show", "-s", "--format=%T", PAYLOAD), BASE, "payload", PLAN, "PASS", 11, 11, 0):
    raise SystemExit("payload local receipt does not bind exact candidate/tree and 11/11 PASS")
if (local_summary.get("candidate_sha"), local_summary.get("check_plan_sha256"), local_summary.get("result"),
        local_summary.get("passed_checks"), local_summary.get("failed_checks")) != (PAYLOAD, PLAN, "PASS", 11, 0):
    raise SystemExit("payload local summary mismatch")

ci_root = f"{SUPPORT_REF}/archive-view/ci/payload-code"
ci = read(f"{ci_root}/collector-summary.json")
collection = read(f"{ci_root}/collection.json")
observation = read(f"{ci_root}/payload-observation.archive.json")
if (ci.get("subject_sha"), ci.get("check_plan_sha256"), ci.get("result"), ci.get("check_count"),
        ci.get("passed_count"), ci.get("run_ids")) != (PAYLOAD, PLAN, "PASS", 4, 4, [CI_RUN]):
    raise SystemExit("payload exact-SHA CI collection mismatch")
if (collection.get("subject_sha"), collection.get("repository_id"), collection.get("collection_id")) != (
        PAYLOAD, 1421031011, ci.get("collection_id")):
    raise SystemExit("payload CI collection identity mismatch")
if (observation.get("subject_sha"), observation.get("parent_sha"), observation.get("observed_remote_sha"),
        observation.get("check_plan_sha256"), observation.get("result"), observation.get("required_set_verified")) != (
        PAYLOAD, BASE, PAYLOAD, PLAN, "PASS", True):
    raise SystemExit("payload observation does not bind the parent, remote, plan and required set")

push_confirmation_root = f"{SUPPORT_REF}/archive-view/operations/payload-code-confirmation-1"
push = read(f"{push_confirmation_root}/push-operation.json")
destination = read(f"{push_confirmation_root}/destination-observation.json")
lifecycle = read(f"{push_confirmation_root}/command-lifecycle.json")
if (push.get("candidate_sha"), push.get("preflight", {}).get("observed_remote_sha"),
        push.get("postflight", {}).get("observed_remote_sha")) != (PAYLOAD, PAYLOAD, PAYLOAD):
    raise SystemExit("payload push confirmation preflight/postflight mismatch")
if (destination.get("candidate_sha"), destination.get("identity_verified"), destination.get("visibility_verified")) != (
        PAYLOAD, True, True) or lifecycle.get("result", {}).get("final_status") != "SUCCEEDED":
    raise SystemExit("payload destination or push lifecycle evidence mismatch")

failure_root = f"{SUPPORT_REF}/archive-view/ci/allocation-attempt-1"
allocation_failure = read(f"{failure_root}/collector-summary.json")
if (allocation_failure.get("subject_sha"), allocation_failure.get("run_id"), allocation_failure.get("result")) != (
        "d70b5cc76490d9cf60457869e77b0cffd19ad06e", "39207091807", "FAIL_PRESERVED"):
    raise SystemExit("allocation failure history is missing or mismatched")

matrix_ref = f"{MATRIX_DIR}/report.json"
matrix = read(matrix_ref)
if (matrix.get("subject_sha"), matrix.get("matrix_sha256"), matrix.get("tracked_worktree_clean"), matrix.get("counts")) != (
        PAYLOAD, "102ad7b48488929753f12220ce20b6afda55667ae00bcead8d4e37df7a599411", True,
        {"cases": 69, "pass": 52, "pass_limited": 17, "fail": 0}):
    raise SystemExit("fresh T034 R1 acceptance matrix result mismatch")

commit_chain_ref = f"{SUPPORT_REF}/payload-commit-chain.json"
commit_chain = {
    "format_version": 1, "kind": "payload_commit_chain", "task_id": TASK, "task_revision": 4,
    "delivery_id": DELIVERY, "allocation_sha": "d70b5cc76490d9cf60457869e77b0cffd19ad06e",
    "commits": [], "payload_sha": PAYLOAD,
}
for commit in ("16136bf18b8d0e77ede1500ccf57399391addcd5", PAYLOAD):
    parent = git("show", "-s", "--format=%P", commit).split()
    commit_chain["commits"].append({
        "sha": commit, "parent_shas": parent, "tree_sha": git("show", "-s", "--format=%T", commit),
        "subject": git("show", "-s", "--format=%s", commit),
        "body_sha256": hashlib.sha256(git("show", "-s", "--format=%B", commit).encode("utf-8")).hexdigest(),
    })
commit_chain_bytes = (json.dumps(commit_chain, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
(ROOT / commit_chain_ref).write_bytes(commit_chain_bytes)

raw = SPEC.read_text(encoding="utf-8", errors="strict")
lines = raw.splitlines()
rows = [index for index, line in enumerate(lines) if line.startswith("| T034 |")]
if len(rows) != 1:
    raise SystemExit("S19 T034 row missing or duplicated")
row_index = rows[0]
cells = lines[row_index].split("|")
if len(cells) != 11 or cells[7].strip() != "IN_PROGRESS" or cells[8].strip() != "Codex":
    raise SystemExit("T034 S19 row is not at expected active state")
parts = cells[9].strip().split(";")
ids, *items = parts
fields: dict[str, str] = {}
order: list[str] = []
for item in items:
    key, separator, value = item.partition("=")
    if not separator or not key or key in fields:
        raise SystemExit(f"invalid S19 field: {item!r}")
    order.append(key)
    fields[key] = value
expected = {
    "task_revision": "4", "delivery": DELIVERY, "phase": "allocation-selected",
    "base_sha": "ea8763c63e711e5ed87fc3bf7307d11ba286a709", "plan_sha256": PLAN,
    "candidate": "VERIFY", "effective": "IN_PROGRESS", "allocation_commit_sha": "PENDING",
    "allocation_CI": "PENDING", "allocation_local": "PENDING", "payload_sha": "PENDING",
    "closing": "NOT_RUN", "archive/live": "NOT_RUN",
}
for key, value in expected.items():
    if fields.get(key) != value:
        raise SystemExit(f"S19 precondition changed: {key}={fields.get(key)!r}")
fields.update({
    "phase": "payload-code-verified", "candidate": "VERIFY", "effective": "IN_PROGRESS",
    "allocation_commit_sha": "d70b5cc76490d9cf60457869e77b0cffd19ad06e",
    "allocation_CI": "FAIL(3/4:39207091807)", "allocation_local": "PASS(11/11)",
    "payload_sha": PAYLOAD, "payload_local": "PASS(11/11)",
    "payload_CI": f"PASS(4/4:{CI_RUN})", "acceptance_matrix": "PASS(69:52-PASS,17-PASS_LIMITED,0-FAIL)",
    "closing": "PENDING", "archive/live": "PENDING",
})
for key in ("payload_local", "payload_CI", "acceptance_matrix"):
    if key not in order:
        order.append(key)
id_list = ids.split(",")
if "E433" in id_list or any(line.startswith("| E433 |") for line in lines):
    raise SystemExit("E433 already exists; preserve and inspect")
id_list.append("E433")
cells[9] = " " + ";".join([",".join(id_list), *(f"{key}={fields[key]}" for key in order)]) + " "
lines[row_index] = "|".join(cells)

e432 = [index for index, line in enumerate(lines) if line.startswith("| E432 | T034 |")]
if len(e432) != 1:
    raise SystemExit("expected exactly one E432 allocation-selection event")
artifact_refs = [
    f"{SUPPORT_REF}/archive-view/ci/allocation-attempt-1/collector-summary.json",
    f"{SUPPORT_REF}/archive-view/ci/allocation-attempt-1/run.json",
    f"{SUPPORT_REF}/archive-view/ci/allocation-attempt-1/windows-spec-contract-failure.log",
    f"{SUPPORT_REF}/payload-commit-chain.json",
    f"{SUPPORT_REF}/local-checks/payload/attempt-1/summary.json",
    f"{SUPPORT_REF}/local-checks/payload/attempt-2/receipt.json",
    f"{local_summary_ref}",
    f"{SUPPORT_REF}/archive-view/operations/payload-code-1/push-operation.json",
    f"{push_confirmation_root}/push-operation.json",
    f"{push_confirmation_root}/destination-observation.json",
    f"{ci_root}/collector-summary.json",
    f"{ci_root}/collection.json",
    f"{ci_root}/payload-observation.archive.json",
    f"{ci_root}/source-graph.json",
    "artifacts/acceptance/T034/matrix.json",
    matrix_ref,
]
artifacts = ";".join(f"{ref}=sha256:{sha(ref)}" for ref in artifact_refs)
now = datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")
event = (
    f"| E433 | T034 | task_revision=4;delivery={DELIVERY};phase=payload-code-verified;"
    f"allocation_commit_sha=d70b5cc76490d9cf60457869e77b0cffd19ad06e;allocation_local=PASS(11/11);"
    f"allocation_CI=FAIL(3/4:39207091807);payload_commit_chain=16136bf18b8d0e77ede1500ccf57399391addcd5,{PAYLOAD};"
    f"payload_parent_sha={BASE};payload_sha={PAYLOAD};plan_sha256={PLAN};payload_local=PASS(11/11);"
    f"payload_CI=PASS(4/4:{CI_RUN});collection_id={ci['collection_id']};push_operation_id={push['push_operation_id']};"
    "acceptance_matrix=PASS(69:52-PASS,17-PASS_LIMITED,0-FAIL);candidate=VERIFY;effective=IN_PROGRESS;"
    f"closing=PENDING;archive/live=PENDING "
    "| Windows 11 Pro x64; Python 3.12.10; Go 1.27.2 windows/amd64; Git 2.45.1; GitHub Actions Ubuntu/Windows; repository ID 1421031011 "
    "| Ghi nhận CI allocation lỗi do clone Windows bỏ qua đường dẫn dài, hai commit sửa self-index harness, kiểm payload exact-SHA và ma trận acceptance R1 mới. "
    f"| PASS — Allocation {allocation_failure['run_id']} FAIL 3/4 được giữ làm lịch sử. Payload {PAYLOAD}: local 11/11 PASS; Actions {CI_RUN} 4/4 PASS, collection {ci['collection_id']}; R1 69 case gồm 52 PASS, 17 PASS_LIMITED, 0 FAIL. T034 vẫn chờ checkpoint, closing, archive/readback và live. "
    f"| {artifacts} | {now} |"
)
lines.insert(e432[0] + 1, event)

update_s22(lines, "local_head_sha", "ea8763c63e711e5ed87fc3bf7307d11ba286a709", PAYLOAD)
update_s22(lines, "last_verified_payload_sha", "42104732e3fd917d0671cc2bd17f1060a88e0572", PAYLOAD)
update_s22(lines, "closing_commit_ref", "93bb13294973554f7cf5978fa57ae2f001f5c2e1", None)
update_s22(lines, "observed_remote_sha", "ea8763c63e711e5ed87fc3bf7307d11ba286a709", PAYLOAD)
update_s22(lines, "last_ci_collection_ref", "artifacts/git-delivery-support/T034-r3-18271be3-f0ae-45bf-9d80-3d830ad21038/archive-view/ci/c/collection.json", f"{ci_root}/collection.json")
update_s22(lines, "last_delivery_evidence_ref", "artifacts/git-delivery-support/T034-r3-18271be3-f0ae-45bf-9d80-3d830ad21038/archive-view/ci/c/closing-observation.archive.json", f"{ci_root}/payload-observation.archive.json")
update_s22(lines, "last_evidence_id", "E407", "E433")
update_s22(lines, "last_checked_at_utc", "2026-10-18T18:47:41.705634Z", now)
update_s22(
    lines, "last_ci_result",
    "T034 r3 closing C ea8763c63e711e5ed87fc3bf7307d11ba286a709: Actions 39205253213 exact-SHA PASS 4/4, collection ebbd8207-c088-4d36-9417-c6b6c2f403b7; archive graph then rejected immutable payload commit ee14e15 for missing required body headings. T034 r4 bd1c82a0-cbbf-4487-880e-0e85a0672c8d is selected on this base; its allocation local checks and exact-SHA CI are pending.",
    f"T034 r4 code payload {PAYLOAD}: local 11/11 PASS; Actions {CI_RUN} exact-SHA PASS 4/4, collection {ci['collection_id']}; fresh R1 matrix 69 cases: 52 PASS, 17 PASS_LIMITED, 0 FAIL. Allocation CI failure 39207091807 is retained and superseded by the verified payload. Payload-ready checkpoint, closing, archive/readback and live remain pending.",
)

updated = "\n".join(lines) + "\n"
sys.path.insert(0, str(ROOT))
from tools.ci.delivery_policy import validate_commit_message

message = (
    "docs(progress): [P0][T034] Ghi nhận payload r4 và kiểm tra Windows\n\n"
    "Mục đích: Ghi nhận code payload T034 r4 sau khi sửa clone self-index trên Windows.\n"
    "Thay đổi: Thêm E433, cập nhật S19/S22; closing và archive/readback/live còn chờ.\n"
    f"Kiểm tra: {PAYLOAD} local 11/11 PASS, Actions {CI_RUN} 4/4 PASS; R1 69 case gồm 52 PASS, 17 PASS_LIMITED, 0 FAIL."
)
errors = validate_commit_message(message, phase="payload", task_id=TASK, priority="P0")
if errors:
    raise SystemExit("payload progress commit message invalid: " + "; ".join(errors))

candidate = SUPPORT / "payload-checkpoint-T034-SPEC-candidate.md"
message_path = SUPPORT / "commit-message-payload-checkpoint.txt"
if candidate.exists() or message_path.exists():
    raise SystemExit("payload checkpoint candidate already exists; preserve and inspect")
candidate.write_text(updated, encoding="utf-8", newline="\n")
SPEC.write_text(updated, encoding="utf-8", newline="\n")
message_path.write_text(message, encoding="utf-8", newline="\n")
print(json.dumps({
    "result": "PREPARED", "evidence_id": "E433", "payload_sha": PAYLOAD,
    "local": "PASS(11/11)", "ci": f"PASS(4/4:{CI_RUN})", "acceptance_cases": 69,
    "allocation_failure_preserved": allocation_failure["run_id"], "candidate": candidate.relative_to(ROOT).as_posix(),
}, sort_keys=True, separators=(",", ":")))
