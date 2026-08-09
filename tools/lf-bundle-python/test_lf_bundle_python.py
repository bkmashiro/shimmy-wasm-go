from __future__ import annotations

import importlib.util
from pathlib import Path
import subprocess
import sys
import zipfile

ROOT = Path(__file__).resolve().parents[2]
BUNDLER = Path(__file__).resolve().parent / "lf_bundle_python.py"
FIXTURES = ROOT / "examples" / "lambda-feedback-fixtures"
ADAPTER = ROOT / "examples" / "lambda-feedback-adapter"


def load_module(path: Path):
    spec = importlib.util.spec_from_file_location("generated_bundle", path)
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def run_bundler(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(BUNDLER), *args],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )


def test_bundle_boilerplate_generates_evaluator_owned_dispatch(tmp_path: Path) -> None:
    out = tmp_path / "boilerplate.bundle.py"

    result = run_bundler(
        "--root",
        str(FIXTURES / "boilerplate-python"),
        "--adapter-root",
        str(ADAPTER),
        "--eval-entrypoint",
        "evaluation_function.evaluation:evaluation_function",
        "--preview-entrypoint",
        "evaluation_function.preview:preview_function",
        "--out",
        str(out),
    )

    assert result.returncode == 0, result.stderr
    assert out.exists()
    bundle = load_module(out)
    assert bundle.dispatch("eval", {"response": "2", "answer": "2", "params": {}}) == {"is_correct": True}
    assert bundle.dispatch("preview", {"response": "x + 1", "answer": "", "params": {}}) == {"preview": {"sympy": "x + 1"}}
    try:
        bundle.dispatch("future/chat.v2", {"messages": []})
    except LookupError as error:
        assert "future/chat.v2" in str(error)
    else:
        raise AssertionError("unsupported method must not fall back to eval")


def test_generated_bundle_does_not_need_fixture_paths_at_runtime(tmp_path: Path) -> None:
    out = tmp_path / "boilerplate.bundle.py"
    result = run_bundler(
        "--root",
        str(FIXTURES / "boilerplate-python"),
        "--adapter-root",
        str(ADAPTER),
        "--eval-entrypoint",
        "evaluation_function.evaluation:evaluation_function",
        "--preview-entrypoint",
        "evaluation_function.preview:preview_function",
        "--out",
        str(out),
    )
    assert result.returncode == 0, result.stderr

    smoke = subprocess.run(
        [
            sys.executable,
            "-c",
            "import importlib.util; "
            f"p={str(out)!r}; "
            "s=importlib.util.spec_from_file_location('b', p); "
            "m=importlib.util.module_from_spec(s); s.loader.exec_module(m); "
            "print(m.dispatch('eval', {'response':'a','answer':'a','params':{}})); "
            "print(m.dispatch('preview', {'response':'a','answer':'','params':{}}))",
        ],
        cwd=tmp_path,
        capture_output=True,
        text=True,
        check=False,
    )

    assert smoke.returncode == 0, smoke.stderr
    assert "{'is_correct': True}" in smoke.stdout
    assert "{'preview': {'sympy': 'a'}}" in smoke.stdout


def test_bundle_can_embed_extra_pure_python_include_roots(tmp_path: Path) -> None:
    include_root = tmp_path / "vendor"
    package = include_root / "helper_pkg"
    package.mkdir(parents=True)
    (package / "__init__.py").write_text("VALUE = 41\n")
    (package / "maths.py").write_text("from . import VALUE\ndef add_one():\n    return VALUE + 1\n")

    fixture = tmp_path / "fixture"
    eval_pkg = fixture / "evaluation_function"
    eval_pkg.mkdir(parents=True)
    (eval_pkg / "__init__.py").write_text("")
    (eval_pkg / "evaluation.py").write_text(
        "from helper_pkg.maths import add_one\n"
        "def evaluation_function(response, answer, params=None):\n"
        "    return {'is_correct': add_one() == 42}\n"
    )

    out = tmp_path / "with-vendor.bundle.py"
    result = run_bundler(
        "--root",
        str(fixture),
        "--adapter-root",
        str(ADAPTER),
        "--include-root",
        str(include_root),
        "--eval-entrypoint",
        "evaluation_function.evaluation:evaluation_function",
        "--out",
        str(out),
    )

    assert result.returncode == 0, result.stderr
    bundle = load_module(out)
    assert bundle.dispatch("eval", {"response": "", "answer": "", "params": {}}) == {"is_correct": True}


