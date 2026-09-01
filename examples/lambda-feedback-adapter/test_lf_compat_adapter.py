from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
import json
import subprocess
import sys
import types
from typing import Any

import pytest

from lf_compat_adapter import call_function, load_entrypoint, normalize_result

ROOT = Path(__file__).resolve().parent
FIXTURES = ROOT.parent / "lambda-feedback-fixtures"


@contextmanager
def with_paths(*paths: Path):
    """Temporarily prepend paths and clear fixture package imports."""
    original_sys_path = list(sys.path)
    for p in paths:
        sys.path.insert(0, str(p))

    try:
        yield
    finally:
        sys.path = original_sys_path
        for name in list(sys.modules):
            if name.startswith("evaluation_function"):
                sys.modules.pop(name, None)


@contextmanager
def with_fake_sympy():
    """Provide tiny sympy stubs used only for fixture import-time checks."""

    class _DummyBoolean:
        def __new__(cls, *args, **kwargs):
            return super().__new__(cls)

    class _DummyOp(_DummyBoolean):
        def __init__(self, *args, **kwargs):
            self.args = args

    class _DummyNot(_DummyBoolean):
        pass

    def symbols(value):
        return value

    def simplify_logic(value):
        return value

    def Equivalent(left, right):
        return (left, right)

    boolalg_module = types.ModuleType("sympy.logic.boolalg")
    boolalg_module.Boolean = _DummyBoolean
    boolalg_module.And = _DummyOp
    boolalg_module.Or = _DummyOp
    boolalg_module.Xor = _DummyOp
    boolalg_module.Not = _DummyNot

    core_symbol_module = types.ModuleType("sympy.core.symbol")
    core_symbol_module.symbols = symbols

    logic_module = types.ModuleType("sympy.logic")
    logic_module.boolalg = boolalg_module

    core_module = types.ModuleType("sympy.core")
    core_module.symbol = core_symbol_module

    sympy_module = types.ModuleType("sympy")
    sympy_module.logic = logic_module
    sympy_module.core = core_module
    sympy_module.logic.boolalg = boolalg_module
    sympy_module.core.symbol = core_symbol_module
    sympy_module.simplify_logic = simplify_logic
    sympy_module.Equivalent = Equivalent

    stubs = {
        "sympy": sympy_module,
        "sympy.logic": logic_module,
        "sympy.logic.boolalg": boolalg_module,
        "sympy.core": core_module,
        "sympy.core.symbol": core_symbol_module,
    }

    original = {}
    for name, mod in stubs.items():
        if name in sys.modules:
            original[name] = sys.modules[name]
        sys.modules[name] = mod

    try:
        yield
    finally:
        for name in list(stubs):
            if name in original:
                sys.modules[name] = original[name]
            else:
                sys.modules.pop(name, None)


def test_normalize_result_returns_dict_contents() -> None:
    payload = {"a": 1, "b": 2}
    assert normalize_result(payload) == payload


class ModelLike:
    def model_dump(self) -> dict[str, Any]:
        return {"kind": "model_dump"}


class DictLike:
    def dict(self) -> dict[str, Any]:
        return {"kind": "dict"}


class ScalarLike:
    def item(self) -> bool:
        return True


@pytest.mark.parametrize(
    "value, expected",
    [
        (ModelLike(), {"kind": "model_dump"}),
        (DictLike(), {"kind": "dict"}),
        ({"is_correct": ScalarLike()}, {"is_correct": True}),
    ],
)
def test_normalize_result_handles_model_or_dict_methods(value: object, expected: dict[str, Any]) -> None:
    assert normalize_result(value) == expected


@dataclass
class PublicData:
    answer: int
    note: str


class PublicFields:
    def __init__(self, answer: int, note: str):
        self.answer = answer
        self.note = note
        self._private = "hidden"


@pytest.mark.parametrize(
    "value, expected",
    [
        (PublicData(answer=3, note="okay"), {"answer": 3, "note": "okay"}),
        (PublicFields(answer=4, note="fine"), {"answer": 4, "note": "fine"}),
    ],
)
def test_normalize_result_public_fields(value: object, expected: dict[str, Any]) -> None:
    assert normalize_result(value) == expected


def test_call_function_preview_two_arg_signature() -> None:
    calls = []

    def preview_fn(response: Any, params: dict) -> dict:
        calls.append((response, params))
        return {"ok": True}

    out = call_function(preview_fn, method="preview", response="r", answer="a", params={"p": 1})

    assert calls == [("r", {"p": 1})]
    assert out == {"ok": True}


