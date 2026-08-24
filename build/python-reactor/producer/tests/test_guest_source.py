from __future__ import annotations

import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
RUNTIME_C = ROOT / "guest" / "src" / "runtime.c"
HEADER = ROOT / "guest" / "include" / "shimmy_python_runtime_v1.h"
EMBEDDER = ROOT / "tools" / "embed_bootstrap.py"


class GuestSourceContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.runtime = RUNTIME_C.read_text()
        cls.header = HEADER.read_text()
        cls.embedder = EMBEDDER.read_text()

    def test_owned_exports_are_present(self) -> None:
        for symbol in (
            "shimmy_python_runtime_identity",
            "shimmy_python_init",
            "shimmy_python_prepare",
            "alloc",
            "dealloc",
            "evaluate",
        ):
            self.assertIn(f'export_name("{symbol}")', self.runtime)
            self.assertIn(symbol, self.header)

    def test_bounds_and_response_layout_are_explicit(self) -> None:
        self.assertIn("SHIMMY_REQUEST_MAX_BYTES (1u << 20)", self.header)
        self.assertIn("SHIMMY_RESPONSE_MAX_BYTES (1u << 20)", self.header)
        self.assertIn("SHIMMY_RESPONSE_PREFIX_BYTES 4u", self.header)
        self.assertIn("write_u32_le", self.runtime)

    def test_no_external_runtime_or_custom_host_contract(self) -> None:
        combined = (self.runtime + self.header + self.embedder).lower()
        self.assertNotIn("agent-python-runtime", combined)
        self.assertNotIn("webassembly-language-runtimes", combined)
        self.assertNotIn("agent_runtime_v1", combined)
        self.assertNotIn("host_call", combined)

    def test_bootstrap_is_generated_not_duplicated(self) -> None:
        self.assertIn('#include "shimmy_python_bootstrap.inc"', self.runtime)
        self.assertIn("bootstrap_path.read_bytes()", self.embedder)
        self.assertIn("output_path.write_text", self.embedder)

    def test_numpy_core_registration_is_compile_time_only(self) -> None:
        self.assertIn("#ifdef SHIMMY_NUMPY_CORE", self.runtime)
        self.assertIn('PyImport_AppendInittab("numpy._core._multiarray_umath"', self.runtime)
        self.assertIn('PyImport_AppendInittab("numpy.linalg._umath_linalg"', self.runtime)
        self.assertNotIn("PyInit_agent", self.runtime)


if __name__ == "__main__":
    unittest.main()
