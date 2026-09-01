"use strict";

function pythonString(value) {
  return JSON.stringify(value);
}

function buildPackageBootstrap({ evaluatorRoot, adapterRoot, adapterPath }) {
  const root = pythonString(evaluatorRoot);
  const lfAdapterRoot = pythonString(adapterRoot);
  const lfAdapterPath = pythonString(adapterPath);

  return `
import importlib
import importlib.util
import os
import sys

# Make evaluator package modules importable and resolve its relative assets from
# the same root used by native evaluator images.
if ${root} not in sys.path:
    sys.path.insert(0, ${root})
os.chdir(${root})

# A prepared evaluator bundle may carry NLTK corpora alongside its source.
_nltk_data = os.path.join(${root}, "nltk_data")
if os.path.isdir(_nltk_data):
    os.environ["NLTK_DATA"] = _nltk_data

# Make the adapter's sibling lf_toolkit shim importable.
if ${lfAdapterRoot} not in sys.path:
    sys.path.insert(0, ${lfAdapterRoot})

# Keep common temp locations importable.
if "/tmp" not in sys.path:
    sys.path.insert(0, "/tmp")

# Make adapter available by loading source in the Pyodide FS.
spec = importlib.util.spec_from_file_location("lf_compat_adapter", ${lfAdapterPath})
if spec is None or spec.loader is None:
    raise RuntimeError("Failed to build loader for lf_compat_adapter module")

lf_adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lf_adapter)

_eval_entrypoint = __eval_entrypoint__
_preview_entrypoint = __preview_entrypoint__

if not _eval_entrypoint:
    raise RuntimeError("FUNCTION_PYODIDE_EVAL_ENTRYPOINT is required in package mode")

_eval_fn = lf_adapter.load_entrypoint(_eval_entrypoint)
_preview_fn = lf_adapter.load_entrypoint(_preview_entrypoint) if _preview_entrypoint else None


def __lf_invoke(method, response, answer, params):
    if method == "preview" and _preview_fn is not None:
        fn = _preview_fn
    else:
        fn = _eval_fn

    if fn is None:
        raise RuntimeError("No evaluation function available")

    payload = {"response": response, "answer": answer, "params": params}
    normalized_method = "preview" if method == "preview" else "eval"
    return lf_adapter.normalize_result(
        lf_adapter.call_function(
            fn,
            normalized_method,
            payload["response"],
            payload["answer"],
            payload["params"],
        )
    )
`;
}

module.exports = { buildPackageBootstrap };
