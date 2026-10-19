"""Verify one critical cache guard using an isolated, disposable Git archive."""

from __future__ import annotations

import hashlib
import io
import json
import os
import subprocess
import tarfile
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
TARGET = Path("internal/indexer/invalidation.go")
TEST = "TestPlanIncrementalNeverNoopsOnIncompleteScope"
ORIGINAL = b"if !plan.InputScopeComplete {"
MUTANT = b"if false && !plan.InputScopeComplete {"


def _run_test(cwd: Path) -> subprocess.CompletedProcess[bytes]:
    environment = os.environ.copy()
    environment.update({"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off"})
    return subprocess.run(
        ["go", "test", "./internal/indexer", "-run", f"^{TEST}$", "-count=1"],
        cwd=cwd,
        env=environment,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )


def _extract_git_archive(destination: Path) -> None:
    archive = subprocess.run(
        ["git", "archive", "--format=tar", "HEAD"],
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if archive.returncode:
        raise RuntimeError(f"git archive failed: {archive.stderr.decode('utf-8', errors='replace')}")
    with tarfile.open(fileobj=io.BytesIO(archive.stdout), mode="r:") as bundle:
        members = bundle.getmembers()
        for member in members:
            path = Path(member.name)
            if path.is_absolute() or ".." in path.parts or member.issym() or member.islnk():
                raise RuntimeError(f"unsafe member in own source archive: {member.name}")
        bundle.extractall(destination, members=members, filter="data")


def run_mutation_probe() -> dict[str, object]:
    source_sha = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, capture_output=True, check=True
    ).stdout.strip()
    baseline = _run_test(ROOT)
    with tempfile.TemporaryDirectory(prefix="mce4d-t034-mutation-") as temporary:
        copy = Path(temporary)
        _extract_git_archive(copy)
        target = copy / TARGET
        source = target.read_bytes()
        if source.count(ORIGINAL) != 1:
            raise RuntimeError("expected exactly one incomplete-scope guard in archived source")
        mutated = source.replace(ORIGINAL, MUTANT, 1)
        target.write_bytes(mutated)
        mutant = _run_test(copy)
        result = {
            "format_version": 1,
            "kind": "targeted_mutation_probe",
            "source_sha": source_sha,
            "guard_path": TARGET.as_posix(),
            "test": TEST,
            "baseline_exit_code": baseline.returncode,
            "baseline_log_sha256": hashlib.sha256(baseline.stdout).hexdigest(),
            "mutated_source_sha256": hashlib.sha256(mutated).hexdigest(),
            "mutant_exit_code": mutant.returncode,
            "mutant_log_sha256": hashlib.sha256(mutant.stdout).hexdigest(),
            "expected": "baseline_pass_and_mutant_fail",
            "result": "PASS" if baseline.returncode == 0 and mutant.returncode != 0 else "FAIL",
        }
    if result["result"] != "PASS":
        result["baseline_output"] = baseline.stdout.decode("utf-8", errors="replace")[-4000:]
        result["mutant_output"] = mutant.stdout.decode("utf-8", errors="replace")[-4000:]
    return result


def main() -> int:
    try:
        result = run_mutation_probe()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(json.dumps({"kind": "targeted_mutation_probe", "result": "FAIL", "error": str(error)}, ensure_ascii=False))
        return 1
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    return 0 if result["result"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
