#!/usr/bin/env python3
"""Advisory checks for caller-produced Python Reactor artifacts.

This tool is deliberately outside Shimmy's request path. It reports native/WASM
semantic risks without deciding whether evaluator business logic is suitable.
"""

from __future__ import annotations

import argparse
import ast
import json
from pathlib import Path
import subprocess
import sys
from dataclasses import asdict, dataclass


@dataclass(frozen=True, order=True)
class Warning:
    code: str
    path: str
    line: int
    message: str


CATEGORY_MESSAGES = {
    "process": "process creation may be unavailable or behave differently in the WASM sandbox",
    "network": "network access may be unavailable or behave differently in the WASM sandbox",
    "filesystem": "filesystem access depends on explicitly allowed sandbox paths and may differ from native execution",
    "concurrency": "thread, process, or signal behavior may differ from native execution",
    "dynamic": "dynamic loading or native extension behavior may differ from native execution",
}


class AdvisoryVisitor(ast.NodeVisitor):
    def __init__(self) -> None:
        self.categories: dict[str, int] = {}

    def mark(self, category: str, node: ast.AST) -> None:
        self.categories.setdefault(category, getattr(node, "lineno", 1))

    def visit_Import(self, node: ast.Import) -> None:
        for alias in node.names:
            self._classify_module(alias.name, node)
        self.generic_visit(node)

    def visit_ImportFrom(self, node: ast.ImportFrom) -> None:
        if node.module:
            self._classify_module(node.module, node)
        self.generic_visit(node)

    def visit_Call(self, node: ast.Call) -> None:
        name = dotted_name(node.func)
        if name == "open" or name.startswith(("pathlib.", "tempfile.")):
            self.mark("filesystem", node)
        if name.startswith("subprocess.") or name in {"os.system", "os.popen", "os.fork", "os.forkpty"} or name.startswith(("os.exec", "os.spawn")):
            self.mark("process", node)
        if name.startswith(("socket.", "requests.", "urllib.", "httpx.", "aiohttp.", "boto3.")):
            self.mark("network", node)
        if name.startswith(("threading.", "multiprocessing.", "signal.")):
            self.mark("concurrency", node)
        if name in {"ctypes.CDLL", "ctypes.PyDLL", "importlib.import_module"}:
            self.mark("dynamic", node)
        self.generic_visit(node)

    def _classify_module(self, module: str, node: ast.AST) -> None:
        root = module.split(".", 1)[0]
        if root in {"subprocess"}:
            self.mark("process", node)
        elif root in {"socket", "requests", "urllib", "httpx", "aiohttp", "boto3"}:
            self.mark("network", node)
        elif root in {"pathlib", "tempfile"}:
            self.mark("filesystem", node)
        elif root in {"threading", "multiprocessing", "signal"}:
            self.mark("concurrency", node)
        elif root in {"ctypes", "cffi", "importlib"}:
            self.mark("dynamic", node)


def dotted_name(node: ast.AST) -> str:
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        prefix = dotted_name(node.value)
        return f"{prefix}.{node.attr}" if prefix else node.attr
    return ""


def scan_source(root: Path) -> list[Warning]:
    warnings: set[Warning] = set()
    for path in sorted(root.rglob("*.py")):
        try:
            source = path.read_text(encoding="utf-8")
            tree = ast.parse(source, filename=str(path))
        except (OSError, UnicodeError, SyntaxError) as error:
            line = getattr(error, "lineno", 1) or 1
            warnings.add(Warning(
                "source-uninspected",
                str(path),
                line,
                f"static advisory scan could not inspect this file: {error}",
            ))
            continue
        visitor = AdvisoryVisitor()
        visitor.visit(tree)
        for category, line in visitor.categories.items():
            warnings.add(Warning(category, str(path), line, CATEGORY_MESSAGES[category]))

    native_suffixes = {".so", ".dylib", ".dll", ".pyd", ".a"}
    for path in sorted(item for item in root.rglob("*") if item.is_file() and item.suffix.lower() in native_suffixes):
        warnings.add(Warning(
            "native-artifact",
            str(path),
            1,
            "native binary artifact detected; it must be rebuilt or packaged for the selected WASM runtime",
        ))
    return sorted(warnings)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--source", type=Path, help="Python evaluator source tree to scan advisorially")
    result.add_argument("--build-command", help="explicit producer command to run before checking the artifact")
    result.add_argument("--build-dir", type=Path, default=Path("."), help="working directory for --build-command")
    result.add_argument("--artifact", type=Path, help="expected artifact produced by the explicit build command")
    result.add_argument("--json", action="store_true", help="emit a JSON report")
    return result


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    errors: list[str] = []
    warnings: list[Warning] = []

    if args.source is not None:
        if not args.source.is_dir():
            errors.append(f"source directory does not exist: {args.source}")
        else:
            warnings = scan_source(args.source)

    if args.build_command:
        completed = subprocess.run(args.build_command, cwd=args.build_dir, shell=True, check=False)
        if completed.returncode != 0:
            errors.append(f"explicit build command exited with status {completed.returncode}")

    if args.artifact is not None and not args.artifact.is_file():
        errors.append(f"expected artifact does not exist: {args.artifact}")

    if args.source is None and not args.build_command and args.artifact is None:
        errors.append("provide --source, --build-command, or --artifact")

    report = {
        "status": "error" if errors else "ok",
        "warnings": [asdict(warning) for warning in warnings],
        "errors": errors,
        "note": "warnings are advisory; runtime ABI and capability failures are checked separately by shimmy-artifact-check",
    }
    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        for warning in warnings:
            print(f"WARNING {warning.code} {warning.path}:{warning.line}: {warning.message}")
        for error in errors:
            print(f"ERROR: {error}", file=sys.stderr)
        if not errors:
            print(f"OK: advisory scan completed with {len(warnings)} warning(s)")
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main())
