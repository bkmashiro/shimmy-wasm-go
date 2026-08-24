#!/usr/bin/env python3
"""Generate a deterministic Shimmy Python artifact manifest."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import re


COMMIT_RE = re.compile(r"[0-9a-f]{40}")
REPOSITORY_RE = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+")


def digest(path: pathlib.Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            value.update(chunk)
    return value.hexdigest()


def build_manifest(
    *,
    artifact: pathlib.Path,
    profile: str,
    repository: str,
    commit: str,
    source_date_epoch: int,
    contract: dict,
    source_lock_path: pathlib.Path,
    wasm_shape: dict,
    patch_paths: list[pathlib.Path],
) -> dict:
    if COMMIT_RE.fullmatch(commit) is None:
        raise ValueError("producer commit must be full 40-hex")
    if REPOSITORY_RE.fullmatch(repository) is None:
        raise ValueError("producer repository must be owner/name")
    if profile not in contract["profiles"]:
        raise ValueError("profile is not declared by the contract")
    profile_modules = contract.get("profile_python_modules")
    if not isinstance(profile_modules, dict) or set(profile_modules) != set(contract["profiles"]):
        raise ValueError("profile_python_modules must cover every declared profile")
    python_modules = profile_modules[profile]
    if not isinstance(python_modules, list) or any(not isinstance(name, str) or not name for name in python_modules):
        raise ValueError("profile python modules must be non-empty strings")
    if source_date_epoch <= 0:
        raise ValueError("source date epoch must be positive")
    sources_document = json.loads(source_lock_path.read_text())
    patch_root = source_lock_path.resolve().parent
    patches = []
    for path in sorted(patch_paths):
        resolved = path.resolve()
        try:
            relative = resolved.relative_to(patch_root)
        except ValueError as error:
            raise ValueError(f"patch is outside producer root: {path}") from error
        patches.append({"path": relative.as_posix(), "sha256": digest(resolved)})
    timestamp = dt.datetime.fromtimestamp(source_date_epoch, tz=dt.timezone.utc)
    return {
        "schema": "shimmy-python-runtime-artifact/v1",
        "artifact_contract": contract["artifact_contract"],
        "profile": profile,
        "target": contract["target"],
        "execution_model": contract["execution_model"],
        "python_modules": python_modules,
        "identity_u32": contract["identity_u32"],
        "producer": {
            "project": "shimmy",
            "repository": repository,
            "commit": commit,
            "dirty": False,
        },
        "source_date_epoch": source_date_epoch,
        "source_date_utc": timestamp.isoformat().replace("+00:00", "Z"),
        "source_lock_sha256": digest(source_lock_path),
        "sources": sources_document["sources"],
        "patches": patches,
        "artifact": {
            "name": artifact.name,
            "size": artifact.stat().st_size,
            "sha256": digest(artifact),
        },
        "wasm": wasm_shape,
        "limits": {
            "request_max_bytes": contract["request_max_bytes"],
            "response_max_bytes": contract["response_max_bytes"],
        },
        "capabilities": contract["capabilities"],
        "validation": {
            "structure": "passed",
            "runtime_identity": "pending-consumer-e2e",
            "base_smoke": "pending-consumer-e2e",
        },
        "unsupported": [
            "host calls",
            "filesystem preopens",
            "network",
            "dynamic package installation",
            "SciPy",
            "Pandas",
        ],
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--artifact", type=pathlib.Path, required=True)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--source-date-epoch", type=int, required=True)
    parser.add_argument("--contract", type=pathlib.Path, required=True)
    parser.add_argument("--source-lock", type=pathlib.Path, required=True)
    parser.add_argument("--shape", type=pathlib.Path, required=True)
    parser.add_argument("--patch", type=pathlib.Path, action="append", default=[])
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    manifest = build_manifest(
        artifact=args.artifact,
        profile=args.profile,
        repository=args.repository,
        commit=args.commit,
        source_date_epoch=args.source_date_epoch,
        contract=json.loads(args.contract.read_text()),
        source_lock_path=args.source_lock,
        wasm_shape=json.loads(args.shape.read_text()),
        patch_paths=args.patch,
    )
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
