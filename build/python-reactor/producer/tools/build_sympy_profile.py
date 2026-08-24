#!/usr/bin/env python3
"""Assemble a pure-Python SymPy profile over a source-bound base artifact."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import pathlib
import shutil
import stat
import sys
import zipfile


PRODUCER_ROOT = pathlib.Path(__file__).resolve().parents[1]
REPO_ROOT = PRODUCER_ROOT.parents[2]
NATIVE_SUFFIXES = (".so", ".pyd", ".dylib", ".dll", ".a")
SYMPY_PATCH_PATH = PRODUCER_ROOT / "patches/sympy/wasi-compat.json"


def load_tool(name: str):
    path = PRODUCER_ROOT / "tools" / f"{name}.py"
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def _safe_parts(name: str) -> tuple[str, ...]:
    path = pathlib.PurePosixPath(name)
    if path.is_absolute() or not path.parts or any(part in {"", ".", ".."} for part in path.parts):
        raise ValueError(f"unsafe wheel member: {name!r}")
    return path.parts


def stage_pure_python_wheels(
    wheels: dict[str, pathlib.Path], site_packages: pathlib.Path
) -> list[str]:
    """Copy package sources from pinned pure-Python wheels into a VFS stage."""
    site_packages.mkdir(parents=True, exist_ok=True)
    staged: list[str] = []
    for module, wheel in sorted(wheels.items()):
        copied = 0
        with zipfile.ZipFile(wheel) as archive:
            members = archive.infolist()
            for member in members:
                parts = _safe_parts(member.filename)
                if member.filename.lower().endswith(NATIVE_SUFFIXES):
                    raise ValueError(f"native extension in pure-Python profile wheel: {member.filename}")
                unix_mode = member.external_attr >> 16
                if stat.S_ISLNK(unix_mode):
                    raise ValueError(f"symlink in pure-Python profile wheel: {member.filename}")
                if parts[0] != module or member.is_dir():
                    continue
                if "tests" in parts[1:] or "__pycache__" in parts or parts[-1].endswith(".pyc"):
                    continue
                destination = site_packages.joinpath(*parts)
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.write_bytes(archive.read(member))
                copied += 1
        if copied == 0 or not (site_packages / module / "__init__.py").is_file():
            raise ValueError(f"wheel did not provide importable package {module!r}")
        staged.append(module)
    return staged


def apply_compatibility_patches(site_packages: pathlib.Path) -> list[pathlib.Path]:
    """Apply exact, idempotent WASI compatibility patches to staged packages."""
    patches = json.loads(SYMPY_PATCH_PATH.read_text())
    changed: list[pathlib.Path] = []
    for item in patches:
        if set(item) != {"path", "old", "new"}:
            raise ValueError("SymPy patch entry has unknown fields")
        relative = pathlib.PurePosixPath(item["path"])
        if relative.is_absolute() or ".." in relative.parts:
            raise ValueError(f"unsafe SymPy patch path: {relative}")
        target = site_packages / relative
        source = target.read_text()
        old_count = source.count(item["old"])
        new_count = source.count(item["new"])
        if old_count == 1 and new_count == 0:
            target.write_text(source.replace(item["old"], item["new"]))
        elif old_count != 0 or new_count != 1:
            raise ValueError(f"SymPy patch no longer matches exactly: {relative}")
        if target not in changed:
            changed.append(target)
    return changed


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--work-dir", type=pathlib.Path, required=True)
    parser.add_argument("--base-dist-dir", type=pathlib.Path, required=True)
    parser.add_argument("--dist-dir", type=pathlib.Path, required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--commit")
    parser.add_argument("--source-date-epoch", type=int)
    args = parser.parse_args(argv)

    br = load_tool("build_runtime")
    wc = load_tool("wasm_contract")
    wm = load_tool("write_manifest")

    commit = args.commit or br._git_value("rev-parse", "HEAD")
    epoch = args.source_date_epoch or int(br._git_value("show", "-s", "--format=%ct", commit))
    if br._git_value("status", "--porcelain", "--untracked-files=no"):
        raise ValueError("tracked producer tree must be clean")

    work = args.work_dir.resolve()
    base_dist = args.base_dist_dir.resolve()
    dist = args.dist_dir.resolve()
    if dist.exists():
        shutil.rmtree(dist)
    dist.mkdir(parents=True)

    base_manifest = json.loads((base_dist / "manifest.json").read_text())
    if base_manifest.get("profile") != "base":
        raise ValueError("SymPy profile requires a base-profile manifest")
    if base_manifest.get("producer", {}).get("commit") != commit:
        raise ValueError("base artifact producer commit does not match requested commit")
    if base_manifest.get("source_lock_sha256") != hashlib.sha256(br.LOCK_PATH.read_bytes()).hexdigest():
        raise ValueError("base artifact source lock does not match current source lock")
    raw_artifact = base_dist / "shimmy-python-runtime-base.raw.wasm"
    if not raw_artifact.is_file():
        raise ValueError(f"base raw artifact missing: {raw_artifact}")

    entries = br._source_index()
    cache = work / "downloads"
    wheels = {
        name: br._download(entries[name], cache)
        for name in ("sympy", "mpmath")
    }

    base_stage = work / "vfs" / "python3.14"
    if not base_stage.is_dir():
        raise ValueError(f"base VFS stage missing: {base_stage}")
    stage = work / "vfs-sympy" / "python3.14"
    if stage.parent.exists():
        shutil.rmtree(stage.parent)
    shutil.copytree(base_stage, stage)
    modules = stage_pure_python_wheels(wheels, stage / "site-packages")
    if modules != ["mpmath", "sympy"]:
        raise ValueError(f"unexpected staged modules: {modules}")
    apply_compatibility_patches(stage / "site-packages")

    sources = work / "sources"
    vfs_cli = next((sources / "wasi-vfs-cli-linux-x86-64").rglob("wasi-vfs"))
    artifact = dist / "shimmy-python-runtime-sympy.wasm"
    br._run(
        [
            os.fspath(vfs_cli),
            "pack",
            os.fspath(raw_artifact),
            "--mapdir",
            f"/usr/local/lib/python3.14::{stage}",
            "-o",
            os.fspath(artifact),
        ],
        cwd=REPO_ROOT,
        env=os.environ.copy(),
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
    patch_paths = [PRODUCER_ROOT / item["path"] for item in base_manifest.get("patches", [])]
    patch_paths.append(SYMPY_PATCH_PATH)
    manifest = wm.build_manifest(
        artifact=artifact,
        profile="sympy",
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
    paths = [artifact, manifest_path, shape_path, bundled_lock]
    (dist / "SHA256SUMS").write_text(
        "".join(
            f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
            for path in paths
        )
    )
    print(
        json.dumps(
            {
                "profile": "sympy",
                "artifact": os.fspath(artifact),
                "python_modules": manifest["python_modules"],
                "sha256": manifest["artifact"]["sha256"],
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
