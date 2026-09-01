# Lambda Feedback fixture snapshots

This directory contains a small, local subset of real Lambda Feedback evaluator repositories,
copied from `.demo-lambda-sources` so tests can run without network access.

## Capability matrix

The machine-readable matrix lives in [`capability-matrix.json`](./capability-matrix.json).
It records historical fixtures. Current Agent Python qualification is narrower
than the old reactor matrix:

| Fixture | Agent Python status | Requirements | Pyodide stance |
|---|---|---|---|
| `boilerplate-python` | package bundle supported | evaluator + adapter only | supported |
| `array-equal` | package bundle supported | NumPy core | supported |
| `is-similar` | package bundle supported | NumPy core | supported |
| `compare-boolean` | not qualified | SymPy is not packaged and no compatibility polyfill is injected | supported |
| `symbolic-equal` | not qualified | SymPy/LaTeX stack is not packaged | supported |
| `short-text-answer` | Pyodide route | `nltk` data, `gensim`/SciPy, and plotting stack | eval E2E verified |

Rules of thumb:
- Pure Python evaluator code and dependencies can be bundled with `tools/lf-bundle-python --include-root`.
- Native/WASI dependencies must already be present in the pinned Agent Python artifact.
- Heavy scientific/data/rendering stacks remain Pyodide-only unless they become first-class artifact requirements.

## Fixtures

### `boilerplate-python`
- **Source repo/folder:** `.demo-lambda-sources/evaluation-function-boilerplate-python`
- **Contains (runtime files only):** `evaluation_function/main.py`, `evaluation_function/__init__.py`,
  `evaluation_function/evaluation.py`, `evaluation_function/preview.py`
- **Compatibility behavior covered:**
  - Minimal package-style evaluator layout.
  - Uses `lf_toolkit.create_server(...)` registration with `server.eval(...)` and `server.preview(...)`.
  - Direct `lf_toolkit`-style result objects (`Result`, `Params`).

### `compare-boolean`
- **Source repo/folder:** `.demo-lambda-sources/compareBoolean`
- **Contains (runtime files only):** `evaluation_function/main.py`, `evaluation_function/__init__.py`,
  `evaluation_function/evaluation.py`, `evaluation_function/preview.py`, `evaluation_function/parse.py`,
  `evaluation_function/lex.py`, `evaluation_function/ast.py`
- **Compatibility behavior covered:**
  - Relative imports inside evaluator packages (e.g. `from .parse import ...`).
  - Rich parser/AST helper modules needed at runtime.
  - Preview/eval flow with real parse-side error handling (`FeedbackException`).
  - Historical parser coverage only; the current Agent Python path does not inject
    the deleted `ctypes` polyfill and has not qualified this fixture.

### `array-equal`
- **Source repo/folder:** `.demo-lambda-sources/ArrayEqual/app`
- **Contains (runtime files only):** `evaluation_function/evaluation.py`, `evaluation_function/main.py`,
  `evaluation_function_utils/errors.py`
- **Compatibility behavior covered:**
  - NumPy array construction and `np.allclose(...)`; current-artifact eval E2E verified.
  - Small compatibility shim for `evaluation_function_utils.errors.EvaluationException`.

### `is-similar`
- **Source repo/folder:** `.demo-lambda-sources/IsSimilar/app`
- **Contains (runtime files only):** `evaluation_function/evaluation.py`, `evaluation_function/main.py`
- **Compatibility behavior covered:**
  - NumPy scalar helper import (`from numpy import spacing`).
  - Numeric tolerance feedback fields returned through bundle normalization;
    current-artifact eval E2E verified.

### `symbolic-equal`
- **Source repo/folder:** `.demo-lambda-sources/SymbolicEqual/app`
- **Contains (runtime files only):** `evaluation_function/evaluation.py`, `evaluation_function/preview.py`,
  parser helpers, feedback messages, and minimal `main.py` registration.
- **Compatibility behavior covered:**
  - Larger real evaluator package copied from a flat `app/` layout into `evaluation_function/`.
  - Basic symbolic comparison via SymPy.
  - LaTeX preview via pure-Python `latex2sympy2` + `antlr4-python3-runtime` bundled through `--include-root`.
  - Bundler compatibility rewrite for old `antlr4-python3-runtime` imports (`typing.io` -> `typing`).

No `requirements.txt` files were copied. Extra pure-Python dependencies must be
embedded during Host-side bundling with `--include-root`; guest runtime paths are
not mounted.

## Notes
- Network-dependent and/or non-runtime artifacts were intentionally omitted: `.git/`, CI workflow files,
  Dockerfiles, tests, and unrelated docs.
- `scipy` and very heavy/data-bearing dependencies remain out of Agent Python scope; use Pyodide for those.
