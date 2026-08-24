#!/usr/bin/env python3
"""Source-bound builder for Shimmy's CPython/WASI artifact."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import pathlib
import posixpath
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
import zipfile


PRODUCER_ROOT = pathlib.Path(__file__).resolve().parents[1]
REPO_ROOT = pathlib.Path(__file__).resolve().parents[4]
LOCK_PATH = PRODUCER_ROOT / "sources.lock.json"
CONTRACT_PATH = PRODUCER_ROOT / "contract" / "shimmy-python-runtime-v1.json"


def _load_sibling(name: str):
    path = PRODUCER_ROOT / "tools" / f"{name}.py"
    spec = importlib.util.spec_from_file_location(f"shimmy_{name}", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"unable to load {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def sha256(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            value.update(chunk)
    return value.hexdigest()


def verify_blob(path: pathlib.Path, *, expected_size: int, expected_sha256: str) -> None:
    actual_size = path.stat().st_size
    if actual_size != expected_size:
        raise ValueError(
            f"size mismatch for {path.name}: expected {expected_size}, got {actual_size}"
        )
    actual_sha256 = sha256(path)
    if actual_sha256 != expected_sha256:
        raise ValueError(
            f"SHA-256 mismatch for {path.name}: expected {expected_sha256}, got {actual_sha256}"
        )


def _safe_member(name: str) -> None:
    path = pathlib.PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts or not path.parts:
        raise ValueError(f"unsafe archive member: {name}")


def _safe_link(name: str, linkname: str, *, relative_to_parent: bool) -> None:
    link = pathlib.PurePosixPath(linkname)
    if link.is_absolute():
        raise ValueError(f"unsafe archive link: {name} -> {linkname}")
    base = pathlib.PurePosixPath(name).parent if relative_to_parent else pathlib.PurePosixPath()
    normalized = posixpath.normpath(str(base / link))
    if normalized == ".." or normalized.startswith("../"):
        raise ValueError(f"unsafe archive link: {name} -> {linkname}")


def extract_archive(archive: pathlib.Path, destination: pathlib.Path) -> None:
    destination.mkdir(parents=True, exist_ok=True)
    if tarfile.is_tarfile(archive):
        with tarfile.open(archive, "r:*") as handle:
            members = handle.getmembers()
            for member in members:
                _safe_member(member.name)
                if member.issym():
                    _safe_link(member.name, member.linkname, relative_to_parent=True)
                elif member.islnk():
                    _safe_link(member.name, member.linkname, relative_to_parent=False)
                elif member.isdev():
                    raise ValueError(f"unsafe archive member: {member.name}")
            if sys.version_info >= (3, 12):
                handle.extractall(destination, members=members, filter="fully_trusted")
            else:
                handle.extractall(destination, members=members)
        return
    if zipfile.is_zipfile(archive):
        with zipfile.ZipFile(archive) as handle:
            for item in handle.infolist():
                _safe_member(item.filename)
                mode = item.external_attr >> 16
                if stat.S_ISLNK(mode):
                    raise ValueError(f"unsafe archive member: {item.filename}")
            handle.extractall(destination)
        return
    raise ValueError(f"unsupported archive format: {archive.name}")


def cpython_build_command(
    cpython_root: pathlib.Path, wasi_sdk_root: pathlib.Path
) -> list[str]:
    del cpython_root
    return [
        sys.executable,
        "Tools/wasm/wasi",
        "build",
        "--wasi-sdk",
        os.fspath(wasi_sdk_root),
    ]


def _run(command: list[str], *, cwd: pathlib.Path, env: dict[str, str]) -> None:
    print("+", " ".join(command), flush=True)
    subprocess.run(command, cwd=cwd, env=env, check=True)


def _source_index() -> dict[str, dict]:
    verifier = _load_sibling("verify_sources_lock")
    document = json.loads(LOCK_PATH.read_text())
    verifier.validate_lock(document)
    return {entry["name"]: entry for entry in document["sources"]}


def _download(entry: dict, cache_dir: pathlib.Path) -> pathlib.Path:
    cache_dir.mkdir(parents=True, exist_ok=True)
    filename = pathlib.PurePosixPath(entry["url"]).name
    destination = cache_dir / filename
    if destination.exists():
        verify_blob(
            destination,
            expected_size=entry["size"],
            expected_sha256=entry["sha256"],
        )
        return destination
    with tempfile.NamedTemporaryFile(dir=cache_dir, delete=False) as temporary:
        temporary_path = pathlib.Path(temporary.name)
        with urllib.request.urlopen(entry["url"], timeout=120) as response:
            shutil.copyfileobj(response, temporary)
    try:
        verify_blob(
            temporary_path,
            expected_size=entry["size"],
            expected_sha256=entry["sha256"],
        )
        temporary_path.replace(destination)
    except Exception:
        temporary_path.unlink(missing_ok=True)
        raise
    return destination


def _extract_entry(entry: dict, archive: pathlib.Path, sources_dir: pathlib.Path) -> pathlib.Path:
    destination = sources_dir / entry["name"]
    marker = destination / ".shimmy-source-complete"
    if marker.exists():
        return destination / entry["archive_root"] if entry["archive_root"] != "." else destination
    if destination.exists():
        shutil.rmtree(destination)
    temporary = sources_dir / f".{entry['name']}.extracting"
    if temporary.exists():
        shutil.rmtree(temporary)
    temporary.mkdir(parents=True)
    extract_archive(archive, temporary)
    marker = temporary / ".shimmy-source-complete"
    marker.write_text(entry["sha256"] + "\n")
    temporary.replace(destination)
    return destination / entry["archive_root"] if entry["archive_root"] != "." else destination


def _apply_cpython_policy(cpython_root: pathlib.Path) -> list[pathlib.Path]:
    target = cpython_root / "Tools" / "wasm" / "wasi" / "config.site-wasm32-wasi"
    policy = PRODUCER_ROOT / "patches" / "cpython" / "relative-nanosleep.site"
    original = target.read_text()
    additions = policy.read_text()
    settings = ("ac_cv_func_clock_nanosleep", "ac_cv_lib_rt_clock_nanosleep")
    present = [setting in original for setting in settings]
    if not any(present):
        target.write_text(original.rstrip() + "\n\n" + additions)
    elif not all(present) or additions.strip() not in original:
        raise ValueError("upstream timer policy is partial or no longer matches")

    jobs_patch = PRODUCER_ROOT / "patches" / "cpython" / "bounded-build-jobs.json"
    replacement = json.loads(jobs_patch.read_text())
    if set(replacement) != {"path", "old", "new"}:
        raise ValueError("bounded build jobs patch has unknown fields")
    helper = cpython_root / replacement["path"]
    helper_source = helper.read_text()
    old_count = helper_source.count(replacement["old"])
    new_count = helper_source.count(replacement["new"])
    if old_count == 1 and new_count == 0:
        helper.write_text(helper_source.replace(replacement["old"], replacement["new"]))
    elif old_count != 0 or new_count != 1:
        raise ValueError("bounded build jobs patch is partial or no longer matches")
    return [policy, jobs_patch]


def _copy_stdlib(cpython_root: pathlib.Path, target_build: pathlib.Path, stage: pathlib.Path) -> None:
    excluded_roots = {"test", "idlelib", "tkinter", "turtledemo", "ensurepip"}

    def ignore(directory: str, names: list[str]) -> set[str]:
        path = pathlib.Path(directory)
        ignored = {name for name in names if name == "__pycache__" or name.endswith((".pyc", ".pyo"))}
        if path == cpython_root / "Lib":
            ignored |= excluded_roots & set(names)
        return ignored

    if stage.exists():
        shutil.rmtree(stage)
    shutil.copytree(cpython_root / "Lib", stage, ignore=ignore)
    (stage / "site-packages").mkdir(exist_ok=True)
    sysconfig_files = sorted(target_build.glob("build/lib.wasi-wasm32-3.14/_sysconfigdata*.py"))
    if len(sysconfig_files) != 1:
        raise ValueError(f"expected one target sysconfig module, found {len(sysconfig_files)}")
    shutil.copy2(sysconfig_files[0], stage / sysconfig_files[0].name)


def _write_notices(path: pathlib.Path, entries: dict[str, dict]) -> None:
    lines = ["# Third-party inputs", ""]
    for name in sorted(entries):
        entry = entries[name]
        lines.extend(
            [
                f"## {entry['name']} {entry['version']}",
                "",
                f"- License: `{entry['license']}`",
                f"- Source: {entry['url']}",
                f"- SHA-256: `{entry['sha256']}`",
                "",
            ]
        )
    path.write_text("\n".join(lines))


def build_base(
    *,
    work_dir: pathlib.Path,
    dist_dir: pathlib.Path,
    repository: str,
    commit: str,
    source_date_epoch: int,
    jobs: int,
) -> pathlib.Path:
    entries = _source_index()
    required = (
        "cpython",
        "wasi-sdk",
        "wasi-vfs-library",
        "wasi-vfs-cli-linux-x86-64",
        "wasmtime-linux-x86-64",
    )
    cache_dir = work_dir / "downloads"
    sources_dir = work_dir / "sources"
    sources_dir.mkdir(parents=True, exist_ok=True)
    roots: dict[str, pathlib.Path] = {}
    for name in required:
        archive = _download(entries[name], cache_dir)
        roots[name] = _extract_entry(entries[name], archive, sources_dir)

    cpython_root = roots["cpython"]
    wasi_sdk_root = roots["wasi-sdk"]
    wasmtime_root = roots["wasmtime-linux-x86-64"]
    wasi_vfs_tools = roots["wasi-vfs-cli-linux-x86-64"]
    wasi_vfs_library_root = roots["wasi-vfs-library"]
    wasmtime = wasmtime_root / "wasmtime"
    wasi_vfs = wasi_vfs_tools / "wasi-vfs"
    for executable in (wasmtime, wasi_vfs):
        if not executable.is_file():
            raise FileNotFoundError(executable)
        executable.chmod(0o755)
    libraries = sorted(wasi_vfs_library_root.rglob("libwasi_vfs.a"))
    if len(libraries) != 1:
        raise ValueError(f"expected one libwasi_vfs.a, found {len(libraries)}")

    patch_paths = _apply_cpython_policy(cpython_root)
    env = os.environ.copy()
    env.update(
        {
            "PATH": os.pathsep.join([os.fspath(wasmtime.parent), env["PATH"]]),
            "WASMTIME": os.fspath(wasmtime),
            "SOURCE_DATE_EPOCH": str(source_date_epoch),
            "PYTHONHASHSEED": "0",
            "SHIMMY_BUILD_JOBS": str(jobs),
        }
    )
    _run(cpython_build_command(cpython_root, wasi_sdk_root), cwd=cpython_root, env=env)

    target_build = cpython_root / "cross-build" / "wasm32-wasip1"
    if not (target_build / "libpython3.14.a").is_file():
        raise FileNotFoundError(target_build / "libpython3.14.a")
    generated = work_dir / "generated"
    generated.mkdir(parents=True, exist_ok=True)
    bootstrap_include = generated / "shimmy_python_bootstrap.inc"
    _run(
        [
            sys.executable,
            os.fspath(PRODUCER_ROOT / "tools" / "embed_bootstrap.py"),
            os.fspath(PRODUCER_ROOT / "guest" / "bootstrap" / "runtime.py"),
            os.fspath(bootstrap_include),
        ],
        cwd=REPO_ROOT,
        env=env,
    )

    dist_dir.mkdir(parents=True, exist_ok=True)
    raw_artifact = dist_dir / "shimmy-python-runtime-base.raw.wasm"
    link_command = [
        "make",
        "--no-print-directory",
        "-f",
        "Makefile",
        "-f",
        os.fspath(PRODUCER_ROOT / "build" / "link-reactor.mk"),
        f"SHIMMY_RUNTIME_SOURCE={PRODUCER_ROOT / 'guest' / 'src' / 'runtime.c'}",
        f"SHIMMY_RUNTIME_INCLUDE={PRODUCER_ROOT / 'guest' / 'include'}",
        f"SHIMMY_GENERATED_INCLUDE={generated}",
        f"SHIMMY_WASI_VFS_LIBRARY={libraries[0]}",
        f"SHIMMY_OUTPUT={raw_artifact}",
        "shimmy-python-runtime",
    ]
    _run(link_command, cwd=target_build, env=env)

    stage = work_dir / "vfs" / "python3.14"
    _copy_stdlib(cpython_root, target_build, stage)
    artifact = dist_dir / "shimmy-python-runtime-base.wasm"
    _run(
        [
            os.fspath(wasi_vfs),
            "pack",
            os.fspath(raw_artifact),
            "--mapdir",
            f"/usr/local/lib/python3.14::{stage}",
            "-o",
            os.fspath(artifact),
        ],
        cwd=REPO_ROOT,
        env=env,
    )

    contract = json.loads(CONTRACT_PATH.read_text())
    wasm_contract = _load_sibling("wasm_contract")
    shape = wasm_contract.inspect_wasm(artifact.read_bytes())
    wasm_contract.verify_shape(
        shape,
        required_exports=contract["required_exports"],
        allowed_import_modules=contract["allowed_import_modules"],
    )
    shape_path = dist_dir / "wasm-shape.json"
    shape_path.write_text(json.dumps(shape, indent=2, sort_keys=True) + "\n")

    manifest_writer = _load_sibling("write_manifest")
    manifest = manifest_writer.build_manifest(
        artifact=artifact,
        profile="base",
        repository=repository,
        commit=commit,
        source_date_epoch=source_date_epoch,
        contract=contract,
        source_lock_path=LOCK_PATH,
        wasm_shape=shape,
        patch_paths=patch_paths,
    )
    manifest_path = dist_dir / "manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    bundled_lock = dist_dir / LOCK_PATH.name
    shutil.copy2(LOCK_PATH, bundled_lock)
    checksummed = [artifact, raw_artifact, manifest_path, shape_path, bundled_lock]
    (dist_dir / "SHA256SUMS").write_text(
        "".join(f"{sha256(path)}  {path.name}\n" for path in checksummed)
    )
    _write_notices(dist_dir / "THIRD_PARTY_NOTICES.md", entries)
    return artifact


def _git_value(*args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=REPO_ROOT, text=True).strip()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--profile", choices=["base"], default="base")
    parser.add_argument("--work-dir", type=pathlib.Path, default=pathlib.Path(".cache/shimmy-python"))
    parser.add_argument("--dist-dir", type=pathlib.Path, default=pathlib.Path("dist/shimmy-python"))
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY", "bkmashiro/shimmy"))
    parser.add_argument("--commit")
    parser.add_argument("--source-date-epoch", type=int)
    parser.add_argument(
        "--jobs",
        type=int,
        default=int(os.environ.get("SHIMMY_BUILD_JOBS", min(os.cpu_count() or 1, 8))),
    )
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    if args.jobs < 1:
        parser.error("--jobs must be positive")

    commit = args.commit or _git_value("rev-parse", "HEAD")
    source_date_epoch = args.source_date_epoch or int(_git_value("show", "-s", "--format=%ct", commit))
    if args.dry_run:
        print(
            json.dumps(
                {
                    "profile": args.profile,
                    "repository": args.repository,
                    "commit": commit,
                    "source_date_epoch": source_date_epoch,
                    "jobs": args.jobs,
                    "source_lock": os.fspath(LOCK_PATH.relative_to(REPO_ROOT)),
                    "cpython_command": cpython_build_command(
                        pathlib.Path("<cpython>"), pathlib.Path("<wasi-sdk>")
                    ),
                },
                indent=2,
            )
        )
        return 0
    dirty = _git_value("status", "--porcelain", "--untracked-files=no")
    if dirty:
        raise SystemExit("tracked worktree must be clean before artifact build")
    build_base(
        work_dir=args.work_dir.resolve(),
        dist_dir=args.dist_dir.resolve(),
        repository=args.repository,
        commit=commit,
        source_date_epoch=source_date_epoch,
        jobs=args.jobs,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
