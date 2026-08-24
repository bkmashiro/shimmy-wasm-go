from __future__ import annotations

import importlib.util
import json
import pathlib
import sys
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
BOOTSTRAP = ROOT / "guest" / "bootstrap" / "runtime.py"


def load_bootstrap():
    spec = importlib.util.spec_from_file_location("shimmy_guest_bootstrap", BOOTSTRAP)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load bootstrap")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class BootstrapContractTests(unittest.TestCase):
    def setUp(self) -> None:
        self.runtime = load_bootstrap()

    def call(self, method: str, params: dict) -> dict:
        encoded = json.dumps({"method": method, "params": params})
        return json.loads(self.runtime._shimmy_handle_request(encoded))

    def test_prepare_and_eval(self) -> None:
        self.runtime._shimmy_prepare(
            "def evaluation_function(response, answer, params):\n"
            "    return {'correct': response == answer, 'tag': params['tag']}\n"
        )
        self.assertEqual(
            self.call("eval", {"response": 4, "answer": 4, "params": {"tag": "ok"}}),
            {"status": "ok", "result": {"correct": True, "tag": "ok"}},
        )

    def test_preview_uses_preview_function(self) -> None:
        self.runtime._shimmy_prepare(
            "def evaluation_function(response, answer, params): return {'eval': True}\n"
            "def preview_function(response, params): return {'preview': response, 'params': params}\n"
        )
        self.assertEqual(
            self.call("preview", {"response": "x", "params": {"n": 2}}),
            {"status": "ok", "result": {"preview": "x", "params": {"n": 2}}},
        )

    def test_unknown_fields_and_methods_fail_closed(self) -> None:
        self.runtime._shimmy_prepare(
            "def evaluation_function(response, answer, params): return {}\n"
        )
        unknown = json.loads(
            self.runtime._shimmy_handle_request(
                json.dumps({"method": "eval", "params": {}, "script": "forbidden"})
            )
        )
        method = self.call("exec", {})
        self.assertEqual(unknown["status"], "error")
        self.assertEqual(unknown["error"]["type"], "ValueError")
        self.assertEqual(method["status"], "error")
        self.assertEqual(method["error"]["type"], "ValueError")

    def test_prepare_requires_evaluation_function(self) -> None:
        with self.assertRaisesRegex(ValueError, "evaluation_function"):
            self.runtime._shimmy_prepare("x = 1\n")

    def test_errors_are_typed_and_bounded_by_shape(self) -> None:
        self.runtime._shimmy_prepare(
            "def evaluation_function(response, answer, params): raise RuntimeError('boom')\n"
        )
        result = self.call("eval", {})
        self.assertEqual(result["status"], "error")
        self.assertEqual(result["error"], {"type": "RuntimeError", "message": "boom"})


if __name__ == "__main__":
    unittest.main()
