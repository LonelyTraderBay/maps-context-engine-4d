from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

SUPPORT = Path(__file__).resolve().parent
ROOT = SUPPORT.parents[2]
DELIVERY = "36849008-7c57-4479-8bc8-89afb11dbcfa"
TASK = "T057"
REVISION = 1
PRIORITY = "P2"
BASE_SHA = "325da400611c1ff6e5a586d39ba14a3588b76587"
PLAN_SHA = "bd4b7b5ec234984d85f263b034fe279d7fc230a2fdebc688b026372c4a997bb1"
PLAN = SUPPORT / "check-plan-canonical.json"
MESSAGE = SUPPORT / "payload-commit-message.txt"
NODE_HOME = Path(r"C:\Users\Joker-PC\AppData\Local\mce4d4d\toolchains\T056-node-24.22.0\node-v24.22.0-win-x64")
NPM_CMD = NODE_HOME / "npm.cmd"
NODE_EXE = NODE_HOME / "node.exe"
CMD = Path(r"C:\Windows\System32\cmd.exe")
ATTEMPT = SUPPORT / "local-checks" / "payload-attempt-1"
CANONICAL = SUPPORT / "local-checks" / "payload"
UI_CHECKOUT = Path(os.environ.get("MCE4D_T057_UI_CHECKOUT", r"C:\Users\Joker-PC\AppData\Local\Temp\mce4dT057-npm-ci"))


def output(args: list[str], cwd: Path = ROOT) -> str:
    return subprocess.check_output(args, cwd=cwd, text=True, encoding="utf-8").strip()


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


def utc_text(value: datetime) -> str:
    return value.isoformat(timespec="microseconds").replace("+00:00", "Z")


def write_json(path: Path, value: object) -> bytes:
    encoded = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(encoded)
    return encoded


