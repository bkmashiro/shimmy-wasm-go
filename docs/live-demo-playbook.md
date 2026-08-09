# Shimmy-WASM live demo playbook

This is the presenter-facing script for a short Shimmy-WASM demo. It assumes a
fresh checkout on macOS or Linux and uses only commands that are safe to run live.

## Goal

Show three things, in order:

1. **A real evaluator runs behind Shimmy's HTTP API.**
2. **The evaluator is warm, but guest mutable state is reset between requests.**
3. **Python has two explicit routes:** `reactor-python` for lightweight/fast cases
   and Pyodide for heavy scientific packages such as SciPy.

The reactor interface remains the primary Python target, but its old artifact is
frozen. The live commands below demonstrate generic WASM and Pyodide only until a
clean replacement passes handoff.

The shortest successful demo is just:

```bash
scripts/demo-wasm.sh
scripts/demo-python-examples.sh pyodide-only
scripts/demo-lambda-feedback-fixtures.sh all
```

## Before the meeting

Run these once from the repository root:

```bash
go test ./...
scripts/demo-wasm.sh
scripts/demo-python-examples.sh pyodide-only
scripts/demo-lambda-feedback-fixtures.sh all
```

Expected status on macOS:

- `go test ./...` passes.
- `scripts/demo-wasm.sh` passes.
- `scripts/demo-python-examples.sh pyodide-only` passes the SciPy/Pyodide HTTP path.
- `scripts/demo-lambda-feedback-fixtures.sh all` passes Pyodide fixtures; the
  local `compare-boolean` fixture may skip if host Python does not have SymPy.

Optional cleanup before presenting:

```bash
rm -rf .demo-logs .demo-wasm-server.log
```

## 5-minute demo script

### 1. Start with the one-line problem statement

Suggested wording:

> Lambda Feedback evaluators are untrusted grading functions. We want to keep
> them warm for latency, but we must not let state from one student request leak
> into the next. Shimmy-WASM runs the evaluator as a WASI module and restores its
> memory snapshot after each request.

### 2. Run the state-reset WASM demo

Command:

```bash
scripts/demo-wasm.sh
```

What the script does:

1. Builds `bin/shimmy-demo`.
2. Compiles `examples/demo-stateful/` to `examples/demo-stateful/eval.wasm`.
3. Starts `shimmy serve` on a random local port.
4. Sends two HTTP `eval` requests to the same warm evaluator.
5. Asserts that the guest global counter is reset for request #2.

Call out this part of the output:

```json
{
  "guest_invocation_count": 1,
  "snapshot_isolation_ok": true
}
```

The important point is that **both requests** report:

```text
guest_invocation_count = 1
snapshot_isolation_ok = true
```

Suggested explanation:

> The evaluator increments a global counter every time `evaluate` runs. If the
> same warm WASM instance leaked state, request #2 would show counter `2`. It
> still shows `1`, so the host restored the guest memory snapshot after request
> #1.

### 3. Show the evaluator code, if asked

Open:

```text
examples/demo-stateful/main.go
```

The useful talking points:

- It is a normal Go function compiled to `GOOS=wasip1 GOARCH=wasm`.
- It deliberately mutates module-global state.
- The host exposes the usual Shimmy HTTP `eval` contract, not a custom demo API.

Manual equivalent, if someone wants to see the moving parts:

```bash
go build -trimpath -buildvcs=false -o bin/shimmy-demo .
(cd examples/demo-stateful && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o eval.wasm .)

FUNCTION_INTERFACE=wasm \
FUNCTION_COMMAND="$PWD/examples/demo-stateful/eval.wasm" \
FUNCTION_MAX_PROCS=1 \
FUNCTION_WORKER_SEND_TIMEOUT=5s \
bin/shimmy-demo serve --host 127.0.0.1 --port 18080
```

Then from another terminal:

```bash
curl -sS -X POST http://127.0.0.1:18080/ \
  -H 'Content-Type: application/json' \
  -H 'Command: eval' \
  --data '{"response":"42","answer":"42","params":{}}' \
  | python3 -m json.tool
```

For live demos, prefer `scripts/demo-wasm.sh`; the manual flow is mainly for
explaining the architecture.

## 10-minute extended demo

### 4. Run the active Python compatibility route

Command:

```bash
scripts/demo-python-examples.sh pyodide-only
```

What to say before running it:

