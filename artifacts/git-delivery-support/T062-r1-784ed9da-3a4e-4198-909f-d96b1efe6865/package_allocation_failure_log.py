from __future__ import annotations

import hashlib
import json
import zipfile
from io import BytesIO
from pathlib import Path


ROOT = Path.cwd()
SUPPORT = Path("artifacts/git-delivery-support/T062-r1-784ed9da-3a4e-4198-909f-d96b1efe6865")
RAW = SUPPORT / "archive-view/ci/allocation-attempt-1-failure.log"
ARCHIVE = SUPPORT / "archive-view/ci/allocation-attempt-1-failure-log.zip"
SUMMARY = SUPPORT / "archive-view/ci/allocation/collector-summary.json"
MEMBER = "allocation-attempt-1-failure.log"


def write_once(path: Path, value: bytes) -> None:
    target = ROOT / path
    target.parent.mkdir(parents=True, exist_ok=True)
    if target.exists():
        if target.read_bytes() != value:
            raise SystemExit(f"preserving conflicting output: {path}")
        return
    with target.open("xb") as stream:
        stream.write(value)
        stream.flush()


raw = (ROOT / RAW).read_bytes()
buffer = BytesIO()
info = zipfile.ZipInfo(MEMBER, date_time=(1980, 1, 1, 0, 0, 0))
info.compress_type = zipfile.ZIP_DEFLATED
info.external_attr = 0o600 << 16
with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
    archive.writestr(info, raw)
archive_bytes = buffer.getvalue()
write_once(ARCHIVE, archive_bytes)
with zipfile.ZipFile(ROOT / ARCHIVE, "r") as archive:
    if archive.testzip() is not None or archive.read(MEMBER) != raw:
        raise SystemExit("compressed failure log failed CRC or byte-for-byte readback")

summary_path = ROOT / SUMMARY
summary = json.loads(summary_path.read_bytes())
attempt = summary["attempt_1"]
attempt.pop("failure_log_sha256", None)
attempt["failure_log_ref"] = ARCHIVE.as_posix()
attempt["failure_log_member"] = MEMBER
attempt["failure_log_content_sha256"] = hashlib.sha256(raw).hexdigest()
attempt["failure_log_archive_sha256"] = hashlib.sha256(archive_bytes).hexdigest()
summary_bytes = (json.dumps(summary, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
summary_path.write_bytes(summary_bytes)
print(
    json.dumps(
        {
            "archive_ref": ARCHIVE.as_posix(),
            "archive_sha256": hashlib.sha256(archive_bytes).hexdigest(),
            "raw_content_sha256": hashlib.sha256(raw).hexdigest(),
            "raw_bytes": len(raw),
            "summary_ref": SUMMARY.as_posix(),
            "summary_sha256": hashlib.sha256(summary_bytes).hexdigest(),
        },
        sort_keys=True,
    )
)
