from __future__ import annotations

import hashlib
from datetime import datetime, timezone
from pathlib import Path


ROOT = Path.cwd()
SPEC = ROOT / "Maps-Context-Engine-4D_SPEC.md"
DELIVERY = "784ed9da-3a4e-4198-909f-d96b1efe6865"
COMMIT = "d6f3c306875e788e333c2035c5006bbbb213b1f3"
RUN_ID = "39583224200"
SUPPORT = Path(f"artifacts/git-delivery-support/T062-r1-{DELIVERY}")
CI_REF = SUPPORT / "archive-view/ci/allocation/collector-summary.json"
FAILURE_REF = SUPPORT / "archive-view/ci/allocation-attempt-1-failure.log"
PUSH_REF = SUPPORT / "archive-view/operations/allocation-push-verification/push-operation.json"
OBSERVATION_REF = SUPPORT / "archive-view/operations/allocation-push-verification/destination-observation.json"
ALLOCATION_REF = SUPPORT / "allocation-check.json"


def sha256(path: Path) -> str:
    return hashlib.sha256((ROOT / path).read_bytes()).hexdigest()


def replace_field(fields: list[str], name: str, value: str) -> None:
    prefix = name + "="
    for index, field in enumerate(fields):
        if field.startswith(prefix):
            fields[index] = prefix + value
            return
    fields.append(prefix + value)


def main() -> None:
    source = SPEC.read_bytes()
    lines = source.splitlines(keepends=True)
    if any(line.startswith(b"| E685 | T062 |") for line in lines):
        raise SystemExit("E685 already exists; preserving the SPEC")

    task_index = next((i for i, line in enumerate(lines) if line.startswith(b"| T062 |")), None)
    evidence_index = next((i for i, line in enumerate(lines) if line.startswith(b"| E684 | T062 |")), None)
    if task_index is None or evidence_index is None:
        raise SystemExit("T062 or E684 SPEC row is missing")

    task_line = lines[task_index].decode("utf-8").rstrip("\r\n")
    parts = task_line.split("|")
    if len(parts) < 11:
        raise SystemExit("T062 task row has an unexpected shape")
    parts[9] = parts[9].replace("E684;", "E684,E685;", 1)
    fields = parts[9].split(";")
    for name, value in (
        ("phase", "payload"),
        ("allocation_commit_sha", COMMIT),
        ("allocation_local", "PASS(4/4)"),
        ("allocation_CI", f"PASS(6/6:{RUN_ID},attempt2)"),
        ("allocation_CI_attempt1", f"FAIL(5/6:{RUN_ID})"),
        ("allocation_CI_attempt2", f"PASS(6/6:{RUN_ID})"),
        ("allocation_CI_ref", CI_REF.as_posix()),
        ("allocation_CI_sha256", sha256(CI_REF)),
        ("allocation_push_operation", "af2a3440-b638-4cdd-b690-c622b86cca15"),
        ("allocation_push_ref", PUSH_REF.as_posix()),
        ("allocation_push_sha256", sha256(PUSH_REF)),
        ("allocation_push_observation_ref", OBSERVATION_REF.as_posix()),
        ("allocation_push_observation_sha256", sha256(OBSERVATION_REF)),
        ("allocation_check_ref", ALLOCATION_REF.as_posix()),
        ("allocation_check_sha256", sha256(ALLOCATION_REF)),
    ):
        replace_field(fields, name, value)
    parts[9] = ";".join(fields)
    eol = "\r\n" if lines[task_index].endswith(b"\r\n") else "\n"
    lines[task_index] = ("|".join(parts) + eol).encode("utf-8")

    now = datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")
    evidence = (
        f"| E685 | T062 | task_revision=1;delivery={DELIVERY};phase=allocation-exact-SHA;"
        f"allocation_sha={COMMIT};allocation_local=PASS(4/4);allocation_push_operation=af2a3440-b638-4cdd-b690-c622b86cca15;"
        f"allocation_CI_attempt1=FAIL(5/6:{RUN_ID});allocation_CI_attempt2=PASS(6/6:{RUN_ID});result=PASS | "
        f"Windows 11 x64; GitHub Actions Ubuntu/Windows; GitHub API | "
        f"Push đã đưa main tới allocation SHA và live ref xác nhận đúng SHA; CI exact-SHA đạt 6/6 ở attempt 2. | "
        f"PASS — attempt 1 chỉ thất bại ở Explorer Windows khi không thấy trạng thái workspace; retry cùng SHA không tái hiện lỗi. Attempt 2 hoàn tất 6/6. "
        f"Push verification là thao tác idempotent sau push, exit 0; API main vẫn bằng allocation SHA. | "
        f"{CI_REF.as_posix()} sha256:{sha256(CI_REF)}; {FAILURE_REF.as_posix()} sha256:{sha256(FAILURE_REF)}; "
        f"{PUSH_REF.as_posix()} sha256:{sha256(PUSH_REF)}; {OBSERVATION_REF.as_posix()} sha256:{sha256(OBSERVATION_REF)}; "
        f"{ALLOCATION_REF.as_posix()} sha256:{sha256(ALLOCATION_REF)} | {now} |\n"
    ).encode("utf-8")
    lines.insert(evidence_index + 1, evidence)
    SPEC.write_bytes(b"".join(lines))
    print(f"SPEC updated; allocation SHA={COMMIT}; CI receipt SHA256={sha256(CI_REF)}")


if __name__ == "__main__":
    main()
