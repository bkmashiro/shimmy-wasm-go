# Deployment recipes

`FUNCTION_INTERFACE` selects the execution boundary, not the evaluator language.
Runtime selection is explicit; Shimmy never scans imports, requirements, or file
extensions.

## Runtime matrix

| Scenario | Required configuration | Notes |
|---|---|---|
| Generic WASM | `FUNCTION_INTERFACE=wasm`; `FUNCTION_WASM_PROFILE=generic`; `FUNCTION_WASM_MODULE=/path/eval.wasm` | Guest exports Shimmy `alloc + dispatch` ABI. Generic snapshot modes remain available. |
| Python Reactor | `FUNCTION_INTERFACE=wasm`; `FUNCTION_WASM_PROFILE=python-reactor`; `FUNCTION_WASM_MODULE=/path/python-reactor.wasm`; `FUNCTION_WASM_MANIFEST=/path/manifest.json`; `FUNCTION_WASM_PYTHON_SCRIPT=/path/evaluator.bundle.py` | Prepared script owns `dispatch(method, payload)`. Post-prepare snapshot/memcpy by default; explicit single-use and fresh modes remain available. |
| Existing LF Python package | Run the optional LF bundler before Shimmy, then use the Python Reactor row | Evaluator-specific packaging is external to sandbox startup. |
| Pyodide compatibility | `FUNCTION_INTERFACE=pyodide`; runner plus script or package-mode variables | Compatibility lane for SciPy/Pandas and Emscripten packages. |
| RPC/file migration | `FUNCTION_INTERFACE=rpc` or `file`; `FUNCTION_COMMAND=...` | Existing subprocess protocols. |
| Full Linux via QEMU | Existing `rpc`/`file` configuration plus explicit `FUNCTION_QEMU_*` artifact and lifecycle values | Transparent terminal fallback; QEMU and DBI are mutually exclusive. |

Shared HTTP, stdio JSON-RPC, and Pyodide frames default to 4 MiB. Python
Reactor's guest ABI additionally bounds each request and response to 1 MiB.

## Python Reactor

Canonical repository paths:

```text
build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm
build/python-reactor/artifacts/manifest.json
```

Artifact identity:

```text
size:     63,626,531 bytes
sha256:   90c27951b2d8c2c7a8b42705b365cb4231c6dad207aad5260d55d2f9a85f1034
commit:   76b49158cc6c4824491561531bfe7e34872cb820
ABI:      v1
profile:  numpy-core
```

### Script

```bash
FUNCTION_INTERFACE=wasm \
FUNCTION_WASM_PROFILE=python-reactor \
FUNCTION_WASM_MODULE=build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm \
FUNCTION_WASM_MANIFEST=build/python-reactor/artifacts/manifest.json \
FUNCTION_WASM_PYTHON_SCRIPT=examples/eval-python/eval.py \
FUNCTION_WASM_PYTHON_LIFECYCLE=snapshot \
FUNCTION_WASM_SNAPSHOT_MODE=memcpy \
FUNCTION_WASM_MAX_MEMORY_PAGES=8192 \
FUNCTION_MAX_PROCS=1 \
./shimmy serve
```

`FUNCTION_WASM_PYTHON_PRELOAD=evaluator` is the default and executes the trusted
script through `runtime_prepare`. `off` remains accepted for compatibility and
executes the trusted script inside each fresh request namespace.

`FUNCTION_WASM_PYTHON_LIFECYCLE` accepts `snapshot` (default), `single-use`, or
`fresh`. Snapshot mode accepts the same strategy names as generic WASM. On
Linux, `FUNCTION_WASM_SNAPSHOT_MODE=cow` uses one sealed image per Python Reactor
slot; on other platforms it explicitly falls back to `memcpy`. Single-use uses
`FUNCTION_WASM_PYTHON_PREPARED_CAPACITY=1..4` and never returns a served module
to its ready pool.

### Optional Lambda Feedback producer

