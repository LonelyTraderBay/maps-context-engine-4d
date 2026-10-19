from __future__ import annotations

import argparse
import hashlib
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath
from urllib.parse import quote

ROOT = Path.cwd()
RELEASE = ROOT / "artifacts/release"
SCRIPT_REF = "artifacts/release/generate_candidate.py"
T002_REF = "artifacts/preflight/T002/dependency-review.json"
SUPPORT_REF = "artifacts/git-delivery-support/T041-r2-936739b7-193e-4428-887a-a0da3bcd69fa"
VULN_REF = SUPPORT_REF + "/security/govulncheck-attempt-1/record.json"
FOCUSED_REF = SUPPORT_REF + "/security/govulncheck-attempt-1/focused-controls-tests.log"
VULN_LOG_REF = SUPPORT_REF + "/security/govulncheck-attempt-1/scan.log"
VULN_VERSION_REF = SUPPORT_REF + "/security/govulncheck-attempt-1/version.log"
REPO = "github.com/LonelyTraderBay/maps-context-engine-4d"
T041_SOURCE = "c39b08be3ebf99fc10af7a228d8e88a64e908460"
T041_ALLOCATION = "1991e07ae2bbc92380df54afe562d237512e1659"
T041_PROGRESS_RUN = 39327929671
T041_ALLOCATION_RUN = 39327034066
REPO_ID = 1421031011
LICENSE_ATTRIBUTES_REF = "artifacts/release/licenses/.gitattributes"
LICENSE_ATTRIBUTES = b"* -text whitespace=-blank-at-eof\n"
RELEASE_ATTRIBUTES_REF = "artifacts/release/.gitattributes"
RELEASE_ATTRIBUTES = b"* -text whitespace=-blank-at-eof,-blank-at-eol\n"


