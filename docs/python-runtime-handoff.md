# Python Reactor runtime integration

Shimmy consumes Python Reactor Runtime v1 only through the explicit
`FUNCTION_INTERFACE=wasm` and `FUNCTION_WASM_PROFILE=python-reactor` selection.
Evaluator-specific packaging happens before Shimmy startup.

This is an integration result, not a deployment or release claim.

## Pinned bundle

The repository carries the complete verified `numpy-core` producer bundle under
`build/python-reactor/artifacts/`:

```text
artifact:        agent-python-runtime-numpy-core.wasm
size:            63,626,531 bytes
sha256:          90c27951b2d8c2c7a8b42705b365cb4231c6dad207aad5260d55d2f9a85f1034
producer commit: 76b49158cc6c4824491561531bfe7e34872cb820
ABI:             v1
profile:         numpy-core
CPython:         3.14.0
NumPy:           2.5.1
target:          wasm32-wasip1 reactor
```

`SHA256SUMS` binds the Wasm, manifest, SBOM, notices, and extension selection.
Shimmy verifies the artifact filename, size, SHA-256, target, and execution model,
then compiles it and compares the manifest with the actual exports, imports, and
function signatures before instantiation.

The producer guest implementation last changed at signed commit
`9a571176bb58c2d6a41312d01ad789abdd6b82e6`. The neutral v1 contract copied into
Shimmy is pinned to that guest source. Later producer work on Host transactions
and evaluation infrastructure is intentionally not copied into Shimmy.

## Runtime architecture

The Host adapter is implemented in:

```text
internal/execution/wasm/agent_python.go
internal/execution/wasm/agent_python_protocol.go
```

Lifecycle:

1. verify and compile the pinned artifact once;
2. instantiate a module and call `_initialize`, `runtime_init`, and optional
   `runtime_prepare` before it enters service;
3. adapt Shimmy's existing `method + params` request to
   `{run_id, code, inputs}`;
4. call `execute` and copy the length-prefixed ABI v1 response into Host memory;
5. enforce the configured Host-owned lifecycle:
   - `snapshot` (default): restore the post-prepare linear-memory baseline and
     return the healthy slot to the pool;
   - `single-use`: consume a never-served prepared candidate exactly once,
     close it, and refill the bounded ready pool in the background;
   - `fresh`: initialize and close one module synchronously per request;
6. discard instead of reusing any module that times out, traps, grows beyond its
   baseline, or fails restore.

`FUNCTION_WASM_SNAPSHOT_MODE` selects `memcpy`, `soft-dirty`, `mprotect`,
`uffd`, or `cow` for the `snapshot` lifecycle. Linux COW owns one sealed image
per Python Reactor slot so independently randomized CPython baselines are never
silently collapsed into one shared hash seed. Non-Linux COW requests explicitly
fall back to full copy. Snapshot strategies restore WASM linear memory only;
Shimmy keeps `agent_runtime_v1.host_call` denied, so this evaluator profile has
no request-owned Host capability state to claim as restored.

## Evaluator-owned dispatch

The prepared script exports one generic entrypoint:

```python
def dispatch(method, payload):
    # The evaluator owns any method registry or business mapping.
    ...
```

The script is passed through `runtime_prepare`. Shimmy calls
`dispatch(method, payload)` with the exact method and structure-preserving
payload. It does not know `eval`, `preview`, `chat`, callable names, or framework
registries. Missing or unknown methods do not fall back. Python exceptions are
returned as typed execution errors preserving code, message, error type, and
traceback instead of being disguised as successful result objects.

## Sandbox boundary

The module receives:

- WASI Preview 1 with no inherited filesystem, environment, arguments, stdin,
  or stdout;
- cryptographic Host entropy for `random_get`;
- exactly one custom import: `agent_runtime_v1.host_call`.

Shimmy currently returns `-1` from `host_call`. Guest capabilities therefore
fail closed with a structured Python exception. The producer's transaction,
credential, and network broker is not copied into Shimmy.

Request, response, trusted-script, and diagnostic sizes are bounded. Context
cancellation and timeout close the active module; snapshot mode replaces the
slot instead of returning a possibly poisoned instance, while single-use and
fresh modes already close every served instance.

## Real acceptance gates

The `Python Runtime Routes` workflow pulls only the pinned LFS object and runs:

- neutral evaluator compatibility;
- preview compatibility;
- structured Python exceptions;
- repeated post-prepare `memcpy` restore checks against the real `numpy-core`
  artifact;
- Linux per-slot COW selection and repeated state-reset checks;
- never-served single-use candidate checkout, refill, and repeated-state checks;
- explicit Host capability denial;
- timeout followed by a successful replacement request;
- NumPy core operations;
- real Lambda Feedback boilerplate bundle eval and preview;
- IEEE binary128 preservation through the final Shimmy path.

The binary128 canary requires 16-byte storage, at least 112 explicit mantissa
bits, precision wider than binary64, preservation of
`1.0000000000000000000000000000000002`, and narrowing to binary64 `1.0` only
at explicit conversion.

## Reproducibility boundary

Producer bitwise reproducibility remains a per-candidate result:

- run [`29962630062`](https://github.com/bkmashiro/agent-python-runtime/actions/runs/29962630062)
  reported `exact_match: true` for commit
  `770b37d17002f3107ebf920630d3fed0273281e4`;
- run [`29982102694`](https://github.com/bkmashiro/agent-python-runtime/actions/runs/29982102694)
  later reported `exact_match: false` because Wasm data section 11 differed.

Shimmy does not generalize the earlier successful result to every producer
commit. `scripts/python-runtime-handoff/compare_bundles.py` remains the
independent two-bundle acceptance tool for a future artifact update.
