#!/usr/bin/env python3
"""Strict offline validation for Shimmy's Python/WASI source lock."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys
from urllib.parse import urlparse


TOP_FIELDS = {"schema", "sources"}
ENTRY_FIELDS = {
    "name",
    "version",
    "kind",
    "url",
    "sha256",
    "size",
    "archive_root",
    "license",
}
ALLOWED_HOSTS = {"www.python.org", "files.pythonhosted.org", "github.com"}
FORBIDDEN_REPOSITORIES = (
    "bkmashiro/agent-python-runtime",
    "bkmashiro/webassembly-language-runtimes",
)
MUTABLE_URL_MARKERS = (
    "/latest",
    "/refs/heads/",
    "/archive/main.",
    "/archive/master.",
    "/tarball/main",
    "/tarball/master",
)
SHA256_RE = re.compile(r"[0-9a-f]{64}")
VERSION_RE = re.compile(r"[0-9]+(?:\.[0-9]+){1,3}")


def _reject_unknown(actual: set[str], expected: set[str], where: str) -> None:
    unknown = sorted(actual - expected)
    missing = sorted(expected - actual)
    if unknown:
        raise ValueError(f"{where}: unknown fields: {', '.join(unknown)}")
    if missing:
        raise ValueError(f"{where}: missing fields: {', '.join(missing)}")


def validate_lock(document: object) -> None:
    if not isinstance(document, dict):
        raise ValueError("source lock must be an object")
    _reject_unknown(set(document), TOP_FIELDS, "source lock")
    if document["schema"] != "shimmy-python-runtime-sources/v1":
        raise ValueError("unexpected source lock schema")

    sources = document["sources"]
    if not isinstance(sources, list) or not sources:
        raise ValueError("sources must be a non-empty array")

    names: set[str] = set()
    for index, source in enumerate(sources):
        where = f"sources[{index}]"
        if not isinstance(source, dict):
            raise ValueError(f"{where}: entry must be an object")
        _reject_unknown(set(source), ENTRY_FIELDS, where)

        name = source["name"]
        if not isinstance(name, str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]*", name):
            raise ValueError(f"{where}: invalid source name")
        if name in names:
            raise ValueError(f"{where}: duplicate source name {name}")
        names.add(name)

        version = source["version"]
        if not isinstance(version, str) or VERSION_RE.fullmatch(version) is None:
            raise ValueError(f"{where}: version must be a stable numeric release")

        url = source["url"]
        if not isinstance(url, str):
            raise ValueError(f"{where}: url must be a string")
        lowered = url.lower()
        if any(repo in lowered for repo in FORBIDDEN_REPOSITORIES):
            raise ValueError(f"{where}: forbidden repository")
        if any(marker in lowered for marker in MUTABLE_URL_MARKERS):
            raise ValueError(f"{where}: mutable source URL")
        parsed = urlparse(url)
        if parsed.scheme != "https" or parsed.hostname not in ALLOWED_HOSTS:
            raise ValueError(f"{where}: source URL is not an allowed official host")

        digest = source["sha256"]
        if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
            raise ValueError(f"{where}: invalid SHA-256")
        if not isinstance(source["size"], int) or source["size"] <= 0:
            raise ValueError(f"{where}: invalid byte size")
        if not isinstance(source["archive_root"], str) or not source["archive_root"]:
            raise ValueError(f"{where}: invalid archive root")
        if not isinstance(source["license"], str) or not source["license"]:
            raise ValueError(f"{where}: missing license identity")
        if source["kind"] not in {"source", "toolchain", "library", "tool"}:
            raise ValueError(f"{where}: unsupported source kind")


def verify_path(path: pathlib.Path) -> str:
    raw = path.read_bytes()
    try:
        document = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ValueError(f"invalid JSON: {exc}") from exc
    validate_lock(document)
    return hashlib.sha256(raw).hexdigest()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("path", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        digest = verify_path(args.path)
    except (OSError, ValueError) as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        return 1
    print(f"PASS: {len(json.loads(args.path.read_text())['sources'])} sources; lock_sha256={digest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
