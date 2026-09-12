"""Focused generated acceptance codec checks; no provider or persistence claim."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location(
    "acceptance_rec", ROOT / "py/abstraction/job/acceptance/rec.py")
rec = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rec)


class AcceptanceCodecTests(unittest.TestCase):
    def test_corpus_outcomes_roundtrip(self):
        for case in json.loads((ROOT / "testdata/acceptance.json").read_text()):
            with self.subTest(case=case["name"]):
                value = {"outcome": case["want"], "reason": case["name"]}
                if case["want"] == "accepted":
                    value["receipt"] = {
                        "identity": {"key": "stable-key", "history_epoch": "epoch"},
                        "logical_owner": "owner", "operation_id": "original",
                        "accepted_guarantees": ["reconcile@1"],
                        "history_retention_ms": 60000,
                    }
                decoded = rec.decode(json.dumps(value).encode())
                self.assertEqual(json.loads(rec.encode(decoded)), value)
                if driver := os.environ.get("OA_ACCEPTANCE_CPP"):
                    result = subprocess.run([driver], input=rec.encode(decoded),
                                            capture_output=True, check=True)
                    self.assertEqual(json.loads(result.stdout), value)

    def test_unknown_enum_is_refused(self):
        with self.assertRaises(rec.Refusal) as caught:
            rec.decode(b'{"outcome":"future-meaning","reason":""}')
        self.assertIn("bad_enum", str(caught.exception))
        if driver := os.environ.get("OA_ACCEPTANCE_CPP"):
            result = subprocess.run([driver], input=b'{"outcome":"future-meaning","reason":""}',
                                    capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b"bad_enum", result.stderr)


if __name__ == "__main__":
    unittest.main()