def run(argv: list[str], *, input_bytes: bytes | None = None) -> bytes:
    result = subprocess.run(argv, cwd=ROOT, input=input_bytes, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, check=False)
    if result.returncode:
        raise SystemExit(f"Command failed ({result.returncode}): {argv!r}\n" +
                         result.stderr.decode("utf-8", "replace")[-4000:])
    return result.stdout


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def canonical(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8") + b"\n"


def json_stream(argv: list[str]) -> list[dict]:
    raw = run(argv).decode("utf-8", "strict")
    decoder = json.JSONDecoder()
    values = []
    cursor = 0
    while cursor < len(raw):
        while cursor < len(raw) and raw[cursor].isspace():
            cursor += 1
        if cursor == len(raw):
            break
        item, cursor = decoder.raw_decode(raw, cursor)
        if not isinstance(item, dict):
            raise SystemExit("Expected JSON object stream")
        values.append(item)
    return values


def source_files(revision: str) -> tuple[str, str, list[dict[str, object]]]:
    tree = run(["git", "rev-parse", f"{revision}^{{tree}}"]).decode().strip()
    epoch = int(run(["git", "show", "-s", "--format=%ct", revision]).decode().strip())
    created = datetime.fromtimestamp(epoch, timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")
    raw = run(["git", "ls-tree", "-r", "-z", "--format=%(objectname)%x09%(path)", revision])
    entries = []
    for record in raw.split(b"\0"):
        if not record:
            continue
        object_id, sep, path = record.partition(b"\t")
        if not sep:
            raise SystemExit("Malformed git ls-tree record")
        entries.append((object_id.decode("ascii"), path.decode("utf-8", "strict")))
    batch_input = b"".join(object_id.encode("ascii") + b"\n" for object_id, _ in entries)
    batch = run(["git", "cat-file", "--batch"], input_bytes=batch_input)
    cursor = 0
    files = []
    for object_id, path in entries:
        end = batch.find(b"\n", cursor)
        if end < 0:
            raise SystemExit("Truncated git cat-file header")
        header = batch[cursor:end].decode("ascii", "strict").split()
        if len(header) != 3 or header[0] != object_id or header[1] != "blob":
            raise SystemExit("Git blob identity mismatch")
        size = int(header[2])
        start = end + 1
        content = batch[start:start + size]
        if len(content) != size or batch[start + size:start + size + 1] != b"\n":
            raise SystemExit("Truncated git blob")
        cursor = start + size + 1
        files.append({"path": path, "bytes": size, "sha256": sha(content)})
    if cursor != len(batch):
        raise SystemExit("Unexpected trailing git cat-file output")
    return tree, created, files


def spdx_license_info(classification: str) -> tuple[str, list[str]]:
    known = {"Apache-2.0", "BSD-3-Clause", "MIT", "MPL-2.0"}
    if classification in known:
        return classification, [classification]
    if classification.startswith("Mixed: BSD-3-Clause package"):
        return "NOASSERTION", ["BSD-3-Clause", "MIT", "NOASSERTION"]
    if classification.startswith("Mixed: MIT + Apache-2.0 + CC-BY-4.0"):
        return "NOASSERTION", ["Apache-2.0", "CC-BY-4.0", "MIT", "NOASSERTION"]
    if classification.startswith("BSD-3-Clause package; third-party manifest"):
        return "NOASSERTION", ["BSD-3-Clause", "NOASSERTION"]
    raise SystemExit("No reviewed SPDX mapping for classification: " + classification)


def build_outputs(revision: str) -> dict[str, bytes]:
    if revision != T041_SOURCE:
        raise SystemExit("T041 candidate generator is frozen to the verified source revision")
    if run(["git", "cat-file", "-e", revision]).strip():
        raise SystemExit("Source revision did not resolve to a commit")
    go_mod = (ROOT / "go.mod").read_bytes()
    go_sum = (ROOT / "go.sum").read_bytes()
    if run(["git", "show", f"{revision}:go.mod"]) != go_mod or run(["git", "show", f"{revision}:go.sum"]) != go_sum:
        raise SystemExit("Current go.mod/go.sum differ from the frozen source revision")
    t002_bytes = (ROOT / T002_REF).read_bytes()
    t002 = json.loads(t002_bytes.decode("utf-8", "strict"))
    license_review = {(item["path"], item["version"]): item for item in t002["license_review"]["modules"]}
    dependency_review = {(item["path"], item["version"]): item for item in t002["dependencies"]["modules"]}
    if set(license_review) != set(dependency_review):
        raise SystemExit("T002 dependency and license review module sets differ")
    expected = {key: {**dependency_review[key], **license_review[key]} for key in license_review}
    graph = { (item["Path"], item["Version"]): item
              for item in json_stream(["go", "list", "-m", "-json", "all"])
              if not item.get("Main") }
    if set(graph) != set(expected) or len(graph) != 38:
        raise SystemExit("Current module graph differs from the 38 reviewed T002 modules")
    modfile = json.loads(run(["go", "mod", "edit", "-json"]).decode("utf-8", "strict"))
    direct = {(item["Path"], item["Version"]) for item in modfile.get("Require", []) if not item.get("Indirect", False)}
    if len(direct) != 4:
        raise SystemExit(f"Expected current go.mod to have four direct modules; got {len(direct)}")
    compiled = {(item["Module"]["Path"], item["Module"]["Version"])
                for item in json_stream(["go", "list", "-deps", "-json", "./..."])
                if isinstance(item.get("Module"), dict) and item["Module"].get("Path") and item["Module"].get("Version")}
    if not compiled.issubset(graph):
        raise SystemExit("Compiled package graph contains a module absent from the selected graph")
    license_files: dict[str, bytes] = {}
    inventory_modules = []
    for key in sorted(graph):
        module = graph[key]
        old = expected[key]
        if (module.get("Sum"), module.get("GoModSum")) != (old.get("sum"), old.get("go_mod_sum")):
            raise SystemExit("Go module checksum differs from T002: " + "@".join(key))
        declared, ids = spdx_license_info(old["classification"])
        files = []
        module_dir = Path(module.get("Dir", ""))
        if not module_dir.is_dir():
            raise SystemExit("Module source is unavailable in cache: " + "@".join(key))
        for license in sorted(old["license_files"], key=lambda item: item["name"]):
            source = module_dir / license["name"]
            if not source.is_file():
                raise SystemExit("Reviewed license file is absent: " + str(source))
            data = source.read_bytes()
            if len(data) != license["bytes"] or sha(data) != license["sha256"]:
                raise SystemExit("Reviewed license text hash mismatch: " + "@".join(key) + "/" + license["name"])
            rel = PurePosixPath("licenses", *key[0].split("/"), key[1], license["name"]).as_posix()
            license_files[rel] = data
            files.append({"name": license["name"], "path": "artifacts/release/" + rel,
                          "bytes": len(data), "sha256": sha(data)})
        inventory_modules.append({
            "path": key[0], "version": key[1],
            "dependency_kind": "direct" if key in direct else "indirect",
            "present_in_go_list_deps": key in compiled,
            "go_sum": module["Sum"], "go_mod_sum": module["GoModSum"],
            "classification": old["classification"],
            "classification_basis": old["classification_basis"],
            "spdx_license_declared": declared, "spdx_license_info_in_files": ids,
            "license_files": files,
        })
    if len(license_files) != 45 or sum(len(item["license_files"]) for item in inventory_modules) != 45:
        raise SystemExit("Expected all 45 reviewed license texts")
    dep_inventory = {
        "format_version": 1, "kind": "dependency_license_inventory",
        "project": {"name": "Maps-Context-Engine-4D", "module": REPO, "license": "NOASSERTION"},
        "source_revision": revision,
        "go_version": run(["go", "version"]).decode("utf-8", "strict").strip(),
        "go_mod_sha256": sha(go_mod), "go_sum_sha256": sha(go_sum),
        "selected_module_count": len(graph), "direct_module_count": len(direct),
        "indirect_module_count": len(graph) - len(direct),
        "compiled_non_test_module_count": len(compiled),
        "t002_evidence": {"ref": T002_REF, "sha256": sha(t002_bytes),
                          "historical_go_mod_sha256": t002["dependencies"]["go_mod_sha256"],
                          "historical_direct_module_count": t002["dependencies"]["direct_module_count"],
                          "historical_transitive_module_count": t002["dependencies"]["transitive_module_count"],
                          "module_paths_versions_and_checksums_match": True,
                          "current_direct_count_recomputed_from_go_mod": True},
        "license_review_result": t002["license_review"]["result"],
        "classification_basis": t002["license_review"]["basis"],
        "modules": inventory_modules,
    }
    outputs: dict[str, bytes] = {"artifacts/release/dependency-licenses.json": canonical(dep_inventory)}
    outputs.update({"artifacts/release/" + name: data for name, data in license_files.items()})
    # Preserve upstream license bytes across Windows checkouts with core.autocrlf enabled.
    outputs[LICENSE_ATTRIBUTES_REF] = LICENSE_ATTRIBUTES
    # Preserve every hash-bound release artifact byte-for-byte on all checkout platforms.
    outputs[RELEASE_ATTRIBUTES_REF] = RELEASE_ATTRIBUTES

    lines = [
        "Maps-Context-Engine-4D — Third-Party Notices",
        "",
        "Project license: NOASSERTION. The project has not declared an outbound license.",
        "This candidate is NOT FOR DISTRIBUTION. Third-party notices do not grant rights to the project source.",
        "Retain the full license and third-party manifest files under the listed artifacts/release/licenses paths.",
        "License classifications were copied from the T002 review and verified against current module paths, versions, checksums, file sizes, and SHA-256 digests.",
        "",
    ]
    for item in inventory_modules:
        paths = ", ".join(file["path"] for file in item["license_files"])
        lines.append(f"- {item['path']}@{item['version']} ({item['dependency_kind']}; selected Go module graph): {item['classification']}")
        lines.append(f"  License files: {paths}")
    lines += ["", "No claim is made that the project license, trademark, patent, or redistribution rights have been cleared.", ""]
    outputs["artifacts/release/THIRD_PARTY_NOTICES.txt"] = "\n".join(lines).encode("utf-8")

    vuln_bytes = (ROOT / VULN_REF).read_bytes()
    vuln = json.loads(vuln_bytes.decode("utf-8", "strict"))
    focused_bytes = (ROOT / FOCUSED_REF).read_bytes()
    vuln_log = (ROOT / VULN_LOG_REF).read_bytes()
    version_log = (ROOT / VULN_VERSION_REF).read_bytes()
    if (vuln.get("source_commit"), vuln.get("result"), vuln.get("finding_count"), vuln.get("tool_version")) != (revision, "PASS", 0, "v1.9.0"):
        raise SystemExit("Pinned govulncheck evidence does not bind the source revision")
    if b"tests/acceptance" not in focused_bytes or b"tests/integration" not in focused_bytes:
        raise SystemExit("Focused control test output does not include expected packages")
    outputs["artifacts/release/security/govulncheck.log"] = vuln_log
    outputs["artifacts/release/security/govulncheck-version.log"] = version_log
    outputs["artifacts/release/security/focused-controls-tests.log"] = focused_bytes
    security = {
        "format_version": 1, "kind": "release_security_audit",
        "source_revision": revision,
        "project_license": "NOASSERTION",
        "vulnerability_scan": {
            "tool": "govulncheck", "version": "v1.9.0", "result": "PASS", "finding_count": 0,
            "database": vuln["database"], "database_update_observed": vuln["database_update_observed"],
            "command": vuln["scan_command"]["argv"],
            "source_record_ref": VULN_REF, "source_record_sha256": sha(vuln_bytes),
            "bundled_log": "artifacts/release/security/govulncheck.log",
            "bundled_log_sha256": sha(vuln_log),
        },
        "focused_controls": [
            {"case": "C046", "result": "PASS", "evidence": "TestPoisonedGoEnvironmentDoesNotDownloadOrAcceptMutatedCapture", "log": "artifacts/release/security/focused-controls-tests.log", "log_sha256": sha(focused_bytes)},
            {"case": "C047", "result": "PASS", "evidence": "TestPoisonedGoEnvironmentDoesNotDownloadOrAcceptMutatedCapture; synthetic proxy request count remained zero and missing module stayed explicit partial", "log": "artifacts/release/security/focused-controls-tests.log", "log_sha256": sha(focused_bytes)},
            {"case": "C048", "result": "PASS", "evidence": "TestTelemetry* focused integration tests; fixed schema/redaction/byte-cap/concurrency", "log": "artifacts/release/security/focused-controls-tests.log", "log_sha256": sha(focused_bytes)},
        ],
        "sqlite_compatibility_evidence": {
            "result": "PASS_SCOPED_TO_PRIOR_TASK_EVIDENCE",
            "tasks": ["T036", "T037", "T106"],
            "summary": "Actual pinned modernc.org/sqlite v1.60.1 with embedded SQLite 3.53.4; Windows/WSL2 Linux smoke, WAL/FTS5/pool timeout evidence is retained in those task records.",
            "limitations": "This T041 candidate does not rerun multi-process stress on every end-user target.",
        },
        "package_integration": {
            "result": "PASS_ON_SOURCE_REVISION",
            "evidence": "The 11/11 allocation-progress local checks include go test ./..., go vet ./..., and go build ./... on the exact tracked source tree.",
            "final_combined_108_task_tree": "NOT_RUN; reserved for T042 final gate",
        },
        "codex_security_scan": {
            "status": "NOT_RUN",
            "reason": "Prompt-only scan start failed while reading a pre-existing untracked file; a comprehensive Codex Security result is unavailable.",
        },
        "release_gate": {
            "status": "BLOCKED",
            "reasons": [
                "Project license is NOASSERTION; no owner decision or outbound license text exists.",
                "Mixed and contribution-dependent third-party terms require final rights review before any distribution claim.",
                "Codex Security scan is unavailable; govulncheck alone is not a complete security audit.",
                "No package/binary was built or published and no production/UAT control proof is claimed.",
            ],
            "distribution_authorized": False,
            "production_claim_authorized": False,
        },
    }
    outputs["artifacts/release/security-audit.json"] = canonical(security)

    source_tree, created, files = source_files(revision)
    if not files:
        raise SystemExit("Source revision has no tracked files")
    source_content_hash = sha(canonical(files))
    artifact_entries = []
    for ref, data in sorted(outputs.items()):
        artifact_entries.append({"path": ref, "bytes": len(data), "sha256": sha(data)})
    script = (ROOT / SCRIPT_REF).read_bytes()
    artifact_entries.append({"path": SCRIPT_REF, "bytes": len(script), "sha256": sha(script), "role": "generator"})
    manifest = {
        "format_version": 1, "kind": "reproducible_release_candidate_manifest",
        "project": {"name": "Maps-Context-Engine-4D", "module": REPO,
                    "license": "NOASSERTION", "version": "NOT_ASSIGNED"},
        "candidate_status": "NOT_FOR_DISTRIBUTION",
        "production_claim_gate": "BLOCKED",
        "publication_authorized": False,
        "source": {"commit": revision, "git_tree_object_id": source_tree,
                   "source_date_epoch": int(run(["git", "show", "-s", "--format=%ct", revision]).decode().strip()),
                   "created_from_commit_time_utc": created,
                   "tracked_file_count": len(files), "tracked_files_sha256": source_content_hash,
                   "tracked_files": files},
        "toolchain": {"go_version": dep_inventory["go_version"],
                      "go_mod_sha256": dep_inventory["go_mod_sha256"],
                      "go_sum_sha256": dep_inventory["go_sum_sha256"],
                      "selected_module_count": len(graph)},
        "generation": {"tool": "mce4d-release-candidate-generator", "version": "1.0.0",
                       "path": SCRIPT_REF, "sha256": sha(script),
                       "deterministic": True, "timestamp_source": "source commit committer time; no wall clock"},
        "release_artifacts": artifact_entries,
        "manifest_exclusions": ["artifacts/release/release-candidate.json"],
        "reproducibility_notes": [
            "The manifest is deterministic for the same source revision, T002 license evidence, current pinned Go module graph, module cache license bytes, security evidence, and generator bytes.",
            "The manifest excludes its own hash to avoid self-reference; the containing Git commit and external evidence hash bind it.",
            "No binary/package is produced, signed, published, or authorized by this candidate.",
        ],
        "security_audit_ref": "artifacts/release/security-audit.json",
        "sbom_ref": "artifacts/release/sbom.spdx.json",
        "dependency_inventory_ref": "artifacts/release/dependency-licenses.json",
        "notices_ref": "artifacts/release/THIRD_PARTY_NOTICES.txt",
    }
    root_package_id = "SPDXRef-Package-MCE4D"
    package_records = [{
        "name": "Maps-Context-Engine-4D", "SPDXID": root_package_id,
        "versionInfo": "NOT_ASSIGNED", "downloadLocation": "NOASSERTION",
        "filesAnalyzed": False, "licenseConcluded": "NOASSERTION",
        "licenseDeclared": "NOASSERTION", "copyrightText": "NOASSERTION",
        "primaryPackagePurpose": "APPLICATION",
        "sourceInfo": f"Go module {REPO}; source commit {revision}; project outbound license is unassigned.",
        "comment": "Internal release candidate only; no distribution or production claim is authorized.",
    }]
    relationships = [{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": root_package_id}]
    for item in inventory_modules:
        license_declared, _ = spdx_license_info(item["classification"])
        package_id = "SPDXRef-Package-" + hashlib.sha256((item["path"] + "@" + item["version"]).encode()).hexdigest()[:20].upper()
        purl = "pkg:golang/" + quote(item["path"], safe="/!._-") + "@" + quote(item["version"], safe="._-+~")
        package_records.append({
            "name": item["path"], "SPDXID": package_id,
            "versionInfo": item["version"], "downloadLocation": "NOASSERTION",
            "filesAnalyzed": False, "licenseConcluded": "NOASSERTION",
            "licenseDeclared": license_declared, "copyrightText": "NOASSERTION",
            "primaryPackagePurpose": "LIBRARY",
            "externalRefs": [{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": purl}],
            "sourceInfo": f"Selected Go module; sum={item['go_sum']}; go.mod sum={item['go_mod_sum']}; license evidence from T002 artifact hashes.",
            "licenseComments": f"Observed license classification: {item['classification']}. Full retained files are listed in dependency-licenses.json.",
        })
        relationships.append({"spdxElementId": root_package_id, "relationshipType": "DEPENDS_ON", "relatedSpdxElement": package_id})
    sbom = {
        "spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
        "name": f"Maps-Context-Engine-4D source candidate {revision}",
        "documentNamespace": f"https://spdx.org/spdxdocs/maps-context-engine-4d-{revision}",
        "creationInfo": {"creators": ["Tool: mce4d-release-candidate-generator-1.0.0"], "created": created},
        "comment": "SBOM for the pinned Go module graph at the stated source revision. License and security status does not grant distribution rights.",
        "documentDescribes": [root_package_id],
        "packages": package_records,
        "relationships": relationships,
    }
    outputs["artifacts/release/sbom.spdx.json"] = canonical(sbom)
    # The manifest binds the generated SBOM too, so rebuild entries after adding it.
    artifact_entries = [{"path": ref, "bytes": len(data), "sha256": sha(data)} for ref, data in sorted(outputs.items())]
    artifact_entries.append({"path": SCRIPT_REF, "bytes": len(script), "sha256": sha(script), "role": "generator"})
    manifest["release_artifacts"] = artifact_entries
    outputs["artifacts/release/release-candidate.json"] = canonical(manifest)
    return outputs


def main() -> None:
    parser = argparse.ArgumentParser(description="Generate or verify deterministic MCE4D release-candidate evidence.")
    parser.add_argument("--source-revision", required=True)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", action="store_true")
    mode.add_argument("--replace-existing", action="store_true",
                      help="explicitly regenerate and replace existing release artifacts")
    mode.add_argument("--check", action="store_true")
    args = parser.parse_args()
    expected = build_outputs(args.source_revision)
    script_path = ROOT / SCRIPT_REF
    actual_files = {path.relative_to(ROOT).as_posix() for path in RELEASE.rglob("*") if path.is_file()}
    expected_files = set(expected) | {SCRIPT_REF}
    extras = actual_files - expected_files
    if extras:
        raise SystemExit("Unexpected release files: " + ", ".join(sorted(extras)))
    if args.check:
        missing = sorted(ref for ref in expected if not (ROOT / ref).is_file())
        mismatches = sorted(ref for ref, data in expected.items() if (ROOT / ref).is_file() and (ROOT / ref).read_bytes() != data)
        if missing or mismatches:
            raise SystemExit("Candidate verification failed; missing=" + repr(missing) + "; mismatched=" + repr(mismatches))
        print(json.dumps({"result": "PASS", "mode": "check", "files": len(expected),
                          "manifest_sha256": sha((ROOT / "artifacts/release/release-candidate.json").read_bytes()),
                          "source_revision": args.source_revision}, sort_keys=True))
        return
    mismatches = sorted(ref for ref, data in expected.items() if (ROOT / ref).exists() and (ROOT / ref).read_bytes() != data)
    if mismatches and not args.replace_existing:
        raise SystemExit("Refusing to overwrite existing release artifacts with different bytes: " + repr(mismatches))
    for ref, data in expected.items():
        path = ROOT / ref
        path.parent.mkdir(parents=True, exist_ok=True)
        if args.replace_existing or not path.exists():
            path.write_bytes(data)
    print(json.dumps({"result": "REPLACED" if args.replace_existing else "WRITTEN", "files": len(expected),
                      "manifest_sha256": sha((ROOT / "artifacts/release/release-candidate.json").read_bytes()),
                      "source_revision": args.source_revision}, sort_keys=True))


if __name__ == "__main__":
    main()
