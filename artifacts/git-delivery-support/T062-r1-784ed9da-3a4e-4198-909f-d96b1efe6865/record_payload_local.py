from __future__ import annotations

import hashlib
import json
import sys
from datetime import datetime, timezone
from pathlib import Path


ROOT = Path.cwd()
SPEC = ROOT / "Maps-Context-Engine-4D_SPEC.md"
SUPPORT = Path("artifacts/git-delivery-support/T062-r1-784ed9da-3a4e-4198-909f-d96b1efe6865")
ATTEMPT = int(sys.argv[1]) if len(sys.argv) > 1 else 1
SUMMARY = SUPPORT / f"local-checks/payload/attempt-{ATTEMPT}/summary.json"


def sha256(path: Path) -> str:
    return hashlib.sha256((ROOT / path).read_bytes()).hexdigest()


def set_field(fields: list[str], key: str, value: str) -> None:
    prefix = key + "="
    for index, field in enumerate(fields):
        if field.startswith(prefix):
            fields[index] = prefix + value
            return
    fields.append(prefix + value)


def main() -> None:
    summary = json.loads((ROOT / SUMMARY).read_bytes())
    if summary.get("kind") != "payload_local_check_summary" or summary.get("result") != "PASS":
        raise SystemExit("payload local check summary is not PASS")
    if summary.get("passed_count") != 12 or summary.get("failed_count") != 0:
        raise SystemExit("payload local check count mismatch")
    data = SPEC.read_bytes()
    lines = data.splitlines(keepends=True)
    evidence_id = f"E{685 + ATTEMPT}"
    if any(line.startswith(f"| {evidence_id} | T062 |".encode("ascii")) for line in lines):
        raise SystemExit(f"{evidence_id} already exists; preserving the SPEC")
    task_index = next((i for i, line in enumerate(lines) if line.startswith(b"| T062 |")), None)
    prior_id = f"E{684 + ATTEMPT}"
    evidence_index = next((i for i, line in enumerate(lines) if line.startswith(f"| {prior_id} | T062 |".encode("ascii"))), None)
    if task_index is None or evidence_index is None:
        raise SystemExit(f"T062 or {prior_id} SPEC row is missing")

    task_line = lines[task_index].decode("utf-8").rstrip("\r\n")
    parts = task_line.split("|")
    event_sequence = ",".join(f"E{number}" for number in range(684, 686 + ATTEMPT))
    old_events = parts[9].split(";", 1)[0]
    parts[9] = parts[9].replace(old_events, event_sequence, 1)
    fields = parts[9].split(";")
    set_field(fields, "payload_local", f"PASS(12/12,attempt{ATTEMPT})")
    set_field(fields, "payload_local_ref", SUMMARY.as_posix())
    set_field(fields, "payload_local_sha256", sha256(SUMMARY))
    set_field(fields, "payload_mode", "changes")
    parts[9] = ";".join(fields)
    eol = "\r\n" if lines[task_index].endswith(b"\r\n") else "\n"
    lines[task_index] = ("|".join(parts) + eol).encode("utf-8")

    observed = datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")
    row = (
        f"| {evidence_id} | T062 | task_revision=1;delivery=784ed9da-3a4e-4198-909f-d96b1efe6865;"
        f"phase=payload-local-attempt-{ATTEMPT};payload_local=PASS(12/12);result=PASS | Windows local; Go and Python toolchains | "
        "Thêm parser SCXML 1.0 subset gắn SourceRef, kiểm tra scope/initial/hierarchy/targets và gate approval theo source snapshot. | "
        "PASS — 12/12 check; full Go suite, vet, build, gofmt, 142 spec fixtures, policy/checkpoint fixtures và import boundary đạt. "
        "Guard/action chỉ là source reference unknown; unsupported parallel/history/XInclude/DTD/action nesting là gap; không gọi filesystem, network hoặc interpreter. | "
        f"{SUMMARY.as_posix()} sha256:{sha256(SUMMARY)} | {observed} |\n"
    ).encode("utf-8")
    lines.insert(evidence_index + 1, row)
    SPEC.write_bytes(b"".join(lines))
    print(f"payload local record added as {evidence_id}; summary sha256={sha256(SUMMARY)}")


if __name__ == "__main__":
    main()
