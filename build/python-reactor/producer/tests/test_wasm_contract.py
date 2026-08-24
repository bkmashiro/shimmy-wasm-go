from __future__ import annotations

import importlib.util
import pathlib
import struct
import sys
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "tools" / "wasm_contract.py"


def load_module():
    spec = importlib.util.spec_from_file_location("wasm_contract", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load wasm parser")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def uleb(value: int) -> bytes:
    encoded = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        encoded.append(byte | (0x80 if value else 0))
        if not value:
            return bytes(encoded)


def name(value: str) -> bytes:
    data = value.encode()
    return uleb(len(data)) + data


def section(identifier: int, payload: bytes) -> bytes:
    return bytes([identifier]) + uleb(len(payload)) + payload


def synthetic_wasm(import_module: str = "wasi_snapshot_preview1") -> bytes:
    imports = uleb(1) + name(import_module) + name("fd_write") + b"\x00" + uleb(0)
    exports = (
        uleb(2)
        + name("memory") + b"\x02" + uleb(0)
        + name("evaluate") + b"\x00" + uleb(0)
    )
    return b"\x00asm" + struct.pack("<I", 1) + section(2, imports) + section(7, exports)


class WasmContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.module = load_module()

    def test_reads_actual_imports_and_exports(self) -> None:
        shape = self.module.inspect_wasm(synthetic_wasm())
        self.assertEqual(shape["imports"], [{"module": "wasi_snapshot_preview1", "name": "fd_write", "kind": "function"}])
        self.assertEqual(shape["exports"], [{"name": "memory", "kind": "memory"}, {"name": "evaluate", "kind": "function"}])

    def test_rejects_forbidden_import_module(self) -> None:
        with self.assertRaisesRegex(ValueError, "forbidden import module"):
            self.module.verify_shape(
                self.module.inspect_wasm(synthetic_wasm("forbidden_host")),
                required_exports=["memory", "evaluate"],
                allowed_import_modules=["wasi_snapshot_preview1"],
            )

    def test_rejects_missing_export(self) -> None:
        with self.assertRaisesRegex(ValueError, "missing exports"):
            self.module.verify_shape(
                self.module.inspect_wasm(synthetic_wasm()),
                required_exports=["memory", "evaluate", "shimmy_python_init"],
                allowed_import_modules=["wasi_snapshot_preview1"],
            )

    def test_rejects_invalid_magic_and_truncated_sections(self) -> None:
        for payload in (b"not-wasm", b"\x00asm\x01\x00\x00\x00\x02\x80"):
            with self.subTest(payload=payload), self.assertRaises(ValueError):
                self.module.inspect_wasm(payload)


if __name__ == "__main__":
    unittest.main()
