#!/usr/bin/env python3
"""Assemble the Shimmy NumPy-core CPython/WASI profile from locked sources."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import pathlib
import shutil
import subprocess
import sys
import venv


PRODUCER_ROOT = pathlib.Path(__file__).resolve().parents[1]
REPO_ROOT = PRODUCER_ROOT.parents[2]
NUMPY_PATCH_PATH = PRODUCER_ROOT / "patches/numpy/static-core.json"


def load_tool(name: str):
    path = PRODUCER_ROOT / "tools" / f"{name}.py"
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def run(command: list[str], *, cwd: pathlib.Path, env: dict[str, str] | None = None) -> None:
    print("+", " ".join(command), flush=True)
    subprocess.run(command, cwd=cwd, env=env, check=True)


def stage_numpy(numpy_root: pathlib.Path, numpy_build: pathlib.Path, site_packages: pathlib.Path) -> None:
    destination = site_packages / "numpy"

    def ignore(_path: str, names: list[str]) -> list[str]:
        return [
            name
            for name in names
            if name in {"tests", "src", "meson.build", "__pycache__"}
            or name.endswith((".pyc", ".c", ".cpp", ".h", ".pxd", ".pyx", ".pyi"))
        ]

    shutil.copytree(numpy_root / "numpy", destination, dirs_exist_ok=True, ignore=ignore)
    config = numpy_build / "numpy" / "__config__.py"
    if not config.is_file():
        raise ValueError("NumPy build did not generate numpy/__config__.py")
    shutil.copy2(config, destination / "__config__.py")
    if len(list(destination.rglob("*.py"))) < 100:
        raise ValueError("staged NumPy package is unexpectedly incomplete")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--work-dir", type=pathlib.Path, required=True)
    parser.add_argument("--dist-dir", type=pathlib.Path, required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--commit")
    parser.add_argument("--source-date-epoch", type=int)
    parser.add_argument("--jobs", type=int, default=1)
    args = parser.parse_args(argv)
    if args.jobs < 1:
        parser.error("--jobs must be positive")

    br = load_tool("build_runtime")
    bn = load_tool("build_numpy_core")
    wc = load_tool("wasm_contract")
    wm = load_tool("write_manifest")

    commit = args.commit or br._git_value("rev-parse", "HEAD")
    epoch = args.source_date_epoch or int(br._git_value("show", "-s", "--format=%ct", commit))
    if br._git_value("status", "--porcelain", "--untracked-files=no"):
        raise ValueError("tracked producer tree must be clean")

    work = args.work_dir.resolve()
    dist = args.dist_dir.resolve()
    if dist.exists():
        shutil.rmtree(dist)
    dist.mkdir(parents=True)
    entries = br._source_index()
    cache = work / "downloads"
    sources = work / "sources"
    sources.mkdir(parents=True, exist_ok=True)
    extracted: dict[str, pathlib.Path] = {}
    for name in (
        "numpy",
        "cython",
        "ninja-linux-x86-64",
        "setuptools",
        "wheel",
        "packaging",
    ):
        archive = br._download(entries[name], cache)
        if name in {"numpy", "cython"}:
            extracted[name] = br._extract_entry(entries[name], archive, sources)

    tool_venv = work / "numpy-tool-venv"
    if tool_venv.exists():
        shutil.rmtree(tool_venv)
    venv.EnvBuilder(with_pip=True).create(tool_venv)
    bin_dir = tool_venv / "bin"
    pip = bin_dir / "pip"
    wheels = [
        cache / pathlib.PurePosixPath(entries[name]["url"]).name
        for name in ("packaging", "setuptools", "wheel", "ninja-linux-x86-64")
    ]
    run([os.fspath(pip), "install", "--no-index", *map(os.fspath, wheels)], cwd=REPO_ROOT)
    cython_archive = cache / pathlib.PurePosixPath(entries["cython"]["url"]).name
    run(
        [os.fspath(pip), "install", "--no-index", "--no-build-isolation", os.fspath(cython_archive)],
        cwd=REPO_ROOT,
    )

    cpython_root = sources / "cpython" / entries["cpython"]["archive_root"]
    target_build = cpython_root / "cross-build/wasm32-wasip1"
    wasi_sdk = sources / "wasi-sdk" / entries["wasi-sdk"]["archive_root"]
    wasmtime = sources / "wasmtime-linux-x86-64" / entries["wasmtime-linux-x86-64"]["archive_root"] / "wasmtime"
    for required in (cpython_root, target_build, wasi_sdk, wasmtime):
        if not required.exists():
            raise ValueError(f"base work-dir prerequisite missing: {required}")

    os.environ["SOURCE_DATE_EPOCH"] = str(epoch)
    numpy_build = work / "numpy-build"
    inventory_path = bn.build_static_core(
        numpy_root=extracted["numpy"],
        cpython_root=cpython_root,
        target_build=target_build,
        wasi_sdk=wasi_sdk,
        wasmtime=wasmtime,
        native_python=bin_dir / "python",
        cython=bin_dir / "cython",
        ninja_dir=bin_dir,
        build_dir=numpy_build,
        output_dir=work / "numpy-output",
        jobs=args.jobs,
    )
    inventory = json.loads(inventory_path.read_text())
    archives = inventory["libraries"]
    if not archives:
        raise ValueError("NumPy static archive inventory is empty")

    generated = work / "generated-numpy"
    generated.mkdir(parents=True, exist_ok=True)
    run(
        [
            sys.executable,
            os.fspath(PRODUCER_ROOT / "tools/embed_bootstrap.py"),
            os.fspath(PRODUCER_ROOT / "guest/bootstrap/runtime.py"),
            os.fspath(generated / "shimmy_python_bootstrap.inc"),
        ],
        cwd=REPO_ROOT,
    )
    raw_artifact = dist / "shimmy-python-runtime-numpy-core.raw.wasm"
    vfs_library = next((sources / "wasi-vfs-library").rglob("libwasi_vfs.a"))
    make_command = [
        "make", "-C", os.fspath(target_build), "-f", "Makefile", "-f",
        os.fspath(PRODUCER_ROOT / "build/link-reactor.mk"),
        f"SHIMMY_RUNTIME_SOURCE={PRODUCER_ROOT / 'guest/src/runtime.c'}",
        f"SHIMMY_RUNTIME_INCLUDE={PRODUCER_ROOT / 'guest/include'}",
        f"SHIMMY_GENERATED_INCLUDE={generated}",
        f"SHIMMY_WASI_VFS_LIBRARY={vfs_library}",
        f"SHIMMY_NUMPY_ARCHIVES={' '.join(archives)}",
        f"SHIMMY_OUTPUT={raw_artifact}",
        "shimmy-python-runtime",
    ]
    run(make_command, cwd=REPO_ROOT)

    stage = work / "vfs-numpy/python3.14"
    if stage.parent.exists():
        shutil.rmtree(stage.parent)
    stage.mkdir(parents=True)
    br._copy_stdlib(cpython_root, target_build, stage)
    stage_numpy(extracted["numpy"], numpy_build, stage / "site-packages")
    artifact = dist / "shimmy-python-runtime-numpy-core.wasm"
    vfs_cli = next((sources / "wasi-vfs-cli-linux-x86-64").rglob("wasi-vfs"))
    run(
        [os.fspath(vfs_cli), "pack", os.fspath(raw_artifact), "--mapdir", f"/usr/local/lib/python3.14::{stage}", "-o", os.fspath(artifact)],
        cwd=REPO_ROOT,
    )

    contract = json.loads(br.CONTRACT_PATH.read_text())
    shape = wc.inspect_wasm(artifact.read_bytes())
    wc.verify_shape(
        shape,
        required_exports=contract["required_exports"],
        allowed_import_modules=contract["allowed_import_modules"],
    )
    shape_path = dist / "wasm-shape.json"
    shape_path.write_text(json.dumps(shape, indent=2, sort_keys=True) + "\n")
    patch_paths = sorted((PRODUCER_ROOT / "patches/cpython").glob("*.*"))
    patch_paths.append(NUMPY_PATCH_PATH)
    manifest = wm.build_manifest(
        artifact=artifact,
        profile="numpy-core",
        repository=args.repository,
        commit=commit,
        source_date_epoch=epoch,
        contract=contract,
        source_lock_path=br.LOCK_PATH,
        wasm_shape=shape,
        patch_paths=patch_paths,
    )
    manifest_path = dist / "manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    bundled_lock = dist / br.LOCK_PATH.name
    shutil.copy2(br.LOCK_PATH, bundled_lock)
    br._write_notices(dist / "THIRD_PARTY_NOTICES.md", entries)
    paths = [artifact, raw_artifact, manifest_path, shape_path, bundled_lock]
    (dist / "SHA256SUMS").write_text(
        "".join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in paths)
    )
    print(json.dumps({"profile": "numpy-core", "artifact": os.fspath(artifact), "sha256": manifest["artifact"]["sha256"]}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
