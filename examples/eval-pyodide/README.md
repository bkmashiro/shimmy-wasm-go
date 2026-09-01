# eval-pyodide — Pyodide/Node.js fallback runner

A Node.js-based runner that executes Python eval functions using
[Pyodide](https://pyodide.org) (Python compiled to WebAssembly for Node.js).

## Why this exists

CPython-WASI cannot load scipy or pandas because those packages depend on
LAPACK/BLAS via f2c-compiled Fortran code that requires native shared objects.
Pyodide ships its own WebAssembly builds of numpy, scipy, and pandas, so all
three work inside Node.js without any native libraries.

`shimmy` exposes this as an explicit backend:

```
FUNCTION_INTERFACE=pyodide
FUNCTION_PYODIDE_RUNNER=/path/to/runner.js
FUNCTION_PYODIDE_SCRIPT=/path/to/eval.py
```

Under the hood the dispatcher reuses Shimmy's JSON-RPC subprocess path over
stdio, so the runner speaks the same JSON-RPC 2.0 protocol as other subprocess
workers.

## Supported runner modes

The runner supports two modes.

### 1) Legacy script mode

This is the historical mode and remains for compatibility.

- provide `<eval.py>` as argv[2], or set `FUNCTION_PYODIDE_SCRIPT`
- the file must define `dispatch(method, payload)` or
  `evaluation_function(response, answer, params)`
- legacy defaults to installing `scipy` on startup unless `FUNCTION_PYODIDE_PACKAGES`
is set

Example:

```bash
node runner.js eval.py
# or
FUNCTION_PYODIDE_SCRIPT=eval.py node runner.js
```

### 2) Lambda Feedback package mode

Use when evaluator is a package layout instead of a single script.

Required env:

- `FUNCTION_PYODIDE_ROOT` points to the evaluator package root
- `FUNCTION_PYODIDE_EVAL_ENTRYPOINT` (e.g. `evaluation_function.evaluation:evaluation_function`)

Optional env:

- `FUNCTION_PYODIDE_PREVIEW_ENTRYPOINT` (e.g. `evaluation_function.preview:preview_function`)
- `FUNCTION_PYODIDE_ADAPTER` path to `lf_compat_adapter.py`
- `FUNCTION_PYODIDE_PACKAGES` comma-separated Pyodide packages to preinstall

Example:

```bash
FUNCTION_INTERFACE=pyodide \
FUNCTION_PYODIDE_RUNNER=/path/to/examples/eval-pyodide/runner.js \
FUNCTION_PYODIDE_ROOT=/path/to/evaluator/root \
FUNCTION_PYODIDE_EVAL_ENTRYPOINT=evaluation_function.evaluation:evaluation_function \
FUNCTION_PYODIDE_PREVIEW_ENTRYPOINT=evaluation_function.preview:preview_function \
FUNCTION_PYODIDE_ADAPTER=/path/to/examples/lambda-feedback-adapter/lf_compat_adapter.py \
FUNCTION_PYODIDE_PACKAGES=scipy
```

Package mode does not take a script argument. It mirrors the evaluator root into the
Pyodide runtime, loads the adapter once, and dispatches requests via
`lf_compat_adapter.call_function + normalize_result`.

For `preview` RPCs, the preview entrypoint is used when provided; otherwise the
runner falls back to the eval entrypoint.

## State isolation

Legacy script mode runs `evaluation_function` in a fresh Python namespace via
`exec(source, {})` for each request, giving state isolation without snapshots.

Package mode uses a persisted evaluator import for performance; package modules should be
written to avoid cross-request mutable global state that must be isolated.

## Prerequisites

- Node.js ≥ 18
- the repository-pinned Pyodide 0.27.7 package

```bash
cd examples/eval-pyodide
npm ci
```

The version is pinned because 0.27.7 includes the WASM build of `gensim` used
by Lambda Feedback's `shortTextAnswer`; Pyodide 0.28.3 no longer publishes
that package in its package index.

Pyodide's npm package (~100 MB) ships the Python runtime and a package index.
By default, the legacy script mode preinstalls `scipy`; package mode only installs
packages listed in `FUNCTION_PYODIDE_PACKAGES`.

## Running

```bash
# Legacy direct:
node runner.js eval.py

# Package mode direct:
FUNCTION_PYODIDE_ROOT=/path/to/root \
FUNCTION_PYODIDE_EVAL_ENTRYPOINT=evaluation_function.evaluation:evaluation_function \
FUNCTION_PYODIDE_ADAPTER=/path/to/examples/lambda-feedback-adapter/lf_compat_adapter.py \
node examples/eval-pyodide/runner.js

# Via shimmy (legacy demo):
FUNCTION_INTERFACE=pyodide \
FUNCTION_PYODIDE_RUNNER=$(pwd)/runner.js \
FUNCTION_PYODIDE_SCRIPT=$(pwd)/eval.py \
  shimmy
```

## Fixture demos

```bash
# From repo root: local adapter + Pyodide package-mode smoke tests.
scripts/demo-lambda-feedback-fixtures.sh all

# Prepare and execute the real data-bearing shortTextAnswer evaluator.
scripts/prepare-short-text-pyodide-bundle.py \
  --source .demo-lambda-sources/shortTextAnswer/app \
  --out .demo-pyodide-bundles/short-text-answer
scripts/demo-lambda-feedback-fixtures.sh pyodide-short-text

# Run SciPy and a real matplotlib.pyplot PNG render through Shimmy HTTP E2E.
scripts/demo-python-examples.sh pyodide-only
```

## Wire protocol

The runner speaks JSON-RPC 2.0 framed with LSP-style `Content-Length` headers
(same as shimmy's built-in rpc subprocess adapter):

```
Content-Length: <N>\r\n
\r\n
{"jsonrpc":"2.0","id":1,"method":"evaluate","params":[{"response":"42","answer":"42"}]}
```

shimmy sends `method` as the configured function interface method name
(typically `"evaluate"` for eval paths or `"preview"` for preview paths). The runner
always loads the configured entrypoint, and falls back to eval behavior when preview
is unavailable.

## env vars

- `FUNCTION_PYODIDE_SCRIPT`: legacy script mode path
- `FUNCTION_PYODIDE_ROOT`: package mode evaluator root
- `FUNCTION_PYODIDE_EVAL_ENTRYPOINT`: package eval entrypoint
- `FUNCTION_PYODIDE_PREVIEW_ENTRYPOINT`: optional package preview entrypoint
- `FUNCTION_PYODIDE_ADAPTER`: path to lambda-feedback adapter shim
- `FUNCTION_PYODIDE_PACKAGES`: comma-separated list of Pyodide packages

## eval.py contract

The loaded Python script must define:

```python
def evaluation_function(response, answer, params=None) -> dict:
    ...
    return {
        "is_correct": bool,
        "feedback": str,
        # any additional fields are returned to the caller
    }
```

## Example eval.py

`eval.py` in this directory demonstrates:

- **numeric mode** (default): exact floating-point comparison using
  `math.isclose` with configurable absolute tolerance.
- **ttest mode**: one-sample t-test via `scipy.stats.ttest_1samp` to check
  whether a set of student samples is consistent with the reference mean.

```python
# numeric
{"response": "3.14159", "answer": "3.14159", "params": {"test": "numeric"}}

# t-test
{"response": null, "answer": "5.0", "params": {
    "test": "ttest",
    "samples": [4.9, 5.1, 5.0, 5.2, 4.8]
}}
```

## Offline / air-gapped deployments

To avoid CDN fetches, pre-install Pyodide packages and point the runner at a
local mirror:

```bash
# Download package wheels into a local dir
node -e "
const { loadPyodide } = require('pyodide');
loadPyodide().then(py => py.loadPackage(['scipy']));
"
```

Or set `PYODIDE_PACKAGE_URL` to a local HTTP server hosting the Pyodide
package index.