def main() -> int:
    if output(["git", "branch", "--show-current"]) != "codex/t056-r4":
        raise SystemExit("unexpected T057 branch")
    if output(["git", "rev-parse", "HEAD"]) != BASE_SHA:
        raise SystemExit("HEAD differs from the allocated T057 base")
    if subprocess.run(["git", "diff", "--quiet"], cwd=ROOT).returncode:
        raise SystemExit("tracked unstaged changes exist; preserve and inspect them first")
    if hashlib.sha256(PLAN.read_bytes()).hexdigest() != PLAN_SHA:
        raise SystemExit("frozen T057 check-plan hash mismatch")
    staged = set(output(["git", "diff", "--cached", "--name-only"]).splitlines())
    allowed = {
        "Maps-Context-Engine-4D_SPEC.md",
        "internal/analysis/providers/provider.go",
        "internal/analysis/providers/python_profile.go",
        "internal/analysis/providers/python_profile_test.go",
        "packages/python-provider/README.md",
        "packages/python-provider/package.json",
        "packages/python-provider/src/mce4d_python_provider/__init__.py",
        "packages/python-provider/src/mce4d_python_provider/analyze.py",
        "packages/python-provider/src/mce4d_python_provider/cli.py",
        "packages/python-provider/src/mce4d_python_provider/descriptor.py",
        "packages/python-provider/src/mce4d_python_provider/profile.py",
        "packages/python-provider/src/mce4d_python_provider/protocol.py",
        "packages/python-provider/src/mce4d_python_provider/python-profile.v1.schema.json",
        "packages/python-provider/test/test_provider.py",
        "packages/python-provider/test/test_schema.py",
    }
    if staged != allowed:
        raise SystemExit(f"staged payload differs from the reviewed scope: {sorted(staged ^ allowed)}")
    if ATTEMPT.exists() and any(ATTEMPT.iterdir()):
        raise SystemExit(f"preserving existing attempt evidence: {ATTEMPT}")
    if CANONICAL.exists() and any(path.is_file() for path in CANONICAL.rglob("*")):
        raise SystemExit(f"preserving existing canonical local-check evidence: {CANONICAL}")
    if not NODE_HOME.is_dir() or not NPM_CMD.is_file() or not NODE_EXE.is_file() or not CMD.is_file():
        raise SystemExit("pinned Node/npm toolchain is unavailable")
    if output([str(NODE_EXE), "--version"]) != "v24.22.0":
        raise SystemExit("Node runtime differs from the Explorer contract pin")
    if output([str(CMD), "/d", "/c", f"call {NPM_CMD} --version"]) != "11.19.1":
        raise SystemExit("npm runtime differs from the sealed workspace pin")
    if output(["git", "rev-parse", "HEAD"], cwd=UI_CHECKOUT) != BASE_SHA:
        raise SystemExit("isolated UI checkout is not bound to the current T057 base")
    ui_paths = ["package.json", "package-lock.json", "tools/ci/check-plan.json", ".github/workflows/bootstrap.yml", "tests/contract/ui", "web/explorer"]
    if subprocess.run(["git", "diff", "--quiet", "HEAD", "--", *ui_paths], cwd=ROOT).returncode:
        raise SystemExit("Explorer verification inputs differ from the isolated base checkout")
    if subprocess.run(["git", "diff", "--quiet", "HEAD", "--", *ui_paths], cwd=UI_CHECKOUT).returncode:
        raise SystemExit("isolated Explorer checkout contains modified verification inputs")

    tree = output(["git", "write-tree"])
    go = shutil.which("go")
    if not go:
        raise SystemExit("Go toolchain is unavailable")
    python = sys.executable
    plan_ref = str(PLAN.resolve())
    commands: list[tuple[str, str, list[str], Path, dict[str, str] | None]] = [
        ("bootstrap", "python -B tools/ci/bootstrap_check.py", [python, "-B", "tools/ci/bootstrap_check.py"], ROOT, None),
        ("bootstrap-fixtures", "python -B tools/ci/test_bootstrap_check.py", [python, "-B", "tools/ci/test_bootstrap_check.py"], ROOT, None),
        ("delivery-plan", "python -B tools/ci/delivery_policy.py check-plan", [python, "-B", "tools/ci/delivery_policy.py", "check-plan", "--plan", plan_ref, "--sha256", PLAN_SHA], ROOT, None),
        ("spec-check", "python -B tools/spec_check/check.py", [python, "-B", "tools/spec_check/check.py"], ROOT, None),
        ("spec-check-fixtures", "python -B tools/spec_check/test_spec_check.py", [python, "-B", "tools/spec_check/test_spec_check.py"], ROOT, None),
        ("delivery-policy-fixtures", "python -B tools/ci/test_delivery_policy.py", [python, "-B", "tools/ci/test_delivery_policy.py"], ROOT, None),
        ("checkpoint-digest-fixtures", "python -B tools/ci/test_checkpoint_digest.py", [python, "-B", "tools/ci/test_checkpoint_digest.py"], ROOT, None),
        ("go-format", "python -B tools/ci/check_gofmt.py", [python, "-B", "tools/ci/check_gofmt.py"], ROOT, None),
        ("go-test", "go test ./...", [go, "test", "./..."], ROOT, None),
        ("go-vet", "go vet ./...", [go, "vet", "./..."], ROOT, None),
        ("go-build", "go build ./...", [go, "build", "./..."], ROOT, None),
        ("explorer-install", "npm ci --ignore-scripts --no-audit --no-fund", [str(CMD), "/d", "/c", f"call {NPM_CMD} ci --ignore-scripts --no-audit --no-fund"], UI_CHECKOUT, None),
        ("explorer-contract", "npm run test:contract:ui", [str(CMD), "/d", "/c", f"call {NPM_CMD} run test:contract:ui"], UI_CHECKOUT, None),
        ("python-provider-tests", "python -B -m unittest discover -s packages/python-provider/test -v", [python, "-B", "-m", "unittest", "discover", "-s", "packages/python-provider/test", "-v"], ROOT, None),
        ("git-diff-check", "git diff --check --cached", ["git", "diff", "--check", "--cached"], ROOT, None),
    ]
    frozen_plan = json.loads(PLAN.read_text(encoding="utf-8"))
    expected_commands = {command for check in frozen_plan["checks"] for command in check["command_refs"]}
    if expected_commands != {command_ref for _, command_ref, _, _, _ in commands[:13]}:
        raise SystemExit("local command set differs from the frozen T057 check-plan")

    ATTEMPT.mkdir(parents=True, exist_ok=True)
    records_dir = ATTEMPT / "records"
    logs_dir = ATTEMPT / "outputs"
    records_dir.mkdir()
    logs_dir.mkdir()
    node_env = os.environ.copy()
    node_env["PATH"] = os.pathsep.join([str(NODE_HOME), r"C:\Windows\System32", r"C:\Windows"])
    node_env["ComSpec"] = str(CMD)
    node_env["npm_config_shell"] = str(CMD)
    all_records: list[dict[str, object]] = []
    began = utc_now()
    for index, (check_id, command_ref, argv, cwd, custom_env) in enumerate(commands, start=1):
        started = utc_now()
        timer = time.monotonic()
        environment = custom_env or (node_env if check_id.startswith("explorer-") else os.environ.copy())
        timed_out = False
        try:
            completed = subprocess.run(argv, cwd=cwd, env=environment, capture_output=True, timeout=900)
            exit_code = completed.returncode
            log_bytes = completed.stdout + b"\n--- STDERR ---\n" + completed.stderr
        except subprocess.TimeoutExpired as error:
            exit_code = 124
            timed_out = True
            log_bytes = (error.stdout or b"") + b"\n--- STDERR ---\n" + (error.stderr or b"") + b"\nTIMEOUT after 900 seconds\n"
        duration = round(time.monotonic() - timer, 3)
        result = "PASS" if exit_code == 0 and not timed_out else "FAIL"
        log_path = logs_dir / f"{index:02d}-{check_id}.log"
        log_path.write_bytes(log_bytes)
        log_ref = log_path.relative_to(ROOT).as_posix()
        record = {
            "format_version": 1,
            "kind": "local_check",
            "command_ref": command_ref,
            "task_id": TASK,
            "task_revision": REVISION,
            "delivery_id": DELIVERY,
            "phase": "payload",
            "revision": tree,
            "result": result,
            "execution_verified": True,
            "log_refs": [log_ref],
            "reused_from_ref": None,
        }
        attempt_record = records_dir / f"{check_id}.json"
        write_json(attempt_record, record)
        sys.path.insert(0, str(ROOT))
        from tools.spec_check.schema import validate_governance_file

        schema_errors = validate_governance_file(ROOT / "Maps-Context-Engine-4D_SPEC.md", attempt_record, "GitLocalCheckEvidence")
        if schema_errors:
            raise SystemExit(f"{check_id} evidence schema failed: {schema_errors}")
        canonical_record = CANONICAL / "records" / f"{check_id}.json"
        all_records.append({
            "check_id": check_id,
            "command_ref": command_ref,
            "cwd": str(cwd),
            "duration_seconds": duration,
            "started_at_utc": utc_text(started),
            "completed_at_utc": utc_text(utc_now()),
            "exit_code": exit_code,
            "timed_out": timed_out,
            "result": result,
            "output_ref": log_ref,
            "output_sha256": hashlib.sha256(log_bytes).hexdigest(),
            "attempt_record_ref": attempt_record.relative_to(ROOT).as_posix(),
            "attempt_record_sha256": hashlib.sha256(attempt_record.read_bytes()).hexdigest(),
            "record_ref": canonical_record.relative_to(ROOT).as_posix(),
            "record_sha256": hashlib.sha256(attempt_record.read_bytes()).hexdigest(),
        })
        print(f"[{index}/{len(commands)}] {check_id}: {result} ({duration:.1f}s)", flush=True)

    message = MESSAGE.read_text(encoding="utf-8")
    from tools.ci.delivery_policy import validate_commit_message

    message_errors = validate_commit_message(message, phase="payload", task_id=TASK, priority=PRIORITY)
    message_log = ATTEMPT / "outputs" / "commit-message-gate.log"
    message_log.write_text("PASS\n" if not message_errors else "FAIL\n" + "\n".join(message_errors) + "\n", encoding="utf-8")
    message_record = {
        "format_version": 1,
        "kind": "commit_message_gate",
        "task_id": TASK,
        "task_revision": REVISION,
        "delivery_id": DELIVERY,
        "phase": "payload",
        "message_ref": MESSAGE.relative_to(ROOT).as_posix(),
        "message_sha256": hashlib.sha256(MESSAGE.read_bytes()).hexdigest(),
        "result": "PASS" if not message_errors else "FAIL",
        "errors": message_errors,
        "output_ref": message_log.relative_to(ROOT).as_posix(),
        "output_sha256": hashlib.sha256(message_log.read_bytes()).hexdigest(),
    }
    write_json(ATTEMPT / "commit-message-gate.json", message_record)

    frozen_records = all_records[:13]
    passed_frozen = sum(item["result"] == "PASS" for item in frozen_records)
    passed_total = sum(item["result"] == "PASS" for item in all_records)
    overall = "PASS" if passed_frozen == 13 and passed_total == len(all_records) and not message_errors else "FAIL"
    completed_at = utc_now()
    summary = {
        "format_version": 1,
        "kind": "local_check_summary",
        "task_id": TASK,
        "task_revision": REVISION,
        "delivery_id": DELIVERY,
        "phase": "payload",
        "parent_sha": BASE_SHA,
        "candidate_tree_sha": tree,
        "check_plan_sha256": PLAN_SHA,
        "started_at_utc": utc_text(began),
        "completed_at_utc": utc_text(completed_at),
        "frozen_check_count": 13,
        "passed_frozen_checks": passed_frozen,
        "check_count": len(all_records),
        "passed_checks": passed_total,
        "failed_checks": len(all_records) - passed_total + (1 if message_errors else 0),
        "commit_message_gate": message_record,
        "explorer_install_equivalence": {
            "result": "PASS",
            "checkout_ref": str(UI_CHECKOUT),
            "checkout_sha": BASE_SHA,
            "unchanged_paths": ui_paths,
            "basis": "The T057 staged diff is outside the frozen Explorer package, workflow, plan, and test paths.",
        },
        "records": all_records,
        "result": overall,
    }
    summary_bytes = write_json(ATTEMPT / "summary.json", summary)
    if output(["git", "write-tree"]) != tree:
        raise SystemExit("staged candidate tree changed during validation")
    if overall == "PASS":
        for record in all_records[:13]:
            source = ATTEMPT / "records" / f"{record['check_id']}.json"
            destination = CANONICAL / "records" / f"{record['check_id']}.json"
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(source.read_bytes())
        (CANONICAL / "outputs").mkdir(parents=True, exist_ok=True)
        for record in all_records[:13]:
            source = ATTEMPT / "outputs" / Path(str(record["output_ref"])).name
            destination = CANONICAL / "outputs" / source.name
            destination.write_bytes(source.read_bytes())
        write_json(CANONICAL / "summary.json", summary)
    print(f"SUMMARY {overall}: frozen {passed_frozen}/13, total {passed_total}/{len(all_records)}, message gate {'PASS' if not message_errors else 'FAIL'}, tree {tree}, summary sha256 {hashlib.sha256(summary_bytes).hexdigest()}", flush=True)
    return 0 if overall == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
