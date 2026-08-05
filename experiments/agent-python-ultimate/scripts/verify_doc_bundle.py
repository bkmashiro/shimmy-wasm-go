#!/usr/bin/env python3
"""Independently verify a prepared DoC bundle against a signed Git commit."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import posixpath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

BUNDLE_FILES = {
    "job.sh",
    "safe-extract-tar-zst.py",
    "safe-extract.sha256",
    "input.tar.zst",
    "input.sha256",
    "bundle.sha256",
}
INPUT_FILES = {
    "agent-python-ultimate",
    "ultimate.json",
    "agent-python-runtime-numpy-core.wasm",
    "manifest.json",
    "plan-seed.txt",
    "plan.preview.json",
    "input-manifest.json",
    "input-files.sha256",
}
MANIFEST_PAYLOAD_FILES = INPUT_FILES - {"input-manifest.json", "input-files.sha256"}


def digest(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def run(argv: list[str], *, cwd: pathlib.Path, env: dict[str, str] | None = None) -> None:
    subprocess.run(argv, cwd=cwd, env=env, check=True)


def parse_checksum(path: pathlib.Path, expected_names: set[str]) -> dict[str, str]:
    records: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9._-]+)", line)
        if not match or match.group(2) in records:
            raise ValueError(f"invalid checksum line in {path.name}")
        records[match.group(2)] = match.group(1)
    if set(records) != expected_names:
        raise ValueError(f"checksum file set mismatch in {path.name}")
    return records


def extract_signed_git_archive(archive_path: pathlib.Path, destination: pathlib.Path) -> None:
    seen: set[str] = set()
    total = 0
    with tarfile.open(archive_path, "r:") as archive:
        for member in archive:
            raw = member.name.rstrip("/")
            canonical = posixpath.normpath(raw)
            if (
                not raw
                or canonical != raw
                or canonical.startswith("/")
                or canonical == ".."
                or canonical.startswith("../")
                or canonical in seen
                or not (member.isfile() or member.isdir())
            ):
                raise ValueError(f"unsafe signed Git archive member: {member.name!r}")
            seen.add(canonical)
            if len(seen) > 100000:
                raise ValueError("signed Git archive member budget exceeded")
            target = destination.joinpath(*pathlib.PurePosixPath(canonical).parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=False)
                continue
            total += member.size
            if member.size > 1073741824 or total > 4294967296:
                raise ValueError("signed Git archive byte budget exceeded")
            target.parent.mkdir(parents=True, exist_ok=True)
            source = archive.extractfile(member)
            if source is None:
                raise ValueError(f"cannot read signed Git archive member: {member.name}")
            with source, target.open("xb") as output:
                shutil.copyfileobj(source, output, length=1024 * 1024)


def verify(bundle: pathlib.Path, repository: pathlib.Path) -> None:
    bundle = bundle.resolve(strict=True)
    repository = repository.resolve(strict=True)
    current_commit = subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=repository, text=True
    ).strip()
    run(["git", "verify-commit", current_commit], cwd=repository)
    for relative, local in (
        ("experiments/agent-python-ultimate/scripts/verify_doc_bundle.py", pathlib.Path(__file__).resolve()),
        ("experiments/agent-python-ultimate/scripts/safe_extract_tar_zst.py", repository / "experiments/agent-python-ultimate/scripts/safe_extract_tar_zst.py"),
    ):
        committed = subprocess.check_output(
            ["git", "show", f"{current_commit}:{relative}"], cwd=repository
        )
        if local.read_bytes() != committed:
            raise ValueError(f"local verifier dependency differs from signed HEAD: {relative}")
    entries = {entry.name for entry in bundle.iterdir()}
    if entries != BUNDLE_FILES:
        raise ValueError(f"bundle file set mismatch: {sorted(entries ^ BUNDLE_FILES)}")
    for name in BUNDLE_FILES:
        path = bundle / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size <= 0:
            raise ValueError(f"invalid bundle file: {name}")

    bundle_records = parse_checksum(
        bundle / "bundle.sha256", BUNDLE_FILES - {"bundle.sha256"}
    )
    for name, expected in bundle_records.items():
        if digest(bundle / name) != expected:
            raise ValueError(f"bundle checksum mismatch: {name}")
    input_records = parse_checksum(bundle / "input.sha256", {"input.tar.zst"})
    if digest(bundle / "input.tar.zst") != input_records["input.tar.zst"]:
        raise ValueError("input archive checksum mismatch")
    extractor_records = parse_checksum(
        bundle / "safe-extract.sha256", {"safe-extract-tar-zst.py"}
    )
    if digest(bundle / "safe-extract-tar-zst.py") != extractor_records["safe-extract-tar-zst.py"]:
        raise ValueError("safe extractor checksum mismatch")

    with tempfile.TemporaryDirectory(prefix="shimmy-doc-bundle-verify-") as temporary:
        temp = pathlib.Path(temporary)
        extracted = temp / "input"
        safe_extractor = repository / "experiments/agent-python-ultimate/scripts/safe_extract_tar_zst.py"
        run(
            [
                str(safe_extractor), str(bundle / "input.tar.zst"), str(extracted),
                "--max-compressed-bytes", "268435456", "--max-members", "8",
                "--max-total-bytes", "1073741824", "--max-file-bytes", "536870912",
                *sum((["--allowed-path", name] for name in sorted(INPUT_FILES)), []),
            ],
            cwd=repository,
        )
        internal_records = parse_checksum(
            extracted / "input-files.sha256", INPUT_FILES - {"input-files.sha256"}
        )
        for name, expected in internal_records.items():
            if digest(extracted / name) != expected:
                raise ValueError(f"input checksum mismatch: {name}")

        manifest = json.loads((extracted / "input-manifest.json").read_text(encoding="utf-8"))
        if set(manifest) != {"schema", "source_commit", "config_path", "plan_seed", "files"}:
            raise ValueError("unexpected input manifest fields")
        source_commit = manifest.get("source_commit")
        if manifest.get("schema") != "shimmy-agent-python-doc-input/v1" or not isinstance(source_commit, str) or not re.fullmatch(r"[0-9a-f]{40}", source_commit):
            raise ValueError("invalid input manifest identity")
        if source_commit != current_commit:
            raise ValueError("bundle source commit differs from current HEAD")

        config_path = manifest.get("config_path")
        if not isinstance(config_path, str) or pathlib.PurePosixPath(config_path).is_absolute() or ".." in pathlib.PurePosixPath(config_path).parts:
            raise ValueError("invalid committed config path")
        seed = manifest.get("plan_seed")
        if not isinstance(seed, int) or seed <= 0:
            raise ValueError("invalid plan seed")
        files = manifest.get("files")
        if not isinstance(files, dict) or set(files) != MANIFEST_PAYLOAD_FILES:
            raise ValueError("input manifest payload set mismatch")
        for name in MANIFEST_PAYLOAD_FILES:
            record = files.get(name)
            path = extracted / name
            if record != {"bytes": path.stat().st_size, "sha256": digest(path)}:
                raise ValueError(f"input manifest file mismatch: {name}")

        source_tree = temp / "source"
        source_tree.mkdir(mode=0o700)
        archive_path = temp / "source.tar"
        with archive_path.open("wb") as output:
            subprocess.run(
                ["git", "archive", "--format=tar", source_commit],
                cwd=repository,
                stdout=output,
                check=True,
            )
        extract_signed_git_archive(archive_path, source_tree)
        committed_config = source_tree / pathlib.PurePosixPath(config_path)
        if not committed_config.is_file() or committed_config.read_bytes() != (extracted / "ultimate.json").read_bytes():
            raise ValueError("bundle config differs from signed source")
        committed_job = source_tree / "experiments/agent-python-ultimate/slurm/job.sh"
        committed_extractor = source_tree / "experiments/agent-python-ultimate/scripts/safe_extract_tar_zst.py"
        if committed_job.read_bytes() != (bundle / "job.sh").read_bytes():
            raise ValueError("bundle job.sh differs from signed source")
        if committed_extractor.read_bytes() != (bundle / "safe-extract-tar-zst.py").read_bytes():
            raise ValueError("bundle extractor differs from signed source")

        rebuilt = temp / "agent-python-ultimate"
        build_env = os.environ.copy()
        build_env.update({"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64"})
        run(
            [
                "go", "build", "-trimpath", "-ldflags", f"-X main.sourceCommit={source_commit}",
                "-o", str(rebuilt), "./experiments/agent-python-ultimate",
            ],
            cwd=source_tree,
            env=build_env,
        )
        if digest(rebuilt) != digest(extracted / "agent-python-ultimate"):
            raise ValueError("runner binary is not the deterministic signed-source build")
        regenerated_plan = temp / "plan.preview.json"
        run(
            [
                "go", "run", "./experiments/agent-python-ultimate", "plan",
                "--config", str(committed_config), "--seed", str(seed),
                "--output", str(regenerated_plan),
            ],
            cwd=source_tree,
        )
        if regenerated_plan.read_bytes() != (extracted / "plan.preview.json").read_bytes():
            raise ValueError("plan preview differs from signed-source expansion")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("bundle", type=pathlib.Path)
    parser.add_argument("repository", type=pathlib.Path)
    args = parser.parse_args()
    try:
        verify(args.bundle, args.repository)
    except (OSError, ValueError, subprocess.CalledProcessError, json.JSONDecodeError, tarfile.TarError) as error:
        print(f"bundle verification failed: {error}", file=sys.stderr)
        return 2
    print("bundle verification passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
