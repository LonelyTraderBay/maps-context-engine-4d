from __future__ import annotations

import hashlib
import json
import subprocess
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd().resolve()
TASK = "T062"
DELIVERY = "784ed9da-3a4e-4198-909f-d96b1efe6865"
PAYLOAD = "43f11e50525f4317ad6f0dcee3e9ea216a503711"
RUN_ID = 39586469820
PLAN = "58dc7f851ccdd86fa87f2bdcd3b794b82087d1e92b332f17a20ab1f63deb1a7e"
SUPPORT = Path(f"artifacts/git-delivery-support/T062-r1-{DELIVERY}")


def canonical(value: object) -> bytes:
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")


def digest(path: str) -> str:
    return hashlib.sha256((ROOT / path).read_bytes()).hexdigest()


def command(*args: str) -> str:
    return subprocess.run([*args], cwd=ROOT, check=True, stdout=subprocess.PIPE).stdout.decode("ascii").strip()


def main() -> None:
    if command("git", "rev-parse", "HEAD") != PAYLOAD:
        raise SystemExit("HEAD is not the verified source payload")
    if command("git", "ls-remote", "origin", "refs/heads/main").split()[0] != PAYLOAD:
        raise SystemExit("remote main is not the verified source payload")

    ci_path = f"{SUPPORT.as_posix()}/archive-view/ci/payload/collector-summary.json"
    preflight_path = "artifacts/preflight/T062/payload-ci.json"
    local_path = f"{SUPPORT.as_posix()}/local-checks/payload/attempt-3/summary.json"
    ci = json.loads((ROOT / ci_path).read_text(encoding="utf-8"))
    preflight = json.loads((ROOT / preflight_path).read_text(encoding="utf-8"))
    local = json.loads((ROOT / local_path).read_text(encoding="utf-8"))
    if ci.get("subject_sha") != PAYLOAD or ci.get("workflow_run_id") != RUN_ID or ci.get("result") != "PASS(6/6)":
        raise SystemExit("GitHub receipt is not exact-source 6/6 success")
    if preflight.get("sha") != PAYLOAD or preflight.get("run_id") != RUN_ID or preflight.get("result") != "PASS":
        raise SystemExit("preflight receipt does not match exact-source CI")
    if local.get("base_sha") != "d6f3c306875e788e333c2035c5006bbbb213b1f3" or local.get("result") != "PASS" or local.get("passed_count") != 12 or local.get("failed_count") != 0 or len(local.get("checks", [])) != 12:
        raise SystemExit("payload local checks are not 12/12")
    if local.get("check_plan_sha256") != PLAN:
        raise SystemExit("local check summary uses another plan")
    for ref, expected in local.get("code_file_sha256", {}).items():
        captured = subprocess.run(["git", "show", f"{PAYLOAD}:{ref}"], cwd=ROOT, check=True, stdout=subprocess.PIPE).stdout
        if hashlib.sha256(captured).hexdigest() != expected:
            raise SystemExit(f"local check source hash differs from exact payload: {ref}")

    spec_path = ROOT / "Maps-Context-Engine-4D_SPEC.md"
    before = spec_path.read_bytes()
    lines = before.splitlines(keepends=True)
    row_count = 0
    evidence_count = 0
    s22_count: dict[str, int] = {}
    now = datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")
    evidence_refs = [ci_path, preflight_path, local_path]
    evidence_hashes = {ref: digest(ref) for ref in evidence_refs}
    references = "; ".join(f"{ref} sha256:{sha}" for ref, sha in evidence_hashes.items())
    evidence = (
        f"| E689 | T062 | task_revision=1;delivery={DELIVERY};phase=source-payload-exact-ci;"
        f"subject_sha={PAYLOAD};payload_local=PASS(12/12);payload_CI=PASS(6/6:{RUN_ID});"
        f"plan_sha256={PLAN};result=PASS | Windows 11 x64; GitHub Actions; Go; Python | "
        "Xác nhận source payload T062 bằng bộ kiểm tra local đã lưu và sáu context Actions trên đúng SHA. | "
        "PASS — bootstrap, explorer-contract và spec-contract đều thành công trên Ubuntu/Windows; "
        "12/12 local checks gắn đúng SHA payload; đây là source payload trước commit candidate có tracker receipt. | "
        f"{references} | {now} |\n"
    ).encode("utf-8")

    for index, line in enumerate(lines):
        if line.startswith(b"| T062 |"):
            columns = line.split(b"|")
            if len(columns) != 11 or columns[7].strip() != b"IN_PROGRESS" or columns[8].strip() != b"Codex":
                raise SystemExit("T062 row is not the expected IN_PROGRESS/Codex payload candidate")
            fields: list[bytes] = []
            seen: set[bytes] = set()
            for item in columns[9].strip().split(b";"):
                item = item.strip()
                key = item.split(b"=", 1)[0]
                if key in seen:
                    raise SystemExit(f"duplicate T062 field: {key!r}")
                seen.add(key)
                if key == b"phase":
                    item = b"phase=payload-candidate"
                elif key == b"candidate":
                    item = b"candidate=VERIFY"
                elif key == b"effective":
                    item = b"effective=VERIFY"
                fields.append(item)
            fields.extend(
                [
                    b"payload_sha=PENDING",
                    b"payload_CI=PENDING",
                    b"closing_sha=PENDING",
                    b"closing_CI=PENDING",
                    b"archive/live=PENDING",
                    b"closing_parent_sha=PENDING",
                ]
            )
            columns[7] = b" VERIFY "
            columns[9] = b" " + b";".join(fields) + b" "
            lines[index] = b"|".join(columns)
            row_count += 1
        elif line.startswith(b"| E688 | T062 |"):
            if any(item.startswith(b"| E689 | T062 |") for item in lines):
                raise SystemExit("E689 already exists")
            lines.insert(index + 1, evidence)
            evidence_count += 1
        elif line.startswith(b'    allocation_commit_sha: "PENDING"'):
            lines[index] = line.replace(b'"PENDING"', b'"d6f3c306875e788e333c2035c5006bbbb213b1f3"', 1)
            s22_count["allocation_commit_sha"] = s22_count.get("allocation_commit_sha", 0) + 1
        elif line.startswith(b"    last_verified_payload_sha:"):
            old = b'"bcc02200f96ba43b50d045160a3b28366bfd7caa"'
            if old not in line:
                raise SystemExit("S22 previous verified payload differs from observed T061 state")
            lines[index] = line.replace(old, b'"' + PAYLOAD.encode("ascii") + b'"', 1)
            s22_count["last_verified_payload_sha"] = s22_count.get("last_verified_payload_sha", 0) + 1
        elif line.startswith(b"    observed_remote_sha:"):
            old = b'"b4422e5dccd46523115ba4bd299332007282c227"'
            if old not in line:
                raise SystemExit("S22 previous remote SHA differs from observed T061 state")
            lines[index] = line.replace(old, b'"' + PAYLOAD.encode("ascii") + b'"', 1)
            s22_count["observed_remote_sha"] = s22_count.get("observed_remote_sha", 0) + 1
        elif line.startswith(b"    last_ci_collection_ref:"):
            old = b"artifacts/git-delivery-support/T061-r1-0809ec6c-1f28-419a-a9da-51c45e30db34/archive-view/ci/closing-C-final/collector-summary.json"
            if old not in line:
                raise SystemExit("S22 previous collection ref differs from observed T061 state")
            lines[index] = line.replace(old, ci_path.encode("utf-8"), 1)
            s22_count["last_ci_collection_ref"] = s22_count.get("last_ci_collection_ref", 0) + 1
        elif line.startswith(b"    last_ci_result:"):
            suffix = f"; T062 source payload {PAYLOAD} exact Actions {RUN_ID} PASS 6/6; local PASS 12/12; receipt {ci_path} sha256:{digest(ci_path)}"
            ending = b"\r\n" if line.endswith(b"\r\n") else b"\n" if line.endswith(b"\n") else b""
            body = line[:-len(ending)] if ending else line
            if not body.endswith(b'"'):
                raise SystemExit("S22 last_ci_result is not quoted")
            lines[index] = body[:-1] + suffix.encode("ascii") + b'"' + ending
            s22_count["last_ci_result"] = s22_count.get("last_ci_result", 0) + 1

    if row_count != 1 or evidence_count != 1 or s22_count != {
        "allocation_commit_sha": 1,
        "last_verified_payload_sha": 1,
        "observed_remote_sha": 1,
        "last_ci_collection_ref": 1,
        "last_ci_result": 1,
    }:
        raise SystemExit(f"candidate edit cardinality mismatch: row={row_count} evidence={evidence_count} S22={s22_count}")

    spec_path.write_bytes(b"".join(lines))
    receipt = {
        "delivery_id": DELIVERY,
        "format_version": 1,
        "kind": "payload_candidate_receipt",
        "payload_sha": PAYLOAD,
        "payload_ci_run_id": RUN_ID,
        "payload_ci_ref": ci_path,
        "payload_ci_sha256": digest(ci_path),
        "payload_local_ref": local_path,
        "payload_local_sha256": digest(local_path),
        "result": "PASS",
        "task_id": TASK,
        "task_revision": 1,
    }
    receipt_path = ROOT / "artifacts/preflight/T062/payload-candidate.json"
    data = canonical(receipt)
    if receipt_path.exists() and receipt_path.read_bytes() != data:
        raise SystemExit(f"preserving conflicting receipt: {receipt_path}")
    if not receipt_path.exists():
        receipt_path.parent.mkdir(parents=True, exist_ok=True)
        with receipt_path.open("xb") as stream:
            stream.write(data)
            stream.flush()
    print("PASS: T062 payload candidate VERIFY; exact-source CI and local receipts recorded; S22 derived fields updated.")


if __name__ == "__main__":
    main()
