"""Run the frozen T034 acceptance matrix and retain command logs by revision."""

from __future__ import annotations

import argparse
import hashlib
import json
import platform
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MATRIX = ROOT / "artifacts" / "acceptance" / "T034" / "matrix.json"
RUNS = ROOT / "artifacts" / "acceptance" / "T034" / "runs"


def _run_version(command: list[str]) -> str:
    result = subprocess.run(command, cwd=ROOT, text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f"cannot read tool version for {command[0]}: {result.stderr.strip()}")
    return result.stdout.strip()


def run_matrix(report_dir: Path) -> int:
    matrix_bytes = MATRIX.read_bytes()
    matrix = json.loads(matrix_bytes)
    if matrix.get("task_id") != "T034" or matrix.get("format_version") != 1:
        raise ValueError("matrix identity or format version is invalid")

    report_root = report_dir.resolve()
    if not report_root.is_relative_to(RUNS.resolve()):
        raise ValueError("report directory must remain under artifacts/acceptance/T034/runs")

    sha = _run_version(["git", "rev-parse", "HEAD"])
    diff = subprocess.run(["git", "diff", "--quiet", "HEAD", "--"], cwd=ROOT, check=False)
    if diff.returncode != 0:
        raise ValueError("tracked source changes exist; commit the tested source before recording revision evidence")
    report_root.mkdir(parents=True, exist_ok=False)

    command_results: dict[str, dict[str, object]] = {}
    for command_id, command in matrix["commands"].items():
        if not isinstance(command, list) or not command or command[0] not in {"go", "python"}:
            raise ValueError(f"command {command_id!r} is not a fixed Go/Python argv list")
        argv = list(command)
        if argv[:2] == ["go", "test"]:
            if "-count=1" not in argv:
                argv.append("-count=1")
            if "-v" not in argv:
                argv.append("-v")
        if argv[0] == "go" and argv[1:2] != ["test"]:
            raise ValueError(f"command {command_id!r} must invoke go test directly")
        if argv[0] == "python" and tuple(argv[1:]) not in {
            ("-B", "tools/ci/test_bootstrap_check.py"),
            ("-B", "tools/ci/test_delivery_policy.py"),
            ("-B", "tools/spec_check/test_spec_check.py"),
            ("-B", "tests/acceptance/mutation_probe.py"),
        }:
            raise ValueError(f"command {command_id!r} is outside the fixed Python fixture allowlist")
        log_path = report_root / f"{command_id}.log"
        started = datetime.now(timezone.utc).isoformat()
        with log_path.open("xb") as log:
            result = subprocess.run(argv, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, check=False)
        log_bytes = log_path.read_bytes()
        command_results[command_id] = {
            "argv": argv,
            "started_at_utc": started,
            "exit_code": result.returncode,
            "log": log_path.relative_to(ROOT).as_posix(),
            "log_bytes": len(log_bytes),
            "log_sha256": hashlib.sha256(log_bytes).hexdigest(),
        }
        print(f"[{command_id}] exit={result.returncode} log={log_path.relative_to(ROOT).as_posix()}")

    all_logs = "\n".join(
        (ROOT / str(result["log"])).read_text(encoding="utf-8", errors="replace")
        for result in command_results.values()
    )
    cases = []
    failures = 0
    for item in matrix["cases"]:
        command_ids = [item["command"], *item.get("additional_commands", [])]
        passed = all(command_results[command_id]["exit_code"] == 0 for command_id in command_ids)
        test_refs = list(dict.fromkeys([item["fixture"], *item["evidence"]]))
        test_results = [{"ref": ref, "result": _test_ref_result(ref, all_logs)} for ref in test_refs]
        passed = passed and all(test["result"] == "PASS" for test in test_results)
        if not passed:
            failures += 1
        if not passed:
            status = "FAIL"
        elif item["coverage"] == "limited":
            status = "PASS_LIMITED"
        else:
            status = "PASS"
        cases.append({
            "id": item["id"],
            "command_ids": command_ids,
            "result": status,
            "fixture": item["fixture"],
            "evidence": item["evidence"],
            "test_results": test_results,
            "independent_oracle": item["oracle"],
            "limitation": item["limitation"],
        })

    report = {
        "format_version": 1,
        "kind": "acceptance_matrix_run",
        "task_id": "T034",
        "spec_version": matrix["spec_version"],
        "subject_sha": sha,
        "tracked_worktree_clean": True,
        "environment": {
            "platform": platform.platform(),
            "python": _run_version([sys.executable, "--version"]),
            "go": _run_version(["go", "version"]),
            "git": _run_version(["git", "--version"]),
        },
        "matrix_sha256": hashlib.sha256(matrix_bytes).hexdigest(),
        "command_results": command_results,
        "case_results": cases,
        "counts": {
            "cases": len(cases),
            "pass": sum(item["result"] == "PASS" for item in cases),
            "pass_limited": sum(item["result"] == "PASS_LIMITED" for item in cases),
            "fail": failures,
        },
    }
    report_path = report_root / "report.json"
    report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")
    print(f"report={report_path.relative_to(ROOT).as_posix()}")
    print(f"counts={json.dumps(report['counts'], sort_keys=True)}")
    return 1 if failures else 0


def _test_ref_result(reference: str, logs: str) -> str:
    _, separator, symbol = reference.partition("#")
    if not separator or not symbol:
        return "MISSING"
    if symbol == "run_mutation_probe":
        matches = re.findall(r'"kind":\s*"targeted_mutation_probe"[^\r\n]*"result":\s*"(PASS|FAIL)"', logs)
        return "PASS" if "PASS" in matches else "FAIL" if matches else "MISSING"
    if symbol.startswith("Test"):
        match = re.search(rf"(?m)^--- (PASS|SKIP|FAIL): {re.escape(symbol)}(?:/|\s|$)", logs)
        return match.group(1) if match else "MISSING"
    if symbol.startswith("Benchmark"):
        match = re.search(rf"(?m)^{re.escape(symbol)}-\d+\s", logs)
        return "PASS" if match else "MISSING"
    if symbol.startswith("test_"):
        matches = re.findall(rf"(?m)^\s*{re.escape(symbol)}\s+\([^\r\n]*?\)\s+\.\.\.\s+(ok|skipped|ERROR|FAIL)", logs)
        if not matches:
            return "MISSING"
        return "PASS" if all(value == "ok" for value in matches) else "SKIP" if "skipped" in matches else "FAIL"
    return "MISSING"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report-dir", required=True, type=Path, help="new evidence folder below artifacts/acceptance/T034/runs")
    args = parser.parse_args()
    try:
        return run_matrix(args.report_dir if args.report_dir.is_absolute() else ROOT / args.report_dir)
    except (OSError, ValueError, RuntimeError, json.JSONDecodeError) as error:
        print(f"acceptance matrix failed: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
