from __future__ import annotations

import hashlib
from pathlib import Path


ROOT = Path.cwd()
SPEC = ROOT / "Maps-Context-Engine-4D_SPEC.md"
SUPPORT = Path("artifacts/git-delivery-support/T062-r1-784ed9da-3a4e-4198-909f-d96b1efe6865")
SUMMARY = SUPPORT / "archive-view/ci/allocation/collector-summary.json"
ARCHIVE = SUPPORT / "archive-view/ci/allocation-attempt-1-failure-log.zip"
RAW_SHA = "414905436e99ec43732c931853e09385ec1757ff648d87f52c8f1da0f2914f21"
OLD_SUMMARY_SHA = "1fae52c8660a0c91dc5dc81c3e7681d5099136ef19b0f653322464f813af2361"


def sha(path: Path) -> str:
    return hashlib.sha256((ROOT / path).read_bytes()).hexdigest()


def set_field(fields: list[str], key: str, value: str) -> None:
    prefix = key + "="
    for index, field in enumerate(fields):
        if field.startswith(prefix):
            fields[index] = prefix + value
            return
    fields.append(prefix + value)


text = SPEC.read_text(encoding="utf-8")
summary_sha = sha(SUMMARY)
archive_sha = sha(ARCHIVE)
old_artifact = (
    f"{SUPPORT.as_posix()}/archive-view/ci/allocation-attempt-1-failure.log sha256:{RAW_SHA}"
)
new_artifact = (
    f"{ARCHIVE.as_posix()} sha256:{archive_sha} (raw-content-sha256:{RAW_SHA})"
)
if old_artifact not in text or text.count(OLD_SUMMARY_SHA) != 2:
    raise SystemExit("expected SPEC allocation artifact references are missing or changed")
text = text.replace(old_artifact, new_artifact, 1).replace(OLD_SUMMARY_SHA, summary_sha)
lines = text.splitlines(keepends=True)
task_index = next((i for i, line in enumerate(lines) if line.startswith("| T062 |")), None)
if task_index is None:
    raise SystemExit("T062 task row is missing")
parts = lines[task_index].rstrip("\r\n").split("|")
fields = parts[9].split(";")
set_field(fields, "allocation_CI_sha256", summary_sha)
set_field(fields, "allocation_CI_log_archive_ref", ARCHIVE.as_posix())
set_field(fields, "allocation_CI_log_archive_sha256", archive_sha)
parts[9] = ";".join(fields)
eol = "\r\n" if lines[task_index].endswith("\r\n") else "\n"
lines[task_index] = "|".join(parts) + eol
SPEC.write_text("".join(lines), encoding="utf-8", newline="")
print(f"SPEC allocation CI references updated; summary={summary_sha}; archive={archive_sha}")
