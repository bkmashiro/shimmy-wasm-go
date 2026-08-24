from __future__ import annotations

import json
import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
CONTRACT = ROOT / "contract" / "shimmy-python-runtime-v1.json"


class ShimmyPythonContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.contract = json.loads(CONTRACT.read_text())

    def test_contract_identity_and_target_are_frozen(self) -> None:
        self.assertEqual(
            self.contract["schema"], "shimmy-python-runtime-contract/v1"
        )
        self.assertEqual(
            self.contract["artifact_contract"], "shimmy-python-runtime/v1"
        )
        self.assertEqual(self.contract["target"], "wasm32-wasip1")
        self.assertEqual(self.contract["identity_u32"], 0x53505231)

    def test_contract_requires_only_the_owned_guest_surface(self) -> None:
        self.assertEqual(
            self.contract["required_exports"],
            [
                "memory",
                "_initialize",
                "shimmy_python_runtime_identity",
                "shimmy_python_init",
                "shimmy_python_prepare",
                "alloc",
                "dealloc",
                "evaluate",
            ],
        )
        self.assertEqual(self.contract["allowed_import_modules"], ["wasi_snapshot_preview1"])
        self.assertEqual(self.contract["forbidden_import_modules"], ["agent_runtime_v1"])

    def test_contract_bounds_requests_and_responses(self) -> None:
        self.assertEqual(self.contract["request_max_bytes"], 1 << 20)
        self.assertEqual(self.contract["response_max_bytes"], 1 << 20)
        self.assertEqual(self.contract["response_layout"], "u32le-length-prefixed-json")

    def test_profiles_declare_importable_python_modules(self) -> None:
        self.assertEqual(
            self.contract["profile_python_modules"],
            {
                "base": [],
                "numpy-core": ["numpy"],
                "sympy": ["mpmath", "sympy"],
            },
        )
        self.assertEqual(sorted(self.contract["profiles"]), ["base", "numpy-core", "sympy"])

    def test_contract_contains_no_external_producer_identity(self) -> None:
        encoded = json.dumps(self.contract, sort_keys=True).lower()
        self.assertNotIn("agent-python-runtime", encoded)
        self.assertNotIn("webassembly-language-runtimes", encoded)


if __name__ == "__main__":
    unittest.main()
