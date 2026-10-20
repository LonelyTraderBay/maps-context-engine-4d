from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

PACKAGE_SRC = Path(__file__).resolve().parents[1] / "src"
sys.path.insert(0, str(PACKAGE_SRC))

from mce4d_python_provider.analyze import analyze_provider_request
from mce4d_python_provider.descriptor import compute_provider_manifest_digest, language_provider_descriptor
from mce4d_python_provider.profile import ProviderInputError, create_local_profile
from mce4d_python_provider.protocol import ProtocolError, decode_frame, encode_frame


def request_for(sources: dict[str, bytes], mode: str = "syntax") -> dict[str, object]:
    files = []
    for path, content in sorted(sources.items(), key=lambda item: item[0].encode("utf-8")):
        files.append({
            "path": path,
            "content_base64": base64.b64encode(content).decode("ascii"),
            "content_sha256": hashlib.sha256(content).hexdigest(),
        })
    roots = sorted({path.split("/", 1)[0] for path in sources if path.endswith((".py", ".pyi"))})
    return {
        "schema_version": 1,
        "message_type": "analyze_request",
        "request_id": "12345678-1234-1234-1234-123456789abc",
        "language": "python",
        "profile": create_local_profile(roots, "a" * 64, mode),
        "files": files,
        "execution_policy": {
            "target_code": "never_execute",
            "project_scripts": "never_execute",
            "project_plugins": "never_execute",
            "notebook_cells": "never_execute",
            "network": False,
        },
    }