def test_bundle_can_add_zip_payloads_to_sys_path(tmp_path: Path) -> None:
    dep_root = tmp_path / "dep-src"
    package = dep_root / "zip_dep"
    package.mkdir(parents=True)
    (package / "__init__.py").write_text("from .maths import value\n")
    (package / "maths.py").write_text("def value():\n    return 42\n")

    zip_path = tmp_path / "deps.zip"
    with zipfile.ZipFile(zip_path, "w") as zf:
        for path in sorted(dep_root.rglob("*.py")):
            zf.write(path, path.relative_to(dep_root).as_posix())

    fixture = tmp_path / "fixture"
    eval_pkg = fixture / "evaluation_function"
    eval_pkg.mkdir(parents=True)
    (eval_pkg / "__init__.py").write_text("")
    (eval_pkg / "evaluation.py").write_text(
        "from zip_dep import value\n"
        "def evaluation_function(response, answer, params=None):\n"
        "    return {'is_correct': value() == 42}\n"
    )

    out = tmp_path / "with-zip-payload.bundle.py"
    result = run_bundler(
        "--root",
        str(fixture),
        "--adapter-root",
        str(ADAPTER),
        "--sys-path",
        str(zip_path),
        "--eval-entrypoint",
        "evaluation_function.evaluation:evaluation_function",
        "--out",
        str(out),
    )

    assert result.returncode == 0, result.stderr
    text = out.read_text()
    assert str(zip_path) in text
    bundle = load_module(out)
    assert bundle.dispatch("eval", {"response": "", "answer": "", "params": {}}) == {"is_correct": True}


def test_bundle_rewrites_legacy_typing_io_imports_for_python_314(tmp_path: Path) -> None:
    include_root = tmp_path / "vendor"
    package = include_root / "old_antlr_like"
    package.mkdir(parents=True)
    (package / "__init__.py").write_text("")
    (package / "lexer.py").write_text(
        "from typing.io import TextIO\n"
        "def typename():\n"
        "    return TextIO.__name__\n"
    )

    fixture = tmp_path / "fixture"
    eval_pkg = fixture / "evaluation_function"
    eval_pkg.mkdir(parents=True)
    (eval_pkg / "__init__.py").write_text("")
    (eval_pkg / "evaluation.py").write_text(
        "from old_antlr_like.lexer import typename\n"
        "def evaluation_function(response, answer, params=None):\n"
        "    return {'is_correct': typename() == 'TextIO'}\n"
    )

    out = tmp_path / "legacy-typing.bundle.py"
    result = run_bundler(
        "--root",
        str(fixture),
        "--adapter-root",
        str(ADAPTER),
        "--include-root",
        str(include_root),
        "--eval-entrypoint",
        "evaluation_function.evaluation:evaluation_function",
        "--out",
        str(out),
    )

    assert result.returncode == 0, result.stderr
    assert "from typing import TextIO" in out.read_text()


def test_bundle_fails_when_entrypoint_module_is_missing(tmp_path: Path) -> None:
    result = run_bundler(
        "--root",
        str(FIXTURES / "boilerplate-python"),
        "--adapter-root",
        str(ADAPTER),
        "--eval-entrypoint",
        "evaluation_function.missing:evaluation_function",
        "--out",
        str(tmp_path / "bad.bundle.py"),
    )

    assert result.returncode != 0
    assert "evaluation_function.missing" in result.stderr
