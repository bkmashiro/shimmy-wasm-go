"""
eval-numpy: shimmy evaluation function using NumPy.

Demonstrates numpy-based grading running inside the CPython-WASI sandbox with
wasi-wheels numpy mounted via AllowedPaths / PYTHONPATH.

Supported methods
-----------------
eval    – numeric array comparison using np.allclose (default tolerances)
preview – render a human-readable preview of the submitted array

Request params
--------------
response : str   comma-separated floats, e.g. "1.0, 2.0, 3.0"
answer   : str   comma-separated expected floats
params   : dict  optional; recognised keys:
    rtol  (float) relative tolerance, default 1e-5
    atol  (float) absolute tolerance, default 1e-8
"""

import numpy as np


def _parse(s: str) -> np.ndarray:
    """Parse a comma-separated string of floats into a 1-D numpy array."""
    return np.array([float(x.strip()) for x in s.split(",")])


def _evaluate(response, answer, params=None):
    params = params or {}
    rtol = float(params.get("rtol", 1e-5))
    atol = float(params.get("atol", 1e-8))

    try:
        resp_arr = _parse(str(response))
        ans_arr = _parse(str(answer))
    except (ValueError, TypeError) as exc:
        return {
            "is_correct": False,
            "feedback": f"Parse error: {exc}",
        }

    if resp_arr.shape != ans_arr.shape:
        return {
            "is_correct": False,
            "feedback": (
                f"Shape mismatch: got {resp_arr.shape}, expected {ans_arr.shape}"
            ),
        }

    correct = bool(np.allclose(resp_arr, ans_arr, rtol=rtol, atol=atol))
    if correct:
        feedback = "Correct. Your answer matches the expected values."
    else:
        diff = np.abs(resp_arr - ans_arr)
        worst = int(np.argmax(diff))
        feedback = (
            f"Incorrect. Largest deviation at index {worst}: "
            f"got {resp_arr[worst]:.6g}, expected {ans_arr[worst]:.6g} "
            f"(diff={diff[worst]:.3e})."
        )

    return {"is_correct": correct, "feedback": feedback}


def _preview(response, answer, params=None):
    try:
        resp_arr = _parse(str(response))
        preview_str = f"Submitted array: {resp_arr.tolist()}"
    except Exception as exc:
        preview_str = f"Could not parse response: {exc}"

    return {"preview": preview_str}

def dispatch(method, payload):
    """Evaluator-owned method routing for the Python Reactor ABI."""
    if not isinstance(payload, dict):
        raise TypeError("payload must be a dict")
    if method == "eval":
        return _evaluate(payload.get("response"), payload.get("answer"), payload.get("params", {}))
    if method == "preview":
        return _preview(payload.get("response"), payload.get("answer"), payload.get("params", {}))
    raise LookupError("unsupported method: " + str(method))
