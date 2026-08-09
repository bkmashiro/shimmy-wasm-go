#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/python-reactor-artifact.env
source "${ROOT}/scripts/python-reactor-artifact.env"

usage() {
  cat >&2 <<'EOF'
usage: scripts/verify-python-reactor-artifact.sh [artifact.wasm [manifest.json]]

Verifies the pinned Python Reactor runtime bundle, manifest binding, and actual
Wasm v1 imports/exports. No artifact is downloaded implicitly.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

wasm="${1:-${SHIMMY_REACTOR_WASM}}"
manifest="${2:-$(dirname "${wasm}")/${SHIMMY_REACTOR_MANIFEST}}"

python3 - "${wasm}" "${manifest}" "${SHIMMY_REACTOR_SHA256}" "${SHIMMY_REACTOR_PRODUCER_COMMIT}" <<'PY'
import hashlib
import json
import re
import sys
from pathlib import Path

wasm_path = Path(sys.argv[1])
manifest_path = Path(sys.argv[2])
expected_sha = sys.argv[3]
expected_commit = sys.argv[4]

if not wasm_path.is_file():
    raise SystemExit(f"error: pinned artifact is missing: {wasm_path}")
if not manifest_path.is_file():
    raise SystemExit(f"error: pinned manifest is missing: {manifest_path}")
if wasm_path.is_symlink() or manifest_path.is_symlink():
    raise SystemExit("error: artifact and manifest must not be symlinks")

manifest = json.loads(manifest_path.read_text())
data = wasm_path.read_bytes()
digest = hashlib.sha256(data).hexdigest()
artifact = manifest.get("artifact", {})
build = manifest.get("build", {})

checks = {
    "schema_version": manifest.get("schema_version") == 2,
    "abi_version": manifest.get("abi_version") == "v1",
    "artifact_profile": manifest.get("artifact_profile") == "numpy-core",
    "target": manifest.get("target") == "wasm32-wasip1",
    "execution_model": build.get("execution_model") == "reactor",
    "compiler_target": build.get("compiler_target") == "wasm32-wasip1",
    "producer_commit": build.get("repository_commit") == expected_commit,
    "artifact_filename": artifact.get("filename") == wasm_path.name,
    "artifact_size": artifact.get("size") == len(data),
    "artifact_sha256": artifact.get("sha256") == digest == expected_sha,
}
failed = [name for name, ok in checks.items() if not ok]
if failed:
    raise SystemExit("error: manifest binding failed: " + ", ".join(failed))
if not re.fullmatch(r"[0-9a-f]{40}", build.get("repository_commit", "")):
    raise SystemExit("error: producer commit is not full lowercase hex")
if data[:8] != b"\0asm\x01\0\0\0":
    raise SystemExit("error: artifact is not a WebAssembly core module")


def read_u32(pos):
    value = 0
    shift = 0
    while True:
        if pos >= len(data) or shift > 35:
            raise SystemExit("error: malformed Wasm varuint")
        byte = data[pos]
        pos += 1
        value |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return value, pos
        shift += 7


def read_name(pos):
    length, pos = read_u32(pos)
    end = pos + length
    if end > len(data):
        raise SystemExit("error: malformed Wasm name")
    return data[pos:end].decode("utf-8"), end


def skip_limits(pos):
    flags, pos = read_u32(pos)
    _, pos = read_u32(pos)
    if flags & 1:
        _, pos = read_u32(pos)
    return pos


imports = []
exports = []
pos = 8
while pos < len(data):
    section_id = data[pos]
    pos += 1
    section_length, pos = read_u32(pos)
    end = pos + section_length
    if end > len(data):
        raise SystemExit("error: malformed Wasm section length")
    if section_id == 2:
        count, pos = read_u32(pos)
        for _ in range(count):
            module, pos = read_name(pos)
            name, pos = read_name(pos)
            imports.append((module, name))
            kind = data[pos]
            pos += 1
            if kind == 0:
                _, pos = read_u32(pos)
            elif kind == 1:
                pos += 1
                pos = skip_limits(pos)
            elif kind == 2:
                pos = skip_limits(pos)
            elif kind == 3:
                pos += 2
            elif kind == 4:
                _, pos = read_u32(pos)
                _, pos = read_u32(pos)
            else:
                raise SystemExit(f"error: unsupported Wasm import kind {kind}")
    elif section_id == 7:
        count, pos = read_u32(pos)
        for _ in range(count):
            name, pos = read_name(pos)
            exports.append(name)
            pos += 1
            _, pos = read_u32(pos)
    pos = end

manifest_imports = sorted((item["module"], item["name"]) for item in manifest["wasm"]["imports"])
manifest_exports = sorted(manifest["wasm"]["exports"])
if sorted(imports) != manifest_imports:
    raise SystemExit("error: actual Wasm imports differ from manifest")
if sorted(exports) != manifest_exports:
    raise SystemExit("error: actual Wasm exports differ from manifest")
custom = [(module, name) for module, name in imports if module != "wasi_snapshot_preview1"]
if custom != [("agent_runtime_v1", "host_call")]:
    raise SystemExit(f"error: unexpected custom imports: {custom}")
required = {"memory", "_initialize", "runtime_init", "runtime_prepare", "alloc", "dealloc", "execute"}
missing = sorted(required - set(exports))
if missing:
    raise SystemExit("error: missing required exports: " + ", ".join(missing))

checksums = manifest_path.with_name("SHA256SUMS")
if not checksums.is_file() or checksums.is_symlink():
    raise SystemExit(f"error: checksum inventory is missing: {checksums}")
for line in checksums.read_text().splitlines():
    checksum, filename = line.split(maxsplit=1)
    candidate = checksums.parent / filename
    if candidate.parent != checksums.parent or not candidate.is_file() or candidate.is_symlink():
        raise SystemExit(f"error: unsafe or missing bundle file: {filename}")
    actual = hashlib.sha256(candidate.read_bytes()).hexdigest()
    if actual != checksum:
        raise SystemExit(f"error: checksum mismatch: {filename}")

print(f"artifact: {wasm_path}")
print(f"profile:  {manifest['artifact_profile']}")
print(f"commit:   {build['repository_commit']}")
print(f"sha256:   {digest}")
print("exports:  " + ", ".join(sorted(required)))
print("PASS: Python Reactor runtime bundle verified")
PY