```bash
python3 tools/lf-bundle-python/lf_bundle_python.py \
  --root examples/lambda-feedback-fixtures/boilerplate-python \
  --adapter-root examples/lambda-feedback-adapter \
  --eval-entrypoint evaluation_function.evaluation:evaluation_function \
  --preview-entrypoint evaluation_function.preview:preview_function \
  --out /tmp/evaluator.bundle.py

FUNCTION_INTERFACE=wasm \
FUNCTION_WASM_PROFILE=python-reactor \
FUNCTION_WASM_MODULE=build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm \
FUNCTION_WASM_MANIFEST=build/python-reactor/artifacts/manifest.json \
FUNCTION_WASM_PYTHON_SCRIPT=/tmp/evaluator.bundle.py \
./shimmy serve
```

The bundler is an evaluator-specific artifact producer. Shimmy does not read
`FUNCTION_LF_*`, inspect package layout, or run the bundler at startup. Add
repeatable `--include-root` arguments when the producer must embed pure-Python
dependencies.

The only custom guest import is `agent_runtime_v1.host_call`. Shimmy currently
denies it, so no network, credential, or transaction capability is granted.

Verify locally:

```bash
scripts/smoke-python-reactor-handoff.sh artifact-only
scripts/smoke-python-reactor-handoff.sh direct
scripts/demo-python-examples.sh reactor-only
```

## Generic WASM snapshot modes

Generic `FUNCTION_WASM_PROFILE=generic` supports explicit snapshot strategies
through `FUNCTION_WASM_SNAPSHOT_MODE`: `memcpy`, `soft-dirty`, `mprotect`,
`uffd`, and `cow` where available. Full copy remains the default.

The Linux COW prototype uses a dispatcher-scoped sealed prepared-memory image
and fixed-size private mappings. It covers linear memory only, not globals,
tables, WASI/Host state, external effects, RNG, clocks, or descriptors. It must
not be used to claim whole-instance freshness. Python Reactor uses the same
strategy implementations but gives each COW slot its own image because separate
CPython initialization carries independently randomized state.

## QEMU full-Linux fallback

QEMU wraps an existing process worker; keep `FUNCTION_INTERFACE=file` or `rpc`.
A minimal file-worker configuration is:

```bash
FUNCTION_INTERFACE=file \
FUNCTION_COMMAND=/opt/evaluator/eval \
FUNCTION_QEMU_ENABLED=true \
FUNCTION_QEMU_RUNNER=/opt/shimmy/bin/shimmy-qemu-runner \
FUNCTION_QEMU_BINARY=/usr/bin/qemu-system-x86_64 \
FUNCTION_QEMU_ROOTFS=/opt/shimmy-qemu/evaluator.squashfs \
FUNCTION_QEMU_IMAGE_MANIFEST=/opt/shimmy-qemu/manifest.json \
FUNCTION_QEMU_ACCELERATOR=tcg \
FUNCTION_QEMU_NETWORK_PROFILE=none \
FUNCTION_QEMU_MEMORY_MB=512 \
FUNCTION_QEMU_VCPUS=1 \
./shimmy serve
```

The manifest and every image component are SHA-256 verified. `file` owns one
fresh VM per request. Non-Lambda `rpc` is persistent; Lambda defaults to lazy
single-use ownership. Shimmy never retries a failed native/DBI request under
QEMU. There is no silent KVM-to-TCG fallback.

## Artifact checks

```bash
go run ./cmd/shimmy-artifact-check --profile generic --module evaluator.wasm
go run ./cmd/shimmy-artifact-check --profile python-reactor \
  --module build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm \
  --manifest build/python-reactor/artifacts/manifest.json
python3 tools/python-reactor-check/python_reactor_check.py --source evaluator/
```

The Go checker reports objective artifact/ABI errors. The Python checker emits
advisory warnings for operations whose WASM behavior may differ from native
execution. Warnings do not block packaging or runtime selection.
