# WASM backend model

Shimmy separates:

1. **execution interface** — process, protocol, and ownership boundary;
2. **WASM profile** — ABI and lifecycle expected from a supplied module;
3. **build recipe** — how source code becomes that module or evaluator bundle.

Do not create one `FUNCTION_INTERFACE` value per source language, and do not
infer a runtime from imports, requirements, or file extensions.

## Support tiers

| Tier | Paths | Positioning |
|---|---|---|
| Primary | Generic WASM; Agent Python | In-process wazero execution. |
| Migration | `file`; `rpc`; Pyodide | Existing workers and heavy Python compatibility. |
| Terminal fallback | DynamoRIO; QEMU | Explicit wrappers around `file`/`rpc`, never automatic retries. |
| Generic memory options | COW; UFFD; soft-dirty; mprotect | Explicit generic-WASM experiments; full copy remains default. |

## Execution interfaces

| Interface | Meaning |
|---|---|
| `wasm` | Primary in-process wazero boundary. `FUNCTION_WASM_PROFILE` selects the guest contract. |
| `rpc` | Persistent subprocess with JSON-RPC transport. |
| `file` | Subprocess-per-request file protocol. |
| `pyodide` | Node/Pyodide compatibility lane for heavy packages. |
| `reactor-python` | Legacy configuration alias that routes to Agent Python. |
| `python-wasm` | Independent older resident Python/WASM comparison path. |

## WASM profiles

### Generic

```bash
FUNCTION_INTERFACE=wasm
FUNCTION_WASM_PROFILE=generic
FUNCTION_WASM_MODULE=/path/to/evaluator.wasm
```

The guest directly exports Shimmy's `alloc + evaluate` ABI. The source language
is irrelevant at runtime. Generic modules can opt into the existing snapshot
strategies.

### Agent Python

```bash
FUNCTION_INTERFACE=wasm
FUNCTION_WASM_PROFILE=agent-python
FUNCTION_WASM_MODULE=/path/to/agent-python-runtime-numpy-core.wasm
FUNCTION_WASM_MANIFEST=/path/to/manifest.json
FUNCTION_WASM_PYTHON_SCRIPT=/path/to/eval.py
```

The profile consumes Agent Python Runtime ABI v1:

```text
_initialize
runtime_init
runtime_prepare
alloc / dealloc
execute
agent_runtime_v1.host_call
```

The artifact and manifest are verified before compilation. Shimmy compiles once,
then selects one explicit Host-owned lifecycle: post-prepare linear-memory
`snapshot` restore (the default), never-served prepared `single-use` slots, or
synchronous `fresh` instances. Snapshot mode can select full-copy, COW or an
eligible dirty-page strategy; timeout, trap, memory-size drift or restore failure
closes the unsafe slot instead of returning it to the prepared pool. See the
[source-level WASM/memory guide](https://bkmashiro.github.io/shimmy-docs/architecture/wasm-wazero-memory.html) for the
actual object ownership, snapshot timing and reset system calls.

`python-reactor`, `reactor-python`, and `FUNCTION_INTERFACE=reactor-python` are
configuration aliases for Agent Python. They do not activate the deleted legacy
loader.

## Memory-strategy boundary

Generic snapshot modes cover WASM linear memory only. They do not reset globals,
tables, Host/WASI state, descriptors, clocks, entropy, Go buffers, or external
effects. Generic Linux COW uses a dispatcher-scoped sealed image; Agent Python
uses one sealed image per independently randomized prepared slot. Both use
fixed-size private mappings and neither is whole-instance cloning.

Agent Python exposes three explicit Host-owned lifecycle policies: post-prepare
linear-memory `snapshot` restore (default), never-served `single-use` prepared
candidates, and synchronous `fresh` instances. Timeout, trap, memory-size drift,
or restore failure always discards the slot. These policies do not widen a
linear-memory claim into whole-instance reset.

## Build recipes

| Source | Recipe | Runtime shape |
|---|---|---|
| Go | `GOOS=wasip1 GOARCH=wasm go build` plus Shimmy ABI | Generic WASM. |
| Rust | `cargo build --target wasm32-wasip1` plus ABI wrapper | Generic WASM. |
| C/C++ | WASI SDK targeting `wasm32-wasip1` | Generic WASM. |
| Plain Python / NumPy core | Pinned Agent Python artifact plus evaluator script or startup bundle | Agent Python profile. |
| SciPy/heavy Python | Pyodide package ecosystem | Pyodide compatibility lane. |
| JavaScript | Javy/QuickJS-to-WASI plus an explicit ABI adapter | Future generic integration; current demo remains RPC-style. |

The current remaining product gaps are release/deployment qualification for the
new Python artifact, in-process JavaScript ABI integration, and an explicit
retirement decision for the independent resident `python-wasm` path.
