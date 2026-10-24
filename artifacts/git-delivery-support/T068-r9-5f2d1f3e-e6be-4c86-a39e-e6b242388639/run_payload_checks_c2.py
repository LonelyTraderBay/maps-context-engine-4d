from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SUPPORT = Path(__file__).resolve().parent
OUT = SUPPORT / "local-checks" / "payload" / "exact-C2b"
RECORDS = OUT / "records"
TASK_TEMP = Path(r"C:\Users\Joker-PC\Documents\Codex\Work\maps-context-engine-4d\audit-20261023-01a16846\work\tmp\T068-r9-p3")
SUBJECT_SHA = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
DELIVERY_ID = "5f2d1f3e-e6be-4c86-a39e-e6b242388639"
TASK_ID = "T068"
TASK_REVISION = 7
PYTHON = Path(r"C:\Users\Joker-PC\.cache\codex-runtimes\codex-primary-runtime\dependencies\python\python.exe")
GO = Path(r"C:\Users\Joker-PC\Documents\Codex\Work\maps-context-engine-4d\audit-20261023-01a16846\work\toolchains-20261024\go\bin\go.exe")
NPM = Path(r"C:\Users\Joker-PC\Documents\Codex\Work\maps-context-engine-4d\audit-20261023-01a16846\work\toolchains-20261024\node-v24.22.0-win-x64\npm.cmd")


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_json(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")


def main() -> int:
    if not ROOT.is_dir() or ROOT != Path(subprocess.check_output(["git", "rev-parse", "--show-toplevel"], cwd=ROOT, text=True).strip()):
        raise SystemExit("repository root mismatch")
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    if head != SUBJECT_SHA:
        raise SystemExit(f"HEAD mismatch: {head} != {SUBJECT_SHA}")
    if OUT.exists():
        raise SystemExit(f"refusing to overwrite existing output: {OUT}")
    for executable in (PYTHON, GO, NPM):
        if not executable.is_file():
            raise SystemExit(f"required runtime missing: {executable}")

    RECORDS.mkdir(parents=True)
    for directory in (TASK_TEMP / "tmp", TASK_TEMP / "npm-cache", TASK_TEMP / "go-build", TASK_TEMP / "go-mod"):
        directory.mkdir(parents=True, exist_ok=True)

    env = os.environ.copy()
    env.update(
        {
            "TMP": str(TASK_TEMP / "tmp"),
            "TEMP": str(TASK_TEMP / "tmp"),
            "TMPDIR": str(TASK_TEMP / "tmp"),
            "npm_config_cache": str(TASK_TEMP / "npm-cache"),
            "GOCACHE": str(TASK_TEMP / "go-build"),
            "GOMODCACHE": str(TASK_TEMP / "go-mod"),
        }
    )
    env["PATH"] = os.pathsep.join([str(GO.parent), str(NPM.parent), str(PYTHON.parent), env.get("PATH", "")])
    env["GIT_CONFIG_COUNT"] = "1"
    env["GIT_CONFIG_KEY_0"] = "core.longpaths"
    env["GIT_CONFIG_VALUE_0"] = "true"

    checks = [
        ("bootstrap", "python -B tools/ci/bootstrap_check.py", [str(PYTHON), "-B", "tools/ci/bootstrap_check.py"], False),
        ("bootstrap-fixtures", "python -B tools/ci/test_bootstrap_check.py", [str(PYTHON), "-B", "tools/ci/test_bootstrap_check.py"], False),
        ("delivery-plan", "python -B tools/ci/delivery_policy.py check-plan", [str(PYTHON), "-B", "tools/ci/delivery_policy.py", "check-plan"], False),
        ("spec-check", "python -B tools/spec_check/check.py", [str(PYTHON), "-B", "tools/spec_check/check.py"], False),
        ("spec-check-fixtures", "python -B tools/spec_check/test_spec_check.py", [str(PYTHON), "-B", "tools/spec_check/test_spec_check.py"], False),
        ("delivery-policy-fixtures", "python -B tools/ci/test_delivery_policy.py", [str(PYTHON), "-B", "tools/ci/test_delivery_policy.py"], False),
        ("checkpoint-digest-fixtures", "python -B tools/ci/test_checkpoint_digest.py", [str(PYTHON), "-B", "tools/ci/test_checkpoint_digest.py"], False),
        ("go-format", "python -B tools/ci/check_gofmt.py", [str(PYTHON), "-B", "tools/ci/check_gofmt.py"], False),
        ("go-test", "go test ./...", [str(GO), "test", "./..."], False),
        ("go-vet", "go vet ./...", [str(GO), "vet", "./..."], False),
        ("go-build", "go build ./...", [str(GO), "build", "./..."], False),
        ("explorer-install", "npm ci --ignore-scripts --no-audit --no-fund", [str(NPM), "ci", "--ignore-scripts", "--no-audit", "--no-fund"], True),
        ("explorer-contract", "npm run test:contract:ui", [str(NPM), "run", "test:contract:ui"], True),
    ]

    records: list[dict[str, object]] = []
    failed = 0
    for check_id, command_ref, argv, use_cmd_shell in checks:
        record_ref = f"{SUPPORT.relative_to(ROOT).as_posix()}/local-checks/payload/exact-C2b/records/{check_id}.json"
        log_ref = f"{SUPPORT.relative_to(ROOT).as_posix()}/local-checks/payload/exact-C2b/records/{check_id}.log"
        record_path = ROOT / Path(record_ref)
        log_path = ROOT / Path(log_ref)
        started = utc_now()
        start = time.monotonic()
        check_env = env.copy()
        if check_id == "spec-check-fixtures":
            # This fixture builds deeply nested archive paths; use the approved
            # short task temp alias for Python's Windows path-length boundary.
            short_temp = r"R:\T068-r8-P2\tmp\T068-r9-p3\tmp"
            check_env.update({"TMP": short_temp, "TEMP": short_temp, "TMPDIR": short_temp})
        if use_cmd_shell:
            shell_command = '"' + argv[0] + '" ' + " ".join(argv[1:])
            completed = subprocess.run(shell_command, cwd=ROOT, env=check_env, shell=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        else:
            completed = subprocess.run(argv, cwd=ROOT, env=check_env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        elapsed = round(time.monotonic() - start, 3)
        log_path.write_bytes(completed.stdout)
        record = {
            "argv": argv,
            "command_ref": command_ref,
            "check_id": check_id,
            "cwd": str(ROOT),
            "delivery_id": DELIVERY_ID,
            "elapsed_seconds": elapsed,
            "ended_at_utc": utc_now(),
            "exit_code": completed.returncode,
            "format_version": 1,
            "kind": "local_check_execution",
            "log_ref": log_ref,
            "log_sha256": sha256(log_path),
            "record_ref": record_ref,
            "result": "PASS" if completed.returncode == 0 else "FAIL",
            "started_at_utc": started,
            "subject_sha": SUBJECT_SHA,
            "task_id": TASK_ID,
            "task_revision": TASK_REVISION,
        }
        write_json(record_path, record)
        records.append({"record_ref": record_ref, "sha256": sha256(record_path)})
        if completed.returncode != 0:
            failed += 1
        print(f"{check_id}: {record['result']} ({elapsed:.3f}s)", flush=True)
        if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip() != SUBJECT_SHA:
            raise SystemExit("HEAD changed during local check run")

    summary = {
        "check_refs": records,
        "delivery_id": DELIVERY_ID,
        "failed_count": failed,
        "format_version": 1,
        "kind": "local_check_summary",
        "passed_count": len(records) - failed,
        "result": "PASS" if failed == 0 else "FAIL",
        "subject_sha": SUBJECT_SHA,
        "task_id": TASK_ID,
        "task_revision": TASK_REVISION,
    }
    write_json(OUT / "summary.json", summary)
    print(f"summary: {summary['result']} ({summary['passed_count']}/{len(records)})", flush=True)
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