def test_call_function_eval_three_arg_signature() -> None:
    calls = []

    def eval_fn(response: Any, answer: Any, params: dict) -> dict:
        calls.append((response, answer, params))
        return {"ok": True}

    out = call_function(eval_fn, method="eval", response="r", answer="a", params={"p": 1})

    assert calls == [("r", "a", {"p": 1})]
    assert out == {"ok": True}


def test_load_entrypoint_loads_eval_entrypoint_from_fixture_package() -> None:
    fixture_root = FIXTURES / "boilerplate-python"

    with with_paths(ROOT, fixture_root):
        fn = load_entrypoint("evaluation_function.evaluation:evaluation_function")

    assert callable(fn)
    assert fn.__name__ == "evaluation_function"
    assert fn(3.14, 3.14159, {"tolerance": 0.01}) is not None


def test_load_entrypoint_imports_compare_boolean_preview_with_relative_imports() -> None:
    fixture_root = FIXTURES / "compare-boolean"

    with with_fake_sympy():
        with with_paths(ROOT, fixture_root):
            fn = load_entrypoint("evaluation_function.preview:preview_function")
            output = fn("A", {"is_latex": False})

    assert output is not None
    result = normalize_result(output)
    assert isinstance(result, dict)
    assert "preview" in result
    assert isinstance(result["preview"], dict)
    assert result["preview"].get("sympy") == "A"



def run_cli(args: list[str]) -> subprocess.CompletedProcess[str]:
    """Run the local run_lf_eval CLI in a subprocess and return completion output."""

    command = [
        sys.executable,
        str(ROOT / "run_lf_eval.py"),
        *args,
    ]
    return subprocess.run(
        command,
        capture_output=True,
        text=True,
        check=False,
    )


def test_run_lf_eval_cli_eval_invokes_eval_entrypoint_and_prints_json() -> None:
    input_payload = {
        "response": "3.14",
        "answer": "3.14159",
        "params": {"tolerance": 0.01},
    }

    result = run_cli(
        [
            "--root",
            str(FIXTURES / "boilerplate-python"),
            "--eval-entrypoint",
            "evaluation_function.evaluation:evaluation_function",
            "--preview-entrypoint",
            "evaluation_function.preview:preview_function",
            "--method",
            "eval",
            "--input",
            json.dumps(input_payload),
        ]
    )

    assert result.returncode == 0
    assert result.stderr == ""
    payload = json.loads(result.stdout)
    assert payload == {"is_correct": False}


def test_run_lf_eval_cli_preview_invokes_preview_entrypoint_and_prints_json() -> None:
    input_payload = {
        "response": "3.14",
        "answer": "3.14159",
        "params": {"tolerance": 0.01},
    }

    result = run_cli(
        [
            "--root",
            str(FIXTURES / "boilerplate-python"),
            "--eval-entrypoint",
            "evaluation_function.evaluation:evaluation_function",
            "--preview-entrypoint",
            "evaluation_function.preview:preview_function",
            "--method",
            "preview",
            "--input",
            json.dumps(input_payload),
        ]
    )

    assert result.returncode == 0
    assert result.stderr == ""
    payload = json.loads(result.stdout)
    assert payload == {"preview": {"sympy": "3.14"}}


def test_run_lf_eval_cli_preview_falls_back_to_eval_entrypoint_when_not_provided() -> None:
    input_payload = {
        "response": "3.14",
        "answer": "3.14159",
        "params": {"tolerance": 0.01},
    }

    result = run_cli(
        [
            "--root",
            str(FIXTURES / "boilerplate-python"),
            "--eval-entrypoint",
            "evaluation_function.evaluation:evaluation_function",
            "--method",
            "preview",
            "--input",
            json.dumps(input_payload),
        ]
    )

    assert result.returncode == 0
    assert result.stderr == ""
    payload = json.loads(result.stdout)
    assert payload == {"is_correct": False}


def test_run_lf_eval_cli_fails_on_invalid_json() -> None:
    result = run_cli(
        [
            "--root",
            str(FIXTURES / "boilerplate-python"),
            "--eval-entrypoint",
            "evaluation_function.evaluation:evaluation_function",
            "--preview-entrypoint",
            "evaluation_function.preview:preview_function",
            "--method",
            "eval",
            "--input",
            "{not-json}",
        ]
    )

    assert result.returncode != 0
    assert result.stdout == ""
    assert result.stderr.strip() != ""