> Python is not one route. Lightweight Python and NumPy-compatible cases target
> the reactor interface after its clean artifact replacement; heavy scientific
> packages use the explicit Pyodide compatibility path today. We do not guess
> from imports at runtime.

SciPy should run through Pyodide and return `is_correct: true`.

Call out the SciPy/Pyodide result:

```json
{
  "is_correct": true,
  "sample_mean": 5,
  "p_value": 1
}
```

Suggested explanation:

> This is the compatibility story: if a real evaluator needs SciPy or other heavy
> packages, we route it to Pyodide. Plain Python and a CPython-WASI-ready package
> subset target the faster reactor path once its replacement artifact is accepted.

### 5. Run realistic Lambda Feedback fixtures

Command:

```bash
scripts/demo-lambda-feedback-fixtures.sh all
```

Expected shape:

```text
PASS summary: local-boilerplate:PASS | local-compare-boolean:SKIP | pyodide-boilerplate:PASS | pyodide-compare-boolean:PASS
```

A `SKIP` for `local-compare-boolean` is acceptable when host Python lacks SymPy.
The Pyodide cases should still pass.

Suggested explanation:

> These are package-style Lambda Feedback fixtures rather than toy single-file
> evaluators. Shimmy can adapt that package layout into the same runtime contract.

## Target Lambda Feedback reactor configuration

Build the evaluator-owned adapter before Shimmy startup:

```bash
python3 tools/lf-bundle-python/lf_bundle_python.py \
  --root /var/task \
  --adapter-root examples/lambda-feedback-adapter \
  --eval-entrypoint evaluation_function.evaluation:evaluation_function \
  --preview-entrypoint evaluation_function.preview:preview_function \
  --include-root /opt/lf-puredeps \
  --out /tmp/evaluator.bundle.py
```

Then pass only prepared artifact paths to the sandbox:

```bash
FUNCTION_INTERFACE=wasm
FUNCTION_WASM_PROFILE=python-reactor
FUNCTION_WASM_MODULE=/opt/python-reactor/python-reactor.wasm
FUNCTION_WASM_MANIFEST=/opt/python-reactor/manifest.json
FUNCTION_WASM_PYTHON_SCRIPT=/tmp/evaluator.bundle.py
```

Shimmy has no LF defaults, package discovery, or `FUNCTION_LF_*` startup mode.
The optional producer owns its explicit method mapping; the sandbox calls only
`dispatch(method, payload)`.

## If something goes wrong live

### `scripts/demo-wasm.sh` fails to build

Check Go first:

```bash
go version
go env GOOS GOARCH
```

The demo needs Go with `wasip1/wasm` support. Go 1.24+ is expected for this repo.

### Server does not become ready

The script prints a server log path such as:

```text
.demo-wasm-server.log
```

Open that log first. Most failures are either a build failure or an invalid WASM
artifact path.

### Python demo skips NumPy or SymPy locally

That is not a demo failure. Say:

> This laptop does not have that host Python package installed. The Pyodide path
> still demonstrates the heavy dependency route; Linux/Docker is the correct
> environment for a real `reactor-python` run.

### Need a clean rerun

```bash
rm -rf .demo-logs .demo-wasm-server.log bin/shimmy-demo examples/demo-stateful/eval.wasm
scripts/demo-wasm.sh
```

## What not to claim

- Do not claim that all Python packages run in `reactor-python`. SciPy-heavy
  evaluators should use Pyodide today.
- Do not claim automatic import/requirements routing. Runtime selection is
  explicit via `FUNCTION_INTERFACE`.
- Python Reactor runs through the same wazero adapter on macOS and Linux; do not
  substitute Pyodide or a native Python process and label it Python Reactor.
- The checked-in bundle is acceptance evidence, not proof of a deployed image.
- Do not claim zpoline/soft-dirty as the Lambda path. Current probe results favor
  `userfaultfd` write-protect support over soft-dirty/zpoline assumptions.

## One-slide summary

Use this as the closing slide or spoken summary:

```text
Shimmy-WASM keeps evaluators warm without leaking guest state.

- WASI/WASM evaluator behind the normal Shimmy HTTP API
- Snapshot/restore resets guest memory after every request
- Explicit runtime routes:
  - wasm: native WASI modules
  - reactor-python: primary Python/NumPy-compatible target after clean artifact handoff
  - pyodide: compatibility path for SciPy/heavy Python packages
- Lambda Feedback package mode can be configured with one root or one JSON file
```
