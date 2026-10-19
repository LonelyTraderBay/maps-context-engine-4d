from __future__ import annotations

import hashlib
import argparse
import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[4]
DATA = ROOT / "tools/bench/compare/testdata"
FIXTURES = DATA / "fixtures"

REPOSITORIES = {
    "alpha": {
        "module": "example.com/synthetic/alpha",
        "files": {
            "calc/calc.go": "package calc\n\nimport \"strings\"\n\nfunc Add(left, right int) int { return left + right }\n\nfunc Clamp(value, low, high int) int {\n\tif value < low { return low }\n\tif value > high { return high }\n\treturn value\n}\n\nfunc NormalizeCode(value string) string { return strings.TrimSpace(strings.ToUpper(value)) }\n",
            "service/processor.go": "package service\n\nimport \"context\"\n\ntype Item struct { ID string; Value int }\ntype Processor interface { Process(context.Context, Item) (Item, error) }\ntype Worker struct{}\nfunc (Worker) Process(_ context.Context, item Item) (Item, error) { return item, nil }\n",
            "api/contracts.go": "package api\n\ntype Result struct { Value int; Err error }\ntype Handler func(string) (Result, error)\n",
            "integration/consumer.go": "package integration\n\nimport (\n\t\"context\"\n\t\"example.com/synthetic/alpha/service\"\n)\n\nfunc Run(p service.Processor, item service.Item) (service.Item, error) { return p.Process(context.Background(), item) }\n",
        },
        "fanout_import": "example.com/synthetic/alpha/calc",
        "fanout_expr": "calc.Add(value, {n})",
    },
    "beta": {
        "module": "example.com/synthetic/beta",
        "files": {
            "store/store.go": "package store\n\ntype Store interface { Get(string) (string, bool); Put(string, string) }\ntype MemoryStore struct { values map[string]string }\nfunc NewMemoryStore() *MemoryStore { return &MemoryStore{values: map[string]string{}} }\nfunc (s *MemoryStore) Get(key string) (string, bool) { value, ok := s.values[key]; return value, ok }\nfunc (s *MemoryStore) Put(key, value string) { s.values[key] = value }\n",
            "worker/queue.go": "package worker\n\ntype Job struct { ID string; Attempts int }\ntype Queue struct { jobs []Job }\nfunc (q *Queue) Push(job Job) { q.jobs = append(q.jobs, job) }\nfunc (q *Queue) Pop() (Job, bool) { if len(q.jobs) == 0 { return Job{}, false }; job := q.jobs[0]; q.jobs = q.jobs[1:]; return job, true }\n",
            "api/codec.go": "package api\n\ntype Envelope struct { Version int; Payload []byte }\ntype Decoder interface { Decode([]byte) (Envelope, error) }\n",
            "integration/consumer.go": "package integration\n\nimport \"example.com/synthetic/beta/store\"\n\nfunc Save(s store.Store, key, value string) { s.Put(key, value) }\n",
        },
        "fanout_import": "example.com/synthetic/beta/store",
        "fanout_expr": "lenStore(value, {n})",
    },
    "gamma": {
        "module": "example.com/synthetic/gamma",
        "files": {
            "graph/graph.go": "package graph\n\ntype Node struct { ID string }\ntype Graph struct { edges map[string][]string }\nfunc New() *Graph { return &Graph{edges: map[string][]string{}} }\nfunc (g *Graph) AddEdge(from, to string) { g.edges[from] = append(g.edges[from], to) }\nfunc (g *Graph) Neighbors(id string) []string { return append([]string(nil), g.edges[id]...) }\n",
            "search/index.go": "package search\n\ntype Index interface { Lookup(string) []string }\ntype MemoryIndex struct { terms map[string][]string }\nfunc (i *MemoryIndex) Lookup(term string) []string { return append([]string(nil), i.terms[term]...) }\n",
            "client/client.go": "package client\n\ntype Request struct { Query string; Limit int }\ntype Response struct { IDs []string; Pending bool }\n",
            "integration/consumer.go": "package integration\n\nimport (\n\t\"example.com/synthetic/gamma/client\"\n\t\"example.com/synthetic/gamma/search\"\n)\n\nfunc Query(i search.Index, request client.Request) client.Response { return client.Response{IDs: i.Lookup(request.Query)} }\n",
        },
        "fanout_import": "example.com/synthetic/gamma/client",
        "fanout_expr": "value + {n}",
    },
}

SCENARIOS = [
    "local_rename", "interface_change", "field_change", "type_change", "behavior_bug",
    "contract_preservation", "dirty_worktree", "missing_dependency", "large_fanout", "local_rename",
]


def tree_sha(path: Path) -> str:
    rows = []
    for item in sorted((p for p in path.rglob("*") if p.is_file()), key=lambda p: p.relative_to(path).as_posix()):
        relative = item.relative_to(path).as_posix()
        rows.append((relative, hashlib.sha256(normalize_line_endings(item.read_bytes())).hexdigest()))
    digest = hashlib.sha256()
    for relative, content_sha in rows:
        digest.update(relative.encode("utf-8")); digest.update(b"\0")
        digest.update(content_sha.encode("ascii")); digest.update(b"\n")
    return digest.hexdigest()


def normalize_line_endings(data: bytes) -> bytes:
    try:
        data.decode("utf-8")
    except UnicodeDecodeError:
        return data
    if b"\0" in data:
        return data
    return data.replace(b"\r\n", b"\n")


def setup_sha(files: dict[str, str]) -> str:
    digest = hashlib.sha256()
    for relative in sorted(files):
        digest.update(relative.encode("utf-8")); digest.update(b"\0")
        digest.update(hashlib.sha256(files[relative].encode("utf-8")).hexdigest().encode("ascii")); digest.update(b"\n")
    return digest.hexdigest()


