from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path


ROOT = Path(__file__).resolve().parent
REPOSITORIES = {
    "alpha": {
        "module": "synthetic/polyglot/alpha",
        "languages": ["go", "json", "python", "sql", "typescript", "yaml"],
        "dependencies": ["package.json", "python/pyproject.toml", "python/requirements.txt"],
        "files": {
            "package.json": '{"name":"synthetic-alpha","private":true,"dependencies":{"typescript":"5.9.3"}}\n',
            "tsconfig.json": '{"compilerOptions":{"baseUrl":".","paths":{"@/*":["src/*"]}},"references":[{"path":"./tsconfig.shared.json"}]}\n',
            "tsconfig.shared.json": '{"compilerOptions":{"composite":true,"strict":true}}\n',
            "src/client.ts": 'import type { Result } from "@/types";\nexport type Plugin = any;\nexport function fetchResult(): Result { return { id: "x" }; }\n',
            "src/index.ts": 'export { fetchResult } from "./client.js";\n',
            "src/types.ts": 'export interface Result { id: string }\n',
            "src/plugin.config.ts": '// Synthetic plugin config: inspect as data; never import or execute.\nexport const pluginName = "fixture-plugin";\n',
            "web/routes.tsx": 'export const route = { service: "frontend", path: "/health" };\n',
            "backend/routes.go": 'package backend\nconst Route = "/health"\nconst Service = "orders-api"\n',
            "python/decorators.py": '@route("/health")\ndef handler():\n    return importlib.import_module("optional.module")\n',
            "python/pyproject.toml": '[project]\nname = "synthetic-alpha"\nrequires-python = ">=3.12"\n',
            "python/requirements.txt": "# Synthetic dependency boundary; no installation is part of this fixture.\n",
            "notebooks/workflow.ipynb": json.dumps({"cells": [{"cell_type": "code", "metadata": {}, "outputs": [], "execution_count": None, "source": ["Path('fixture-marker').write_text('executed')"]}], "metadata": {"kernelspec": {"name": "python3"}}, "nbformat": 4, "nbformat_minor": 5}, ensure_ascii=False, separators=(",", ":")) + "\n",
            "db/schema.sql": "-- MySQL fixture: declared only; never applied to a database.\nCREATE PROCEDURE refresh_item() BEGIN SET @q = CONCAT('SELECT ', @column_name); PREPARE stmt FROM @q; END;\n",
        },
    },
    "beta": {
        "module": "synthetic/polyglot/beta",
        "languages": ["go", "json", "protobuf", "yaml"],
        "dependencies": ["go.mod"],
        "files": {
            "go.mod": "module synthetic/polyglot/beta\n\ngo 1.27.0\n",
            "contracts/openapi.yaml": "openapi: 3.1.0\ninfo: {title: Beta, version: '1.0'}\npaths: {}\ncomponents:\n  schemas:\n    Item:\n      $ref: './schemas/item.json'\n    Remote:\n      $ref: 'https://example.invalid/schema.json'\n",
            "contracts/schemas/item.json": '{"type":"object","properties":{"id":{"type":"string"}}}\n',
            "contracts/messages.proto": 'syntax = "proto3";\npackage beta.v1;\nmessage Item { string id = 1; }\n',
            "events/asyncapi.yaml": "asyncapi: 3.0.0\ninfo: {title: Beta events, version: '1.0'}\nservers:\n  prod: {host: broker-a, protocol: kafka}\n  staging: {host: broker-b, protocol: kafka}\nchannels:\n  orders: {address: orders, servers: [prod]}\n",
            "workflow/state-machine.json": '{"status":"proposed","states":["pending","approved","stale"],"guard":"python: assert account == active","action":"publish"}\n',
            "federation/snapshot.json": '{"namespace":"tenant-beta","members":[{"id":"fresh","status":"available"},{"id":"denied","status":"denied"},{"id":"old","status":"stale"}],"generated":false}\n',
            "snapshots/base.json": '{"provider":"v1","symbols":[{"id":"node-a","name":"Order"}]}\n',
            "snapshots/current.json": '{"provider":"v2","symbols":[{"id":"node-b","name":"Order"}]}\n',
        },
    },
    "gamma": {
        "module": "synthetic/polyglot/gamma",
        "languages": ["go", "json", "yaml"],
        "dependencies": ["go.mod"],
        "files": {
            "go.mod": "module synthetic/polyglot/gamma\n\ngo 1.27.0\n",
            "trading/reserve.json": '{"reserve":"r1","events":["retry","partial_fill","cancel"],"ledger":"pending","precision":"unspecified"}\n',
            "commerce/webhook.json": '{"events":[{"id":"e1","sequence":2,"kind":"partial_refund"},{"id":"e1","sequence":2,"kind":"duplicate"},{"id":"e0","sequence":1,"kind":"payment"}],"order":"open","payment":"partial","stock":"held"}\n',
            "ai/model-card.json": '{"prompt":"p2","tokenizer":"tok-b","embedding_dimension":1536,"dataset":"d2","evaluation":"e1","checkpoint":{"uri":"opaque://fixture","load":false},"weights":"NOT_INCLUDED"}\n',
            "architecture/allowed-dependencies.json": '{"rules":[{"from":"api","to":"domain","allowed":true},{"from":"api","to":"storage","allowed":false}],"coverage":{"api":true,"provider-x":false},"exemptions":[]}\n',
            "benchmark/cohort.json": '{"modes":["baseline","full","incremental"],"source":"synthetic","real_repository_result":"NOT_RUN","tasks":30,"repositories":3,"repeats":3,"seed":68055}\n',
        },
    },
}

