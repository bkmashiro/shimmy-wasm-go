# Shimmy Python Runtime Producer

This directory is the source of truth for Shimmy's CPython/WASI guest artifacts.
The producer is intentionally self-contained: Shimmy-authored guest and build code
is combined only with digest-locked official upstream sources and tools.

## Contract

`contract/shimmy-python-runtime-v1.json` defines the complete Host/Guest seam.
The module is a `wasm32-wasip1` reactor, imports only WASI Preview 1, and exposes
an explicit Shimmy identity plus bounded `init`, `prepare`, allocation, and
request evaluation functions.

The Guest receives a prepared evaluator before the snapshot boundary. Requests
contain only a method and params object. Responses are bounded, length-prefixed
JSON objects copied into Host-owned memory before the Host restores or discards
the instance.

## Profiles

- `base`: CPython and the selected standard library only.
- `numpy-core`: `base` plus a source-built, statically registered NumPy subset.
- `sympy`: `base` plus pinned pure-Python SymPy and mpmath packages in the
  read-only artifact VFS. No native port or runtime package installation is
  involved.

Each artifact manifest declares its importable top-level Python modules.
SciPy and Pandas remain outside the Reactor profiles and use the Pyodide
compatibility path.

No profile grants environment variables, filesystem preopens, networking,
or custom Host calls.

## Provenance rules

- `sources.lock.json` is strict and offline-verifiable.
- Mutable URLs and unpinned package resolution are rejected.
- Prebuilt Python runtimes from other product repositories are not accepted.
- Generated artifacts are CI outputs, not tracked Git blobs or releases.
- Artifact, manifest, benchmark binary, schemas, and source receipt must bind one
  clean Shimmy commit before benchmark promotion.

## Cheap gate

```bash
python3 -m unittest discover \
  -s build/python-reactor/producer/tests -p 'test_*.py' -v
python3 build/python-reactor/producer/tools/verify_sources_lock.py \
  build/python-reactor/producer/sources.lock.json
```

Real Linux Guest builds and Host execution are manual-only gates. The
`shimmy-python` workflow lane builds all profiles and runs the base profile
through Shimmy's production container before uploading the exact bundle.