def tasks_for(repo_id: str, config: dict) -> list[dict]:
    module = config["module"]
    tasks = []
    for index, scenario in enumerate(SCENARIOS, 1):
        task_id = f"{repo_id}-{index:02d}-{scenario}"
        setup_files: dict[str, str] = {}
        if scenario == "dirty_worktree":
            setup_files = {f"notes/{task_id}.md": "Synthetic pre-existing user note. Benchmark variants must preserve these exact bytes.\n"}
        elif scenario == "missing_dependency":
            setup_files = {"integration/missing_dependency.go": "package integration\n\nimport _ \"example.com/fixture/missing-dependency\"\n\nfunc OptionalDependencyFixture() {}\n"}
        prompts = {
            "local_rename": "Rename one local helper and every in-repository reference; preserve behavior and public contracts.",
            "interface_change": "Add one required method to the target interface and update every implementation and caller.",
            "field_change": "Change the named data field consistently across constructors, readers, and consumers.",
            "type_change": "Change the named internal type while preserving serialized/public compatibility.",
            "behavior_bug": "Fix the described edge case and add or update a focused regression check.",
            "contract_preservation": "Update the implementation while preserving the documented API and error behavior.",
            "dirty_worktree": "Implement the requested local change and leave every pre-existing dirty file byte-identical.",
            "missing_dependency": "Restore a buildable optional integration without network access or changing the dependency policy.",
            "large_fanout": "Change the shared helper and update all downstream fanout consumers with no unrelated edits.",
        }
        expected_paths = ["calc/calc.go"] if repo_id == "alpha" else ["store/store.go"] if repo_id == "beta" else ["graph/graph.go"]
        if scenario == "large_fanout": expected_paths += [f"fanout/consumer_{n:02d}.go" for n in range(1, 9)]
        if scenario == "missing_dependency": expected_paths += ["integration/missing_dependency.go"]
        setup = {"files": setup_files, "sha256": setup_sha(setup_files)}
        tasks.append({
            "id": task_id, "repository_id": repo_id, "scenario": scenario,
            "request": f"{prompts[scenario]} Synthetic target: {module}; case {index:02d}.",
            "expected_changed_paths": sorted(expected_paths), "forbidden_changed_paths": ["go.mod", "go.sum"],
            "required_invariants": ["Không sửa ngoài scope được yêu cầu.", "Chạy oracle độc lập và ghi critical omissions/false edits."],
            "oracle": "Oracle độc lập của task phải kiểm diff paths, test của module và các invariant nêu trên; không dùng context pack của variant làm oracle.",
            "setup": setup,
        })
    return tasks


def main() -> None:
    global DATA, FIXTURES
    parser = argparse.ArgumentParser(description="Generate an isolated synthetic A/B/C benchmark corpus.")
    parser.add_argument("--output-dir", type=Path, default=DATA, help="new or empty output directory")
    args = parser.parse_args()
    DATA = args.output_dir.resolve()
    FIXTURES = DATA / "fixtures"
    if DATA.exists() and any(DATA.iterdir()):
        raise SystemExit(f"Refuse overwrite non-empty benchmark output directory: {DATA}")
    repos = []
    tasks = []
    for repo_id, config in REPOSITORIES.items():
        repo = FIXTURES / repo_id
        repo.mkdir(parents=True, exist_ok=False)
        (repo / "go.mod").write_text(f"module {config['module']}\n\ngo 1.27.0\n", encoding="utf-8", newline="\n")
        for relative, content in config["files"].items():
            path = repo / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8", newline="\n")
        fanout_import = config["fanout_import"]
        expr = config["fanout_expr"]
        for number in range(1, 9):
            if repo_id == "beta":
                helper = "func lenStore(value string, salt int) int { return len(value) + salt }\n" if number == 1 else ""
                body = f"{helper}func Use{number:02d}(value string) int {{ return lenStore(value, {number}) }}\n"
                source = f"package fanout\n\nimport \"{fanout_import}\"\n\nvar _ store.Store = (*store.MemoryStore)(nil)\n\n{body}"
            elif repo_id == "gamma":
                source = f"package fanout\n\nfunc Use{number:02d}(value int) int {{ return {expr.format(n=number)} }}\n"
            else:
                source = f"package fanout\n\nimport \"{fanout_import}\"\n\nfunc Use{number:02d}(value int) int {{ return {expr.format(n=number)} }}\n"
            path = repo / f"fanout/consumer_{number:02d}.go"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(source, encoding="utf-8", newline="\n")
        fixture_sha = tree_sha(repo)
        go_mod_sha = hashlib.sha256(normalize_line_endings((repo / "go.mod").read_bytes())).hexdigest()
        repos.append({
            "id": repo_id, "module": config["module"], "fixture_path": f"fixtures/{repo_id}",
            "fixture_revision": fixture_sha, "fixture_sha256": fixture_sha,
            "dependency_sha256": go_mod_sha, "dependency_files": ["go.mod"],
        })
        tasks.extend(tasks_for(repo_id, config))
    corpus = {
        "format_version": 1, "suite_id": "mce4d-context-benchmark-synthetic-v1",
        "synthetic": True, "recommended_repeats": 3,
        "repositories": repos, "tasks": tasks,
    }
    DATA.mkdir(parents=True, exist_ok=True)
    output = DATA / "corpus.json"
    output.write_text(json.dumps(corpus, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")
    print(json.dumps({"corpus": output.as_posix(), "repositories": len(repos), "tasks": len(tasks), "fixture_sha256": {repo["id"]: repo["fixture_sha256"] for repo in repos}}, sort_keys=True))


if __name__ == "__main__":
    main()