CASES = [
    ("C055", "typescript", "src/client.ts", "contract_preservation"),
    ("C056", "frontend-backend", "web/routes.tsx", "contract_preservation"),
    ("C057", "python", "python/decorators.py", "behavior_bug"),
    ("C058", "python-notebook", "notebooks/workflow.ipynb", "dirty_worktree"),
    ("C059", "sql", "db/schema.sql", "contract_preservation"),
    ("C060", "http-schema-proto", "contracts/openapi.yaml", "missing_dependency"),
    ("C061", "events", "events/asyncapi.yaml", "large_fanout"),
    ("C062", "state-machine", "workflow/state-machine.json", "behavior_bug"),
    ("C063", "federation", "federation/snapshot.json", "large_fanout"),
    ("C064", "temporal-diff", "snapshots/current.json", "type_change"),
    ("C065", "trading", "trading/reserve.json", "behavior_bug"),
    ("C066", "commerce", "commerce/webhook.json", "interface_change"),
    ("C067", "ai-lineage", "ai/model-card.json", "contract_preservation"),
    ("C068", "architecture", "architecture/allowed-dependencies.json", "behavior_bug"),
    ("C069", "benchmark", "benchmark/cohort.json", "large_fanout"),
]


def normalize(data: bytes) -> bytes:
    try:
        data.decode("utf-8")
    except UnicodeDecodeError:
        return data
    if b"\0" in data:
        return data
    return data.replace(b"\r\n", b"\n")


