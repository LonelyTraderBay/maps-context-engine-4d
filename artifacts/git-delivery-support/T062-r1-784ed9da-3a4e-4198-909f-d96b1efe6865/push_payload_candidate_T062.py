from __future__ import annotations

import hashlib
import json
import subprocess
import uuid
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path.cwd().resolve()
TASK = "T062"
DELIVERY = "784ed9da-3a4e-4198-909f-d96b1efe6865"
HEAD = "a342bd77ba607565c8a8411b9177d55253e7a4a1"
PARENT = "43f11e50525f4317ad6f0dcee3e9ea216a503711"
OWNER = "LonelyTraderBay"
REPO = "maps-context-engine-4d"
REPO_ID = 1421031011
SUPPORT = Path(f"artifacts/git-delivery-support/T062-r1-{DELIVERY}")
OUT = SUPPORT / "archive-view/operations/payload-candidate-push"


def now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")


def canonical(value: object) -> bytes:
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")


def write(path: Path, data: bytes) -> None:
    path = ROOT / path
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        if path.read_bytes() != data:
            raise SystemExit(f"preserving conflicting evidence: {path}")
        return
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()


def api(path: str) -> tuple[bytes, dict, str, str]:
    started = now()
    result = subprocess.run(["gh", "api", path], cwd=ROOT, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    finished = now()
    return result.stdout, json.loads(result.stdout), started, finished


def main() -> None:
    def out(*args: str) -> str:
        return subprocess.run([*args], cwd=ROOT, check=True, stdout=subprocess.PIPE).stdout.decode("utf-8", "strict").strip()

    if out("git", "rev-parse", "HEAD") != HEAD or out("git", "rev-parse", "HEAD^") != PARENT:
        raise SystemExit("candidate or direct parent changed")
    remote_url = out("git", "config", "--local", "--get", "remote.origin.url")
    if remote_url != f"https://github.com/{OWNER}/{REPO}.git":
        raise SystemExit("remote URL differs from approved repository")
    remote_config = out("git", "config", "--local", "--get-regexp", r"^remote\.origin\.(url|fetch)$") + "\n"
    remote_digest = hashlib.sha256(remote_config.encode()).hexdigest()

    repo_raw, repo, repo_start, repo_end = api(f"repos/{OWNER}/{REPO}")
    ref_raw, ref, ref_start, ref_end = api(f"repos/{OWNER}/{REPO}/git/ref/heads/main")
    if repo.get("id") != REPO_ID or repo.get("full_name") != f"{OWNER}/{REPO}" or repo.get("visibility") != "public":
        raise SystemExit("fresh repository identity/visibility mismatch")
    remote_sha = ref.get("object", {}).get("sha")
    if remote_sha != PARENT:
        raise SystemExit(f"remote main moved; expected {PARENT}, observed {remote_sha}")

    op_id = str(uuid.uuid4())
    life_id = str(uuid.uuid4())
    query_id = str(uuid.uuid4())
    repository = {
        "host": "github.com",
        "name": REPO,
        "owner": OWNER,
        "publication_authority_ref": f"{SUPPORT.as_posix()}/authority/task-start.txt",
        "ref": "refs/heads/main",
        "remote_name": "origin",
        "remote_url_sanitized": remote_url,
        "repository_id": REPO_ID,
        "visibility": "public",
    }
    pre_query_ref = OUT / "raw/preflight-query-projection.json"
    write(pre_query_ref, canonical({
        "kind": "destination_query_projection", "phase": "preflight", "query_id": query_id,
        "lifecycle_id": life_id, "operation_id": op_id, "repository": repo, "ref": ref,
        "repo_query_started_at_utc": repo_start, "repo_query_completed_at_utc": repo_end,
        "ref_query_started_at_utc": ref_start, "ref_query_completed_at_utc": ref_end,
        "remote_config_digest": remote_digest,
    }))
    write(OUT / "raw/preflight-repository.json", repo_raw)
    write(OUT / "raw/preflight-ref.json", ref_raw)
    started = now()
    argv = ["git", "push", "--porcelain", "origin", f"{HEAD}:refs/heads/main"]
    pushed = subprocess.run(argv, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
    completed = now()
    write(OUT / "push-output.txt", pushed.stdout)

    post_repo_raw, post_repo, post_repo_start, post_repo_end = api(f"repos/{OWNER}/{REPO}")
    post_ref_raw, post_ref, post_ref_start, post_ref_end = api(f"repos/{OWNER}/{REPO}/git/ref/heads/main")
    post_sha = post_ref.get("object", {}).get("sha")
    post_query_id = str(uuid.uuid4())
    post_ref_name = f"{OUT.as_posix()}/raw/postflight-query-projection.json"
    write(ROOT / post_ref_name, canonical({
        "kind": "destination_query_projection", "phase": "postflight", "query_id": post_query_id,
        "lifecycle_id": life_id, "operation_id": op_id, "repository": post_repo, "ref": post_ref,
        "repo_query_started_at_utc": post_repo_start, "repo_query_completed_at_utc": post_repo_end,
        "ref_query_started_at_utc": post_ref_start, "ref_query_completed_at_utc": post_ref_end,
        "remote_config_digest": remote_digest,
    }))
    write(OUT / "raw/postflight-repository.json", post_repo_raw)
    write(OUT / "raw/postflight-ref.json", post_ref_raw)
    if pushed.returncode or post_sha != HEAD or post_repo.get("id") != REPO_ID:
        raise SystemExit(f"push/postflight mismatch: exit={pushed.returncode} remote={post_sha}")

    write(OUT / "destination-observation.json", canonical({
        "candidate_sha": HEAD, "format_version": 1, "identity_verified": True,
        "local_remote_config_digest": remote_digest, "observed_at_utc": ref_end,
        "push_operation_id": op_id, "push_started_at_utc": started,
        "repository": repository, "visibility_verified": True,
    }))
    write(OUT / "push-command-source.json", canonical({
        "argv": argv, "credential_material_recorded": False, "executable": "git",
        "format_version": 1, "git_version": out("git", "--version"),
        "kind": "push_command_source", "working_directory_ref": ".",
    }))
    write(OUT / "remote-config.txt", remote_config.encode("utf-8"))
    operation = {
        "candidate_sha": HEAD,
        "format_version": 1,
        "kind": "push_operation",
        "lifecycle_id": life_id,
        "operation_id": op_id,
        "preflight": {"observed_remote_sha": remote_sha, "query_ref": pre_query_ref.as_posix(), "repository": repository},
        "push": {"argv": argv, "exit_code": pushed.returncode, "started_at_utc": started, "completed_at_utc": completed, "output_ref": (OUT / "push-output.txt").as_posix()},
        "postflight": {"observed_remote_sha": post_sha, "query_ref": post_ref_name, "repository_id": post_repo.get("id")},
        "remote_config_digest": remote_digest,
        "task_id": TASK,
        "task_revision": 1,
    }
    operation_bytes = canonical(operation)
    write(OUT / "push-operation.json", operation_bytes)
    print(json.dumps({"result": "PASS", "head": HEAD, "parent": PARENT, "remote": post_sha,
                      "operation_ref": (OUT / "push-operation.json").as_posix(),
                      "operation_sha256": hashlib.sha256(operation_bytes).hexdigest()}, sort_keys=True))


if __name__ == "__main__":
    main()