class PythonProviderTests(unittest.TestCase):
    def test_descriptor_digest_is_stable_across_line_endings_and_file_order(self) -> None:
        files = [("b.py", b"second\r\n"), ("a.py", b"first\r")]
        equivalent = [("a.py", b"first\n"), ("b.py", b"second\n")]
        self.assertEqual(compute_provider_manifest_digest(files), compute_provider_manifest_digest(equivalent))
        self.assertEqual(len(language_provider_descriptor["digest"]), 64)

    def test_ast_scope_import_and_unknown_facts_do_not_execute_target_code(self) -> None:
        with self.subTest("only captured source is parsed"):
            source = b'''\
import pkg.dep
import importlib as il
from importlib import import_module as load
@decorate
class Service:
    def run(self, value):
        local = value + missing_name
        Service.method = local
        return il.import_module("not_captured") + load("also_missing")

def __imported_but_not_called():
    return pkg.dep.answer
'''
            response = analyze_provider_request(request_for({
                "src/app.py": source,
                "src/pkg/__init__.py": b"",
                "src/pkg/dep.py": b"answer = 42\n",
            }))
        symbols = response["symbols"]
        names = {(item["kind"], item["name"]) for item in symbols}
        self.assertIn(("class", "Service"), names)
        self.assertIn(("function", "run"), names)
        self.assertIn(("parameter", "value"), names)
        self.assertIn(("variable", "local"), names)
        self.assertTrue(any(item["kind"] == "imports" and item["target_symbol_id"] for item in response["relations"]))
        codes = {item["code"] for item in response["diagnostics"]}
        self.assertIn("DECORATOR_SEMANTICS_UNKNOWN", codes)
        self.assertIn("DYNAMIC_IMPORT_UNKNOWN", codes)
        self.assertIn("MONKEY_PATCH_UNKNOWN", codes)
        self.assertIn("UNRESOLVED_BINDING", codes)
        self.assertFalse(any(item["analysis_status"] == "typed" for item in symbols))

    def test_forward_local_bindings_and_unicode_byte_offsets_are_stable(self) -> None:
        source = ('label = "😀"\nclass Example:\n    def owner(self):\n        return __class__\n'
                  'def café(value):\n    return later(value)\n\nlater = lambda value: value\n'
                  'def caller():\n    return helper()\n\ndef helper():\n    return 42\n').encode("utf-8")
        response = analyze_provider_request(request_for({"src/app.py": source}))
        symbols = response["symbols"]
        function = next(item for item in symbols if item["kind"] == "function" and item["name"] == "café")
        self.assertEqual(function["range"]["start_byte"], source.index("def café".encode("utf-8")))
        self.assertNotIn("UNRESOLVED_BINDING", {item["code"] for item in response["diagnostics"]})
        self.assertIn(("function", "helper"), {(item["kind"], item["name"]) for item in symbols})
        again = analyze_provider_request(request_for({"src/app.py": source}))
        self.assertEqual(response, again)

    def test_unbound_nonlocal_store_remains_unknown(self) -> None:
        response = analyze_provider_request(request_for({
            "src/app.py": b"def outer():\n    def inner():\n        nonlocal missing\n        missing = 1\n        return missing\n    return inner\n",
        }))

        self.assertIn("UNRESOLVED_BINDING", {item["code"] for item in response["diagnostics"]})
        self.assertFalse(any(
            item["kind"] == "variable" and item["name"] == "missing"
            for item in response["symbols"]
        ))
        self.assertTrue(any(
            item["kind"] == "reference" and item["target_symbol_id"] is None
            and item["analysis_status"] == "unknown"
            for item in response["relations"]
        ))

    def test_nonlocal_resolves_forward_enclosing_binding_through_nested_scopes(self) -> None:
        source = b"""\
def outer():
    value = 0
    def middle():
        def inner():
            nonlocal value
            value = value + 1
        value = 1
        return inner
    return middle
"""
        response = analyze_provider_request(request_for({"src/app.py": source}))

        value_symbols = [
            item for item in response["symbols"]
            if item["kind"] == "variable" and item["name"] == "value"
        ]
        self.assertEqual(len(value_symbols), 2)
        self.assertNotIn("UNRESOLVED_BINDING", {item["code"] for item in response["diagnostics"]})
        value_ids = {item["id"] for item in value_symbols}
        middle_value = next(
            item for item in value_symbols
            if item["range"]["start_byte"] == source.index(b"value = 1")
        )
        value_references = [
            item for item in response["relations"]
            if item["kind"] == "reference" and item["target_symbol_id"] in value_ids
        ]
        self.assertEqual(len(value_references), 1)
        self.assertEqual(value_references[0]["target_symbol_id"], middle_value["id"])

    def test_unicode_line_separator_does_not_shift_python_byte_ranges(self) -> None:
        source = 'value = "before\u2028after"\r\nnext_name = 1\r\n'.encode("utf-8")
        response = analyze_provider_request(request_for({"src/app.py": source}))
        symbol = next(item for item in response["symbols"] if item["kind"] == "variable" and item["name"] == "next_name")
        start = source.index(b"next_name")
        self.assertEqual(symbol["range"], {"start_byte": start, "end_byte": start + len(b"next_name")})

    def test_common_attribute_mutation_forms_remain_unknown(self) -> None:
        source = b'''\
class Service:
    def run(self):
        return None

Service.method = lambda: None
Service.annotation: int = 1
del Service.method
setattr(Service, "method", lambda: None)
import unittest.mock as mock
mock.patch.object(Service, "method", lambda: None)
'''
        response = analyze_provider_request(request_for({"src/app.py": source}))
        mutation_gaps = [item for item in response["diagnostics"] if item["code"] == "MONKEY_PATCH_UNKNOWN"]
        self.assertEqual(len(mutation_gaps), 5)

    def test_invalid_syntax_returns_bounded_diagnostic_without_source_excerpt(self) -> None:
        response = analyze_provider_request(request_for({"src/broken.py": b"secret_marker = (\n"}))
        self.assertIn("PYTHON_SYNTAX_INVALID", {item["code"] for item in response["diagnostics"]})
        self.assertNotIn("secret_marker", json.dumps(response))

    def test_typed_mode_emits_only_inert_declared_annotation_facts(self) -> None:
        response = analyze_provider_request(request_for({
            "src/app.py": b"class User:\n    pass\n\ncurrent: User\ndef load(value: User, limit: list[int]) -> User:\n    return unresolved(value)\n",
        }, mode="typed_when_available"))
        annotations = [item for item in response["symbols"] if item["kind"] == "type_annotation"]
        self.assertEqual({item["name"] for item in annotations}, {"User", "list[int]"})
        self.assertTrue(all(item["analysis_status"] == "typed" for item in annotations))
        typed_relations = [item for item in response["relations"] if item["kind"] == "typed_annotation"]
        self.assertEqual(len(typed_relations), 4)
        self.assertTrue(all(item["evidence_kind"] == "declared" for item in typed_relations))
        self.assertTrue(all(item["analysis_status"] == "typed" for item in typed_relations))
        self.assertIn("TYPED_DECLARATIONS_ONLY", {item["code"] for item in response["diagnostics"]})
        self.assertIn("UNRESOLVED_BINDING", {item["code"] for item in response["diagnostics"]})
        self.assertEqual(response, analyze_provider_request(request_for({
            "src/app.py": b"class User:\n    pass\n\ncurrent: User\ndef load(value: User, limit: list[int]) -> User:\n    return unresolved(value)\n",
        }, mode="typed_when_available")))

    def test_forward_reference_annotations_remain_unknown_without_evaluation(self) -> None:
        response = analyze_provider_request(request_for({
            "src/app.py": b'def load(value: "SecretType") -> "SecretType":\n    return value\n',
        }, mode="typed_when_available"))
        self.assertIn("TYPE_ANNOTATION_UNKNOWN", {item["code"] for item in response["diagnostics"]})
        self.assertNotIn("SecretType", json.dumps(response))
        self.assertFalse(any(item["kind"] == "type_annotation" for item in response["symbols"]))

    def test_notebook_cells_and_environment_files_are_metadata_only(self) -> None:
        with self.subTest("notebook"):
            with tempfile.TemporaryDirectory(prefix="mce4d-notebook-") as temp_root:
                sentinel = Path(temp_root) / "cell-sentinel"
                cell_source = f'open({str(sentinel)!r}, "w").write("executed")'
                notebook = json.dumps({
                    "cells": [{"cell_type": "code", "source": [cell_source]}],
                    "metadata": {
                        "kernelspec": {"name": "python3", "display_name": "Python 3"},
                        "language_info": {"name": "python", "version": "3.12"},
                        "unapproved": {"secret": "must-not-be-exported"},
                    },
                }).encode("utf-8")
                request = request_for({
                    "src/app.py": b"value = 1\n",
                    "src/demo.ipynb": notebook,
                })
                response = analyze_provider_request(request)
                notebook_entry = next(item for item in request["files"] if item["path"] == "src/demo.ipynb")  # type: ignore[index]
                self.assertEqual(base64.b64decode(notebook_entry["content_base64"]), notebook)  # type: ignore[index]
                self.assertIn("NOTEBOOK_CELLS_UNINSPECTED", {item["code"] for item in response["diagnostics"]})
                notebook_facts = [item for item in response["symbols"] if item["kind"] == "notebook_metadata"]
                self.assertEqual([item["name"] for item in notebook_facts], [
                    "kernelspec.name=python3; kernelspec.display_name=Python 3; "
                    "language_info.name=python; language_info.version=3.12",
                ])
                self.assertNotIn("open(", json.dumps(response))
                self.assertNotIn("must-not-be-exported", json.dumps(response))
                self.assertFalse(sentinel.exists())
        with self.subTest("environment metadata"):
            response = analyze_provider_request(request_for({
                "src/app.py": b"value = 1\n",
                "src/pyproject.toml": b"[build-system]\ninvalid = [",
                "src/pyvenv.cfg": b"home = /untrusted/python\ninclude-system-site-packages = true\n",
            }))
            metadata_gaps = [item for item in response["diagnostics"] if item["code"] == "PYTHON_ENV_METADATA_UNSUPPORTED"]
            self.assertEqual({item["path"] for item in metadata_gaps}, {"src/pyproject.toml", "src/pyvenv.cfg"})

    def test_notebook_metadata_rejects_duplicates_and_enforces_byte_cap(self) -> None:
        duplicate = b'{"metadata":{"kernelspec":{"name":"safe","name":"unsafe"}},"cells":[]}'
        response = analyze_provider_request(request_for({"src/app.py": b"value = 1\n", "src/demo.ipynb": duplicate}))
        self.assertIn("NOTEBOOK_METADATA_INVALID", {item["code"] for item in response["diagnostics"]})
        self.assertFalse(any(item["kind"] == "notebook_metadata" for item in response["symbols"]))

        oversized = b'{"metadata":{},"padding":"' + b"x" * (1024 * 1024) + b'"}'
        request = request_for({"src/app.py": b"value = 1\n", "src/demo.ipynb": oversized})
        response = analyze_provider_request(request)
        self.assertIn("NOTEBOOK_METADATA_LIMIT", {item["code"] for item in response["diagnostics"]})

    def test_python_startup_hooks_are_not_executed_and_capture_bytes_stay_unchanged(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-python-hooks-") as temp_root:
            site_sentinel = Path(temp_root) / "sitecustomize-sentinel"
            user_sentinel = Path(temp_root) / "usercustomize-sentinel"
            sources = {
                "src/sitecustomize.py": f'open({str(site_sentinel)!r}, "w").write("executed")\n'.encode("utf-8"),
                "src/usercustomize.py": f'open({str(user_sentinel)!r}, "w").write("executed")\n'.encode("utf-8"),
            }
            request = request_for(sources)
            captured_before = {
                item["path"]: base64.b64decode(item["content_base64"])
                for item in request["files"]  # type: ignore[index]
            }

            response = analyze_provider_request(request)

            self.assertFalse(site_sentinel.exists())
            self.assertFalse(user_sentinel.exists())
            captured_after = {
                item["path"]: base64.b64decode(item["content_base64"])
                for item in request["files"]  # type: ignore[index]
            }
            self.assertEqual(captured_after, captured_before)
            self.assertIn("PYTHON_ENV_METADATA_ABSENT", {item["code"] for item in response["diagnostics"]})

    def test_profile_and_source_path_fail_closed(self) -> None:
        request = request_for({"src/app.py": b"value = 1\n"})
        request["profile"]["config"]["allow_project_plugins"] = True  # type: ignore[index]
        with self.assertRaises(ProviderInputError):
            analyze_provider_request(request)
        request = request_for({"src/app.py": b"value = 1\n"})
        request["files"][0]["path"] = "src/../app.py"  # type: ignore[index]
        with self.assertRaises(ProviderInputError):
            analyze_provider_request(request)
        request = request_for({"src/app.py": b"value = 1\n"})
        request["profile"]["config"]["python_version"] = "0.0.0"  # type: ignore[index]
        with self.assertRaises(ProviderInputError):
            analyze_provider_request(request)
        request = request_for({"src/app.py": b"value = 1\n"})
        request["profile"]["config"]["python_executable_digest"] = "0" * 64  # type: ignore[index]
        with self.assertRaises(ProviderInputError):
            analyze_provider_request(request)

    def test_content_length_protocol_rejects_duplicates_and_trailing_bytes(self) -> None:
        frame = encode_frame({"request_id": "sample"})
        self.assertEqual(decode_frame(frame), {"request_id": "sample"})
        duplicate = b'Content-Length: 13\r\n\r\n{"x":1,"x":2}'
        with self.assertRaises(ProtocolError):
            decode_frame(duplicate)
        with self.assertRaises(ProtocolError):
            decode_frame(frame + b"extra")

    def test_json_protocol_enforces_depth_and_object_key_budgets(self) -> None:
        depth_32 = b"[" * 32 + b"0" + b"]" * 32
        depth_33 = b"[" * 33 + b"0" + b"]" * 33
        decoded = decode_frame(b"Content-Length: 65\r\n\r\n" + depth_32)
        for _ in range(32):
            self.assertIsInstance(decoded, list)
            decoded = decoded[0]  # type: ignore[index]
        self.assertEqual(decoded, 0)
        with self.assertRaises(ProtocolError):
            decode_frame(b"Content-Length: 67\r\n\r\n" + depth_33)

        max_keys = encode_frame({f"key-{index}": index for index in range(4096)})
        too_many_keys = encode_frame({f"key-{index}": index for index in range(4097)})
        self.assertEqual(len(decode_frame(max_keys)), 4096)  # type: ignore[arg-type]
        with self.assertRaises(ProtocolError):
            decode_frame(too_many_keys)

    def test_json_protocol_rejects_numeric_and_length_overflow(self) -> None:
        oversized_length = b"Content-Length: " + b"9" * 5000 + b"\r\n\r\n0"
        with self.assertRaises(ProtocolError):
            decode_frame(oversized_length)
        with self.assertRaises(ProtocolError):
            decode_frame(b"Content-Length: 129\r\n\r\n" + b"9" * 128 + b"0")
        with self.assertRaises(ProtocolError):
            decode_frame(b"Content-Length: 6\r\n\r\n1e999")
        with self.assertRaises(ProtocolError):
            encode_frame("x" * (16 * 1024 * 1024))

    def test_process_cli_returns_one_frame_and_does_not_run_input_source(self) -> None:
        with tempfile.TemporaryDirectory(prefix="mce4d-python-provider-") as temp_root:
            sentinel = Path(temp_root) / "provider-sentinel"
            source = f'open({str(sentinel)!r}, "w").write("executed")\n'.encode("utf-8")
            request = request_for({"src/app.py": source})
            frame = encode_frame(request)
            environment = os.environ.copy()
            environment["PYTHONPATH"] = str(PACKAGE_SRC)
            completed = subprocess.run(
                [sys.executable, "-B", "-m", "mce4d_python_provider.cli"],
                input=frame,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                cwd=Path(__file__).resolve().parents[3],
                env=environment,
                timeout=15,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr.decode("utf-8", "replace"))
            self.assertEqual(completed.stderr, b"")
            decoded = decode_frame(completed.stdout)
            self.assertEqual(decoded["message_type"], "analyze_response")  # type: ignore[index]
            self.assertFalse(sentinel.exists())


if __name__ == "__main__":
    unittest.main()
