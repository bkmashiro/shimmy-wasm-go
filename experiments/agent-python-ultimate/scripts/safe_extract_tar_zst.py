#!/usr/bin/env python3
"""Safely stream-extract a bounded tar.zst bundle without tarfile.extractall."""

from __future__ import annotations

import argparse
import os
import pathlib
import posixpath
import shutil
import subprocess
import sys
import tarfile


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("archive", type=pathlib.Path)
    parser.add_argument("destination", type=pathlib.Path)
    parser.add_argument("--max-compressed-bytes", type=int, required=True)
    parser.add_argument("--max-members", type=int, required=True)
    parser.add_argument("--max-total-bytes", type=int, required=True)
    parser.add_argument("--max-file-bytes", type=int, required=True)
    parser.add_argument("--required-root")
    parser.add_argument("--allowed-path", action="append", default=[])
    return parser.parse_args()


def canonical_member_name(raw_name: str, is_directory: bool) -> str:
    if "\x00" in raw_name or raw_name.startswith("/"):
        raise ValueError(f"unsafe archive member: {raw_name!r}")
    comparable = raw_name.rstrip("/") if is_directory else raw_name
    normalized = posixpath.normpath(comparable)
    if normalized in ("", ".") or normalized != comparable:
        raise ValueError(f"non-canonical archive member: {raw_name!r}")
    parts = pathlib.PurePosixPath(normalized).parts
    if ".." in parts:
        raise ValueError(f"unsafe archive member: {raw_name!r}")
    return normalized


def copy_exact(source, destination, expected: int) -> None:
    remaining = expected
    while remaining:
        chunk = source.read(min(1024 * 1024, remaining))
        if not chunk:
            raise ValueError("truncated archive member")
        destination.write(chunk)
        remaining -= len(chunk)
    if source.read(1):
        raise ValueError("archive member exceeds declared size")


def extract(args: argparse.Namespace) -> None:
    archive = args.archive.resolve(strict=True)
    if not archive.is_file():
        raise ValueError("archive is not a regular file")
    compressed_size = archive.stat().st_size
    if compressed_size <= 0 or compressed_size > args.max_compressed_bytes:
        raise ValueError(f"compressed archive size out of bounds: {compressed_size}")

    allowed = set(args.allowed_path)
    if allowed and any(canonical_member_name(name, False) != name for name in allowed):
        raise ValueError("allowed paths must be canonical")

    requested_destination = pathlib.Path(os.path.abspath(args.destination))
    try:
        os.lstat(requested_destination)
    except FileNotFoundError:
        pass
    else:
        raise ValueError("destination must not already exist")
    parent = requested_destination.parent.resolve(strict=True)
    if not parent.is_dir():
        raise ValueError("destination parent is not a directory")
    try:
        os.mkdir(requested_destination, mode=0o700)
    except FileExistsError as error:
        raise ValueError("destination must not already exist") from error
    destination = requested_destination.resolve(strict=True)
    if destination.parent != parent or not destination.is_dir():
        shutil.rmtree(destination, ignore_errors=True)
        raise ValueError("destination was not created under its resolved parent")

    try:
        process = subprocess.Popen(
            ["zstd", "-q", "-d", "-c", "--", str(archive)],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
    except BaseException:
        shutil.rmtree(destination, ignore_errors=True)
        raise
    assert process.stdout is not None
    assert process.stderr is not None
    seen: set[str] = set()
    member_count = 0
    total_bytes = 0
    try:
        with tarfile.open(fileobj=process.stdout, mode="r|") as bundle:
            for member in bundle:
                member_count += 1
                if member_count > args.max_members:
                    raise ValueError("archive member budget exceeded")
                if not (member.isfile() or member.isdir()):
                    raise ValueError(f"unsupported archive member type: {member.name!r}")
                normalized = canonical_member_name(member.name, member.isdir())
                if normalized in seen:
                    raise ValueError(f"duplicate normalized archive member: {normalized!r}")
                seen.add(normalized)
                if allowed and normalized not in allowed:
                    raise ValueError(f"unexpected archive member: {normalized!r}")
                if args.required_root and pathlib.PurePosixPath(normalized).parts[0] != args.required_root:
                    raise ValueError(f"archive member outside required root: {normalized!r}")
                if member.size < 0 or member.size > args.max_file_bytes:
                    raise ValueError(f"archive member size out of bounds: {normalized!r}")
                total_bytes += member.size
                if total_bytes > args.max_total_bytes:
                    raise ValueError("archive total byte budget exceeded")

                target = destination.joinpath(*pathlib.PurePosixPath(normalized).parts)
                if member.isdir():
                    target.mkdir(mode=0o700, parents=True, exist_ok=False)
                    continue
                target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                source = bundle.extractfile(member)
                if source is None:
                    raise ValueError(f"cannot read archive member: {normalized!r}")
                with target.open("xb") as output:
                    os.chmod(target, 0o600)
                    copy_exact(source, output, member.size)

        process.stdout.close()
        stderr = process.stderr.read().decode("utf-8", errors="replace")
        return_code = process.wait()
        if return_code != 0:
            raise ValueError(f"zstd failed with exit {return_code}: {stderr.strip()}")
        if not seen:
            raise ValueError("archive is empty")
        if allowed and seen != allowed:
            missing = sorted(allowed - seen)
            raise ValueError(f"archive is missing required members: {missing}")
    except BaseException:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        shutil.rmtree(destination, ignore_errors=True)
        raise


def main() -> int:
    args = parse_args()
    try:
        extract(args)
    except (OSError, ValueError, tarfile.TarError) as error:
        print(f"safe extraction failed: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
