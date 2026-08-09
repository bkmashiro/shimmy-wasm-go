# Multi-scenario Shimmy-WASM demos

There are now two demo entry points:

```bash
scripts/demo-wasm.sh       # shortest end-to-end story: state reset in one WASM evaluator
scripts/demo-scenarios.sh  # scenario matrix: multiple guest languages + sample inputs
```

For the backend model behind these demos, see
[`wasm-backend-model.md`](wasm-backend-model.md). In short: `wasm` is the
runtime interface; language-specific Go/Rust/C/C++/Python/JS compilation is a
build or deployment recipe, not a separate peer `FUNCTION_INTERFACE` for every
language.

## What `scripts/demo-scenarios.sh` runs

The scenario runner builds Shimmy once, then runs one local HTTP server per demo
module and sends a concrete grading request to it.

| Scenario | Guest language/runtime | Sample request | Expected result | Local status |
|---|---|---|---|---|
| `go-demo-stateful-snapshot` | Go → `wasm32-wasip1` | `response="42"`, `answer="42"` | correct; guest counter remains `1` | ✅ macOS/Linux |
| `go-eval-simple-equality` | Go → `wasm32-wasip1` | `response="hello"`, `answer="hello"` | correct | ✅ macOS/Linux |
| `rust-eval-simple-equality` | Rust → `wasm32-wasip1` | `response="rust"`, `answer="rust"` | correct | ✅ if Rust toolchain installed |
| `c-eval-simple-equality` | C → `wasm32-wasip1` | `response="c"`, `answer="c"` | correct | optional; needs WASI SDK |
| `cpp-eval-simple-equality` | C++ → `wasm32-wasip1` | `response="cpp"`, `answer="cpp"` | correct | optional; needs WASI SDK |

The optional cases are skipped with a clear message if their compiler is not
installed, so the script remains safe as a live-demo command.

## Python-focused demo matrix

For the Python story, use the dedicated runner instead of mixing it with the
Go/Rust/C/C++ WASI matrix:

```bash
scripts/demo-python-examples.sh
```

It covers the current Python runtime matrix:

| Route | Example | Runtime | Why |
|---|---|---|---|
| Plain Python | `examples/eval-python/` | `reactor-python` | fastest Python path with snapshot/restore isolation |
| NumPy | `examples/eval-numpy/` | `reactor-python` | array/vector grading within the CPython-WASI-compatible package subset |
| SciPy / heavy Python | `examples/eval-scipy/` | `pyodide` | broad package support for dependencies unavailable in CPython-WASI; slowest path |
| Legacy resident Python | `examples/eval-python/` | `python-wasm` | compatibility/comparison only; interpreter state can leak across requests |

## Real Lambda Feedback source material

To pull public evaluation functions currently in the Lambda Feedback GitHub org:

```bash
scripts/fetch-lambda-examples.sh
```

This clones a reference set into `.demo-lambda-sources/` and writes
`.demo-lambda-sources/SUMMARY.md`. The set currently includes:

- Python numeric/scientific: `IsSimilar`, `ArrayEqual`, `SymbolicEqual`, `compareBoolean`, `compareSets`, `shortTextAnswer`, `evaluatePython`
- Boilerplates: `evaluation-function-boilerplate-python`
- Non-Python language examples: `wolframIsSimilar`, `evaluation-function-boilerplate-lean`

These repositories are not vendored into Shimmy; they are fetched as live
reference material for choosing realistic inputs and explaining what real
evaluation functions look like.

## Python / NumPy demos

Python Reactor runs cross-platform under wazero and uses a fresh instance per
request. The pinned NumPy bundle is available directly from the repository:

```bash
FUNCTION_INTERFACE=wasm \
FUNCTION_WASM_PROFILE=python-reactor \
FUNCTION_WASM_MODULE=$PWD/build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm \
FUNCTION_WASM_MANIFEST=$PWD/build/python-reactor/artifacts/manifest.json \
FUNCTION_WASM_PYTHON_SCRIPT=$PWD/examples/eval-numpy/eval.py \
FUNCTION_WASM_MAX_MEMORY_PAGES=8192 \
FUNCTION_MAX_PROCS=1 \
FUNCTION_WORKER_SEND_TIMEOUT=120s \
bin/shimmy-demo serve --host 127.0.0.1 --port 18080
```

Sample request:

```bash
curl -sS -X POST http://127.0.0.1:18080/ \
  -H 'Content-Type: application/json' \
  -H 'Command: eval' \
  --data '{"response":"1,2,3.000001","answer":"1,2,3","params":{"rtol":0.00001}}' \
  | python3 -m json.tool
```

Good real Lambda Feedback candidates for Python/WASM demo scripts:

| Source repo | Why it is useful | WASM path |
|---|---|---|
| `lambda-feedback/IsSimilar` | numeric tolerance grading using NumPy scalar helpers | Python Reactor candidate; rerun fixture E2E |
| `lambda-feedback/ArrayEqual` | array/matrix comparison using `numpy.allclose` | Python Reactor candidate; rerun fixture E2E |
| `lambda-feedback/SymbolicEqual` | symbolic algebra with SymPy | Pyodide; SymPy is not in the Agent artifact |
| `lambda-feedback/compareBoolean` | boolean expression parsing + SymPy logic | Pyodide; no compatibility polyfill |
| `lambda-feedback/evaluatePython` | code runner / sandbox story | separate security demo; not a pure evaluator port |

## Non-WASM fallback language demos

Some real Lambda Feedback languages are better shown as integration constraints
rather than first-class Shimmy-WASM modules:

- Wolfram Language (`wolframIsSimilar`, `wolframEvaluationFunction`): real Lambda
  workloads exist, but they require a Wolfram runtime/container, not a generic
  WASI module today.
- Lean (`evaluation-function-boilerplate-lean`): real boilerplate exists; useful
  as a future-language story, but it needs a Lean build/runtime integration path.
- JavaScript: `examples/eval-js/` already demonstrates Javy/QuickJS via the
  existing RPC/subprocess lane; it needs `javy` and a WASM runner command.

For supervisor demos, lead with `scripts/demo-wasm.sh` or
`scripts/demo-scenarios.sh`, then use `scripts/fetch-lambda-examples.sh` to show
that the chosen Python/NumPy/SymPy cases correspond to real public Lambda
Feedback evaluation functions.
