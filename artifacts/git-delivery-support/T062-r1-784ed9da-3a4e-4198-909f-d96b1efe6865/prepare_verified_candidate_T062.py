from __future__ import annotations

import hashlib
import json
import subprocess
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd().resolve()
TASK = "T062"
DELIVERY = "784ed9da-3a4e-4198-909f-d96b1efe6865"
SOURCE = "a342bd77ba607565c8a8411b9177d55253e7a4a1"
RUN_ID = 39588010911
PLAN = "58dc7f851ccdd86fa87f2bdcd3b794b82087d1e92b332f17a20ab1f63deb1a7e"
SUPPORT = Path(f"artifacts/git-delivery-support/T062-r1-{DELIVERY}")


def canonical(value: object) -> bytes:
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")


def sha(path: str) -> str:
    return hashlib.sha256((ROOT / path).read_bytes()).hexdigest()


def git(*args: str) -> str:
    return subprocess.run(["git", *args], cwd=ROOT, check=True, stdout=subprocess.PIPE).stdout.decode("ascii").strip()


def main() -> None:
    if git("rev-parse", "HEAD") != SOURCE:
        raise SystemExit("HEAD is not the exact verified candidate")
    if git("ls-remote", "origin", "refs/heads/main").split()[0] != SOURCE:
        raise SystemExit("remote main is not the exact verified candidate")

    ci_ref = f"{SUPPORT.as_posix()}/archive-view/ci/payload-candidate/collector-summary.json"
    ci_preflight_ref = "artifacts/preflight/T062/payload-candidate-ci.json"
    local_ref = f"{SUPPORT.as_posix()}/local-checks/payload-candidate/attempt-1/summary.json"
    local_attempt_ref = f"{SUPPORT.as_posix()}/local-checks/payload-candidate/attempt-1/attempt.json"
    ci = json.loads((ROOT / ci_ref).read_text(encoding="utf-8"))
    ci_preflight = json.loads((ROOT / ci_preflight_ref).read_text(encoding="utf-8"))
    local = json.loads((ROOT / local_ref).read_text(encoding="utf-8"))
    attempt = json.loads((ROOT / local_attempt_ref).read_text(encoding="utf-8"))
    if ci.get("subject_sha") != SOURCE or ci.get("workflow_run_id") != RUN_ID or ci.get("result") != "PASS(6/6)":
        raise SystemExit("candidate CI receipt is not exact-source 6/6")
    if ci_preflight.get("sha") != SOURCE or ci_preflight.get("run_id") != RUN_ID or ci_preflight.get("result") != "PASS":
        raise SystemExit("candidate CI preflight receipt mismatches")
    if local.get("source_sha") != SOURCE or local.get("result") != "PASS" or local.get("passed_count") != 13 or local.get("failed_count") != 0 or len(local.get("checks", [])) != 13:
        raise SystemExit("candidate local checks are not 13/13")
    if local.get("check_plan_sha256") != PLAN or attempt.get("revision") != SOURCE or attempt.get("result") != "PASS":
        raise SystemExit("candidate local receipts use another source or plan")
    for check in local["checks"]:
        log = ROOT / check["log_ref"]
        if check["result"] != "PASS" or hashlib.sha256(log.read_bytes()).hexdigest() != check["log_sha256"]:
            raise SystemExit(f"candidate check log failed hash/status verification: {check['check_id']}")

    spec_path = ROOT / "Maps-Context-Engine-4D_SPEC.md"
    lines = spec_path.read_bytes().splitlines(keepends=True)
    row_count = 0
    evidence_count = 0
    s22_count: dict[str, int] = {}
    now = datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")
    refs = [ci_ref, ci_preflight_ref, local_ref, local_attempt_ref]
    refs_hash = {ref: sha(ref) for ref in refs}
    reftext = "; ".join(f"{ref} sha256:{value}" for ref, value in refs_hash.items())
    evidence = (
        f"| E690 | T062 | task_revision=1;delivery={DELIVERY};phase=candidate-exact-verification;"
        f"subject_sha={SOURCE};local=PASS(13/13);payload_CI=PASS(6/6:{RUN_ID});"
        f"plan_sha256={PLAN};result=PASS | Windows 11 x64; Node.js 24.22.0/npm 11.19.1; Go; Python; GitHub Actions | "
        "Kiểm tra candidate có tracker receipt trên đúng SHA, chạy trọn bộ local frozen plan và exact Actions trên hai hệ điều hành. | "
        "PASS — 13/13 local checks và 6/6 Actions contexts; npm ci chỉ dùng thư mục tạm riêng; log, record, summary, SHA và plan được xác minh. | "
        f"{reftext} | {now} |\n"
    ).encode("utf-8")

    for index, line in enumerate(lines):
        if line.startswith(b"| T062 |"):
            columns = line.split(b"|")
            if len(columns) != 11 or columns[7].strip() != b"VERIFY" or columns[8].strip() != b"Codex":
                raise SystemExit("T062 row is not the expected VERIFY/Codex candidate")
            fields: list[bytes] = []
            seen: set[bytes] = set()
            found_local = False
            found_payload_local = False
            for item in columns[9].strip().split(b";"):
                item = item.strip()
                key = item.split(b"=", 1)[0]
                if key in seen:
                    raise SystemExit(f"duplicate T062 field: {key!r}")
                seen.add(key)
                if key == b"payload_local":
                    item = b"payload_local=PASS(13/13,candidate-attempt1)"
                    found_payload_local = True
                elif key == b"local":
                    item = b"local=PASS(13/13)"
                    found_local = True
                fields.append(item)
            if not found_payload_local:
                fields.append(b"payload_local=PASS(13/13,candidate-attempt1)")
            if not found_local:
                fields.append(b"local=PASS(13/13)")
            columns[9] = b" " + b";".join(fields) + b" "
            lines[index] = b"|".join(columns)
            row_count += 1
        elif line.startswith(b"| E689 | T062 |"):
            if any(item.startswith(b"| E690 | T062 |") for item in lines):
                raise SystemExit("E690 already exists")
            lines.insert(index + 1, evidence)
            evidence_count += 1
        elif line.startswith(b"    last_verified_payload_sha:"):
            old = b'"43f11e50525f4317ad6f0dcee3e9ea216a503711"'
            if old not in line:
                raise SystemExit("S22 verified payload differs from source candidate")
            lines[index] = line.replace(old, b'"' + SOURCE.encode("ascii") + b'"', 1)
            s22_count["last_verified_payload_sha"] = 1
        elif line.startswith(b"    observed_remote_sha:"):
            old = b'"43f11e50525f4317ad6f0dcee3e9ea216a503711"'
            if old not in line:
                raise SystemExit("S22 remote observation differs from prior verified payload")
            lines[index] = line.replace(old, b'"' + SOURCE.encode("ascii") + b'"', 1)
            s22_count["observed_remote_sha"] = 1
        elif line.startswith(b"    last_ci_collection_ref:"):
            previous = b"artifacts/git-delivery-support/T062-r1-784ed9da-3a4e-4198-909f-d96b1efe6865/archive-view/ci/payload/collector-summary.json"
            if previous not in line:
                raise SystemExit("S22 current collection ref is unexpected")
            lines[index] = line.replace(previous, ci_ref.encode("utf-8"), 1)
            s22_count["last_ci_collection_ref"] = 1
        elif line.startswith(b"    last_ci_result:"):
            note = f"; T062 verified candidate {SOURCE} exact Actions {RUN_ID} PASS 6/6; local frozen plan PASS 13/13; receipt {ci_ref} sha256:{sha(ci_ref)}"
            ending = b"\r\n" if line.endswith(b"\r\n") else b"\n" if line.endswith(b"\n") else b""
            body = line[:-len(ending)] if ending else line
            if not body.endswith(b'"'):
                raise SystemExit("S22 last_ci_result is not quoted")
            lines[index] = body[:-1] + note.encode("ascii") + b'"' + ending
            s22_count["last_ci_result"] = 1

    if row_count != 1 or evidence_count != 1 or s22_count != {
        "last_verified_payload_sha": 1,
        "observed_remote_sha": 1,
        "last_ci_collection_ref": 1,
        "last_ci_result": 1,
    }:
        raise SystemExit(f"verification edit cardinality mismatch: row={row_count} evidence={evidence_count} S22={s22_count}")
    spec_path.write_bytes(b"".join(lines))
    receipt = {
        "delivery_id": DELIVERY,
        "format_version": 1,
        "kind": "payload_verified_receipt",
        "local_result": "PASS(13/13)",
        "local_ref": local_ref,
        "local_sha256": sha(local_ref),
        "payload_ci_run_id": RUN_ID,
        "payload_ci_ref": ci_ref,
        "payload_ci_sha256": sha(ci_ref),
        "result": "PASS",
        "source_sha": SOURCE,
        "task_id": TASK,
        "task_revision": 1,
    }
    receipt_path = ROOT / "artifacts/preflight/T062/payload-verified.json"
    receipt_bytes = canonical(receipt)
    if receipt_path.exists() and receipt_path.read_bytes() != receipt_bytes:
        raise SystemExit(f"preserving conflicting receipt: {receipt_path}")
    if not receipt_path.exists():
        with receipt_path.open("xb") as stream:
            stream.write(receipt_bytes)
            stream.flush()
    print("PASS: candidate exact SHA has 13/13 local checks and 6/6 Actions; tracker receipts written.")


if __name__ == "__main__":
    main()
