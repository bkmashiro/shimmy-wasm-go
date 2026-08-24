from __future__ import annotations

import importlib.util
import json
import pathlib
import sys
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
LOCK_PATH = ROOT / "sources.lock.json"
VERIFIER_PATH = ROOT / "tools" / "verify_sources_lock.py"


def load_verifier():
    spec = importlib.util.spec_from_file_location("verify_sources_lock", VERIFIER_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load source lock verifier")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class SourceLockTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.verifier = load_verifier()
        cls.lock = json.loads(LOCK_PATH.read_text())

    def test_checked_in_lock_is_valid(self) -> None:
        self.verifier.validate_lock(self.lock)

    def test_lock_uses_stable_official_sources(self) -> None:
        entries = {entry["name"]: entry for entry in self.lock["sources"]}
        self.assertEqual(entries["cpython"]["version"], "3.14.6")
        self.assertEqual(entries["numpy"]["version"], "2.2.6")
        self.assertEqual(entries["wasi-sdk"]["version"], "33.0")
        self.assertEqual(entries["wasi-vfs-library"]["version"], "0.6.3")
        self.assertEqual(entries["wasi-vfs-cli-linux-x86-64"]["version"], "0.6.3")
        self.assertEqual(entries["wasmtime-linux-x86-64"]["version"], "47.0.2")
        self.assertEqual(entries["sympy"]["version"], "1.14.0")
        self.assertEqual(entries["mpmath"]["version"], "1.3.0")
        self.assertEqual(entries["packaging"]["version"], "26.2")

    def test_rejects_external_yuzhe_runtime_repository(self) -> None:
        mutated = json.loads(json.dumps(self.lock))
        mutated["sources"][0]["url"] = (
            "https://github.com/bkmashiro/agent-python-runtime/releases/download/v1/runtime.wasm"
        )
        with self.assertRaisesRegex(ValueError, "forbidden repository"):
            self.verifier.validate_lock(mutated)

    def test_rejects_mutable_url_and_invalid_digest(self) -> None:
        mutated = json.loads(json.dumps(self.lock))
        mutated["sources"][0]["url"] = "https://github.com/python/cpython/archive/latest.tar.gz"
        mutated["sources"][0]["sha256"] = "bad"
        with self.assertRaises(ValueError):
            self.verifier.validate_lock(mutated)

    def test_rejects_unknown_fields(self) -> None:
        mutated = json.loads(json.dumps(self.lock))
        mutated["sources"][0]["unexpected"] = True
        with self.assertRaisesRegex(ValueError, "unknown fields"):
            self.verifier.validate_lock(mutated)


if __name__ == "__main__":
    unittest.main()