def tree_sha(root: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted((p for p in root.rglob("*") if p.is_file()), key=lambda p: p.relative_to(root).as_posix()):
        relative = path.relative_to(root).as_posix()
        content_sha = hashlib.sha256(normalize(path.read_bytes())).hexdigest()
        digest.update(relative.encode("utf-8")); digest.update(b"\0")
        digest.update(content_sha.encode("ascii")); digest.update(b"\n")
    return digest.hexdigest()


def dependency_sha(root: Path, files: list[str]) -> str:
    if len(files) == 1:
        return hashlib.sha256(normalize((root / files[0]).read_bytes())).hexdigest()
    digest = hashlib.sha256()
    for relative in sorted(files):
        content_sha = hashlib.sha256(normalize((root / relative).read_bytes())).hexdigest()
        digest.update(relative.encode("utf-8")); digest.update(b"\0")
        digest.update(content_sha.encode("ascii")); digest.update(b"\n")
    return digest.hexdigest()


def setup_sha(files: dict[str, str]) -> str:
    digest = hashlib.sha256()
    for relative in sorted(files):
        content_sha = hashlib.sha256(files[relative].encode("utf-8")).hexdigest()
        digest.update(relative.encode("utf-8")); digest.update(b"\0")
        digest.update(content_sha.encode("ascii")); digest.update(b"\n")
    return digest.hexdigest()


def build(output: Path) -> dict:
    if output.exists() and any(output.iterdir()):
        raise SystemExit(f"Refuse to overwrite non-empty output directory: {output}")
    output.mkdir(parents=True, exist_ok=True)
    fixture_root = output / "fixtures"
    repo_rows = []
    for repo_id, repo in REPOSITORIES.items():
        root = fixture_root / repo_id
        root.mkdir(parents=True, exist_ok=False)
        for relative, content in repo["files"].items():
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8", newline="\n")
        fixture_sha = tree_sha(root)
        repo_rows.append({
            "id": repo_id,
            "module": repo["module"],
            "languages": repo["languages"],
            "fixture_path": f"fixtures/{repo_id}",
            "fixture_revision": fixture_sha,
            "fixture_sha256": fixture_sha,
            "dependency_sha256": dependency_sha(root, repo["dependencies"]),
            "dependency_files": repo["dependencies"],
        })

    tasks = []
    for index, (case_id, domain, path, scenario) in enumerate(CASES):
        repo_id = ("alpha", "beta", "gamma")[index // 5]
        for sample in ("a", "b"):
            task_id = f"{case_id.lower()}-{sample}"
            expected = [f"{task_id}-binding", f"{task_id}-boundary"]
            setup_files: dict[str, str] = {}
            tasks.append({
                "id": task_id,
                "repository_id": repo_id,
                "case_id": case_id,
                "domain": domain,
                "scenario": scenario,
                "request": f"Evaluate the independently listed {case_id} facts for {domain} case {sample}.",
                "expected_changed_paths": [path],
                "forbidden_changed_paths": REPOSITORIES[repo_id]["dependencies"],
                "required_invariants": [
                    "Không thực thi plugin, cell, guard, migration, model hoặc fixture source.",
                    "Giữ namespace và boundary được ghi trong fixture.",
                    "So sánh với fact IDs độc lập; engine output không phải oracle.",
                ],
                "oracle": "Expected fact IDs in this suite are the independent oracle; reported IDs are measured inputs only.",
                "expected_fact_ids": expected,
                "critical_fact_ids": [expected[0]],
                "setup": {"files": setup_files, "sha256": setup_sha(setup_files)},
            })

    corpus = {
        "format_version": 1,
        "suite_id": "mce4d-polyglot-benchmark-synthetic-v1",
        "synthetic": True,
        "recommended_repeats": 3,
        "repositories": repo_rows,
        "tasks": tasks,
    }
    target = output / "corpus.json"
    target.write_text(json.dumps(corpus, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")
    return {
        "corpus": target.as_posix(),
        "suite_sha256": hashlib.sha256(normalize(target.read_bytes())).hexdigest(),
        "repositories": len(repo_rows),
        "tasks": len(tasks),
        "cases": len(CASES),
        "domains": len({case[1] for case in CASES}),
        "synthetic_only": True,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Generate the synthetic T068 polyglot cohort.")
    parser.add_argument("--output-dir", type=Path, default=ROOT / "polyglot")
    args = parser.parse_args()
    print(json.dumps(build(args.output_dir.resolve()), ensure_ascii=False, sort_keys=True))


if __name__ == "__main__":
    main()
