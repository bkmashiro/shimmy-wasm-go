# lf-bundle-python

Optional evaluator-specific producer for Lambda Feedback Python packages.

This tool is **outside Shimmy's sandbox core and production request path**. It
bundles package modules into one prepared Python script that exports the Python
Reactor contract:

```python
def dispatch(method: str, payload: dict) -> dict: ...
```

The adapter currently maps explicitly configured LF `eval` and `preview`
entrypoints. Missing or unknown methods raise `LookupError`; there is no
`preview -> eval` or wildcard fallback.

## Build a bundle

```bash
python3 tools/lf-bundle-python/lf_bundle_python.py \
  --root examples/lambda-feedback-fixtures/boilerplate-python \
  --adapter-root examples/lambda-feedback-adapter \
  --eval-entrypoint evaluation_function.evaluation:evaluation_function \
  --preview-entrypoint evaluation_function.preview:preview_function \
  --out /tmp/evaluator.bundle.py
```

Then run the caller-produced bundle:

```bash
FUNCTION_INTERFACE=wasm \
FUNCTION_WASM_PROFILE=python-reactor \
FUNCTION_WASM_MODULE=build/python-reactor/artifacts/agent-python-runtime-numpy-core.wasm \
FUNCTION_WASM_MANIFEST=build/python-reactor/artifacts/manifest.json \
FUNCTION_WASM_PYTHON_SCRIPT=/tmp/evaluator.bundle.py \
./shimmy serve
```

Shimmy does not invoke this tool, inspect package structure, or read
`FUNCTION_LF_*` configuration.

## Dependencies

Embed repeatable pure-Python module roots at packaging time:

```bash
uv pip install --target /tmp/lf-puredeps \
  mpmath typing_extensions antlr4-python3-runtime==4.7.2
python3 tools/lf-bundle-python/lf_bundle_python.py \
  --root examples/lambda-feedback-fixtures/compare-boolean \
  --adapter-root examples/lambda-feedback-adapter \
  --include-root /tmp/lf-puredeps \
  --eval-entrypoint evaluation_function.evaluation:evaluation_function \
  --preview-entrypoint evaluation_function.preview:preview_function \
  --out /tmp/evaluator.bundle.py
```

The producer does not compile native extensions. Runtime support remains an
artifact-producer responsibility. Run the advisory source checker before
packaging:

```bash
python3 tools/python-reactor-check/python_reactor_check.py \
  --source examples/lambda-feedback-fixtures/compare-boolean
```

Warnings describe possible native/WASM semantic differences and do not block
packaging.

## Tests

```bash
python3 -m pytest -q \
  tools/lf-bundle-python/test_lf_bundle_python.py
scripts/smoke-python-reactor-handoff.sh direct
```
