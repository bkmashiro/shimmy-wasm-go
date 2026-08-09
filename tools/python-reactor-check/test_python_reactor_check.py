from __future__ import annotations

import importlib.util
import io
from pathlib import Path
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout


MODULE_PATH = Path(__file__).with_name("python_reactor_check.py")
SPEC = importlib.util.spec_from_file_location("python_reactor_check", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class PythonReactorCheckTests(unittest.TestCase):
    def test_native_semantic_risks_are_advisory(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "evaluator.py").write_text(
                "import subprocess\nimport requests\nfrom pathlib import Path\n"
                "def run():\n    open('x').read()\n    subprocess.run(['python'])\n    requests.get('https://example.test')\n",
                encoding="utf-8",
            )
            stdout = io.StringIO()
            with redirect_stdout(stdout):
                status = MODULE.main(["--source", str(root), "--json"])

        self.assertEqual(0, status)
        output = stdout.getvalue()
        self.assertIn('"status": "ok"', output)
        self.assertIn('"code": "process"', output)
        self.assertIn('"code": "network"', output)
        self.assertIn('"code": "filesystem"', output)

    def test_uninspectable_source_is_warning_not_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "future.py").write_text("def broken(:\n", encoding="utf-8")
            warnings = MODULE.scan_source(root)

        self.assertEqual(["source-uninspected"], [warning.code for warning in warnings])

    def test_explicit_build_failure_is_error(self) -> None:
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            status = MODULE.main(["--build-command", "exit 7"])

        self.assertEqual(1, status)
        self.assertIn("status 7", stderr.getvalue())

    def test_missing_expected_artifact_is_error(self) -> None:
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            status = MODULE.main(["--artifact", "/definitely/missing/evaluator.wasm"])

        self.assertEqual(1, status)
        self.assertIn("expected artifact does not exist", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
