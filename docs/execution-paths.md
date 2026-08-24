# WebAssembly execution paths

Shimmy keeps the existing `rpc` and `file` process interfaces and adds two
explicit, opt-in WebAssembly paths. Selection is configuration-driven; Shimmy
does not inspect source files or silently retry a request under another backend.

## Generic WebAssembly

```bash
FUNCTION_INTERFACE=wasm
FUNCTION_WASM_PROFILE=generic
FUNCTION_WASM_MODULE=/opt/evaluator/evaluator.wasm
```

The module runs in-process under wazero and exports `memory`, `alloc`, and
`dispatch`. Shimmy copies each request into guest linear memory, copies the
response out, and restores the prepared memory before reusing the instance.

Memory reset uses one portable implementation: a full copy of linear memory.
There is no snapshot-strategy selector in this path. If a request grows linear
memory, the instance is discarded because WebAssembly memory cannot shrink back
to the captured size.

The generic path has no host filesystem access unless paths are explicitly
allowed with `FUNCTION_WASM_ALLOWED_PATHS`. Environment variables are similarly
allowlisted with `FUNCTION_WASM_ALLOWED_ENV`.

## Python Reactor

```bash
FUNCTION_INTERFACE=wasm
FUNCTION_WASM_PROFILE=python-reactor
FUNCTION_WASM_MODULE=/opt/runtime/python-reactor.wasm
FUNCTION_WASM_MANIFEST=/opt/runtime/manifest.json
FUNCTION_WASM_PYTHON_SCRIPT=/opt/evaluator/evaluator.py
FUNCTION_WASM_PYTHON_LIFECYCLE=snapshot
```

The prepared trusted script defines `evaluation_function(response, answer, params)`
and may define `preview_function(response, params)`. Artifacts are built
from the self-contained producer under `build/python-reactor/producer/` and use
the `shimmy-python-runtime/v1` ABI. Shimmy verifies
the following before serving requests:

- artifact SHA-256 against the manifest;
- Reactor ABI name and version;
- required imports, exports, and function signatures; and
- manifest-declared `python_modules` against the artifact capability section.

This proves that the selected artifact and manifest are internally consistent.
Artifact authenticity, trusted Producer commit policy, signatures, and release
provenance remain deployment-system responsibilities.

### Lifecycle choices

| Value | Behavior |
|---|---|
| `snapshot` | Prepare once per slot and restore the full linear-memory copy after each successful request. Failed or timed-out slots are discarded and replenished asynchronously. |
| `single-use` | Prepare candidates ahead of time, serve each candidate once, then replace it. |
| `fresh` | Instantiate and prepare a new module for every request. |

`snapshot` is the default and the only lifecycle that restores memory. Its reset
implementation is always full-memory copy; there is no snapshot-strategy
configuration. `single-use` and `fresh` are lifecycle alternatives, not hidden
fallbacks. Shimmy never changes lifecycle after a request fails.

Preparation and background refill use a separate two-minute deadline so slow
artifact imports do not widen request deadlines. Override it with
`FUNCTION_WASM_PYTHON_PREPARE_TIMEOUT`; request execution remains controlled by
`FUNCTION_WORKER_SEND_TIMEOUT`. The linear-memory ceiling remains configurable
with `FUNCTION_WASM_MAX_MEMORY_PAGES`.

Python Reactor does not expose host paths. Leave
`FUNCTION_WASM_ALLOWED_PATHS` unset. Runtime modules are selected by the
manifest-validated artifact profile, for example `base`, `numpy-core`, or
`sympy`.

### Linux HTTP verification

Run the HTTP startup and request-flow check against a real Producer artifact and
its exact manifest:

```bash
SHIMMY_PYTHON_REACTOR_WASM=/opt/runtime/python-reactor.wasm \
SHIMMY_PYTHON_REACTOR_MANIFEST=/opt/runtime/manifest.json \
  tests/e2e/python-reactor/run.sh
```

The check starts Shimmy, sends two `eval` requests and one `preview` request,
and verifies prepared-state restoration between requests.

## Safe Python evaluator example

[`examples/safe-eval-python`](../examples/safe-eval-python/README.md) is a
backend-level Python Reactor example for student Python in `demo`, `io_test`,
`unit_test`, and `preview` modes. It uses:

- wazero's WebAssembly capability boundary;
- request deadlines;
- artifact, ABI, and manifest-capability validation;
- full-copy memory reset and failed-slot replacement; and
- evaluator limits for code, input, tests, and output.

It does not depend on nsjail, privileged Lambda configuration, Node, Docker, or
runtime package installation. AST checks provide early feedback and defense in
depth; they are not a containment boundary.

```bash
SHIMMY_PYTHON_REACTOR_WASM=/path/to/base.wasm \
SHIMMY_PYTHON_REACTOR_MANIFEST=/path/to/base.manifest.json \
  scripts/e2e-safe-eval-python.sh
```

For the guided base, NumPy, and SymPy examples, follow the
[quick start](../examples/safe-eval-python/README.md#start-here-first-successful-evaluation).

## Security boundary

WebAssembly isolation, request deadlines, state reset, and evaluator-level
limits do not form a complete operating-system sandbox. Deployment policy still
owns process memory, aggregate concurrency, authentication, request-size limits,
logging, artifact provenance, and network exposure.

AWS Lambda cannot grant the namespaces or capabilities required to use nsjail
as a security boundary. On supported Linux hosts or containers, an external OS
sandbox may be added as a separate deployment layer; Shimmy does not claim that
boundary for Lambda.
