# Lambda Feedback producer for Python Reactor

**Status:** integrated and covered by local/remote consumer gates. This page does
not claim that a deployment or image has been published.

The runtime and artifact evidence is documented in
[Python Reactor runtime integration](python-runtime-handoff.md).

## Deployment shape

First run the optional evaluator-specific producer. Shimmy itself does not
inspect Lambda Feedback package structure:

```bash
python3 tools/lf-bundle-python/lf_bundle_python.py \
  --root /var/task \
  --adapter-root examples/lambda-feedback-adapter \
  --eval-entrypoint evaluation_function.evaluation:evaluation_function \
  --preview-entrypoint evaluation_function.preview:preview_function \
  --out /tmp/evaluator.bundle.py
```

```bash
FUNCTION_INTERFACE=wasm
FUNCTION_WASM_PROFILE=python-reactor
FUNCTION_WASM_MODULE=/opt/python-reactor/python-reactor.wasm
FUNCTION_WASM_MANIFEST=/opt/python-reactor/manifest.json
FUNCTION_WASM_PYTHON_SCRIPT=/tmp/evaluator.bundle.py
```

The generated script owns `dispatch(method, payload)` and may map LF methods to
its package functions. Shimmy forwards method and payload without knowing that
mapping. Unknown methods raise the adapter's explicit structured error.

## Filesystem and dependency boundary

Python Reactor exposes no Host filesystem paths to guest code. Consequently:

- repeatable producer `--include-root` paths embed pure-Python dependencies
  before sandbox startup;
- no `ctypes`, NumPy random, FFT, filesystem, or import polyfills are injected;
- NumPy core and linear algebra come from the pinned runtime artifact;
- SciPy-heavy evaluators remain on the Pyodide route.

The guest imports only `agent_runtime_v1.host_call` beyond WASI. Shimmy currently
rejects every capability call, so package evaluators cannot obtain network or
credential access through the runtime.

## Artifact identity

```text
file:     agent-python-runtime-numpy-core.wasm
size:     63,626,531 bytes
sha256:   90c27951b2d8c2c7a8b42705b365cb4231c6dad207aad5260d55d2f9a85f1034
commit:   76b49158cc6c4824491561531bfe7e34872cb820
ABI:      v1
profile:  numpy-core
```

The complete bundle is checked in through Git LFS under
`build/python-reactor/artifacts/`. `SHA256SUMS`, `manifest.json`, SBOM, notices,
and extension selection are verified before E2E execution.

## Smoke commands

Fast artifact/protocol/routing gate:

```bash
scripts/smoke-python-reactor-handoff.sh artifact-only
```

Real runtime compatibility, NumPy binary128, capability denial, and timeout
recovery:

```bash
scripts/smoke-python-reactor-handoff.sh direct
```

Real HTTP examples for plain Python and NumPy:

```bash
scripts/demo-python-examples.sh reactor-only
```

The new runtime is cross-platform under wazero; these commands do not require a
Linux-only loader or a Docker fallback.

## Compatibility check

```bash
python3 tools/python-reactor-check/python_reactor_check.py --source /var/task
go run ./cmd/shimmy-artifact-check --profile python-reactor \
  --module /opt/python-reactor/python-reactor.wasm \
  --manifest /opt/python-reactor/manifest.json
```

Static source findings are advisory warnings. Artifact, manifest, ABI, or
explicit build-command failures are errors. Neither checker rewrites evaluator
business logic.
