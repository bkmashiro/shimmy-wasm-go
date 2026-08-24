from __future__ import annotations

import importlib.util
import json
import pathlib
import sys
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "tools" / "write_manifest.py"
CONTRACT_PATH = ROOT / "contract" / "shimmy-python-runtime-v1.json"
LOCK_PATH = ROOT / "sources.lock.json"


def load_module():
    spec = importlib.util.spec_from_file_location("write_manifest", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load manifest writer")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class ManifestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.module = load_module()

    def test_manifest_binds_artifact_and_clean_shimmy_commit(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            artifact = root / "runtime.wasm"
            artifact.write_bytes(b"wasm-bytes")
            manifest = self.module.build_manifest(
                artifact=artifact,
                profile="base",
                repository="bkmashiro/shimmy",
                commit="a" * 40,
                source_date_epoch=1234567890,
                contract=json.loads(CONTRACT_PATH.read_text()),
                source_lock_path=LOCK_PATH,
                wasm_shape={"imports": [], "exports": []},
                patch_paths=[ROOT / "patches/cpython/relative-nanosleep.site"],
            )
        self.assertEqual(manifest["schema"], "shimmy-python-runtime-artifact/v1")
        self.assertEqual(manifest["patches"][0]["path"], "patches/cpython/relative-nanosleep.site")
        self.assertEqual(manifest["artifact_contract"], "shimmy-python-runtime/v1")
        self.assertEqual(manifest["producer"], {"project": "shimmy", "repository": "bkmashiro/shimmy", "commit": "a" * 40, "dirty": False})
        self.assertEqual(manifest["artifact"]["size"], 10)
        self.assertRegex(manifest["artifact"]["sha256"], r"^[0-9a-f]{64}$")
        self.assertRegex(manifest["source_lock_sha256"], r"^[0-9a-f]{64}$")
        self.assertEqual(manifest["python_modules"], [])
        self.assertIn("SciPy", manifest["unsupported"])
        self.assertIn("Pandas", manifest["unsupported"])
        self.assertNotIn("SymPy", manifest["unsupported"])

    def test_profile_modules_come_from_contract(self) -> None:
        contract = json.loads(CONTRACT_PATH.read_text())
        with tempfile.TemporaryDirectory() as directory:
            artifact = pathlib.Path(directory) / "runtime.wasm"
            artifact.write_bytes(b"wasm-bytes")
            manifest = self.module.build_manifest(
                artifact=artifact,
                profile="sympy",
                repository="bkmashiro/shimmy",
                commit="a" * 40,
                source_date_epoch=1234567890,
                contract=contract,
                source_lock_path=LOCK_PATH,
                wasm_shape={"imports": [], "exports": []},
                patch_paths=[],
            )
        self.assertEqual(manifest["python_modules"], ["mpmath", "sympy"])

    def test_rejects_non_full_commit(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            artifact = pathlib.Path(directory) / "runtime.wasm"
            artifact.write_bytes(b"x")
            with self.assertRaisesRegex(ValueError, "40-hex"):
                self.module.build_manifest(
                    artifact=artifact,
                    profile="base",
                    repository="bkmashiro/shimmy",
                    commit="abc",
                    source_date_epoch=1,
                    contract=json.loads(CONTRACT_PATH.read_text()),
                    source_lock_path=LOCK_PATH,
                    wasm_shape={"imports": [], "exports": []},
                    patch_paths=[],
                )


if __name__ == "__main__":
    unittest.main()
