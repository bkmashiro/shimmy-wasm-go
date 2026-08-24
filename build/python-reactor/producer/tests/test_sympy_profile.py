from __future__ import annotations

import importlib.util
import pathlib
import sys
import tempfile
import unittest
import zipfile


ROOT = pathlib.Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "tools/build_sympy_profile.py"


def load_module():
    spec = importlib.util.spec_from_file_location("build_sympy_profile", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load SymPy profile builder")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def make_wheel(path: pathlib.Path, package: str, *, native: bool = False) -> None:
    with zipfile.ZipFile(path, "w") as archive:
        archive.writestr(f"{package}/__init__.py", f"NAME = {package!r}\n")
        archive.writestr(f"{package}/core/value.py", "VALUE = 1\n")
        archive.writestr(f"{package}/tests/test_unused.py", "raise RuntimeError('not runtime')\n")
        archive.writestr(f"{package}-1.0.dist-info/METADATA", f"Name: {package}\n")
        if native:
            archive.writestr(f"{package}/native.so", b"native")


class SymPyProfileBuilderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.module = load_module()

    def test_stage_wheels_keeps_packages_and_prunes_tests(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            sympy = root / "sympy.whl"
            mpmath = root / "mpmath.whl"
            make_wheel(sympy, "sympy")
            make_wheel(mpmath, "mpmath")
            site_packages = root / "site-packages"
            modules = self.module.stage_pure_python_wheels(
                {"sympy": sympy, "mpmath": mpmath}, site_packages
            )
            self.assertEqual(modules, ["mpmath", "sympy"])
            self.assertTrue((site_packages / "sympy/core/value.py").is_file())
            self.assertTrue((site_packages / "mpmath/__init__.py").is_file())
            self.assertFalse((site_packages / "sympy/tests").exists())
            self.assertFalse(any(site_packages.glob("*.dist-info")))

    def test_native_extension_in_wheel_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            wheel = root / "sympy.whl"
            make_wheel(wheel, "sympy", native=True)
            with self.assertRaisesRegex(ValueError, "native extension"):
                self.module.stage_pure_python_wheels(
                    {"sympy": wheel}, root / "site-packages"
                )

    def test_wasi_compatibility_patch_removes_ctypes_dependency_exactly(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            site_packages = pathlib.Path(directory)
            target = site_packages / "sympy/external/gmpy.py"
            target.parent.mkdir(parents=True)
            target.write_text(
                "from __future__ import annotations\n"
                "import os\n"
                "from ctypes import c_long, sizeof\n"
                "LONG_MAX = (1 << (8*sizeof(c_long) - 1)) - 1\n"
            )
            changed = self.module.apply_compatibility_patches(site_packages)
            first = target.read_text()
            changed_again = self.module.apply_compatibility_patches(site_packages)
            self.assertEqual(changed, changed_again)
            self.assertEqual(target.read_text(), first)
            self.assertNotIn("ctypes", first)
            self.assertIn('calcsize("l")', first)

    def test_sympy_patch_is_bound_into_manifest_provenance(self) -> None:
        source = MODULE_PATH.read_text()
        self.assertIn("SYMPY_PATCH_PATH", source)
        self.assertIn("patch_paths.append(SYMPY_PATCH_PATH)", source)

    def test_builder_does_not_use_pip_or_dynamic_installation(self) -> None:
        source = MODULE_PATH.read_text().lower()
        self.assertNotIn("pip install", source)
        self.assertNotIn("pip", source)
        self.assertNotIn("subprocess", source)
        self.assertNotIn("_load_verifier", source)
        self.assertIn("wc.verify_shape(", source)
        self.assertNotIn("wc.validate_shape(", source)
        self.assertIn('br._write_notices(dist / "third_party_notices.md", entries)', source)
        self.assertIn("repo_root = producer_root.parents[2]", source)


if __name__ == "__main__":
    unittest.main()
