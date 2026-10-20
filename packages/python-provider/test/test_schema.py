from __future__ import annotations

import json
from pathlib import Path
import sys
import unittest

PACKAGE_SRC = Path(__file__).resolve().parents[1] / "src"
sys.path.insert(0, str(PACKAGE_SRC))

from mce4d_python_provider.descriptor import language_provider_descriptor


class ProfileSchemaTests(unittest.TestCase):
    def test_python_profile_schema_is_closed_and_sealed(self) -> None:
        schema_path = Path(__file__).resolve().parents[1] / "src/mce4d_python_provider/python-profile.v1.schema.json"
        schema = json.loads(schema_path.read_text(encoding="utf-8"))
        self.assertEqual(schema["additionalProperties"], False)
        self.assertEqual(schema["properties"]["language"]["const"], "python")
        self.assertEqual(schema["properties"]["allow_project_plugins"]["const"], False)
        self.assertIn("python_executable_digest", schema["required"])
        capability_ids = {item["id"] for item in language_provider_descriptor["capabilities"]}
        self.assertIn("relations.typed_annotations", capability_ids)
        self.assertIn("symbols.notebook_metadata", capability_ids)


if __name__ == "__main__":
    unittest.main()
