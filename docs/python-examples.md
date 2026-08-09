# Python runtime routes

Shimmy's Python story is a capability matrix, not several equally recommended
interfaces. Runtime selection remains explicit.

| Route | Directory | Runtime | Use when | Boundary |
|---|---|---|---|---|
| Plain Python | `examples/eval-python/` | `python-reactor` | Standard-library and bundled pure-Python evaluators | Fresh single-use instance; no Host filesystem/network. |
| NumPy core | `examples/eval-numpy/` | `python-reactor` | NumPy core/linalg workloads covered by the pinned artifact | NumPy 2.5.1; binary128 canary covered; SciPy absent. |
| SciPy/heavy Python | `examples/eval-scipy/` | Pyodide | SciPy/Pandas/scikit-learn and broad Emscripten packages | Heavier subprocess compatibility lane. |
| Lambda Feedback package | fixtures + external producer | Python Reactor or Pyodide | Package-style evaluators built into a `dispatch` script before startup | Boilerplate is runtime-qualified; other rows follow `capability-matrix.json`. |

Python Reactor smoke:

```bash
scripts/smoke-python-reactor-handoff.sh direct
scripts/demo-python-examples.sh reactor-only
```

Pyodide smoke:

```bash
(cd examples/eval-pyodide && npm ci)
scripts/demo-python-examples.sh pyodide-only
scripts/demo-lambda-feedback-fixtures.sh pyodide-boilerplate
```

Notes:

- `python-reactor` is the only Python Reactor profile; select it under
  `FUNCTION_INTERFACE=wasm`.
- The bundle is manifest- and checksum-bound under
  `build/python-reactor/artifacts/`.
- The Host denies `agent_runtime_v1.host_call`; no capability is granted.
- Lambda Feedback pure-Python dependencies are embedded by the optional producer
  with repeatable `--include-root` arguments before Shimmy startup.
- SciPy remains a Pyodide route.
